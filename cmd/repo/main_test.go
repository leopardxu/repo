package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/leopardxu/repo-go/cmd/repo/commands"
	"github.com/leopardxu/repo-go/internal/config"
	"github.com/spf13/cobra"
)

// setupRepoTop 创建临时 repo 顶层目录（含 .repo）及其嵌套子目录，
// 切换到子目录执行测试，返回解析后的顶层真实路径。
func setupRepoTop(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, ".repo"), 0o755); err != nil {
		t.Fatalf("MkdirAll .repo: %v", err)
	}
	nested := filepath.Join(dir, "a", "b")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatalf("MkdirAll nested: %v", err)
	}
	old, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	if err := os.Chdir(nested); err != nil {
		t.Fatalf("Chdir: %v", err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(old); err != nil {
			t.Errorf("restore cwd: %v", err)
		}
	})
	return dir
}

// TestFindRepoTopDir 验证从嵌套子目录向上查找含 .repo 的顶层目录。
func TestFindRepoTopDir(t *testing.T) {
	top := setupRepoTop(t)
	if got := findRepoTopDir(); got != top {
		t.Errorf("findRepoTopDir() = %q, want %q", got, top)
	}
}

// TestFindRepoTopDirNotFound 验证不在 repo 检出内时返回空串。
func TestFindRepoTopDirNotFound(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	old, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("Chdir: %v", err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(old); err != nil {
			t.Errorf("restore cwd: %v", err)
		}
	})
	if got := findRepoTopDir(); got != "" {
		t.Errorf("findRepoTopDir() = %q, want empty", got)
	}
}

// TestRootShowToplevelWithoutSubcommand 验证无子命令时 --show-toplevel
// 输出顶层目录且不报错（可脚本化）。
func TestRootShowToplevelWithoutSubcommand(t *testing.T) {
	top := setupRepoTop(t)

	cmd := newRootCmd(commands.VersionInfo{Version: "dev"})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"--show-toplevel"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v, output = %q", err, out.String())
	}
	if got := strings.TrimSpace(out.String()); got != top {
		t.Errorf("output = %q, want %q", got, top)
	}
}

// TestRootNoArgsShowsHelp 验证无子命令且无 flag 时打印帮助并返回 errHelpShown
// （main 据此以退出码 1 结束，与上游一致）。
func TestRootNoArgsShowsHelp(t *testing.T) {
	cmd := newRootCmd(commands.VersionInfo{Version: "dev"})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{})

	err := cmd.Execute()
	if !errors.Is(err, errHelpShown) {
		t.Fatalf("Execute() error = %v, want errHelpShown", err)
	}
	if !strings.Contains(out.String(), "Usage:") {
		t.Errorf("help output missing Usage section: %q", out.String())
	}
	if n := strings.Count(out.String(), "Usage:"); n != 1 {
		t.Errorf("help output has %d Usage sections, want exactly 1: %q", n, out.String())
	}
}

// TestRootHelpNoDuplicateUsage 验证 --help 输出也只有 cobra 生成的唯一 Usage 段
// （Long 中不得再手写 Usage）。
func TestRootHelpNoDuplicateUsage(t *testing.T) {
	cmd := newRootCmd(commands.VersionInfo{Version: "dev"})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"--help"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if n := strings.Count(out.String(), "Usage:"); n != 1 {
		t.Errorf("help output has %d Usage sections, want exactly 1: %q", n, out.String())
	}
}

// TestRootRejectsShowToplevelAfterSubcommand 验证 --show-toplevel 只作为根命令
// 本地 flag：子命令之后的同名形式必须报未知 flag（对齐上游全局选项位置）。
func TestRootRejectsShowToplevelAfterSubcommand(t *testing.T) {
	cmd := newRootCmd(commands.VersionInfo{Version: "dev"})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"status", "--show-toplevel"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("Execute() expected unknown flag error for post-subcommand --show-toplevel")
	}
	if !strings.Contains(err.Error(), "unknown flag") {
		t.Errorf("error = %q, want unknown flag", err.Error())
	}
}

