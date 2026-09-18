package repo_sync

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestRetryWithBackoff_Success(t *testing.T) {
	opts := DefaultRetryOptions()
	opts.BaseDelay = time.Millisecond
	opts.MaxDelay = time.Millisecond * 5
	opts.ShouldRetry = func(_ error) bool { return true }

	attempts := 0
	err := RetryWithBackoff(context.Background(), opts, func(attempt int) error {
		attempts++
		if attempt < 2 {
			return errors.New("temporary error")
		}
		return nil
	})

	if err != nil {
		t.Errorf("Expected success, got error: %v", err)
	}
	if attempts != 3 { // attempt is 0, 1, 2 = 3 tries
		t.Errorf("Expected 3 attempts, got %d", attempts)
	}
}

func TestRetryWithBackoff_ContextTimeout(t *testing.T) {
	opts := DefaultRetryOptions()
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond*50)
	defer cancel()

	err := RetryWithBackoff(ctx, opts, func(_ int) error {
		time.Sleep(time.Millisecond * 100)
		return errors.New("timeout")
	})

	if err == nil {
		t.Error("Expected context error, got nil")
	}
}
