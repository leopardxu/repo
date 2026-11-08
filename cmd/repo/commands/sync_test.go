package commands

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/leopardxu/repo-go/internal/logger"
	"github.com/spf13/cobra"
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

// TestSmartSyncCmdWithoutServerFails 验证 `repo smartsync`（等价 sync -s）
// 无 manifest-server 时同样报错，而不是按常规同步继续。
func TestSmartSyncCmdWithoutServerFails(t *testing.T) {
	repoRoot := readCmdTestRepoRoot(t, `  <project name="foo" path="foo" revision="main" />`)
	readCmdWithCwd(t, repoRoot, func() {
		cmd := SmartSyncCmd()
		cmd.SilenceUsage = true
		cmd.SilenceErrors = true
		cmd.SetArgs([]string{"--local-only", "--quiet"})

		_, _, runErr := readCmdRunCaptured(t, func() error {
			return cmd.Execute()
		})
		if runErr == nil || !strings.Contains(runErr.Error(), "smart-sync failed") {
			t.Fatalf("repo smartsync 无 manifest-server 应报错，实际: %v", runErr)
		}
	})
}

// TestSmartSyncCmdDoesNotRegisterSmartSyncFlag 验证 smartsync 快捷命令不暴露
// --smart-sync（已自动启用），但仍注册 --manifest-server-url。
func TestSmartSyncCmdDoesNotRegisterSmartSyncFlag(t *testing.T) {
	cmd := SmartSyncCmd()
	if cmd.Flags().Lookup("smart-sync") != nil {
		t.Error("smartsync 不应注册 --smart-sync")
	}
	if cmd.Flags().Lookup("manifest-server-url") == nil {
		t.Error("smartsync 应注册 --manifest-server-url")
	}
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
