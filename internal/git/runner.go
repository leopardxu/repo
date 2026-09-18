package git

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/leopardxu/repo-go/internal/config"
	"github.com/leopardxu/repo-go/internal/logger"
)

// 包级别的日志记录器
var log logger.Logger = logger.NewDefaultLogger()
var logMu sync.RWMutex

// SetLogger 设置日志记录器（线程安全）
func SetLogger(l logger.Logger) {
	logMu.Lock()
	defer logMu.Unlock()
	log = l
}

// GitCommandError 表示Git命令执行错误
type GitCommandError struct { //nolint:revive // 命名遵循 CLAUDE.md 约定的 XxxError 错误类型形态，保持包内 API 稳定
	Command  string
	Dir      string
	Err      error
	Stdout   string
	Stderr   string
	ExitCode int
}

func (e *GitCommandError) Error() string {
	if e.ExitCode != 0 {
		return fmt.Sprintf("git command error: '%s' in dir '%s': exit code %d: %v", e.Command, e.Dir, e.ExitCode, e.Err)
	}
	return fmt.Sprintf("git command error: '%s' in dir '%s': %v", e.Command, e.Dir, e.Err)
}

func (e *GitCommandError) Unwrap() error {
	return e.Err
}

// defaultRunner 是默认的Git命令运行器实
type defaultRunner struct {
	Verbose     bool
	Quiet       bool
	MaxRetries  int
	RetryDelay  time.Duration
	concurrency int
	semaphore   chan struct{}
	mutex       sync.RWMutex
	timeout     time.Duration     // 默认超时时间
	environment map[string]string // 环境变量
}

// SetVerbose 设置是否显示详细输出
func (r *defaultRunner) SetVerbose(verbose bool) {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	r.Verbose = verbose
}

// SetQuiet 设置是否静默运行
func (r *defaultRunner) SetQuiet(quiet bool) {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	r.Quiet = quiet
}

// SetMaxRetries 设置最大重试次
func (r *defaultRunner) SetMaxRetries(retries int) {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	r.MaxRetries = retries
}

// SetRetryDelay 设置重试延迟
func (r *defaultRunner) SetRetryDelay(delay time.Duration) {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	r.RetryDelay = delay
}

// SetConcurrency 设置并发
func (r *defaultRunner) SetConcurrency(concurrency int) {
	r.mutex.Lock()
	defer r.mutex.Unlock()

	// 不关闭旧信号量（避免并发使用中触发 send on closed channel panic）
	// 直接替换为新通道，旧通道由 GC 回收

	// 设置新的并发
	r.concurrency = concurrency
	if concurrency > 0 {
		r.semaphore = make(chan struct{}, concurrency)
	} else {
		r.semaphore = nil
	}
}

// SetTimeout 设置默认超时时间
func (r *defaultRunner) SetTimeout(timeout time.Duration) {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	r.timeout = timeout
}

// SetEnvironment 设置环境变量
func (r *defaultRunner) SetEnvironment(env map[string]string) {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	r.environment = env
}

// GetEnvironment 获取环境变量
func (r *defaultRunner) GetEnvironment() map[string]string {
	r.mutex.RLock()
	defer r.mutex.RUnlock()
	return r.environment
}

// Run 执行Git命令
func (r *defaultRunner) Run(args ...string) ([]byte, error) {
	return r.runGitCommand("", 0, args...)
}

// RunInDir 在指定目录执行Git命令
func (r *defaultRunner) RunInDir(dir string, args ...string) ([]byte, error) {
	return r.runGitCommand(dir, 0, args...)
}

// RunWithTimeout 在指定目录执行Git命令并设置超
func (r *defaultRunner) RunWithTimeout(timeout time.Duration, args ...string) ([]byte, error) {
	return r.runGitCommand("", timeout, args...)
}

// RunInDirWithTimeout 在指定目录执行Git命令并设置超
func (r *defaultRunner) RunInDirWithTimeout(dir string, timeout time.Duration, args ...string) ([]byte, error) {
	return r.runGitCommand(dir, timeout, args...)
}

