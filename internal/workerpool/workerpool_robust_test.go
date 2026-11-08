package workerpool

import (
	"sync"
	"testing"
	"time"
)

// TestStopSubmitRaceNoDeadlock 反复并发 Submit 与 Stop，验证不会永久阻塞：
// Submit 持读锁入队、Stop 持写锁停止并排空，Wait 在所有任务回填后返回。
func TestStopSubmitRaceNoDeadlock(t *testing.T) {
	const rounds = 30

	for round := range rounds {
		pool := New(2)

		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			for range 20 {
				res := pool.Submit(func() (interface{}, error) { return nil, nil })
				select {
				case <-res:
				case <-time.After(5 * time.Second):
					t.Errorf("round %d: waiting for task result timed out", round)
					return
				}
			}
		}()
		go func() {
			defer wg.Done()
			pool.Stop()
		}()

		done := make(chan struct{})
		go func() {
			wg.Wait()
			pool.Wait()
			close(done)
		}()

		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatalf("round %d: Submit/Stop/Wait deadlocked", round)
		}
		pool.Stop()
	}
}

// TestPanicInTaskIsRecovered 验证任务 panic 被恢复、回填错误，且池仍可继续使用，
// Wait 不会悬挂、进程不会崩溃。
func TestPanicInTaskIsRecovered(t *testing.T) {
	pool := New(1)
	defer pool.Stop()

	res := pool.Submit(func() (interface{}, error) {
		panic("boom")
	})

	select {
	case r := <-res:
		if r.Error == nil {
			t.Fatal("panicking task should report an error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("panicking task did not return a result (Wait would hang)")
	}

	// 池在 panic 后应继续可用
	value, err := pool.SubmitAndWait(func() (interface{}, error) { return "ok", nil })
	if err != nil {
		t.Fatalf("pool unusable after panic: %v", err)
	}
	if value != "ok" {
		t.Errorf("value = %v, want ok", value)
	}
}

// TestWaitAfterStopDrainsBuffered 验证 Stop 时仍在缓冲区的任务被回填取消结果，
// 调用方读取结果不会阻塞，Wait 也不会悬挂。
func TestWaitAfterStopDrainsBuffered(t *testing.T) {
	pool := New(1)

	release := make(chan struct{})
	started := make(chan struct{})
	first := pool.Submit(func() (interface{}, error) {
		close(started)
		<-release
		return nil, nil
	})

	// 等待第一个任务真正占用唯一 worker，确保后续任务进入缓冲区
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("first task did not start")
	}

	results := make([]<-chan TaskResult, 0, 2)
	for range 2 {
		results = append(results, pool.Submit(func() (interface{}, error) { return nil, nil }))
	}

	pool.Stop()
	close(release)

	done := make(chan struct{})
	go func() {
		pool.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Wait() hung after Stop with buffered tasks")
	}

	for i, res := range append(results, first) {
		select {
		case <-res:
		case <-time.After(2 * time.Second):
			t.Fatalf("result %d was never delivered", i)
		}
	}
}

// TestStopAfterAllTasksStillReturnsResults 验证正常提交后 Stop，所有任务都可达终态。
func TestStopAfterAllTasksStillReturnsResults(t *testing.T) {
	pool := New(3)
	results := make([]<-chan TaskResult, 0, 10)
	for range 10 {
		results = append(results, pool.Submit(func() (interface{}, error) { return 1, nil }))
	}
	pool.Stop()

	for i, res := range results {
		select {
		case <-res:
		case <-time.After(5 * time.Second):
			t.Fatalf("result %d never delivered after Stop", i)
		}
	}
	pool.Wait()
}