// TestRootShowToplevelWithoutSubcommandHandledBeforeArgs 验证全局 --show-toplevel
// 在 cobra Find/Args 校验之前处理：非法子命令不影响输出。
func TestRootShowToplevelWithoutSubcommandHandledBeforeArgs(t *testing.T) {
	top := setupRepoTop(t)

	var out bytes.Buffer
	handled, err := handleShowToplevel([]string{"--show-toplevel", "definitely-not-a-command"}, &out)
	if !handled {
		t.Fatal("handleShowToplevel() handled = false, want true")
	}
	if err != nil {
		t.Fatalf("handleShowToplevel() error = %v", err)
	}
	if got := strings.TrimSpace(out.String()); got != top {
		t.Errorf("output = %q, want %q", got, top)
	}
}

// TestHandleShowToplevelSkipsValueFlags 验证带独立取值的全局 flag 之后
// 仍能识别 --show-toplevel（值不能被当成第一个非 flag 词）。
func TestHandleShowToplevelSkipsValueFlags(t *testing.T) {
	top := setupRepoTop(t)

	var out bytes.Buffer
	handled, err := handleShowToplevel([]string{"--color", "always", "--show-toplevel"}, &out)
	if !handled {
		t.Fatal("handleShowToplevel() handled = false, want true")
	}
	if err != nil {
		t.Fatalf("handleShowToplevel() error = %v", err)
	}
	if got := strings.TrimSpace(out.String()); got != top {
		t.Errorf("output = %q, want %q", got, top)
	}
}

// TestShowToplevelRequestedOnlyBeforeFirstWord 验证只有第一个非 flag 词之前的
// 全局 --show-toplevel 才被预扫描命中。
func TestShowToplevelRequestedOnlyBeforeFirstWord(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want bool
	}{
		{name: "全局形式", args: []string{"--show-toplevel"}, want: true},
		{name: "非法子命令之后不处理", args: []string{"status", "--show-toplevel"}, want: false},
		{name: "合法子命令之后不处理", args: []string{"sync", "--show-toplevel"}, want: false},
		{name: "非 flag 词之后不处理", args: []string{"bogus", "--show-toplevel"}, want: false},
		{name: "false 不触发", args: []string{"--show-toplevel=false"}, want: false},
		{name: "等号 true", args: []string{"--show-toplevel=true"}, want: true},
		{name: "等号 1", args: []string{"--show-toplevel=1"}, want: true},
		{name: "等号 False", args: []string{"--show-toplevel=False"}, want: false},
		{name: "等号 0", args: []string{"--show-toplevel=0"}, want: false},
		{name: "非法布尔值交给 cobra", args: []string{"--show-toplevel=maybe"}, want: false},
		{name: "help 后置优先", args: []string{"--show-toplevel", "--help"}, want: false},
		{name: "help 前置优先", args: []string{"--help", "--show-toplevel"}, want: false},
		{name: "-h 优先", args: []string{"--show-toplevel", "-h"}, want: false},
		{name: "version 优先", args: []string{"--show-toplevel", "--version"}, want: false},
		{name: "未知 flag 交给 cobra", args: []string{"--unknown-flag", "--show-toplevel"}, want: false},
		{name: "show-toplevel 之后未知 flag 交给 cobra", args: []string{"--show-toplevel", "--unknown-flag"}, want: false},
		{name: "非法 color 交给 cobra", args: []string{"--color=bogus", "--show-toplevel"}, want: false},
		{name: "合法 color 独立取值", args: []string{"--color", "auto", "--show-toplevel"}, want: true},
		{name: "双横线终止", args: []string{"--", "--show-toplevel"}, want: false},
		{name: "取值 flag 之后", args: []string{"--color", "never", "--show-toplevel", "x"}, want: true},
		{name: "等号取值 flag 之后", args: []string{"--color=never", "--show-toplevel"}, want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := showToplevelRequested(tt.args); got != tt.want {
				t.Errorf("showToplevelRequested(%v) = %v, want %v", tt.args, got, tt.want)
			}
		})
	}
}

