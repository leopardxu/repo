package hook

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/leopardxu/repo-go/internal/logger"
)

// gitIsolatedEnv 返回剔除 GIT_* 定位变量的环境，避免测试受外层仓库影响。
func gitIsolatedEnv() []string {
	env := make([]string, 0, len(os.Environ()))
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "GIT_DIR=") ||
			strings.HasPrefix(kv, "GIT_WORK_TREE=") ||
			strings.HasPrefix(kv, "GIT_INDEX_FILE=") {
			continue
		}
		env = append(env, kv)
	}
	return env
}

func TestInitHooks(t *testing.T) {
	tmpDir := t.TempDir()

	err := InitHooks(tmpDir)
	if err != nil {
		t.Fatalf("InitHooks: %v", err)
	}

	// Verify .repo/hooks directory was created
	hooksDir := filepath.Join(tmpDir, ".repo", "hooks")
	if _, err := os.Stat(hooksDir); os.IsNotExist(err) {
		t.Error(".repo/hooks directory was not created")
	}

	// Verify some hook files were created
	preCommit := filepath.Join(hooksDir, "pre-commit")
	if _, err := os.Stat(preCommit); os.IsNotExist(err) {
		t.Error("pre-commit hook was not created")
	}
}

func TestInitHooksSetsExecutableBit(t *testing.T) {
	tmpDir := t.TempDir()
	if err := InitHooks(tmpDir); err != nil {
		t.Fatalf("InitHooks: %v", err)
	}

	hooksDir := filepath.Join(tmpDir, ".repo", "hooks")
	info, err := os.Stat(filepath.Join(hooksDir, "pre-commit"))
	if err != nil {
		t.Fatalf("stat pre-commit: %v", err)
	}
	if info.Mode().Perm() != 0755 {
		t.Errorf("pre-commit 权限 = %v, want 0755", info.Mode().Perm())
	}
}

func TestInitHooksFixesPermissionsOfExistingHook(t *testing.T) {
	tmpDir := t.TempDir()
	if err := InitHooks(tmpDir); err != nil {
		t.Fatalf("InitHooks: %v", err)
	}

	hookPath := filepath.Join(tmpDir, ".repo", "hooks", "pre-commit")
	if err := os.Chmod(hookPath, 0644); err != nil {
		t.Fatalf("chmod pre-commit: %v", err)
	}

	// 内容相同会走跳过创建分支，但权限仍必须被修正
	if err := InitHooks(tmpDir); err != nil {
		t.Fatalf("second InitHooks: %v", err)
	}

	info, err := os.Stat(hookPath)
	if err != nil {
		t.Fatalf("stat pre-commit: %v", err)
	}
	if info.Mode().Perm() != 0755 {
		t.Errorf("已有 hook 权限 = %v, want 0755", info.Mode().Perm())
	}
}

func TestPreCommitTemplateCapturesExitCode(t *testing.T) {
	tpl, ok := hookTemplates["pre-commit"]
	if !ok {
		t.Fatal("缺少 pre-commit 模板")
	}

	if !strings.Contains(tpl, "rc=$?") {
		t.Error("pre-commit 模板必须用 rc=$? 保存真实退出码")
	}
	if strings.Contains(tpl, "exit $?") {
		t.Error("pre-commit 模板不能使用 exit $?（此时 $? 恒为 0）")
	}
}

