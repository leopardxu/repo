package commands

import (
	"context"
	"io"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/leopardxu/repo-go/internal/git"
	"github.com/leopardxu/repo-go/internal/project"
)

// TestBuildForallEnv_DisablesGitPager 验证 forall 注入 GIT_PAGER=cat，
// 避免 git log/diff/show 等在并发执行时启动交互式分页器（less）等待按键，
// 导致单个项目阻塞、wg.Wait() 无法返回而使整个 forall 永久挂起。
func TestBuildForallEnv_DisablesGitPager(t *testing.T) {
	proj := &project.Project{
		Name:       "test/project",
		Relpath:    "test/project",
		Revision:   "main",
		Worktree:   ".",
		RemoteName: "origin",
	}

	env := buildForallEnv(proj, 1)

	pager := ""
	pagerSet := false
	for _, kv := range env {
		if strings.HasPrefix(kv, "GIT_PAGER=") {
			pager = strings.TrimPrefix(kv, "GIT_PAGER=")
			pagerSet = true
		}
	}
	if !pagerSet {
		t.Fatal("期望 buildForallEnv 注入 GIT_PAGER，实际未找到")
	}
	if pager != "cat" {
		t.Errorf("期望 GIT_PAGER=cat，实际 GIT_PAGER=%s", pager)
	}
}

// TestBuildForallEnv_OverridesExistingGitPager 验证即使进程环境中已设置 GIT_PAGER，
// buildForallEnv 也会将其替换为 cat（先过滤再追加，避免重复键导致行为不确定）。
func TestBuildForallEnv_OverridesExistingGitPager(t *testing.T) {
	t.Setenv("GIT_PAGER", "less")

	proj := &project.Project{Name: "p", Worktree: "."}
	env := buildForallEnv(proj, 1)

	count := 0
	for _, kv := range env {
		if strings.HasPrefix(kv, "GIT_PAGER=") {
			count++
		}
	}
	if count != 1 {
		t.Errorf("期望 GIT_PAGER 仅出现 1 次（值为 cat），实际出现 %d 次", count)
	}
}

// TestBuildForallEnv_UpstreamVars 验证 forall 注入上游 repo 约定的 REPO_* 环境变量：
// REPO_LREV 为解析后的 commit SHA，REPO_RREV 为 revision 表达式，
// REPO_UPSTREAM/REPO_DEST_BRANCH 取自项目字段而非 revision。
func TestBuildForallEnv_UpstreamVars(t *testing.T) {
	sha := "0123456789abcdef0123456789abcdef01234567"
	proj := &project.Project{
		Name:       "platform/build",
		Relpath:    "build",
		Revision:   "refs/heads/main",
		RevisionID: sha,
		Upstream:   "upstream-main",
		DestBranch: "dest-main",
		Worktree:   ".",
		RemoteName: "origin",
	}

	env := buildForallEnv(proj, 3)

	expected := map[string]string{
		"REPO_PATH":        "build",
		"REPO_PROJECT":     "platform/build",
		"REPO_REMOTE":      "origin",
		"REPO_LREV":        sha,
		"REPO_RREV":        "refs/heads/main",
		"REPO_I":           "3",
		"REPO_UPSTREAM":    "upstream-main",
		"REPO_DEST_BRANCH": "dest-main",
	}

	for _, kv := range env {
		for key, want := range expected {
			if kv == key+"="+want {
				delete(expected, key)
			}
		}
	}

	for key, want := range expected {
		t.Errorf("expected env var %s=%s not found", key, want)
	}
}

