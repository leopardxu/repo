package workerpool

import (
	"sync"
	"testing"
)

// TestSubmitWaitRunsAllTasks 验证 Submit 后立即 Wait 时所有任务都会被执行。
// 回归背景：worker goroutine 的 select 在 tasks 与 quit/ctx.Done 之间偏向后者的
// 就绪分支，Submit 与 Wait 在主 goroutine 连续执行且任务极快时，worker 可能先
// 观察 ctx.Done（Stop 取消）而退出，导致任务静默丢弃（ran=0）。
// 该测试多次重复以放大竞态窗口，任何一轮丢任务都应失败。
func TestSubmitWaitRunsAllTasks(t *testing.T) {
	t.Parallel()

	const rounds = 50
	const taskCount = 3

	for round := 0; round < rounds; round++ {
		pool := New(4)
		var mu sync.Mutex
		ran := 0
		for i := 0; i < taskCount; i++ {
			pool.Submit(func() (interface{}, error) {
				mu.Lock()
				ran++
				mu.Unlock()
				return nil, nil
			})
		}
		pool.Wait()
		got := func() int {
			mu.Lock()
			defer mu.Unlock()
			return ran
		}()
		pool.Stop()
		if got != taskCount {
			t.Fatalf("round %d: executed %d tasks, want %d (tasks silently dropped)", round, got, taskCount)
		}
	}
}
