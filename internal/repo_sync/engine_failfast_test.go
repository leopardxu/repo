package repo_sync

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/leopardxu/repo-go/internal/logger"
	"github.com/leopardxu/repo-go/internal/manifest"
	"github.com/leopardxu/repo-go/internal/project"
)

// funcRunner 基于回调的 git.Runner 测试桩，按命令内容决定成败，
// 用于在不依赖真实 git 的情况下驱动引擎的克隆/检出/重试路径。
type funcRunner struct {
	run func(dir string, args []string) ([]byte, error)
}

func (f *funcRunner) Run(args ...string) ([]byte, error) { return f.run("", args) }
func (f *funcRunner) RunInDir(dir string, args ...string) ([]byte, error) {
	return f.run(dir, args)
}
func (f *funcRunner) RunWithTimeout(_ time.Duration, args ...string) ([]byte, error) {
	return f.run("", args)
}
func (f *funcRunner) RunInDirWithTimeout(dir string, _ time.Duration, args ...string) ([]byte, error) {
	return f.run(dir, args)
}
func (f *funcRunner) SetVerbose(bool)             {}
func (f *funcRunner) SetQuiet(bool)               {}
func (f *funcRunner) SetMaxRetries(int)           {}
func (f *funcRunner) SetRetryDelay(time.Duration) {}
func (f *funcRunner) SetConcurrency(int)          {}

// newFailFastEngine 构造使用 funcRunner 的引擎，返回引擎与注入取消的回调包装。
func newFailFastEngine(t *testing.T, run func(dir string, args []string) ([]byte, error)) *Engine {
	t.Helper()
	e := NewEngine(&Options{Jobs: 1, Quiet: true}, &manifest.Manifest{}, logger.NewDefaultLogger())
	e.gitRunner = &funcRunner{run: run}
	return e
}

// TestCloneProjectRetryInterruptedByCancel 回归测试：
// fail-fast 取消发生在 clone 命令执行期间时，重试退避等待必须被立即中断。
// 修复前重试循环监听的是引擎的父 ctx（永不取消），会跑满 3/6/9 秒退避。
func TestCloneProjectRetryInterruptedByCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var cloneCalls int
	e := newFailFastEngine(t, func(_ string, args []string) ([]byte, error) {
		if len(args) > 0 && args[0] == "clone" {
			cloneCalls++
			// 模拟另一项目失败触发 fail-fast：clone 执行期间 ctx 被取消
			cancel()
			return nil, errors.New("git clone exit status 128")
		}
		return nil, nil
	})
	p := &project.Project{
		Name:       "platform/build",
		RemoteName: "origin",
		RemoteURL:  "https://example.com/platform/build",
		Worktree:   filepath.Join(t.TempDir(), "build"),
	}

	start := time.Now()
	err := e.cloneProject(ctx, p)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("cloneProject() 应在 git clone 失败时返回错误")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("cloneProject() 错误应包装 context.Canceled，实际: %v", err)
	}
	if elapsed > 2*time.Second {
		t.Errorf("取消后的 clone 应立即中止重试退避，实际耗时 %v", elapsed)
	}
	if cloneCalls != 1 {
		t.Errorf("取消后 clone 只应执行 1 次，实际 %d 次", cloneCalls)
	}
}

