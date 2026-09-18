package workerpool

import (
	"context"
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
type WorkerPool struct {
	workers int
	tasks   chan Task
	once    sync.Once
	quit    chan struct{}
	ctx     context.Context
	cancel  context.CancelFunc
	// taskWG 在 Submit 入队前计数、任务到达终态（执行完或被取消回填）时递减。
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
		quit:    make(chan struct{}),
		ctx:     ctx,
		cancel:  cancel,
	}
	pool.start()
	pool.startDrainer()
	return pool
}

// startDrainer 在 ctx 取消后排空 p.tasks 中未被 worker 接收的任务，
// 为每个任务回填取消错误，避免其 done 通道永久阻塞接收者。
func (p *WorkerPool) startDrainer() {
	go func() {
		<-p.ctx.Done()
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
	}()
}

// start 启动工作协程
// 每个 worker 直接在自己的 goroutine 中执行任务，确保并发度 = workers 数
func (p *WorkerPool) start() {
	for i := 0; i < p.workers; i++ {
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
					// 直接在 worker goroutine 中执行任务，不再启动额外 goroutine
					// 这样确保同时执行的任务数不会超过 workers 数
					result, err := task.Fn()
					// 非阻塞发送结果：done 容量为 1，若已被取消路径填充则丢弃，
					// 避免 send on closed channel panic（done 永不关闭）。
					p.sendResult(task.Done, TaskResult{Error: err, Data: result})
					p.taskWG.Done()
				case <-p.quit:
					return
				case <-p.ctx.Done():
					return
				}
			}
		}()
	}
}

// sendResult 非阻塞地向 done 发送结果。done 容量为 1，满时丢弃（取消路径可能已填充）。
// 不关闭 done，避免 worker 与取消路径竞争关闭导致 send on closed channel。
func (p *WorkerPool) sendResult(done chan TaskResult, r TaskResult) {
	select {
	case done <- r:
	default:
	}
}

// Submit 提交任务
func (p *WorkerPool) Submit(fn func() (interface{}, error)) <-chan TaskResult {
	// 已取消时快速返回错误结果，避免任务入队后既无 worker 消费、
	// 也可能错过 drainer 的排空窗口（drainer 只做单趟非阻塞排空）。
	if err := p.ctx.Err(); err != nil {
		done := make(chan TaskResult, 1)
		p.sendResult(done, TaskResult{Error: err})
		return done
	}

	p.taskWG.Add(1)
	done := make(chan TaskResult, 1)
	task := Task{
		Fn:   fn,
		Done: done,
	}

	select {
	case p.tasks <- task:
	case <-p.ctx.Done():
		// 已取消：非阻塞填充错误结果（与 worker 路径互斥，二者均不关闭 done）
		p.sendResult(done, TaskResult{Error: p.ctx.Err()})
		// 任务未入队，由本路径负责计数递减
		p.taskWG.Done()
		return done
	}

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
// 注意：本方法不再关闭 p.tasks（关闭会导致仍在阻塞提交的 Submit 触发
// "send on closed channel" panic）。tasks 通道的关闭统一由 Stop() 负责。
// 调用方应在所有 Submit 返回后再调用 Wait，以确保所有任务被 worker 接收。
func (p *WorkerPool) Wait() {
	p.taskWG.Wait()
}

// Stop 停止工作池。
//
// 通过取消 ctx 与关闭 quit 通知 worker 退出；Submit 中阻塞的发送者会因
// ctx 取消而走取消路径返回。不再关闭 p.tasks——关闭会让仍在阻塞提交的
// Submit 触发 "send on closed channel" panic。
func (p *WorkerPool) Stop() {
	p.once.Do(func() {
		p.cancel()
		close(p.quit)
	})
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