// runGitCommand 是执git 命令的内部辅助函
func (r *defaultRunner) runGitCommand(dir string, timeout time.Duration, args ...string) ([]byte, error) {
	// 获取并发控制信号
	r.mutex.RLock()
	semaphore := r.semaphore
	maxRetries := r.MaxRetries
	retryDelay := r.RetryDelay
	verbose := r.Verbose
	quiet := r.Quiet
	defaultTimeout := r.timeout
	env := r.environment
	r.mutex.RUnlock()

	// 如果没有显式设置超时，使用默认超时
	if timeout == 0 {
		timeout = defaultTimeout
	}

	// 检查REPO_TRACE环境变量
	repoTrace := os.Getenv("REPO_TRACE") != ""
	if repoTrace {
		// 如果设置了REPO_TRACE，强制启用详细输出
		verbose = true
	}

	// 如果设置了并发控
	if semaphore != nil {
		semaphore <- struct{}{}
		defer func() { <-semaphore }()
	}

	cmdArgs := append([]string{}, args...)
	cmdStr := fmt.Sprintf("git %s", strings.Join(cmdArgs, " "))

	if verbose {
		log.Info("执行: %s 在目'%s'", cmdStr, dir)
	} else {
		log.Debug("执行: %s 在目'%s'", cmdStr, dir)
	}

	// 执行命令，支持重
	var lastErr error
	var stdoutBytes []byte
	var stderrBytes []byte
	var exitCode int

	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			log.Debug("重试Git命令 (尝试 %d/%d): %s", attempt, maxRetries, cmdStr)
			time.Sleep(retryDelay)
		}

		// 准备命令
		ctx := context.Background()
		var cancel context.CancelFunc

		if timeout > 0 {
			ctx, cancel = context.WithTimeout(ctx, timeout)
			// 注意：不能在这里defer cancel()，因为我们需要在下面手动调用
		}

		cmd := exec.CommandContext(ctx, "git", cmdArgs...)

		if dir != "" {
			cmd.Dir = dir
		}

		// 设置环境变量：保留系统环境（os.Environ），追加/覆盖用户自定义变量。
		// 对齐上游 repo 行为：git 子进程继承父进程环境（含 HTTP_PROXY、GIT_*、SSH_* 等）。
		if env != nil {
			cmd.Env = os.Environ()
			for k, v := range env {
				cmd.Env = append(cmd.Env, fmt.Sprintf("%s=%s", k, v))
			}
		}

		// 如果设置了REPO_TRACE，记录环境变量
		if repoTrace {
			log.Trace("环境变量: %v", cmd.Env)
		}

		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr

		// 执行命令
		err := cmd.Run()
		stdoutBytes = stdout.Bytes()
		stderrBytes = stderr.Bytes()

		// 手动调用cancel()确保资源释放
		if cancel != nil {
			cancel()
		}

		// 处理输出
		if (verbose || repoTrace) && len(stdoutBytes) > 0 {
			if repoTrace {
				log.Trace("标准输出: %s", string(stdoutBytes))
			} else {
				log.Debug("标准输出: %s", string(stdoutBytes))
			}
		}

		if len(stderrBytes) > 0 && (!quiet || repoTrace) {
			// 只有在非静默模式下或启用REPO_TRACE时才记录stderr
			if err != nil {
				if repoTrace {
					log.Trace("标准错误: %s", string(stderrBytes))
				} else {
					log.Warn("标准错误: %s", string(stderrBytes))
				}
			} else if verbose || repoTrace {
				// 如果命令成功但有stderr输出，且处于详细模式或启用REPO_TRACE，则记录
				if repoTrace {
					log.Trace("标准错误: %s", string(stderrBytes))
				} else {
					log.Debug("标准错误: %s", string(stderrBytes))
				}
			}
		}

		// 处理错误
		if err != nil {
			exitCode = 1 // 默认错误
			if exitErr, ok := err.(*exec.ExitError); ok {
				exitCode = exitErr.ExitCode()
			}

			// 构建错误信息
			lastErr = &GitCommandError{
				Command:  cmdStr,
				Dir:      dir,
				Err:      err,
				Stdout:   string(stdoutBytes),
				Stderr:   string(stderrBytes),
				ExitCode: exitCode,
			}

			// 检查是否应该重
			if !shouldRetry(exitCode, string(stderrBytes)) || attempt >= maxRetries {
				if attempt > 0 {
					log.Warn("Git命令failed，已重试 %d  %s", attempt, cmdStr)
				}
				break
			}
		} else {
			// 命令成功
			if attempt > 0 {
				log.Info("Git命令在第 %d 次尝试后成功: %s", attempt+1, cmdStr)
			}
			return stdoutBytes, nil
		}
	}

	return stdoutBytes, lastErr
}