// TestSplitForallArgs 验证 -c 之后所有 token 归命令（包括以 - 开头的 flag），
// 参数边界不被破坏，且 `--` 之后不再识别 -c。
func TestSplitForallArgs(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		wantFlags  []string
		wantCmd    []string
		wantHasCmd bool
	}{
		{
			name:       "-c 后接命令与 flag",
			args:       []string{"-p", "-c", "git", "log", "--oneline"},
			wantFlags:  []string{"-p"},
			wantCmd:    []string{"git", "log", "--oneline"},
			wantHasCmd: true,
		},
		{
			name:       "项目参数在 -c 之前",
			args:       []string{"proj/a", "proj/b", "-c", "git", "log"},
			wantFlags:  []string{"proj/a", "proj/b"},
			wantCmd:    []string{"git", "log"},
			wantHasCmd: true,
		},
		{
			name:       "-c 后保留 -- 边界",
			args:       []string{"-c", "git", "log", "--", "file"},
			wantFlags:  []string{},
			wantCmd:    []string{"git", "log", "--", "file"},
			wantHasCmd: true,
		},
		{
			name:       "--command=value 形式",
			args:       []string{"--command=git log", "--oneline"},
			wantFlags:  []string{},
			wantCmd:    []string{"git log", "--oneline"},
			wantHasCmd: true,
		},
		{
			name:       "-cVALUE 紧凑形式",
			args:       []string{"-cgit", "log", "--oneline"},
			wantFlags:  []string{},
			wantCmd:    []string{"git", "log", "--oneline"},
			wantHasCmd: true,
		},
		{
			name:       "-- 之后不再识别 -c",
			args:       []string{"--", "-c", "git"},
			wantFlags:  []string{"--", "-c", "git"},
			wantCmd:    nil,
			wantHasCmd: false,
		},
		{
			name:       "无 -c",
			args:       []string{"proj"},
			wantFlags:  []string{"proj"},
			wantCmd:    nil,
			wantHasCmd: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			flags, command, has := splitForallArgs(tt.args)
			if !reflect.DeepEqual(flags, tt.wantFlags) {
				t.Errorf("flagArgs = %#v, want %#v", flags, tt.wantFlags)
			}
			if !reflect.DeepEqual(command, tt.wantCmd) {
				t.Errorf("command = %#v, want %#v", command, tt.wantCmd)
			}
			if has != tt.wantHasCmd {
				t.Errorf("hasCommand = %v, want %v", has, tt.wantHasCmd)
			}
		})
	}
}

// TestParseForallCommand 验证上游 shell=False 判定：
// 命令名仅含安全字符时直接 exec argv（保留边界），否则交由 shell 执行。
func TestParseForallCommand(t *testing.T) {
	tests := []struct {
		name      string
		command   []string
		wantArgv  []string
		wantShell bool
	}{
		{
			name:      "git 参数形式直接 exec",
			command:   []string{"git", "log", "--oneline"},
			wantArgv:  []string{"git", "log", "--oneline"},
			wantShell: false,
		},
		{
			name:      "含空格的命令串需要 shell",
			command:   []string{"git log --oneline"},
			wantArgv:  []string{"git log --oneline"},
			wantShell: true,
		},
		{
			name:      "带管道的命令需要 shell",
			command:   []string{"git log | head"},
			wantArgv:  []string{"git log | head"},
			wantShell: true,
		},
		{
			name:      "脚本路径直接 exec",
			command:   []string{"./build.sh", "arg1"},
			wantArgv:  []string{"./build.sh", "arg1"},
			wantShell: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			argv, useShell := parseForallCommand(tt.command)
			if !reflect.DeepEqual(argv, tt.wantArgv) {
				t.Errorf("argv = %#v, want %#v", argv, tt.wantArgv)
			}
			if useShell != tt.wantShell {
				t.Errorf("useShell = %v, want %v", useShell, tt.wantShell)
			}
		})
	}
}

