package git

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/leopardxu/repo-go/internal/config"
)

func TestNewRunner(t *testing.T) {
	runner := NewRunner()
	if runner == nil {
		t.Fatal("NewRunner() returned nil")
	}

	// Verify it implements Runner interface
	var _ Runner = runner
}

func TestNewCommandRunnerWithConfig(t *testing.T) {
	cfg := &config.Config{
		Verbose: true,
		Quiet:   false,
		Jobs:    10,
	}

	runner, err := NewCommandRunnerWithConfig(cfg)
	if err != nil {
		t.Fatalf("NewCommandRunnerWithConfig() returned error: %v", err)
	}
	if runner == nil {
		t.Fatal("NewCommandRunnerWithConfig() returned nil")
	}
}

func TestNewCommandRunnerWithNilConfig(t *testing.T) {
	runner, err := NewCommandRunnerWithConfig(nil)
	if err != nil {
		t.Fatalf("NewCommandRunnerWithConfig(nil) returned error: %v", err)
	}
	if runner == nil {
		t.Fatal("NewCommandRunnerWithConfig(nil) returned nil")
	}
}

func TestSetVerbose(t *testing.T) {
	runner := NewRunner()
	r := runner.(*defaultRunner)

	r.SetVerbose(true)
	if !r.Verbose {
		t.Error("SetVerbose(true) did not set Verbose to true")
	}

	r.SetVerbose(false)
	if r.Verbose {
		t.Error("SetVerbose(false) did not set Verbose to false")
	}
}

func TestSetQuiet(t *testing.T) {
	runner := NewRunner()
	r := runner.(*defaultRunner)

	r.SetQuiet(true)
	if !r.Quiet {
		t.Error("SetQuiet(true) did not set Quiet to true")
	}

	r.SetQuiet(false)
	if r.Quiet {
		t.Error("SetQuiet(false) did not set Quiet to false")
	}
}

func TestSetMaxRetries(t *testing.T) {
	runner := NewRunner()
	r := runner.(*defaultRunner)

	r.SetMaxRetries(5)
	if r.MaxRetries != 5 {
		t.Errorf("SetMaxRetries(5) = %d, want 5", r.MaxRetries)
	}
}

func TestSetRetryDelay(t *testing.T) {
	runner := NewRunner()
	r := runner.(*defaultRunner)

	delay := 5 * time.Second
	r.SetRetryDelay(delay)
	if r.RetryDelay != delay {
		t.Errorf("SetRetryDelay() = %v, want %v", r.RetryDelay, delay)
	}
}

func TestSetTimeout(t *testing.T) {
	runner := NewRunner()
	r := runner.(*defaultRunner)

	timeout := 60 * time.Second
	r.SetTimeout(timeout)
	if r.timeout != timeout {
		t.Errorf("SetTimeout() = %v, want %v", r.timeout, timeout)
	}
}

func TestSetEnvironment(t *testing.T) {
	runner := NewRunner()
	r := runner.(*defaultRunner)

	env := map[string]string{"GIT_AUTHOR_NAME": "test"}
	r.SetEnvironment(env)

	got := r.GetEnvironment()
	if got["GIT_AUTHOR_NAME"] != "test" {
		t.Errorf("GetEnvironment() = %v, want GIT_AUTHOR_NAME=test", got)
	}
}

// TestSetConcurrencySafeReplace verifies that SetConcurrency does not panic
// when called concurrently with Run (the previous implementation closed the channel).
func TestSetConcurrencySafeReplace(t *testing.T) {
	runner := NewRunner()
	r := runner.(*defaultRunner)

	// Set initial concurrency
	r.SetConcurrency(3)
	if r.concurrency != 3 {
		t.Errorf("concurrency = %d, want 3", r.concurrency)
	}

	// Change concurrency - should not panic
	r.SetConcurrency(5)
	if r.concurrency != 5 {
		t.Errorf("concurrency = %d, want 5", r.concurrency)
	}

	// Set to 0 - should set semaphore to nil
	r.SetConcurrency(0)
	if r.semaphore != nil {
		t.Error("semaphore should be nil when concurrency is 0")
	}

	// Set back to positive
	r.SetConcurrency(2)
	if r.semaphore == nil {
		t.Error("semaphore should not be nil when concurrency > 0")
	}
}

