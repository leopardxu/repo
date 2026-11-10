package repo_sync

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/leopardxu/repo-go/internal/git"
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
//
// MaxRetries 为 0 表示不重试（仅执行一次），与上游 repo 的
// --retry-fetches 默认值 0 对齐；需要默认重试次数时使用 DefaultRetryOptions。
func RetryWithBackoff(ctx context.Context, opts RetryOptions, fn func(attempt int) error) error {
	if opts.MaxRetries < 0 {
		opts.MaxRetries = 0
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

		// fn 执行期间 ctx 被取消（fail-fast/外部取消）：立即返回并保留
		// 取消原因，避免在零重试（MaxRetries=0）时丢失 context 语义。
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fmt.Errorf("操作取消 (上次错误: %w): %w", lastErr, ctxErr)
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

// IsRetryableGitError 判断 Git 错误是否可以通过重试解决。
// 优先通过 errors.As 提取 *git.GitCommandError 的 Stderr（git 的诊断信息只在
// stderr 中），同时保留 %w 链上其它错误的文本，避免包装后丢失判定依据。
func IsRetryableGitError(err error) bool {
	if err == nil {
		return false
	}

	// 收集包含 %w 链的完整错误文本；GitCommandError 的诊断已包含在
	// Error() 中（InfraFix5 的 runner 契约），仅在缺失时补充，避免重复。
	text := err.Error()
	var gitErr *git.GitCommandError
	if errors.As(err, &gitErr) {
		text = appendDiagnosticOnce(text, gitErr.Stderr)
		text = appendDiagnosticOnce(text, gitErr.Stdout)
	}
	return isRetryableGitErrorText(text)
}

// appendDiagnosticOnce 在 text 尚未包含 diagnostic 时追加，避免
// GitCommandError.Error() 已携带 stderr/stdout 时重复拼接。
func appendDiagnosticOnce(text, diagnostic string) string {
	diagnostic = strings.TrimSpace(diagnostic)
	if diagnostic == "" || strings.Contains(text, diagnostic) {
		return text
	}
	return text + "\n" + diagnostic
}

// isRetryableGitErrorText 按 git 输出文本判定错误是否可重试。
func isRetryableGitErrorText(text string) bool {
	lower := strings.ToLower(text)
	// 解锁/锁定类错误优先判定：它们通常包含 "File exists" 等通用文本，
	// 必须先于网络/不可重试规则匹配，避免被误分类。
	if strings.Contains(lower, "index.lock") ||
		(strings.Contains(lower, "unable to create") && strings.Contains(lower, "file exists")) ||
		strings.Contains(lower, "unable to lock") {
		return true
	}

	// 不可重试的错误：认证/仓库不存在/引用不存在等，重试无意义。
	nonRetryableErrors := []string{
		"does not appear to be a git repository",
		"repository not found",
		"authentication failed",
		"permission denied",
		"did not match any file(s) known to git",
		"unknown revision",
		"reference is not a tree",
	}
	for _, nre := range nonRetryableErrors {
		if strings.Contains(lower, nre) {
			return false
		}
	}

	// 网络错误 - 可重试
	networkErrors := []string{
		"fatal: unable to access",
		"could not resolve host",
		"timed out",
		"connection refused",
		"temporarily unavailable",
		"connection reset by peer",
		"connection closed by remote host",
		"connection closed",
		"kex_exchange_identification",
		"failed to connect",
		"operation timed out",
		"temporary failure in name resolution",
		"the remote end hung up unexpectedly",
		"early eof",
		"rpc failed",
		"network is unreachable",
		"no route to host",
	}
	for _, ne := range networkErrors {
		if strings.Contains(lower, ne) {
			return true
		}
	}

	// exit status 128 默认可重试（排除上述不可重试项后）。
	// GitCommandError 的 Error() 文本为 "exit code 128"，普通 exec 错误为
	// "exit status 128"，两者都要识别。
	if strings.Contains(lower, "exit status 128") || strings.Contains(lower, "exit code 128") {
		return true
	}

	return false
}