func TestPreCommitHookPropagatesExitCode(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash 不可用")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git 不可用")
	}

	tests := []struct {
		name     string
		hookExit int
	}{
		{"成功不阻止提交", 0},
		{"失败阻止提交", 7},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repoDir := t.TempDir()
			cmd := exec.Command("git", "init", "-q", repoDir)
			cmd.Env = gitIsolatedEnv()
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("git init: %v: %s", err, out)
			}

			if err := InitHooks(repoDir); err != nil {
				t.Fatalf("InitHooks: %v", err)
			}

			// 模板会从 git 工作树向上找到包含 .repo 的顶层，再定位
			// <top>/.repo/cixtools/hooks。
			toolsDir := filepath.Join(repoDir, ".repo", "cixtools", "hooks")
			if err := os.MkdirAll(toolsDir, 0755); err != nil {
				t.Fatalf("mkdir tools dir: %v", err)
			}
			scriptPath := filepath.Join(toolsDir, "ci-hook.sh")
			script := "#!/bin/bash\nexit " + strconv.Itoa(tt.hookExit) + "\n"
			if err := os.WriteFile(scriptPath, []byte(script), 0755); err != nil {
				t.Fatalf("write hook script: %v", err)
			}

			config := "pre-commit = ci-hook.sh\n"
			if err := os.WriteFile(filepath.Join(repoDir, ".cix-ciconfig"), []byte(config), 0644); err != nil {
				t.Fatalf("write ciconfig: %v", err)
			}

			hookPath := filepath.Join(repoDir, ".repo", "hooks", "pre-commit")
			hookCmd := exec.Command("bash", hookPath)
			hookCmd.Dir = repoDir
			hookCmd.Env = gitIsolatedEnv()
			out, err := hookCmd.CombinedOutput()

			if tt.hookExit == 0 {
				if err != nil {
					t.Fatalf("期望 hook 成功, got %v: %s", err, out)
				}
				return
			}

			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) {
				t.Fatalf("期望 hook 失败, got %v: %s", err, out)
			}
			if exitErr.ExitCode() != tt.hookExit {
				t.Errorf("hook 退出码 = %d, want %d", exitErr.ExitCode(), tt.hookExit)
			}
		})
	}
}

func TestPreCommitHookEmptyConfigDoesNotBlock(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash 不可用")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git 不可用")
	}

	tests := []struct {
		name   string
		config string
	}{
		{"空配置文件", ""},
		{"无 pre-commit 条目", "other-entry = something\n"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repoDir := t.TempDir()
			cmd := exec.Command("git", "init", "-q", repoDir)
			cmd.Env = gitIsolatedEnv()
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("git init: %v: %s", err, out)
			}
			if err := InitHooks(repoDir); err != nil {
				t.Fatalf("InitHooks: %v", err)
			}

			toolsDir := filepath.Join(repoDir, ".repo", "cixtools", "hooks")
			if err := os.MkdirAll(toolsDir, 0755); err != nil {
				t.Fatalf("mkdir tools dir: %v", err)
			}
			if err := os.WriteFile(filepath.Join(toolsDir, "ci-hook.sh"), []byte("#!/bin/bash\nexit 0\n"), 0755); err != nil {
				t.Fatalf("write hook script: %v", err)
			}
			if err := os.WriteFile(filepath.Join(repoDir, ".cix-ciconfig"), []byte(tt.config), 0644); err != nil {
				t.Fatalf("write ciconfig: %v", err)
			}

			hookCmd := exec.Command("bash", filepath.Join(repoDir, ".repo", "hooks", "pre-commit"))
			hookCmd.Dir = repoDir
			hookCmd.Env = gitIsolatedEnv()
			out, err := hookCmd.CombinedOutput()
			if err != nil {
				t.Fatalf("空/无 pre-commit 条目的配置不应阻塞提交, got %v: %s", err, out)
			}
		})
	}
}

