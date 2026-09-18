package repo_sync

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// RetryOptions 重试配置选项
type RetryOptions struct {
	MaxRetries  int                  // 最大重试次数（不含首次执行）
	BaseDelay   time.Duration        // 基础延迟
	MaxDelay    time.Duration        // 最大延迟
	ShouldRetry func(err error) bool // 判断错误是否可重试
}

// DefaultRetryOptions 返回默认的重试配置
func DefaultRetryOptions() RetryOptions {
	return RetryOptions{
		MaxRetries:  3,
		BaseDelay:   2 * time.Second,
		MaxDelay:    30 * time.Second,
		ShouldRetry: IsRetryableGitError,
	}
}

// RetryWithBackoff 使用指数退避策略执行可重试的操作
// fn 返回 error 时，将根据 ShouldRetry 判定是否重试
// 如果 ctx 被取消，会立即返回
func RetryWithBackoff(ctx context.Context, opts RetryOptions, fn func(attempt int) error) error {
	if opts.MaxRetries <= 0 {
		opts.MaxRetries = 3
	}
	if opts.BaseDelay <= 0 {
		opts.BaseDelay = 2 * time.Second
	}
	if opts.MaxDelay <= 0 {
		opts.MaxDelay = 30 * time.Second
	}
	if opts.ShouldRetry == nil {
		opts.ShouldRetry = IsRetryableGitError
	}

	var lastErr error
	for attempt := 0; attempt <= opts.MaxRetries; attempt++ {
		// 检查 context 是否已取消
		select {
		case <-ctx.Done():
			if lastErr != nil {
				// ctx.Err() 也用 %w 包装，保证 errors.Is(err, context.Canceled) 可检测
				return fmt.Errorf("操作取消 (上次错误: %w): %w", lastErr, ctx.Err())
			}
			return ctx.Err()
		default:
		}

		// 非首次尝试时等待退避时间
		if attempt > 0 {
			delay := calculateBackoff(attempt, opts.BaseDelay, opts.MaxDelay)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(delay):
			}
		}

		lastErr = fn(attempt)
		if lastErr == nil {
			return nil // 成功
		}

		// 最后一次尝试不检查是否可重试
		if attempt >= opts.MaxRetries {
			break
		}

		// 判断是否应该重试
		if !opts.ShouldRetry(lastErr) {
			break // 不可重试的错误，直接返回
		}
	}

	return lastErr
}

// calculateBackoff 计算指数退避延迟
func calculateBackoff(attempt int, baseDelay, maxDelay time.Duration) time.Duration {
	delay := baseDelay
	for i := 1; i < attempt; i++ {
		delay *= 2
		if delay > maxDelay {
			delay = maxDelay
			break
		}
	}
	return delay
}

// IsRetryableGitError 判断 Git 错误是否可以通过重试解决
func IsRetryableGitError(err error) bool {
	if err == nil {
		return false
	}
	errMsg := err.Error()

	// 网络错误 - 可重试
	networkErrors := []string{
		"fatal: unable to access",
		"Could not resolve host",
		"timed out",
		"connection refused",
		"temporarily unavailable",
		"Connection reset by peer",
		"Failed to connect",
		"Operation timed out",
		"Temporary failure in name resolution",
		"the remote end hung up unexpectedly",
	}
	for _, ne := range networkErrors {
		if strings.Contains(errMsg, ne) {
			return true
		}
	}

	// 锁定错误 - 可重试
	if strings.Contains(errMsg, "index.lock") ||
		(strings.Contains(errMsg, "Unable to create") && strings.Contains(errMsg, "File exists")) ||
		strings.Contains(errMsg, "Unable to lock") {
		return true
	}

	// 不可重试的错误
	nonRetryableErrors := []string{
		"does not appear to be a git repository",
		"repository not found",
		"authentication failed",
		"Permission denied",
		"did not match any file(s) known to git",
		"unknown revision",
		"reference is not a tree",
	}
	for _, nre := range nonRetryableErrors {
		if strings.Contains(errMsg, nre) {
			return false
		}
	}

	// exit status 128 默认可重试（排除上述不可重试项后）
	if strings.Contains(errMsg, "exit status 128") {
		return true
	}

	return false
}