// TestFetchProjectRetryInterruptedByCancel 回归测试：
// fetch 的 RetryWithBackoff 必须响应取消，且取消错误可被 errors.Is 检测
// （验证 RetryWithBackoff 对 ctx.Err() 的 %w 包装）。
func TestFetchProjectRetryInterruptedByCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var fetchCalls int
	e := newFailFastEngine(t, func(_ string, args []string) ([]byte, error) {
		// fetchProject 构造的参数形如 [-C <worktree> fetch <remote>]
		if len(args) >= 3 && args[0] == "-C" && args[2] == "fetch" {
			fetchCalls++
			cancel() // 模拟 fail-fast 在 fetch 执行期间取消
			return nil, errors.New("fatal: unable to access 'https://example.com/': timed out")
		}
		return nil, nil
	})
	p := &project.Project{
		Name:       "platform/build",
		RemoteName: "origin",
		RemoteURL:  "https://example.com/platform/build",
		Worktree:   filepath.Join(t.TempDir(), "build"),
	}

	start := time.Now()
	err := e.fetchProject(ctx, p)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("fetchProject() 应在 git fetch 失败时返回错误")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("fetchProject() 错误应包装 context.Canceled，实际: %v", err)
	}
	if elapsed > 2*time.Second {
		t.Errorf("取消后的 fetch 应立即中止重试退避，实际耗时 %v", elapsed)
	}
	if fetchCalls != 1 {
		t.Errorf("取消后 fetch 只应执行 1 次，实际 %d 次", fetchCalls)
	}
}

// TestCheckoutProjectRetryInterruptedByCancel 回归测试：
// checkout 重试循环必须响应取消，立即返回包装 context.Canceled 的错误。
func TestCheckoutProjectRetryInterruptedByCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var checkoutCalls int
	e := newFailFastEngine(t, func(_ string, args []string) ([]byte, error) {
		// checkoutRevision 构造的参数形如 [-C <worktree> checkout --detach <sha> --]
		if len(args) >= 3 && args[0] == "-C" && args[2] == "checkout" {
			checkoutCalls++
			cancel() // 模拟 fail-fast 在 checkout 执行期间取消
			return nil, errors.New("error: Your local changes would be overwritten")
		}
		// resolveRevisionID 的 rev-parse 探测:revision 解析成功返回 1111...(本地分支存在)
		if len(args) > 0 && args[0] == "rev-parse" {
			if args[len(args)-1] == "HEAD" {
				// gitHeadSha:HEAD 指向另一提交,确保走 checkout 而非就位直接返回
				return []byte("2222222222222222222222222222222222222222\n"), nil
			}
			return []byte("1111111111111111111111111111111111111111\n"), nil
		}
		// symbolic-ref 失败:游离 HEAD,走 detach 检出路径
		return nil, errors.New("fatal: not a symbolic ref")
	})
	p := &project.Project{
		Name:       "platform/build",
		RemoteName: "origin",
		RemoteURL:  "https://example.com/platform/build",
		Worktree:   filepath.Join(t.TempDir(), "build"),
		Revision:   "main",
	}

	start := time.Now()
	err := e.checkoutProject(ctx, p)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("checkoutProject() 应在 git checkout 失败时返回错误")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("checkoutProject() 错误应包装 context.Canceled，实际: %v", err)
	}
	if elapsed > 2*time.Second {
		t.Errorf("取消后的 checkout 应立即中止重试退避，实际耗时 %v", elapsed)
	}
	if checkoutCalls != 1 {
		t.Errorf("取消后 checkout 只应执行 1 次，实际 %d 次", checkoutCalls)
	}
}

