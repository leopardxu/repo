package workerpool

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestWorkerPool(t *testing.T) {
	pool := New(5)

	doneCount := 0
	tasks := 20
	resChan := make(chan TaskResult, tasks)

	// 用 WaitGroup 追踪所有接收 goroutine，确保 close(resChan) 前它们都已退出，
	// 避免 "send on closed channel"（接收者可能在 Wait 返回后仍向 resChan 发送）。
	var recvWG sync.WaitGroup
	recvWG.Add(tasks)

	for i := 0; i < tasks; i++ {
		idx := i
		res := pool.Submit(func() (interface{}, error) {
			time.Sleep(time.Millisecond * 10)
			return fmt.Sprintf("result %d", idx), nil
		})
		go func(r <-chan TaskResult) {
			defer recvWG.Done()
			resChan <- <-r
		}(res)
	}

	// 先等所有接收 goroutine 完成，再关闭 resChan
	go func() {
		recvWG.Wait()
		close(resChan)
	}()

	for res := range resChan {
		if res.Error != nil {
			t.Errorf("Unexpected error: %v", res.Error)
		}
		doneCount++
	}

	if doneCount != tasks {
		t.Errorf("Expected %d tasks done, got %d", tasks, doneCount)
	}
}

func TestWorkerPoolStop(t *testing.T) {
	pool := New(2)

	// submitting tasks
	resChan := make(chan TaskResult, 10)
	var recvWG sync.WaitGroup
	recvWG.Add(10)
	for i := 0; i < 10; i++ {
		res := pool.Submit(func() (interface{}, error) {
			time.Sleep(time.Millisecond * 30) // taking long time
			return nil, nil
		})
		go func(r <-chan TaskResult) {
			defer recvWG.Done()
			resChan <- <-r
		}(res)
	}

	time.Sleep(time.Millisecond * 15) // let a few tasks start
	pool.Stop()
	pool.Wait() // ensure Wait behaves properly after Stop
	recvWG.Wait()
	close(resChan)

	successCount := 0
	for res := range resChan {
		if res.Error == nil {
			successCount++
		}
	}

	// Stop 取消了未完成的任务，成功完成（无错误）的应少于 10
	if successCount == 10 {
		t.Errorf("Expected fewer than 10 completed tasks due to stop, got 10")
	}
}