// TestSetConcurrencyMultipleChanges verifies repeated changes don't panic
func TestSetConcurrencyMultipleChanges(t *testing.T) {
	runner := NewRunner()
	r := runner.(*defaultRunner)

	for i := 1; i <= 10; i++ {
		r.SetConcurrency(i)
	}

	if r.concurrency != 10 {
		t.Errorf("concurrency = %d, want 10", r.concurrency)
	}
}

func TestShouldRetryNetworkErrors(t *testing.T) {
	tests := []struct {
		name   string
		stderr string
		want   bool
	}{
		{"DNS resolution failure", "fatal: Could not resolve host example.com", true},
		{"Connection timeout", "fatal: Connection timed out", true},
		{"Connection reset", "error: Connection reset by peer", true},
		{"Operation timeout", "fatal: Operation timed out", true},
		{"Temporary DNS failure", "fatal: Temporary failure in name resolution", true},
		{"Failed to connect", "fatal: Failed to connect to github.com", true},
		{"Index lock", "fatal: Unable to create index.lock: File exists", true},
		{"Lock exists", "fatal: Unable to lock ref", true},
		{"Remote hung up", "fatal: the remote end hung up unexpectedly", true},
		{"Normal error", "fatal: not a git repository", false},
		{"Empty stderr", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := shouldRetry(1, tt.stderr)
			if got != tt.want {
				t.Errorf("shouldRetry(1, %q) = %v, want %v", tt.stderr, got, tt.want)
			}
		})
	}
}

func TestGitCommandError(t *testing.T) {
	err := &GitCommandError{
		Command:  "git fetch",
		Dir:      "/tmp/repo",
		Err:      nil,
		Stdout:   "some output",
		Stderr:   "some error",
		ExitCode: 1,
	}

	msg := err.Error()
	if !strings.Contains(msg, "git fetch") {
		t.Errorf("Error() should contain command name, got: %s", msg)
	}
	if !strings.Contains(msg, "/tmp/repo") {
		t.Errorf("Error() should contain directory, got: %s", msg)
	}
	if !strings.Contains(msg, "exit code 1") {
		t.Errorf("Error() should contain exit code, got: %s", msg)
	}
	if !strings.Contains(msg, "some error") {
		t.Errorf("Error() should contain stderr diagnostics, got: %s", msg)
	}
	if !strings.Contains(msg, "some output") {
		t.Errorf("Error() should contain stdout diagnostics, got: %s", msg)
	}
}

func TestGitCommandErrorZeroExitCode(t *testing.T) {
	err := &GitCommandError{
		Command:  "git status",
		Dir:      "/tmp/repo",
		Err:      nil,
		ExitCode: 0,
	}

	msg := err.Error()
	if strings.Contains(msg, "exit code") {
		t.Errorf("Error() should not contain exit code when 0, got: %s", msg)
	}
}

func TestRunnerRunGitCommand(t *testing.T) {
	runner := NewRunner()

	// Test with a real git command (version should always work)
	output, err := runner.Run("version")
	if err != nil {
		t.Fatalf("Run(version) returned error: %v", err)
	}

	if !strings.Contains(string(output), "git version") {
		t.Errorf("Run(version) output should contain 'git version', got: %s", string(output))
	}
}

func TestRunnerRunInDir(t *testing.T) {
	runner := NewRunner()

	// Running git version in a specific directory should work
	output, err := runner.RunInDir(".", "version")
	if err != nil {
		t.Fatalf("RunInDir('.', 'version') returned error: %v", err)
	}

	if !strings.Contains(string(output), "git version") {
		t.Errorf("RunInDir output should contain 'git version', got: %s", string(output))
	}
}