// TestHandleShowToplevelOutsideRepo 验证不在 repo 检出内时返回错误（main 退出 1）。
func TestHandleShowToplevelOutsideRepo(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	old, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("Chdir: %v", err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(old); err != nil {
			t.Errorf("restore cwd: %v", err)
		}
	})

	var out bytes.Buffer
	handled, err := handleShowToplevel([]string{"--show-toplevel"}, &out)
	if !handled {
		t.Fatal("handleShowToplevel() handled = false, want true")
	}
	if err == nil {
		t.Fatal("handleShowToplevel() outside repo expected error, got nil")
	}
}

// TestRootInvalidColorWithoutSubcommand 验证无子命令时 --color 非法值也报错。
func TestRootInvalidColorWithoutSubcommand(t *testing.T) {
	cmd := newRootCmd(commands.VersionInfo{Version: "dev"})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"--color", "bogus"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("Execute() expected error for invalid --color, got nil")
	}
	if !strings.Contains(err.Error(), "invalid color option") {
		t.Errorf("error = %q, want contains invalid color option", err.Error())
	}
}

// TestHandleShowToplevelHelpTakesPrecedence 验证 -h/--help 优先于 --show-toplevel：
// 预扫描不提前退出，交由 cobra 打印帮助。
func TestHandleShowToplevelHelpTakesPrecedence(t *testing.T) {
	for _, args := range [][]string{
		{"--show-toplevel", "--help"},
		{"--help", "--show-toplevel"},
		{"--show-toplevel", "-h"},
	} {
		var out bytes.Buffer
		handled, err := handleShowToplevel(args, &out)
		if handled {
			t.Errorf("handleShowToplevel(%v) handled = true, want false (help wins)", args)
		}
		if err != nil {
			t.Errorf("handleShowToplevel(%v) error = %v, want nil", args, err)
		}
	}
}

// TestRootInvalidShowToplevelBool 验证 --show-toplevel 的非法布尔值交给 cobra 报错，
// 而不是被预扫描当成 true 提前成功退出。
func TestRootInvalidShowToplevelBool(t *testing.T) {
	cmd := newRootCmd(commands.VersionInfo{Version: "dev"})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"--show-toplevel=maybe"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("Execute() expected error for invalid --show-toplevel value, got nil")
	}
	if !strings.Contains(err.Error(), "show-toplevel") {
		t.Errorf("error = %q, want mentions show-toplevel", err.Error())
	}
}

// TestMirrorSafeCommand 验证镜像客户端只放行上游 MirrorSafe 白名单子命令。
func TestMirrorSafeCommand(t *testing.T) {
	tests := []struct {
		name string
		safe bool
	}{
		{name: "init", safe: true},
		{name: "sync", safe: true},
		{name: "smartsync", safe: true},
		{name: "list", safe: true},
		{name: "forall", safe: true},
		{name: "help", safe: true},
		{name: "version", safe: true},
		{name: "selfupdate", safe: true},
		{name: "completion", safe: true},
		{name: "__complete", safe: true},
		{name: "__completeNoDesc", safe: true},
		{name: "config", safe: false},
		{name: "status", safe: false},
		{name: "stage", safe: false},
		{name: "start", safe: false},
		{name: "manifest", safe: false},
		{name: "diff", safe: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isMirrorSafeCommand(tt.name); got != tt.safe {
				t.Errorf("isMirrorSafeCommand(%q) = %v, want %v", tt.name, got, tt.safe)
			}
		})
	}
}