// TestFilterForallProjectsByPatterns 验证 -r/-i 位置参数按大小写不敏感正则
// 匹配项目名/相对路径，反向选择，非法正则返回错误。
func TestFilterForallProjectsByPatterns(t *testing.T) {
	projects := []*project.Project{
		{Name: "platform/build", Relpath: "build"},
		{Name: "platform/kernel", Relpath: "kernel"},
		{Name: "vendor/acme", Relpath: "vendor/acme"},
	}

	tests := []struct {
		name     string
		patterns []string
		inverse  bool
		want     []string
		wantErr  bool
	}{
		{
			name:     "按名称不区分大小写匹配",
			patterns: []string{"PLATFORM"},
			want:     []string{"platform/build", "platform/kernel"},
		},
		{
			name:     "按相对路径匹配",
			patterns: []string{"^kernel$"},
			want:     []string{"platform/kernel"},
		},
		{
			name:     "多模式并集",
			patterns: []string{"build", "acme"},
			want:     []string{"platform/build", "vendor/acme"},
		},
		{
			name:     "反向选择",
			patterns: []string{"platform"},
			inverse:  true,
			want:     []string{"vendor/acme"},
		},
		{
			name:     "无模式时普通模式不选任何项目",
			patterns: nil,
			want:     []string{},
		},
		{
			name:     "无模式时反向选择全部项目",
			patterns: nil,
			inverse:  true,
			want:     []string{"platform/build", "platform/kernel", "vendor/acme"},
		},
		{
			name:     "非法正则返回错误",
			patterns: []string{"[unterminated"},
			wantErr:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := filterForallProjectsByPatterns(projects, tt.patterns, tt.inverse)
			if (err != nil) != tt.wantErr {
				t.Fatalf("error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			names := make([]string, 0, len(got))
			for _, p := range got {
				names = append(names, p.Name)
			}
			if !reflect.DeepEqual(names, tt.want) {
				t.Errorf("matched = %#v, want %#v", names, tt.want)
			}
		})
	}
}

// TestForallWorktreeMirrorUsesGitdir 验证镜像项目以 Gitdir（回退 Objdir）
// 作为工作目录；普通项目仍使用工作树。
func TestForallWorktreeMirrorUsesGitdir(t *testing.T) {
	tests := []struct {
		name   string
		topDir string
		proj   *project.Project
		want   string
	}{
		{
			name:   "mirror uses gitdir",
			topDir: "/repo",
			proj:   &project.Project{Mirror: true, Worktree: "", Gitdir: "/repo/a.git", Objdir: "/repo/a.git"},
			want:   "/repo/a.git",
		},
		{
			name:   "mirror falls back to objdir",
			topDir: "/repo",
			proj:   &project.Project{Mirror: true, Worktree: "", Gitdir: "", Objdir: "/repo/b.git"},
			want:   "/repo/b.git",
		},
		{
			name:   "normal project uses worktree",
			topDir: "/repo",
			proj:   &project.Project{Worktree: "a"},
			want:   "/repo/a",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := forallWorktree(tt.topDir, tt.proj); got != tt.want {
				t.Errorf("forallWorktree() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestBuildForallEnvMirrorSetsGitDir 验证镜像项目导出 GIT_DIR，且 REPO_OUT_PWD
// 取 Gitdir 而不是空 Worktree（filepath.Abs("") 会得到进程 cwd）。
func TestBuildForallEnvMirrorSetsGitDir(t *testing.T) {
	t.Setenv("GIT_DIR", "/stale/repo.git")

	dir := t.TempDir()
	proj := &project.Project{Name: "m", Mirror: true, Gitdir: dir, Objdir: dir}

	env := buildForallEnv(proj, 1)

	gitDirs := make([]string, 0, 1)
	outPwd := ""
	outPwdSet := false
	for _, kv := range env {
		switch {
		case strings.HasPrefix(kv, "GIT_DIR="):
			gitDirs = append(gitDirs, strings.TrimPrefix(kv, "GIT_DIR="))
		case strings.HasPrefix(kv, "REPO_OUT_PWD="):
			outPwd = strings.TrimPrefix(kv, "REPO_OUT_PWD=")
			outPwdSet = true
		}
	}
	if len(gitDirs) != 1 || gitDirs[0] != dir {
		t.Errorf("GIT_DIR = %v, want exactly [%s]", gitDirs, dir)
	}
	if !outPwdSet || outPwd != dir {
		t.Errorf("REPO_OUT_PWD = %q (set=%v), want %q", outPwd, outPwdSet, dir)
	}
}

// TestRunForallCommandMirrorExecutes 验证镜像项目下命令可执行：在裸仓库目录中
// 设置 GIT_DIR 后运行 git 命令。
func TestRunForallCommandMirrorExecutes(t *testing.T) {
	runner := git.NewRunner()
	repoDir := t.TempDir()
	if _, err := runner.Run("init", "--bare", repoDir); err != nil {
		t.Fatalf("git init --bare: %v", err)
	}

	proj := &project.Project{Name: "m", Mirror: true, Gitdir: repoDir, Objdir: repoDir}
	opts := &ForallOptions{Command: []string{"git", "rev-parse", "--is-bare-repository"}}

	out, err := captureForallStdout(t, func() error {
		return runForallCommand(context.Background(), opts, proj, 1, repoDir)
	})
	if err != nil {
		t.Fatalf("runForallCommand() error = %v", err)
	}
	if got := strings.TrimSpace(out); got != "true" {
		t.Errorf("mirror command output = %q, want true", got)
	}
}

// captureForallStdout 临时替换 os.Stdout 以捕获 runForallCommand 的输出。
func captureForallStdout(t *testing.T, fn func() error) (string, error) {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stdout = w
	defer func() { os.Stdout = old }()
	defer func() {
		if cerr := r.Close(); cerr != nil {
			t.Errorf("close pipe reader: %v", cerr)
		}
	}()

	runErr := fn()
	if cerr := w.Close(); cerr != nil {
		t.Fatalf("close pipe writer: %v", cerr)
	}

	out, rerr := io.ReadAll(r)
	if rerr != nil {
		t.Fatalf("read pipe: %v", rerr)
	}
	return string(out), runErr
}
