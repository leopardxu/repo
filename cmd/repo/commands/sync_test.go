package commands

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/leopardxu/repo-go/internal/logger"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// TestValidateSyncOptionsRejectsInvalidCombinations 验证 -n/-l、-n/-d 组合
// 被明确拒绝，而不是静默变成空操作（对齐上游 ValidateOptions）。
func TestValidateSyncOptionsRejectsInvalidCombinations(t *testing.T) {
	tests := []struct {
		name    string
		opts    *SyncOptions
		wantErr string
	}{
		{
			name:    "network-only with local-only",
			opts:    &SyncOptions{NetworkOnly: true, LocalOnly: true},
			wantErr: "cannot combine -n and -l",
		},
		{
			name:    "network-only with detach",
			opts:    &SyncOptions{NetworkOnly: true, Detach: true},
			wantErr: "cannot combine -n and -d",
		},
		{
			name:    "manifest-name with smart-sync",
			opts:    &SyncOptions{ManifestName: "tmp.xml", SmartSync: true},
			wantErr: "cannot combine -m and -s",
		},
		{
			name:    "manifest-name with smart-tag",
			opts:    &SyncOptions{ManifestName: "tmp.xml", SmartTag: "build-1"},
			wantErr: "cannot combine -m and -t",
		},
		{
			name: "smartsync allows manifest-name",
			opts: &SyncOptions{ManifestName: "tmp.xml", SmartSync: true, SmartSyncOptional: true},
		},
		{
			name:    "username without password",
			opts:    &SyncOptions{ManifestServerUsername: "user"},
			wantErr: "both -u and -p must be given",
		},
		{
			name:    "password without username",
			opts:    &SyncOptions{ManifestServerPassword: "pass"},
			wantErr: "both -u and -p must be given",
		},
		{
			name: "username and password together",
			opts: &SyncOptions{ManifestServerUsername: "user", ManifestServerPassword: "pass"},
		},
		{
			name: "valid combination",
			opts: &SyncOptions{NetworkOnly: true},
		},
		{
			name: "local-only with detach",
			opts: &SyncOptions{LocalOnly: true, Detach: true},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateSyncOptions(tt.opts)
			if tt.wantErr == "" {
				if err != nil {
					t.Errorf("validateSyncOptions() error = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("validateSyncOptions() = nil, want error containing %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %q, want containing %q", err, tt.wantErr)
			}
		})
	}
}

// TestRunSyncExplicitSmartSyncWithoutServerFails 验证 `sync -s` 在清单未配置
// manifest-server 时必须报错（显式请求不允许静默回退）。
func TestRunSyncExplicitSmartSyncWithoutServerFails(t *testing.T) {
	repoRoot := readCmdTestRepoRoot(t, `  <project name="foo" path="foo" revision="main" />`)
	readCmdWithCwd(t, repoRoot, func() {
		opts := &SyncOptions{LocalOnly: true, Quiet: true, SmartSync: true}
		cmd := &cobra.Command{Use: "sync"}
		registerSyncFlags(cmd, opts, true)
		if err := cmd.Flags().Set("local-only", "true"); err != nil {
			t.Fatalf("set local-only: %v", err)
		}
		if err := cmd.Flags().Set("smart-sync", "true"); err != nil {
			t.Fatalf("set smart-sync: %v", err)
		}

		_, _, runErr := readCmdRunCaptured(t, func() error {
			return runSync(opts, nil, logger.NewDefaultLogger(), cmd)
		})
		if runErr == nil || !strings.Contains(runErr.Error(), "smart-sync failed") {
			t.Fatalf("显式 -s 无 manifest-server 应报错，实际: %v", runErr)
		}
	})
}

// TestSmartSyncCmdUpstreamFlagSet 验证 smartsync 暴露上游帮助中的选项，
// 且不暴露只属于 sync 的 smart-sync/smart-tag 或 repo-go 的扩展选项。
func TestSmartSyncCmdUpstreamFlagSet(t *testing.T) {
	cmd := SmartSyncCmd()

	wantFlags := []string{
		"jobs", "jobs-network", "jobs-checkout", "force-broken", "fail-fast",
		"force-sync", "force-remove-dirty", "force-overwrite", "local-only",
		"no-manifest-update", "nmu", "network-only", "detach", "current-branch",
		"no-current-branch", "manifest-name", "clone-bundle", "no-clone-bundle",
		"manifest-server-username", "manifest-server-password", "fetch-submodules",
		"use-superproject", "no-use-superproject", "tags", "no-tags",
		"optimized-fetch", "retry-fetches", "prune", "no-prune", "hyper-sync",
		"load-aiconfig", "verbose", "quiet", "outer-manifest", "no-outer-manifest",
		"this-manifest-only", "no-this-manifest-only", "all-manifests", "no-repo-verify",
	}
	for _, name := range wantFlags {
		if cmd.Flags().Lookup(name) == nil {
			t.Errorf("smartsync 缺少上游选项 --%s", name)
		}
	}

	// 上游 smartsync 不暴露 smart-sync/smart-tag，也不接受 repo-go 扩展选项。
	for _, name := range []string{"smart-sync", "smart-tag", "groups", "manifest-server-url", "no-manifest-server", "auto-gc", "git-lfs"} {
		if cmd.Flags().Lookup(name) != nil {
			t.Errorf("smartsync 不应注册 --%s", name)
		}
	}

	// -p 必须是 manifest-server-password（上游语义），--prune 无短选项。
	if f := cmd.Flags().Lookup("manifest-server-password"); f == nil || f.Shorthand != "p" {
		t.Errorf("--manifest-server-password 短选项 = %q, want p", shorthandOf(f))
	}
	if f := cmd.Flags().Lookup("prune"); f == nil || f.Shorthand != "" {
		t.Errorf("--prune 短选项 = %q, want empty", shorthandOf(f))
	}
	if f := cmd.Flags().Lookup("tags"); f == nil || f.Shorthand != "" {
		t.Errorf("--tags 短选项 = %q, want empty", shorthandOf(f))
	}
}

// shorthandOf 返回 flag 的短选项，flag 为 nil 时返回空串。
func shorthandOf(f *pflag.Flag) string {
	if f == nil {
		return ""
	}
	return f.Shorthand
}

// TestRunSyncPrintsNoticeOnFailureAndQuiet 验证 <notice> 在同步失败时也按原样
// 写 stdout，且不受 -q 影响（notice 是结果 payload，不是日志）。
func TestRunSyncPrintsNoticeOnFailureAndQuiet(t *testing.T) {
	repoRoot := readCmdTestRepoRoot(t, `  <project name="foo" path="foo" revision="main" />`)
	manifestPath := filepath.Join(repoRoot, ".repo", "manifest.xml")
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("读取 manifest.xml failed: %v", err)
	}
	const notice = "NOTICE-LINE: contact platform-team"
	patched := strings.Replace(string(data),
		`<default remote="origin" revision="main" />`,
		`<default remote="origin" revision="main" />`+"\n  <notice>"+notice+"</notice>", 1)
	if !strings.Contains(patched, notice) {
		t.Fatal("测试前置失败：未能注入 notice")
	}
	if err := os.WriteFile(manifestPath, []byte(patched), 0o644); err != nil {
		t.Fatalf("写入 manifest.xml failed: %v", err)
	}

	readCmdWithCwd(t, repoRoot, func() {
		for _, quiet := range []bool{false, true} {
			opts := &SyncOptions{LocalOnly: true, Quiet: quiet}
			cmd := &cobra.Command{Use: "sync"}
			registerSyncFlags(cmd, opts, true)

			stdout, _, runErr := readCmdRunCaptured(t, func() error {
				return runSync(opts, nil, logger.NewDefaultLogger(), cmd)
			})
			if runErr == nil {
				t.Fatalf("local-only 下缺失项目应导致同步失败（quiet=%v）", quiet)
			}
			if !strings.Contains(stdout, notice) {
				t.Errorf("stdout 缺少 notice（quiet=%v）:\n%s", quiet, stdout)
			}
		}
	})
}