// TestSyncFailFastInterruptsInFlightRetry 端到端回归测试：
// pFail 在 setup_remote 阶段立即失败并触发 fail-fast 取消，
// pSlow 正处于 clone 重试退避中，必须被取消唤醒而不是睡满 3/6/9 秒；
// 最终错误只应包含 pFail 的真实失败，不含被取消打断项目的 context.Canceled 噪音。
func TestSyncFailFastInterruptsInFlightRetry(t *testing.T) {
	base := t.TempDir()
	pFail := &project.Project{
		Name:       "p/fail",
		RemoteName: "origin",
		RemoteURL:  "https://example.com/p/fail",
		Worktree:   filepath.Join(base, "fail"),
	}
	pSlow := &project.Project{
		Name:       "p/slow",
		RemoteName: "origin",
		RemoteURL:  "https://example.com/p/slow",
		Worktree:   filepath.Join(base, "slow"),
	}

	e := newFailFastEngine(t, func(_ string, args []string) ([]byte, error) {
		if len(args) > 0 && args[0] == "clone" {
			// clone 最后一个参数是目标 worktree，据此区分项目
			if args[len(args)-1] == pSlow.Worktree {
				return nil, errors.New("git clone exit status 128")
			}
			return nil, nil // pFail 克隆成功
		}
		// pFail 克隆后在 setup_remote 阶段立即失败（无重试），触发 fail-fast 取消
		if len(args) >= 2 && args[0] == "remote" && args[1] == "add" {
			return nil, errors.New("failed to add remote")
		}
		return nil, nil
	})
	e.options.Jobs = 2
	e.options.FailFast = true
	e.SetProjects([]*project.Project{pFail, pSlow})
	defer e.Cleanup()

	start := time.Now()
	err := e.Sync()
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("存在失败项目时 Sync() 应返回错误")
	}
	if !strings.Contains(err.Error(), "setup_remote") {
		t.Errorf("Sync() 错误应包含真实失败 (setup_remote)，实际: %v", err)
	}
	if strings.Contains(err.Error(), "context canceled") {
		t.Errorf("Sync() 错误不应包含被取消打断项目的 context.Canceled 噪音，实际: %v", err)
	}
	if elapsed > 2*time.Second {
		t.Errorf("fail-fast 应中断在途项目的重试退避，实际耗时 %v", elapsed)
	}
}

// TestFetchProjectTagsFlags 验证 fetch 的 --tags/--no-tags 参数对齐上游
// Sync_LocalHalf：CLI 显式值（--tags/--no-tags）覆盖项目级，未指定时按
// 项目 sync-tags（继承 default，缺省 true）逐项目判定。
func TestFetchProjectTagsFlags(t *testing.T) {
	tests := []struct {
		name       string
		syncTags   bool // 项目级 sync-tags 最终值
		cliTags    bool // CLI --tags
		cliNoTags  bool // CLI --no-tags（优先级最高）
		wantArg    string
		wantAbsent string
	}{
		{
			name:       "project default true, no cli flags",
			syncTags:   true,
			wantArg:    "--tags",
			wantAbsent: "--no-tags",
		},
		{
			name:       "project explicit false",
			syncTags:   false,
			wantArg:    "--no-tags",
			wantAbsent: "--tags",
		},
		{
			name:       "cli --tags overrides project false",
			syncTags:   false,
			cliTags:    true,
			wantArg:    "--tags",
			wantAbsent: "--no-tags",
		},
		{
			name:       "cli --no-tags overrides project true",
			syncTags:   true,
			cliNoTags:  true,
			wantArg:    "--no-tags",
			wantAbsent: "--tags",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var fetchArgs []string
			e := newFailFastEngine(t, func(_ string, args []string) ([]byte, error) {
				if len(args) >= 3 && args[0] == "-C" && args[2] == "fetch" {
					fetchArgs = args
					return nil, nil
				}
				return nil, nil
			})
			e.options.Tags = tt.cliTags
			e.options.NoTags = tt.cliNoTags

			p := &project.Project{
				Name:       "platform/build",
				RemoteName: "origin",
				RemoteURL:  "https://example.com/platform/build",
				Worktree:   filepath.Join(t.TempDir(), "build"),
				SyncTags:   tt.syncTags,
			}

			if err := e.fetchProject(context.Background(), p); err != nil {
				t.Fatalf("fetchProject() error: %v", err)
			}
			if fetchArgs == nil {
				t.Fatal("fetch 未被执行")
			}
			var hasWant, hasAbsent bool
			for _, a := range fetchArgs {
				if a == tt.wantArg {
					hasWant = true
				}
				if a == tt.wantAbsent {
					hasAbsent = true
				}
			}
			if !hasWant {
				t.Errorf("fetch 参数应含 %s，实际: %v", tt.wantArg, fetchArgs)
			}
			if hasAbsent {
				t.Errorf("fetch 参数不应含 %s，实际: %v", tt.wantAbsent, fetchArgs)
			}
		})
	}
}
