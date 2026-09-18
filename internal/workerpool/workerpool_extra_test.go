package workerpool

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestNewWithZeroWorkers(t *testing.T) {
	pool := New(0)
	if pool == nil {
		t.Fatal("New(0) returned nil")
	}
	defer pool.Stop()

	// Should still work with 1 worker (minimum)
	result, err := pool.SubmitAndWait(func() (interface{}, error) {
		return "ok", nil
	})

	if err != nil {
		t.Errorf("Unexpected error: %v", err)
	}
	if result != "ok" {
		t.Errorf("Expected 'ok', got %v", result)
	}
}

func TestNewWithNegativeWorkers(t *testing.T) {
	pool := New(-1)
	if pool == nil {
		t.Fatal("New(-1) returned nil")
	}
	defer pool.Stop()
}

func TestSubmitAndWait(t *testing.T) {
	pool := New(3)
	defer pool.Stop()

	result, err := pool.SubmitAndWait(func() (interface{}, error) {
		return 42, nil
	})

	if err != nil {
		t.Errorf("Unexpected error: %v", err)
	}
	if result != 42 {
		t.Errorf("Expected 42, got %v", result)
	}
}

func TestSubmitAndWaitWithError(t *testing.T) {
	pool := New(2)
	defer pool.Stop()

	expectedErr := errors.New("task failed")
	result, err := pool.SubmitAndWait(func() (interface{}, error) {
		return nil, expectedErr
	})

	if err != expectedErr {
		t.Errorf("Expected error %v, got %v", expectedErr, err)
	}
	if result != nil {
		t.Errorf("Expected nil result, got %v", result)
	}
}

func TestConcurrencyLimit(t *testing.T) {
	pool := New(3)
	defer pool.Stop()

	var concurrent int32
	var maxConcurrent int32

	tasks := 20
	var wg sync.WaitGroup
	wg.Add(tasks)

	for i := 0; i < tasks; i++ {
		res := pool.Submit(func() (interface{}, error) {
			current := atomic.AddInt32(&concurrent, 1)
			if current > atomic.LoadInt32(&maxConcurrent) {
				atomic.StoreInt32(&maxConcurrent, current)
			}
			time.Sleep(10 * time.Millisecond)
			atomic.AddInt32(&concurrent, -1)
			return nil, nil
		})
		go func(r <-chan TaskResult) {
			defer wg.Done()
			<-r
		}(res)
	}

	wg.Wait()

	peak := atomic.LoadInt32(&maxConcurrent)
	if peak > 3 {
		t.Errorf("Max concurrent tasks = %d, should not exceed 3", peak)
	}
}

func TestSubmitBatch(t *testing.T) {
	pool := New(4)
	defer pool.Stop()

	fns := make([]func() (interface{}, error), 10)
	for i := 0; i < 10; i++ {
		idx := i
		fns[i] = func() (interface{}, error) {
			return fmt.Sprintf("task-%d", idx), nil
		}
	}

	results := pool.SubmitBatch(fns)

	if len(results) != 10 {
		t.Fatalf("Expected 10 results, got %d", len(results))
	}

	for i, res := range results {
		if res.Error != nil {
			t.Errorf("Result %d error: %v", i, res.Error)
		}
		expected := fmt.Sprintf("task-%d", i)
		if res.Data != expected {
			t.Errorf("Result %d = %v, want %s", i, res.Data, expected)
		}
	}
}

func TestSubmitBatchWithErrors(t *testing.T) {
	pool := New(2)
	defer pool.Stop()

	fns := []func() (interface{}, error){
		func() (interface{}, error) { return "ok1", nil },
		func() (interface{}, error) { return nil, errors.New("error2") },
		func() (interface{}, error) { return "ok3", nil },
	}

	results := pool.SubmitBatch(fns)

	if len(results) != 3 {
		t.Fatalf("Expected 3 results, got %d", len(results))
	}

	if results[0].Data != "ok1" {
		t.Errorf("Result 0 = %v, want ok1", results[0].Data)
	}
	if results[1].Error == nil {
		t.Error("Result 1 should have error")
	}
	if results[2].Data != "ok3" {
		t.Errorf("Result 2 = %v, want ok3", results[2].Data)
	}
}

func TestStopIsIdempotent(_ *testing.T) {
	pool := New(2)

	// Multiple Stop calls should not panic
	pool.Stop()
	pool.Stop()
	pool.Stop()
}

func TestWaitWithNoTasks(t *testing.T) {
	pool := New(3)
	defer pool.Stop()

	// Wait with no submitted tasks should return immediately
	done := make(chan struct{})
	go func() {
		pool.Wait()
		close(done)
	}()

	select {
	case <-done:
		// OK
	case <-time.After(time.Second):
		t.Error("Wait() with no tasks should return immediately")
	}
}

func TestTaskExecutionOrder(t *testing.T) {
	pool := New(1) // Single worker for deterministic order
	defer pool.Stop()

	var mu sync.Mutex
	var results []int

	var wg sync.WaitGroup
	wg.Add(5)

	for i := 0; i < 5; i++ {
		idx := i
		res := pool.Submit(func() (interface{}, error) {
			mu.Lock()
			results = append(results, idx)
			mu.Unlock()
			return nil, nil
		})
		go func(r <-chan TaskResult) {
			defer wg.Done()
			<-r
		}(res)
	}

	wg.Wait()

	if len(results) != 5 {
		t.Fatalf("Expected 5 results, got %d", len(results))
	}

	// With a single worker, tasks should execute in submission order
	for i, v := range results {
		if v != i {
			t.Errorf("Results[%d] = %d, want %d (order not preserved with single worker)", i, v, i)
		}
	}
}

func TestSubmitAfterStop(t *testing.T) {
	pool := New(2)
	pool.Stop()

	res := pool.Submit(func() (interface{}, error) {
		return "should not run", nil
	})

	// Submit after Stop should return an error result (either immediately
	// via ctx.Done path or after drainer goroutine drains the task).
	// Use a generous timeout to account for goroutine scheduling.
	select {
	case r := <-res:
		if r.Error == nil {
			t.Error("Submit after Stop should return error result")
		}
	case <-time.After(5 * time.Second):
		t.Error("Submit after Stop should not block indefinitely")
	}
}
