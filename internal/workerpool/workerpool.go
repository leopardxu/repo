package workerpool

import (
	"context"
	"fmt"
	"sync"
)

// TaskResult 任务执行结果
type TaskResult struct {
	Error error
	Data  interface{}
}

// Task 任务定义
type Task struct {
	Fn   func() (interface{}, error)
	Done chan TaskResult
}

// WorkerPool 工作池
//
// 停止与提交的同步协议：
//   - Submit 持 submitMu 读锁完成 taskWG.Add(1) 与入队发送；
//   - Stop 先 cancel ctx 唤醒可能阻塞在发送上的 Submit（发送 select 同时监听
//     ctx.Done），再取写锁标记 stopped。因此 Stop 一定等到所有进行中的提交
//     结束（入队成功或取消返回）后才开始排空，不会出现"任务已计数但未入队"
//     或"停止后仍入队"的窗口，也不会出现 Submit 等缓冲区、Stop 等读锁的死锁；
//   - 停止后 Submit 直接返回取消错误，不再计数、不再入队；
//   - Stop 排空缓冲区，为未被 worker 接收的任务回填取消结果并递减 taskWG，
//     保证 Wait 不会悬挂。
type WorkerPool struct {
	workers int
	tasks   chan Task
	once    sync.Once
	ctx     context.Context
	cancel  context.CancelFunc

	// submitMu 保护 stopped 标志与"检查+计数+入队"这一临界区。
	submitMu sync.RWMutex
	stopped  bool

	// taskWG 在 Submit 入队前计数、任务到达终态（执行完、取消回填）时递减。
	// 不能在 worker 接收后才 Add：Submit 后立即 Wait 且无调度让出时计数仍为 0，
	// Wait 直接返回，缓冲区任务被静默丢弃。
	taskWG sync.WaitGroup
}

// New 创建工作池
func New(workers int) *WorkerPool {
	if workers <= 0 {
		workers = 1
	}

	ctx, cancel := context.WithCancel(context.Background())

	pool := &WorkerPool{
		workers: workers,
		tasks:   make(chan Task, workers*2),
		ctx:     ctx,
		cancel:  cancel,
	}
	pool.start()
	return pool
}

// start 启动工作协程
// 每个 worker 直接在自己的 goroutine 中执行任务，确保并发度 = workers 数
func (p *WorkerPool) start() {
	for range p.workers {
		go func() {
			for {
				select {
				case task, ok := <-p.tasks:
					if !ok {
						return
					}
					// 取消期间不再执行新任务，直接回填取消错误（避免 done 永久阻塞接收者）
					if p.ctx.Err() != nil {
						p.sendResult(task.Done, TaskResult{Error: p.ctx.Err()})
						p.taskWG.Done()
						continue
					}
					p.execute(task)
				case <-p.ctx.Done():
					return
				}
			}
		}()
	}
}

// execute 执行单个任务并保证恰好一次 taskWG.Done。
// Fn panic 时恢复并回填错误，避免整个进程崩溃或 Wait 永久悬挂。
func (p *WorkerPool) execute(task Task) {
	defer p.taskWG.Done()
	defer func() {
		if rec := recover(); rec != nil {
			p.sendResult(task.Done, TaskResult{Error: fmt.Errorf("workerpool: task panicked: %v", rec)})
		}
	}()

	result, err := task.Fn()
	// 非阻塞发送结果：done 容量为 1，若已被取消路径填充则丢弃。
	// 不关闭 done，避免 worker 与取消路径竞争关闭导致 send on closed channel。
	p.sendResult(task.Done, TaskResult{Error: err, Data: result})
}

// sendResult 非阻塞地向 done 发送结果。done 容量为 1，满时丢弃。
func (p *WorkerPool) sendResult(done chan TaskResult, r TaskResult) {
	select {
	case done <- r:
	default:
	}
}

// Submit 提交任务
func (p *WorkerPool) Submit(fn func() (interface{}, error)) <-chan TaskResult {
	done := make(chan TaskResult, 1)

	p.submitMu.RLock()
	if p.stopped {
		p.submitMu.RUnlock()
		p.sendResult(done, TaskResult{Error: p.ctx.Err()})
		return done
	}

	p.taskWG.Add(1)
	// 持读锁发送：Stop 需要写锁，会等本次提交入队完成。
	// select 同时监听 ctx：Stop 在取写锁前先 cancel，阻塞中的发送可由此退出，
	// 不会出现"Submit 等缓冲区、Stop 等读锁"的相互等待。
	select {
	case p.tasks <- Task{Fn: fn, Done: done}:
	case <-p.ctx.Done():
		p.sendResult(done, TaskResult{Error: p.ctx.Err()})
		p.taskWG.Done()
	}
	p.submitMu.RUnlock()

	return done
}

// SubmitAndWait 提交任务并等待完成
func (p *WorkerPool) SubmitAndWait(fn func() (interface{}, error)) (interface{}, error) {
	resultChan := p.Submit(fn)
	result := <-resultChan
	return result.Data, result.Error
}

// Wait 等待所有已提交的任务完成。
//
// 注意：本方法不关闭 p.tasks。所有已计数的任务无论被执行还是被 Stop 排空，
// 都会递减 taskWG，因此 Stop 之后调用 Wait 不会悬挂。
func (p *WorkerPool) Wait() {
	p.taskWG.Wait()
}

// Stop 停止工作池（可重复调用）。
//
// 先 cancel 唤醒可能阻塞在入队发送上的 Submit，再取写锁标记 stopped：此时
// 所有进行中的提交都已结束（入队成功或走取消路径），不会再有新任务入队；
// 随后排空缓冲区，为剩余任务回填取消错误并递减 taskWG。
func (p *WorkerPool) Stop() {
	p.once.Do(func() {
		p.cancel()

		p.submitMu.Lock()
		p.stopped = true
		p.submitMu.Unlock()

		p.drainBuffered()
	})
}

// drainBuffered 排空 tasks 缓冲区中未被 worker 接收的任务。
// 停止后不会再有生产者，worker 也可能已退出，剩余任务必须由本路径回填结果。
func (p *WorkerPool) drainBuffered() {
	for {
		select {
		case task, ok := <-p.tasks:
			if !ok {
				return
			}
			p.sendResult(task.Done, TaskResult{Error: p.ctx.Err()})
			p.taskWG.Done()
		default:
			return
		}
	}
}

// SubmitBatch 提交一批任务并等待全部完成
func (p *WorkerPool) SubmitBatch(fns []func() (interface{}, error)) []TaskResult {
	results := make([]TaskResult, len(fns))
	var wg sync.WaitGroup

	for i, fn := range fns {
		wg.Add(1)
		go func(index int, f func() (interface{}, error)) {
			defer wg.Done()
			result, err := p.SubmitAndWait(f)
			results[index] = TaskResult{Error: err, Data: result}
		}(i, fn)
	}

	wg.Wait()
	return results
}