// TestMirrorSafeError 验证镜像配置下非白名单子命令被拒绝，白名单子命令与非镜像配置放行。
func TestMirrorSafeError(t *testing.T) {
	mirrorCfg := &config.Config{Mirror: true}

	tests := []struct {
		name    string
		command string
		cfg     *config.Config
		wantErr bool
	}{
		{name: "镜像拒绝 status", command: "status", cfg: mirrorCfg, wantErr: true},
		{name: "镜像拒绝 stage", command: "stage", cfg: mirrorCfg, wantErr: true},
		{name: "镜像拒绝 manifest", command: "manifest", cfg: mirrorCfg, wantErr: true},
		{name: "镜像放行 smartsync", command: "smartsync", cfg: mirrorCfg},
		{name: "镜像放行 sync", command: "sync", cfg: mirrorCfg},
		{name: "镜像放行 completion", command: "completion", cfg: mirrorCfg},
		{name: "镜像放行 __complete", command: "__complete", cfg: mirrorCfg},
		{name: "镜像放行 forall", command: "forall", cfg: mirrorCfg},
		{name: "镜像放行 init", command: "init", cfg: mirrorCfg},
		{name: "镜像放行 help", command: "help", cfg: mirrorCfg},
		{name: "镜像放行 version", command: "version", cfg: mirrorCfg},
		{name: "非镜像放行 status", command: "status", cfg: &config.Config{}},
		{name: "nil config 放行", command: "status", cfg: nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := mirrorSafeError(tt.command, tt.cfg)
			if (err != nil) != tt.wantErr {
				t.Fatalf("mirrorSafeError() error = %v, wantErr %v", err, tt.wantErr)
			}
			if err != nil && !strings.Contains(err.Error(), "requires a working directory") {
				t.Errorf("error = %q, want requires a working directory", err.Error())
			}
		})
	}
}

// TestFirstLevelCommandName 验证 MirrorSafe 判定使用用户输入的一级子命令名，
// 而不是 cobra 解析后的 leaf 名（如 completion bash 的 bash）。
func TestFirstLevelCommandName(t *testing.T) {
	root := &cobra.Command{Use: "repo"}
	completion := &cobra.Command{Use: "completion"}
	bash := &cobra.Command{Use: "bash"}
	completion.AddCommand(bash)
	complete := &cobra.Command{Use: "__complete"}
	root.AddCommand(completion, complete)

	if got := firstLevelCommandName(bash); got != "completion" {
		t.Errorf("firstLevelCommandName(bash) = %q, want completion", got)
	}
	if got := firstLevelCommandName(complete); got != "__complete" {
		t.Errorf("firstLevelCommandName(__complete) = %q, want __complete", got)
	}
	if got := firstLevelCommandName(root); got != "repo" {
		t.Errorf("firstLevelCommandName(root) = %q, want repo", got)
	}
}

// TestMirrorGuardExecution 执行级验证镜像客户端下的 MirrorSafe 拦截：
// 以用户输入的一级子命令名判定，smartsync/completion 放行，start/manifest 拒绝。
func TestMirrorGuardExecution(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, ".repo"), 0o755); err != nil {
		t.Fatalf("MkdirAll .repo: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".repo", "config.json"), []byte(`{"mirror": true}`), 0o644); err != nil {
		t.Fatalf("write config.json: %v", err)
	}
	old, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("Chdir: %v", err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(old); err != nil {
			t.Errorf("restore cwd: %v", err)
		}
	})

	root := newRootCmd(commands.VersionInfo{Version: "dev"})
	root.InitDefaultCompletionCmd()

	tests := []struct {
		name    string
		args    []string
		wantErr bool
	}{
		{name: "smartsync 放行", args: []string{"smartsync"}},
		{name: "sync 放行", args: []string{"sync"}},
		{name: "forall 放行", args: []string{"forall"}},
		{name: "completion 叶子命令放行", args: []string{"completion", "bash"}},
		{name: "start 拒绝", args: []string{"start", "b"}, wantErr: true},
		{name: "manifest 拒绝", args: []string{"manifest"}, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd, _, ferr := root.Find(tt.args)
			if ferr != nil {
				t.Fatalf("Find(%v): %v", tt.args, ferr)
			}
			// 合并父命令的持久 flag，PersistentPreRunE 会读取 trace/color 等全局 flag。
			cmd.InheritedFlags()

			gerr := root.PersistentPreRunE(cmd, tt.args[1:])
			if (gerr != nil) != tt.wantErr {
				t.Fatalf("PersistentPreRunE() error = %v, wantErr %v", gerr, tt.wantErr)
			}
			if gerr == nil {
				return
			}
			if !strings.Contains(gerr.Error(), "requires a working directory") {
				t.Errorf("error = %q, want requires a working directory", gerr.Error())
			}
			// 报错必须使用一级子命令名（本例即 args[0]）。
			if !strings.Contains(gerr.Error(), "'"+tt.args[0]+"'") {
				t.Errorf("error = %q, want names first-level command %q", gerr.Error(), tt.args[0])
			}
		})
	}
}