// TestRunSyncDefaultGroupsFilterProjects 验证未显式 -g 时使用
// cfg.EffectiveGroups()（默认 "default"）过滤清单：groups="notdefault" 的项目
// 默认被排除；显式 --groups all 时才纳入。同时验证 notice 在失败时写 stdout。
func TestRunSyncDefaultGroupsFilterProjects(t *testing.T) {
	repoRoot := readCmdTestRepoRoot(t, `  <project name="skip" path="skip" revision="main" groups="notdefault" />`)
	manifestPath := filepath.Join(repoRoot, ".repo", "manifest.xml")
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("读取 manifest.xml failed: %v", err)
	}
	const notice = "NOTICE-LINE: default-groups"
	patched := strings.Replace(string(data),
		`<default remote="origin" revision="main" />`,
		`<default remote="origin" revision="main" />`+"\n  <notice>"+notice+"</notice>", 1)
	if err := os.WriteFile(manifestPath, []byte(patched), 0o644); err != nil {
		t.Fatalf("写入 manifest.xml failed: %v", err)
	}

	tests := []struct {
		name       string
		groups     string
		wantSubstr string
	}{
		{name: "default groups exclude notdefault", groups: "", wantSubstr: "未找到匹配的项目"},
		{name: "all groups include notdefault", groups: "all", wantSubstr: "missing network sync"},
	}
	readCmdWithCwd(t, repoRoot, func() {
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				opts := &SyncOptions{LocalOnly: true, Quiet: true, Groups: tt.groups}
				cmd := &cobra.Command{Use: "sync"}
				registerSyncFlags(cmd, opts, true)
				if err := cmd.Flags().Set("local-only", "true"); err != nil {
					t.Fatalf("set local-only: %v", err)
				}
				if tt.groups != "" {
					if err := cmd.Flags().Set("groups", tt.groups); err != nil {
						t.Fatalf("set groups: %v", err)
					}
				}

				stdout, _, runErr := readCmdRunCaptured(t, func() error {
					return runSync(opts, nil, logger.NewDefaultLogger(), cmd)
				})
				if runErr == nil {
					t.Fatalf("runSync() 应返回错误")
				}
				if !strings.Contains(runErr.Error(), tt.wantSubstr) {
					t.Errorf("error = %q, want containing %q", runErr.Error(), tt.wantSubstr)
				}
				if !strings.Contains(stdout, notice) {
					t.Errorf("stdout 缺少 notice:\n%s", stdout)
				}
			})
		}
	})
}