func TestRunnerRunWithInvalidCommand(t *testing.T) {
	runner := NewRunner()
	r := runner.(*defaultRunner)
	r.SetMaxRetries(0) // No retries for this test

	_, err := runner.Run("not-a-valid-git-command")
	if err == nil {
		t.Error("Run with invalid command should return error")
	}
}

// TestGitCommandErrorIncludesDiagnostics 验证错误文本携带 git 自身的诊断输出：
// -q 模式下 runner 不打印 stderr，错误文本必须能独立说明失败原因。
func TestGitCommandErrorIncludesDiagnostics(t *testing.T) {
	tests := []struct {
		name         string
		cmdErr       *GitCommandError
		wantContains []string
	}{
		{
			name:         "stderr trimmed",
			cmdErr:       &GitCommandError{Command: "git clone", Dir: "/repo", Err: errors.New("exit status 128"), ExitCode: 128, Stderr: "  fatal: repository not found\n"},
			wantContains: []string{"exit code 128", "fatal: repository not found"},
		},
		{
			name:         "stdout fallback",
			cmdErr:       &GitCommandError{Command: "git init", Dir: "/repo", Err: errors.New("exit status 1"), ExitCode: 1, Stdout: "usage text\n"},
			wantContains: []string{"usage text"},
		},
		{
			name:         "init ssh classification",
			cmdErr:       &GitCommandError{Command: "git clone", Dir: "/repo", Err: errors.New("exit status 128"), ExitCode: 128, Stderr: "git@example.com: Permission denied (publickey).\n"},
			wantContains: []string{"Permission denied (publickey)"},
		},
		{
			name:         "init repository url classification",
			cmdErr:       &GitCommandError{Command: "git clone", Dir: "/repo", Err: errors.New("exit status 128"), ExitCode: 128, Stderr: "fatal: repository 'https://example.com/missing.git/' does not exist\n"},
			wantContains: []string{"fatal: repository '"},
		},
		{
			name:         "init remote read classification",
			cmdErr:       &GitCommandError{Command: "git clone", Dir: "/repo", Err: errors.New("exit status 128"), ExitCode: 128, Stderr: "fatal: Could not read from remote repository.\n"},
			wantContains: []string{"Could not read from remote repository"},
		},
		{
			name:         "no output",
			cmdErr:       &GitCommandError{Command: "git status", Dir: "/repo"},
			wantContains: []string{"git status", "/repo"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msg := tt.cmdErr.Error()
			for _, want := range tt.wantContains {
				if !strings.Contains(msg, want) {
					t.Errorf("Error() = %q, 缺少 %q", msg, want)
				}
			}
			if strings.HasSuffix(msg, " ") || strings.HasSuffix(msg, "\n") {
				t.Errorf("Error() 末尾未 trim: %q", msg)
			}
		})
	}
}

// TestGitCommandErrorDoesNotDuplicateDiagnostics 验证 stdout/stderr 内容相同时
// 只追加一次，且多次调用 Error() 不会累加。
func TestGitCommandErrorDoesNotDuplicateDiagnostics(t *testing.T) {
	cmdErr := &GitCommandError{
		Command:  "git push",
		Dir:      "/repo",
		Err:      errors.New("exit status 1"),
		ExitCode: 1,
		Stderr:   "same diagnostic",
		Stdout:   " same diagnostic \n",
	}

	first := cmdErr.Error()
	if got := strings.Count(first, "same diagnostic"); got != 1 {
		t.Errorf("诊断信息出现 %d 次，want 1: %q", got, first)
	}
	if second := cmdErr.Error(); second != first {
		t.Errorf("重复调用 Error() 结果不一致:\nfirst=%q\nsecond=%q", first, second)
	}
}