// TestPreCommitHookConfigEdgeCases 覆盖配置解析的三个边界场景：
// 末行无换行符、空值条目、脚本名含空格。
func TestPreCommitHookConfigEdgeCases(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash 不可用")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git 不可用")
	}

	tests := []struct {
		name       string
		config     string
		scriptName string
	}{
		{
			name:       "最后一行无换行符仍执行",
			config:     "pre-commit = plain.sh",
			scriptName: "plain.sh",
		},
		{
			name:       "空值条目被跳过",
			config:     "pre-commit =\npre-commit = plain.sh\n",
			scriptName: "plain.sh",
		},
		{
			name:       "脚本名含空格按单参数执行",
			config:     "pre-commit = with space.sh\n",
			scriptName: "with space.sh",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repoTop := t.TempDir()
			initCmd := exec.Command("git", "init", "-q", repoTop)
			initCmd.Env = gitIsolatedEnv()
			if out, err := initCmd.CombinedOutput(); err != nil {
				t.Fatalf("git init: %v: %s", err, out)
			}
			if err := InitHooks(repoTop); err != nil {
				t.Fatalf("InitHooks: %v", err)
			}

			record := filepath.Join(t.TempDir(), "record.txt")
			toolsDir := filepath.Join(repoTop, ".repo", "cixtools", "hooks")
			if err := os.MkdirAll(toolsDir, 0755); err != nil {
				t.Fatalf("mkdir tools dir: %v", err)
			}
			script := "#!/bin/bash\nprintf '%s\\n' \"$#\" \"$1\" \"$2\" > \"$RECORD\"\n"
			if err := os.WriteFile(filepath.Join(toolsDir, tt.scriptName), []byte(script), 0755); err != nil {
				t.Fatalf("write hook script: %v", err)
			}
			if err := os.WriteFile(filepath.Join(repoTop, ".cix-ciconfig"), []byte(tt.config), 0644); err != nil {
				t.Fatalf("write ciconfig: %v", err)
			}

			hookCmd := exec.Command("bash", filepath.Join(repoTop, ".repo", "hooks", "pre-commit"))
			hookCmd.Dir = repoTop
			hookCmd.Env = append(gitIsolatedEnv(), "RECORD="+record)
			if out, err := hookCmd.CombinedOutput(); err != nil {
				t.Fatalf("hook 执行失败: %v: %s", err, out)
			}

			content, err := os.ReadFile(record)
			if err != nil {
				t.Fatalf("脚本未被正确执行（缺少记录文件）: %v", err)
			}
			lines := strings.Split(strings.TrimRight(string(content), "\n"), "\n")
			if len(lines) != 3 {
				t.Fatalf("记录内容 = %q, want 3 行", string(content))
			}
			if lines[0] != "2" {
				t.Errorf("参数个数 = %q, want 2（脚本名必须作为单个参数）", lines[0])
			}
			if filepath.Clean(lines[1]) != filepath.Clean(repoTop) {
				t.Errorf("WORKTREE = %q, want repo 顶层 %q", lines[1], repoTop)
			}
			gotGitDir := lines[2]
			if !filepath.IsAbs(gotGitDir) {
				gotGitDir = filepath.Join(repoTop, gotGitDir)
			}
			if filepath.Clean(gotGitDir) != filepath.Join(repoTop, ".git") {
				t.Errorf("GIT_DIR = %q, want %q", lines[2], filepath.Join(repoTop, ".git"))
			}
		})
	}
}

func TestPreCommitHookCopyInstallFindsRepoTools(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash 不可用")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git 不可用")
	}

	repoTop := t.TempDir()
	if err := InitHooks(repoTop); err != nil {
		t.Fatalf("InitHooks: %v", err)
	}

	recordPath := filepath.Join(t.TempDir(), "record.txt")
	toolsDir := filepath.Join(repoTop, ".repo", "cixtools", "hooks")
	if err := os.MkdirAll(toolsDir, 0755); err != nil {
		t.Fatalf("mkdir tools dir: %v", err)
	}
	script := "#!/bin/bash\nprintf '%s\\n%s\\n' \"$1\" \"$2\" > '" + recordPath + "'\nexit 0\n"
	if err := os.WriteFile(filepath.Join(toolsDir, "copy-hook.sh"), []byte(script), 0755); err != nil {
		t.Fatalf("write hook script: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repoTop, ".cix-ciconfig"), []byte("pre-commit = copy-hook.sh\n"), 0644); err != nil {
		t.Fatalf("write ciconfig: %v", err)
	}

	// 模拟 copy 安装：项目工作树 + 复制到 <项目>/.git/hooks。
	projectDir := filepath.Join(repoTop, "proj")
	if err := os.MkdirAll(projectDir, 0755); err != nil {
		t.Fatalf("mkdir project: %v", err)
	}
	gitCmd := exec.Command("git", "init", "-q", projectDir)
	gitCmd.Env = gitIsolatedEnv()
	if out, err := gitCmd.CombinedOutput(); err != nil {
		t.Fatalf("git init project: %v: %s", err, out)
	}

	hookContent, err := os.ReadFile(filepath.Join(repoTop, ".repo", "hooks", "pre-commit"))
	if err != nil {
		t.Fatalf("read installed hook: %v", err)
	}
	projectHook := filepath.Join(projectDir, ".git", "hooks", "pre-commit")
	if err := os.WriteFile(projectHook, hookContent, 0755); err != nil {
		t.Fatalf("copy hook: %v", err)
	}

	hookCmd := exec.Command("bash", projectHook)
	hookCmd.Dir = projectDir
	hookCmd.Env = gitIsolatedEnv()
	out, err := hookCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("copy 安装的 hook 执行失败: %v: %s", err, out)
	}

	// 工具脚本必须被真正执行，并收到 repo 顶层与项目 git dir。
	content, err := os.ReadFile(recordPath)
	if err != nil {
		t.Fatalf("工具脚本未被执行（缺少记录文件）: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(content), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("记录内容 = %q, want 2 行", string(content))
	}
	if filepath.Clean(lines[0]) != filepath.Clean(repoTop) {
		t.Errorf("WORKTREE = %q, want repo 顶层 %q", lines[0], repoTop)
	}
	gotGitDir := lines[1]
	if !filepath.IsAbs(gotGitDir) {
		gotGitDir = filepath.Join(projectDir, gotGitDir)
	}
	if filepath.Clean(gotGitDir) != filepath.Join(projectDir, ".git") {
		t.Errorf("GIT_DIR = %q, want %q", lines[1], filepath.Join(projectDir, ".git"))
	}
}

