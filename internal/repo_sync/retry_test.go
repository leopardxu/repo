package repo_sync

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/leopardxu/repo-go/internal/git"
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

// TestAppendDiagnosticOnceNoDuplicate 验证 GitCommandError.Error() 已携带
// stderr 时分类路径不再重复追加同一诊断。
func TestAppendDiagnosticOnceNoDuplicate(t *testing.T) {
	const stderr = "fatal: unable to access 'https://example.com/': timed out"
	text := "git command error: 'git fetch': " + stderr

	got := appendDiagnosticOnce(text, stderr)
	if got != text {
		t.Errorf("appendDiagnosticOnce duplicated diagnostic:\n got: %q\nwant: %q", got, text)
	}
	if strings.Count(got, stderr) != 1 {
		t.Errorf("stderr appears %d times, want 1:\n%s", strings.Count(got, stderr), got)
	}

	// 缺失时仍会追加（保持原有分类依据）。
	const other = "fatal: Could not resolve host: example.com"
	got = appendDiagnosticOnce(text, other)
	if !strings.Contains(got, other) {
		t.Errorf("appendDiagnosticOnce should append missing diagnostic, got %q", got)
	}
}

// TestIsRetryableGitErrorUsesStderr 验证分类读取 git.GitCommandError 的 Stderr
// （git 诊断信息只在 stderr），并保留 %w 包装链。
func TestIsRetryableGitErrorUsesStderr(t *testing.T) {
	tests := []struct {
		name      string
		err       error
		wantRetry bool
	}{
		{
			name:      "network error in stderr",
			err:       &git.GitCommandError{Command: "git fetch", Stderr: "fatal: unable to access 'https://example.com/': timed out", ExitCode: 128},
			wantRetry: true,
		},
		{
			name:      "auth error in stderr",
			err:       &git.GitCommandError{Command: "git fetch", Stderr: "fatal: Authentication failed", ExitCode: 128},
			wantRetry: false,
		},
		{
			name:      "index lock in stderr",
			err:       &git.GitCommandError{Command: "git fetch", Stderr: "fatal: Unable to create '.git/index.lock': File exists", ExitCode: 128},
			wantRetry: true,
		},
		{
			name:      "ssh connection closed by remote host",
			err:       &git.GitCommandError{Command: "git clone", Stderr: "kex_exchange_identification: Connection closed by remote host\nConnection closed by 10.128.0.11 port 22", ExitCode: 128},
			wantRetry: true,
		},
		{
			name:      "ssh connection reset by peer",
			err:       &git.GitCommandError{Command: "git clone", Stderr: "kex_exchange_identification: read: Connection reset by peer", ExitCode: 128},
			wantRetry: true,
		},
		{
			name: "wrapped git error keeps chain",
			err: fmt.Errorf("fetch failed: %w",
				&git.GitCommandError{Command: "git fetch", Stderr: "Could not resolve host: example.com", ExitCode: 128}),
			wantRetry: true,
		},
		{
			name:      "plain non-git error text",
			err:       errors.New("temporarily unavailable"),
			wantRetry: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsRetryableGitError(tt.err); got != tt.wantRetry {
				t.Errorf("IsRetryableGitError(%v) = %v, want %v", tt.err, got, tt.wantRetry)
			}
		})
	}
}

// TestRetryWithBackoffZeroMaxRetries 验证 MaxRetries=0 只执行一次（不重试），
// 且 fn 执行期间 ctx 取消仍可通过 errors.Is 检测。
func TestRetryWithBackoffZeroMaxRetries(t *testing.T) {
	attempts := 0
	err := RetryWithBackoff(context.Background(), RetryOptions{
		MaxRetries: 0,
		BaseDelay:  time.Millisecond,
		MaxDelay:   time.Millisecond,
	}, func(_ int) error {
		attempts++
		return errors.New("boom")
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if attempts != 1 {
		t.Errorf("attempts = %d, want 1 (MaxRetries=0 must not retry)", attempts)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err = RetryWithBackoff(ctx, RetryOptions{MaxRetries: 0}, func(_ int) error {
		return errors.New("boom")
	})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error should wrap context.Canceled, got %v", err)
	}
}