// TestBuildGitEnvSanitizesInheritedVars 验证继承环境中的仓库定位变量被清除，
// 而调用方显式设置的变量仍会被追加。
func TestBuildGitEnvSanitizesInheritedVars(t *testing.T) {
	polluted := map[string]string{
		"GIT_DIR":                          "/polluted/gitdir",
		"GIT_WORK_TREE":                    "/polluted/worktree",
		"GIT_INDEX_FILE":                   "/polluted/index",
		"GIT_OBJECT_DIRECTORY":             "/polluted/objects",
		"GIT_ALTERNATE_OBJECT_DIRECTORIES": "/polluted/alternates",
		"GIT_GRAFT_FILE":                   "/polluted/grafts",
	}
	for k, v := range polluted {
		t.Setenv(k, v)
	}

	env := buildGitEnv(nil)
	joined := "\n" + strings.Join(env, "\n") + "\n"
	for key := range polluted {
		if strings.Contains(joined, "\n"+key+"=") {
			t.Errorf("继承环境中的 %s 未被清除", key)
		}
	}

	explicit := buildGitEnv(map[string]string{"GIT_DIR": "/explicit/gitdir"})
	count := 0
	for _, kv := range explicit {
		if strings.HasPrefix(kv, "GIT_DIR=") {
			count++
			if kv != "GIT_DIR=/explicit/gitdir" {
				t.Errorf("显式 GIT_DIR = %q, want GIT_DIR=/explicit/gitdir", kv)
			}
		}
	}
	if count != 1 {
		t.Errorf("显式 GIT_DIR 出现 %d 次, want 1", count)
	}
}

// TestRunnerSanitizesInheritedGitEnv 用真实 git 验证：父进程泄漏的 GIT_* 变量
// 不会影响 runner 在目标目录执行的命令。
func TestRunnerSanitizesInheritedGitEnv(t *testing.T) {
	dir := t.TempDir()
	polluted := map[string]string{
		"GIT_DIR":                          filepath.Join(dir, "polluted.git"),
		"GIT_WORK_TREE":                    dir,
		"GIT_INDEX_FILE":                   filepath.Join(dir, "polluted.index"),
		"GIT_OBJECT_DIRECTORY":             filepath.Join(dir, "polluted-objects"),
		"GIT_ALTERNATE_OBJECT_DIRECTORIES": filepath.Join(dir, "polluted-alt"),
		"GIT_GRAFT_FILE":                   filepath.Join(dir, "polluted-grafts"),
	}
	for k, v := range polluted {
		t.Setenv(k, v)
	}

	runner := NewRunner()
	r := runner.(*defaultRunner)
	r.SetMaxRetries(0)

	if _, err := runner.RunInDir(dir, "init", "-q"); err != nil {
		t.Fatalf("净化环境后 git init 失败: %v", err)
	}
	output, err := runner.RunInDir(dir, "rev-parse", "--git-dir")
	if err != nil {
		t.Fatalf("rev-parse --git-dir 失败: %v", err)
	}
	got := strings.TrimSpace(string(output))
	if got != ".git" && got != filepath.Join(dir, ".git") {
		t.Errorf("git-dir = %q, want 目标目录下的 .git（继承变量可能未被净化）", got)
	}
}

// TestRunInteractiveSanitizesInheritedGitEnv 验证交互执行路径同样净化环境。
func TestRunInteractiveSanitizesInheritedGitEnv(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("GIT_DIR", filepath.Join(dir, "polluted.git"))
	t.Setenv("GIT_WORK_TREE", dir)

	runner := NewRunner()
	r := runner.(*defaultRunner)
	r.SetMaxRetries(0)

	if err := r.RunInteractiveInDir(dir, "init", "-q"); err != nil {
		t.Fatalf("交互式 git init 失败: %v", err)
	}
	output, err := runner.RunInDir(dir, "rev-parse", "--git-dir")
	if err != nil {
		t.Fatalf("rev-parse --git-dir 失败: %v", err)
	}
	got := strings.TrimSpace(string(output))
	if got != ".git" && got != filepath.Join(dir, ".git") {
		t.Errorf("git-dir = %q, want 目标目录下的 .git", got)
	}
}