func TestLinkHooks(t *testing.T) {
	projectDir := t.TempDir()
	hooksDir := t.TempDir()

	want := "#!/bin/bash\necho linked\n"
	if err := os.WriteFile(filepath.Join(hooksDir, "pre-commit"), []byte(want), 0755); err != nil {
		t.Fatalf("write source hook: %v", err)
	}

	if err := LinkHooks(projectDir, hooksDir); err != nil {
		t.Fatalf("LinkHooks: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(projectDir, ".git", "hooks", "pre-commit"))
	if err != nil {
		t.Fatalf("read linked hook: %v", err)
	}
	if string(got) != want {
		t.Errorf("linked hook 内容 = %q, want %q", string(got), want)
	}
}

func TestLinkHooksMissingHooksDir(t *testing.T) {
	projectDir := t.TempDir()
	hooksDir := filepath.Join(t.TempDir(), "missing")

	err := LinkHooks(projectDir, hooksDir)
	if err == nil {
		t.Fatal("LinkHooks() 期望失败")
	}
	var hookErr *HookError
	if !errors.As(err, &hookErr) {
		t.Errorf("error 类型 = %T, want *HookError", err)
	}
}

func TestCreateRepoGitConfig(t *testing.T) {
	tmpDir := t.TempDir()

	err := CreateRepoGitConfig(tmpDir)
	if err != nil {
		t.Fatalf("CreateRepoGitConfig: %v", err)
	}

	// Verify config file was created
	configPath := filepath.Join(tmpDir, ".repo", "repo.git")
	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		t.Error("repo.git config file was not created")
	}
}

func TestCreateRepoGitconfig(t *testing.T) {
	tmpDir := t.TempDir()

	err := CreateRepoGitconfig(tmpDir)
	if err != nil {
		t.Fatalf("CreateRepoGitconfig: %v", err)
	}

	// Verify config file was created
	configPath := filepath.Join(tmpDir, ".repo", "repo.gitconfig")
	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		t.Error("repo.gitconfig file was not created")
	}
}

func TestHookError(t *testing.T) {
	err := &HookError{
		Op:   "init",
		Path: "/path/to/hook",
		Err:  nil,
	}

	msg := err.Error()
	if msg == "" {
		t.Error("HookError.Error() should not be empty")
	}
}

func TestHookErrorWithoutPath(t *testing.T) {
	err := &HookError{
		Op:   "init",
		Path: "",
		Err:  nil,
	}

	msg := err.Error()
	if msg == "" {
		t.Error("HookError.Error() without path should not be empty")
	}
}

func TestSetLogger(_ *testing.T) {
	// Should not panic with nil
	SetLogger(nil)

	// Should not panic with a real logger
	originalLog := log
	defer func() { log = originalLog }()
	SetLogger(logger.NewDefaultLogger())
}

// TestPreCommitTemplateReadsConfigBeforeLoop 验证模板先把配置读入数组再循环：
// 循环不能位于 sed 管道右侧，否则 hook 脚本读取 stdin 会吞掉后续条目。
func TestPreCommitTemplateReadsConfigBeforeLoop(t *testing.T) {
	tpl, ok := hookTemplates["pre-commit"]
	if !ok {
		t.Fatal("缺少 pre-commit 模板")
	}
	if !strings.Contains(tpl, `while IFS= read -r script_name`) {
		t.Error("pre-commit 模板必须用 while read 读入配置")
	}
	if !strings.Contains(tpl, `hook_scripts+=("${script_name}")`) {
		t.Error("pre-commit 模板必须把配置逐行读入数组")
	}
	if !strings.Contains(tpl, `done < <(sed`) {
		t.Error("pre-commit 模板必须用进程替换读取配置（while 循环不在管道中）")
	}
	if !strings.Contains(tpl, `for script_name in "${hook_scripts[@]}"`) {
		t.Error("pre-commit 模板必须在数组上 for 循环")
	}
	if strings.Contains(tpl, "| while") {
		t.Error("pre-commit 模板不能把循环放在配置管道右侧（会吞掉 stdin）")
	}
}