// shouldRetry determines whether a git command failure should be retried.
// Network errors, lock contention, and transient failures are retryable.
// Authentication failures, missing repositories, and unknown revisions are not.
func shouldRetry(_ int, stderr string) bool {
	errMsg := stderr

	// Network errors - retryable
	networkErrors := []string{
		"Could not resolve host",
		"Failed to connect",
		"Connection timed out",
		"Connection reset by peer",
		"Operation timed out",
		"Temporary failure in name resolution",
		"fatal: the remote end hung up unexpectedly",
		"fatal: unable to access",
		"timed out",
		"connection refused",
		"temporarily unavailable",
	}
	for _, ne := range networkErrors {
		if strings.Contains(errMsg, ne) {
			return true
		}
	}

	// Lock contention errors - retryable
	if strings.Contains(errMsg, "index.lock") ||
		strings.Contains(errMsg, "Unable to lock") ||
		(strings.Contains(errMsg, "Unable to create") && strings.Contains(errMsg, "File exists")) ||
		(strings.Contains(errMsg, "already exists") && strings.Contains(errMsg, "lock")) {
		return true
	}

	// Non-retryable errors
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

	// exit status 128 is retryable by default (after excluding non-retryable above)
	if strings.Contains(errMsg, "exit status 128") {
		return true
	}

	return false
}

// NewRunner 创建一个新Git 命令运行
func NewRunner() Runner {
	return &defaultRunner{
		MaxRetries:  3,
		RetryDelay:  time.Second * 2,
		concurrency: 5,
		semaphore:   make(chan struct{}, 5),
		timeout:     30 * time.Minute, // 默认30分钟超时
	}
}

// NewCommandRunnerWithConfig 根据配置创建Git命令运行
func NewCommandRunnerWithConfig(cfg *config.Config) (Runner, error) {
	runner := &defaultRunner{
		MaxRetries:  3,
		RetryDelay:  time.Second * 2,
		concurrency: 5,
		semaphore:   make(chan struct{}, 5),
		timeout:     30 * time.Minute, // 默认30分钟超时
	}

	if cfg != nil {
		runner.Verbose = cfg.Verbose
		runner.Quiet = cfg.Quiet
		// 根据配置调整并发数
		if cfg.Jobs > 0 {
			runner.SetConcurrency(cfg.Jobs)
		}
	}

	return runner, nil
}

// Runner 定义了运行Git命令的接
type Runner interface {
	Run(args ...string) ([]byte, error)
	RunInDir(dir string, args ...string) ([]byte, error)
	RunWithTimeout(timeout time.Duration, args ...string) ([]byte, error)
	RunInDirWithTimeout(dir string, timeout time.Duration, args ...string) ([]byte, error)
	SetVerbose(verbose bool)
	SetQuiet(quiet bool)
	SetMaxRetries(retries int)
	SetRetryDelay(delay time.Duration)
	SetConcurrency(concurrency int)
}

// CommandRunner 是Runner的别名，保持向后兼容
type CommandRunner = Runner