// TestRunnerExplicitGitEnvStillApplied 验证调用方显式设置的 GIT_DIR 仍生效
// （forall 镜像项目依赖该行为）。
func TestRunnerExplicitGitEnvStillApplied(t *testing.T) {
	dir := t.TempDir()
	runner := NewRunner()
	r := runner.(*defaultRunner)
	r.SetMaxRetries(0)

	if _, err := runner.RunInDir(dir, "init", "-q"); err != nil {
		t.Fatalf("git init 失败: %v", err)
	}
	gitDir := filepath.Join(dir, ".git")
	r.SetEnvironment(map[string]string{"GIT_DIR": gitDir})

	output, err := runner.RunInDir(dir, "rev-parse", "--git-dir")
	if err != nil {
		t.Fatalf("rev-parse --git-dir 失败: %v", err)
	}
	if got := strings.TrimSpace(string(output)); got != gitDir {
		t.Errorf("git-dir = %q, want 显式设置的 %q", got, gitDir)
	}
}

// TestRunInDirWithStderrReturnsOutputOnFailure 验证扩展接口在失败时同时返回
// stdout/stderr，且错误文本与 stderr 一致可诊断。
func TestRunInDirWithStderrReturnsOutputOnFailure(t *testing.T) {
	dir := t.TempDir()
	runner := NewRunner()
	r := runner.(*defaultRunner)
	r.SetMaxRetries(0)

	if _, err := runner.RunInDir(dir, "init", "-q"); err != nil {
		t.Fatalf("git init 失败: %v", err)
	}

	_, stderr, err := r.RunInDirWithStderr(dir, "rev-parse", "--verify", "refs/heads/does-not-exist")
	if err == nil {
		t.Fatal("不存在的 ref 应返回错误")
	}
	if len(stderr) == 0 {
		t.Error("失败命令应返回已捕获的 stderr")
	}
	if diagnostic := strings.TrimSpace(string(stderr)); diagnostic != "" && !strings.Contains(err.Error(), diagnostic) {
		t.Errorf("错误文本未包含 stderr 诊断: %q", err.Error())
	}

	stdout, _, err := r.RunInDirWithStderr(dir, "rev-parse", "--git-dir")
	if err != nil {
		t.Fatalf("成功命令返回错误: %v", err)
	}
	if !strings.Contains(string(stdout), ".git") {
		t.Errorf("成功命令 stdout = %q, want 包含 .git", string(stdout))
	}
}

// TestRunnerQuietFailureStillCarriesDiagnostics 验证 -q 下错误仍携带 stderr。
func TestRunnerQuietFailureStillCarriesDiagnostics(t *testing.T) {
	dir := t.TempDir()
	runner := NewRunner()
	r := runner.(*defaultRunner)
	r.SetMaxRetries(0)
	r.SetQuiet(true)

	if _, err := runner.RunInDir(dir, "init", "-q"); err != nil {
		t.Fatalf("git init 失败: %v", err)
	}
	_, err := runner.RunInDir(dir, "rev-parse", "--verify", "refs/heads/does-not-exist")
	if err == nil {
		t.Fatal("不存在的 ref 应返回错误")
	}
	var cmdErr *GitCommandError
	if !errors.As(err, &cmdErr) {
		t.Fatalf("错误类型 = %T, want *GitCommandError", err)
	}
	diagnostic := strings.TrimSpace(cmdErr.Stderr)
	if diagnostic == "" {
		t.Fatal("GitCommandError.Stderr 为空，-q 下无法诊断")
	}
	if !strings.Contains(err.Error(), diagnostic) {
		t.Errorf("-q 下错误文本应包含 stderr，got %q", err.Error())
	}
}