// TestPreCommitTemplateBash32Compatible 验证模板只使用 bash 3.2 可用的语法：
// 不得出现 bash 4+ 的 mapfile，且模板能通过 bash -n 语法检查。
func TestPreCommitTemplateBash32Compatible(t *testing.T) {
	tpl, ok := hookTemplates["pre-commit"]
	if !ok {
		t.Fatal("缺少 pre-commit 模板")
	}
	if strings.Contains(tpl, "mapfile") {
		t.Error("pre-commit 模板不能使用 bash 4+ 的 mapfile（macOS 自带 bash 3.2 不可用）")
	}

	bashPath, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash 不可用")
	}
	scriptPath := filepath.Join(t.TempDir(), "pre-commit")
	if err := os.WriteFile(scriptPath, []byte(tpl), 0755); err != nil {
		t.Fatalf("write template: %v", err)
	}
	if out, err := exec.Command(bashPath, "-n", scriptPath).CombinedOutput(); err != nil {
		t.Fatalf("bash -n 语法检查失败: %v: %s", err, out)
	}
}

// TestPreCommitHookTrimsTrailingWhitespaceAndKeepsStdin 验证：
//   - CRLF 与行尾空格/Tab 被裁掉（内部空格保留）；
//   - hook 脚本读取 stdin 不会吞掉后续配置条目。
func TestPreCommitHookTrimsTrailingWhitespaceAndKeepsStdin(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash 不可用")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git 不可用")
	}

	repoTop := t.TempDir()
	initCmd := exec.Command("git", "init", "-q", repoTop)
	initCmd.Env = gitIsolatedEnv()
	if out, err := initCmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	if err := InitHooks(repoTop); err != nil {
		t.Fatalf("InitHooks: %v", err)
	}

	record := filepath.Join(t.TempDir(), "record.txt")
	toolsDir := filepath.Join(repoTop, ".repo", "cixtools", "hooks")
	if err := os.MkdirAll(toolsDir, 0755); err != nil {
		t.Fatalf("mkdir tools dir: %v", err)
	}
	scripts := []string{"first.sh", "second.sh", "third.sh"}
	for _, name := range scripts {
		script := "#!/bin/bash\n" +
			"printf '%s\\n' " + name + " >> \"$RECORD\"\n" +
			"# 故意读取 stdin：循环若在管道右侧会吞掉后续配置条目。\n" +
			"cat >/dev/null\n"
		if err := os.WriteFile(filepath.Join(toolsDir, name), []byte(script), 0755); err != nil {
			t.Fatalf("write hook script %s: %v", name, err)
		}
	}

	// 第一条 CRLF；第二条行尾空格+Tab；第三条行尾多个空格。
	config := "pre-commit = first.sh\r\n" +
		"pre-commit = second.sh \t \r\n" +
		"pre-commit = third.sh   \n"
	if err := os.WriteFile(filepath.Join(repoTop, ".cix-ciconfig"), []byte(config), 0644); err != nil {
		t.Fatalf("write ciconfig: %v", err)
	}

	hookCmd := exec.Command("bash", filepath.Join(repoTop, ".repo", "hooks", "pre-commit"))
	hookCmd.Dir = repoTop
	hookCmd.Env = append(gitIsolatedEnv(), "RECORD="+record)
	if out, err := hookCmd.CombinedOutput(); err != nil {
		t.Fatalf("hook 执行失败: %v: %s", err, out)
	}

	content, err := os.ReadFile(record)
	if err != nil {
		t.Fatalf("读取执行记录失败: %v", err)
	}
	if got := strings.TrimSpace(string(content)); got != strings.Join(scripts, "\n") {
		t.Errorf("执行记录 = %q, want %q", got, strings.Join(scripts, "\n"))
	}
}
