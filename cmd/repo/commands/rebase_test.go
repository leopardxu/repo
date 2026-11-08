package commands

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/leopardxu/repo-go/internal/git"
	"github.com/leopardxu/repo-go/internal/logger"
	"github.com/leopardxu/repo-go/internal/project"
)

// TestBuildRebaseUpstream 验证 rebase 上游 ref 仅来自分支跟踪配置：
// 缺少 branch.<name>.remote 或 merge 时返回空串（不回退 manifest revision）；
// refs/heads 映射到远端跟踪 ref，非分支 ref 原样返回。
func TestBuildRebaseUpstream(t *testing.T) {
	const sha = "0123456789abcdef0123456789abcdef01234567"

	tests := []struct {
		name         string
		branchRemote string
		branchMerge  string
		want         string
	}{
		{
			name:         "分支配置 refs/heads 映射到远端",
			branchRemote: "origin",
			branchMerge:  "refs/heads/main",
			want:         "origin/main",
		},
		{
			name:         "分支配置远端为 . 时保持本地完整 ref",
			branchRemote: ".",
			branchMerge:  "refs/heads/main",
			want:         "refs/heads/main",
		},
		{
			name:         "分支配置 tag ref 原样使用",
			branchRemote: "origin",
			branchMerge:  "refs/tags/v1",
			want:         "refs/tags/v1",
		},
		{
			name:         "分支配置 SHA 原样使用",
			branchRemote: "origin",
			branchMerge:  sha,
			want:         sha,
		},
		{
			name:         "裸分支名映射到远端",
			branchRemote: "origin",
			branchMerge:  "main",
			want:         "origin/main",
		},
		{
			name:         "远端为 . 时裸分支名转完整 ref",
			branchRemote: ".",
			branchMerge:  "main",
			want:         "refs/heads/main",
		},
		{
			name:        "缺少 remote 不回退 manifest",
			branchMerge: "refs/heads/main",
			want:        "",
		},
		{
			name:         "缺少 merge 不回退 manifest",
			branchRemote: "origin",
			want:         "",
		},
		{
			name: "空配置返回空串",
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildRebaseUpstream(tt.branchRemote, tt.branchMerge)
			if got != tt.want {
				t.Errorf("buildRebaseUpstream() = %q, want %q", got, tt.want)
			}
		})
	}
}

// rebaseTestProjectWithRunner 构造用于 rebase 目标解析测试的项目。
func rebaseTestProjectWithRunner(name string, runner git.Runner) *project.Project {
	return &project.Project{
		Name:     name,
		Path:     "/repo/" + name,
		Worktree: "/repo/" + name,
		GitRepo:  git.NewRepository("/repo/"+name, runner),
	}
}

// TestRebaseUpstreamRefNoTracking 验证未配置 branch.<name>.remote/merge 时返回
// 空串，绝不回退项目 upstream/revision（manifest）。
func TestRebaseUpstreamRefNoTracking(t *testing.T) {
	runner := newPruneSequenceRunner(map[string][]mockGitResp{
		"config --get branch.main.remote": {{err: pruneTestError("no remote")}},
		"config --get branch.main.merge":  {{err: pruneTestError("no merge")}},
	})
	proj := rebaseTestProjectWithRunner("p", runner)

	if got := rebaseUpstreamRef(proj, "main"); got != "" {
		t.Fatalf("rebaseUpstreamRef() = %q, want empty (no manifest fallback)", got)
	}
}

// TestResolveRebaseTargetsSingleProject 验证单项目场景：
// detached HEAD / 未跟踪远程分支都必须报错，而不是 rebase 到 manifest。
func TestResolveRebaseTargetsSingleProject(t *testing.T) {
	t.Run("detached HEAD 报错", func(t *testing.T) {
		runner := newPruneSequenceRunner(map[string][]mockGitResp{
			"symbolic-ref HEAD": {{err: pruneTestError("detached")}},
		})
		proj := rebaseTestProjectWithRunner("p", runner)
		_, err := resolveRebaseTargets([]*project.Project{proj}, true, pruneTestLogger())
		if err == nil || !strings.Contains(err.Error(), "detached HEAD") {
			t.Fatalf("error = %v, want detached HEAD error", err)
		}
	})

	t.Run("无跟踪上游报错", func(t *testing.T) {
		runner := newPruneSequenceRunner(map[string][]mockGitResp{
			"symbolic-ref HEAD":               {{out: []byte("refs/heads/main\n")}},
			"config --get branch.main.remote": {{err: pruneTestError("no remote")}},
			"config --get branch.main.merge":  {{err: pruneTestError("no merge")}},
		})
		proj := rebaseTestProjectWithRunner("p", runner)
		_, err := resolveRebaseTargets([]*project.Project{proj}, true, pruneTestLogger())
		if err == nil || !strings.Contains(err.Error(), "does not track any remote branches") {
			t.Fatalf("error = %v, want no tracking error", err)
		}
	})
}

// TestResolveRebaseTargetsMultiProjectSkips 验证多项目场景：
// 未跟踪远程分支的项目被跳过，其余项目仍可 rebase，不整体报错。
func TestResolveRebaseTargetsMultiProjectSkips(t *testing.T) {
	runner := newPruneSequenceRunner(map[string][]mockGitResp{
		"symbolic-ref HEAD": {
			{out: []byte("refs/heads/main\n")},
			{out: []byte("refs/heads/main\n")},
		},
		"config --get branch.main.remote": {
			{out: []byte("origin\n")},
			{err: pruneTestError("no remote")},
		},
		"config --get branch.main.merge": {
			{out: []byte("refs/heads/main\n")},
			{err: pruneTestError("no merge")},
		},
	})
	a := rebaseTestProjectWithRunner("a", runner)
	b := rebaseTestProjectWithRunner("b", runner)

	targets, err := resolveRebaseTargets([]*project.Project{a, b}, true, pruneTestLogger())
	if err != nil {
		t.Fatalf("resolveRebaseTargets() error = %v, want nil", err)
	}
	if len(targets) != 1 {
		t.Fatalf("targets = %+v, want only project a", targets)
	}
	if targets[0].Project != a || targets[0].Upstream != "origin/main" {
		t.Fatalf("target = %+v, want project a -> origin/main", targets[0])
	}
}

// TestResolveRebaseTargetsNoUpstreamNeeded 验证 --abort/--continue/--skip
// 模式不解析上游，所有项目都进入 targets。
func TestResolveRebaseTargetsNoUpstreamNeeded(t *testing.T) {
	runner := newPruneSequenceRunner(nil)
	a := rebaseTestProjectWithRunner("a", runner)
	b := rebaseTestProjectWithRunner("b", runner)

	targets, err := resolveRebaseTargets([]*project.Project{a, b}, false, pruneTestLogger())
	if err != nil {
		t.Fatalf("resolveRebaseTargets() error = %v", err)
	}
	if len(targets) != 2 || targets[0].Upstream != "" || targets[1].Upstream != "" {
		t.Fatalf("targets = %+v, want both projects with empty upstream", targets)
	}
}

// rebaseRecordingRunner 记录普通/交互调用，交互调用不影响普通命令记录。
type rebaseRecordingRunner struct {
	*mockGitRunner
	interactiveCalls [][]string
	runInDirCalls    [][]string
	interactiveErr   error
}

func newRebaseRecordingRunner() *rebaseRecordingRunner {
	return &rebaseRecordingRunner{
		mockGitRunner: &mockGitRunner{responses: make(map[string]mockGitResp)},
	}
}

func (r *rebaseRecordingRunner) RunInDir(dir string, args ...string) ([]byte, error) {
	r.runInDirCalls = append(r.runInDirCalls, append([]string(nil), args...))
	return r.mockGitRunner.RunInDir(dir, args...)
}

func (r *rebaseRecordingRunner) RunInteractiveInDir(_ string, args ...string) error {
	r.interactiveCalls = append(r.interactiveCalls, append([]string(nil), args...))
	return r.interactiveErr
}

func newRebaseTestProject(runner git.Runner) *project.Project {
	return &project.Project{
		Name:     "p",
		Worktree: "/repo/p",
		GitRepo:  git.NewRepository("/repo/p", runner),
	}
}

// TestBuildRebaseBaseArgs 验证交互/非交互 rebase 的参数路径：
// -i 只追加 --interactive，中断类动作优先且不带其它选项。
func TestBuildRebaseBaseArgs(t *testing.T) {
	log := logger.NewDefaultLogger()
	log.SetLevel(logger.LogLevelError)

	tests := []struct {
		name string
		opts *RebaseOptions
		want []string
	}{
		{
			name: "默认 rebase",
			opts: &RebaseOptions{},
			want: []string{"rebase"},
		},
		{
			name: "交互 rebase",
			opts: &RebaseOptions{Interactive: true},
			want: []string{"rebase", "--interactive"},
		},
		{
			name: "交互 + quiet",
			opts: &RebaseOptions{Interactive: true, Quiet: true},
			want: []string{"rebase", "--interactive", "--quiet"},
		},
		{
			name: "abort 优先于 interactive",
			opts: &RebaseOptions{Abort: true, Interactive: true},
			want: []string{"rebase", "--abort"},
		},
		{
			name: "continue",
			opts: &RebaseOptions{Continue: true},
			want: []string{"rebase", "--continue"},
		},
		{
			name: "skip",
			opts: &RebaseOptions{Skip: true},
			want: []string{"rebase", "--skip"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildRebaseBaseArgs(tt.opts, log)
			if strings.Join(got, " ") != strings.Join(tt.want, " ") {
				t.Errorf("buildRebaseBaseArgs() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestCheckRebaseInteractive 验证交互 rebase 前置校验：
// 仅允许单项目；完全无编辑器且无 TTY 时才拒绝，配置 GIT_SEQUENCE_EDITOR 或
// GIT_EDITOR 的非 TTY 场景允许执行。
func TestCheckRebaseInteractive(t *testing.T) {
	ttyTrue := func() bool { return true }
	ttyFalse := func() bool { return false }

	tests := []struct {
		name         string
		opts         *RebaseOptions
		projectCount int
		editorVar    string // 非空时设置该环境变量为 true
		wantErr      bool
	}{
		{name: "非交互不校验", opts: &RebaseOptions{}, projectCount: 3},
		{name: "交互单项目 TTY", opts: &RebaseOptions{Interactive: true, interactiveTTY: ttyTrue}, projectCount: 1},
		{name: "交互单项目非 TTY 无编辑器", opts: &RebaseOptions{Interactive: true, interactiveTTY: ttyFalse}, projectCount: 1, wantErr: true},
		{name: "非 TTY 配 GIT_SEQUENCE_EDITOR", opts: &RebaseOptions{Interactive: true, interactiveTTY: ttyFalse}, projectCount: 1, editorVar: "GIT_SEQUENCE_EDITOR"},
		{name: "非 TTY 配 GIT_EDITOR", opts: &RebaseOptions{Interactive: true, interactiveTTY: ttyFalse}, projectCount: 1, editorVar: "GIT_EDITOR"},
		{name: "交互多项目报错", opts: &RebaseOptions{Interactive: true, interactiveTTY: ttyTrue}, projectCount: 2, wantErr: true},
		{name: "交互零项目报错", opts: &RebaseOptions{Interactive: true, interactiveTTY: ttyTrue}, projectCount: 0, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("GIT_SEQUENCE_EDITOR", "")
			t.Setenv("GIT_EDITOR", "")
			if tt.editorVar != "" {
				t.Setenv(tt.editorVar, "true")
			}
			err := checkRebaseInteractive(tt.opts, tt.projectCount)
			if (err != nil) != tt.wantErr {
				t.Fatalf("checkRebaseInteractive() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

// TestRebaseSelectProjects 验证 rebase 项目筛选：
// 未显式指定项目时跳过未 checkout 项目（过滤发生在 -i/oneProject 计数之前），
// 显式指定项目时要求全部已同步。
func TestRebaseSelectProjects(t *testing.T) {
	root := t.TempDir()
	synced := &project.Project{Name: "synced", Worktree: root}
	missing := &project.Project{Name: "missing", Worktree: filepath.Join(root, "not-checked-out")}

	got, err := rebaseSelectProjects([]*project.Project{synced, missing}, false)
	if err != nil {
		t.Fatalf("rebaseSelectProjects(implicit) error = %v", err)
	}
	if len(got) != 1 || got[0] != synced {
		t.Fatalf("rebaseSelectProjects(implicit) = %v, want only synced", got)
	}

	got, err = rebaseSelectProjects([]*project.Project{synced}, true)
	if err != nil || len(got) != 1 || got[0] != synced {
		t.Fatalf("rebaseSelectProjects(explicit synced) = %v, %v", got, err)
	}

	if _, err := rebaseSelectProjects([]*project.Project{synced, missing}, true); err == nil {
		t.Fatal("rebaseSelectProjects(explicit missing) error = nil, want unsynced error")
	}
}

// TestRebaseWriteOutput 验证 rebase 的 git 输出写到给定 writer（stdout payload），
// 空输出不产生内容，缺少结尾换行时补齐。
func TestRebaseWriteOutput(t *testing.T) {
	var buf bytes.Buffer
	rebaseWriteOutput(&buf, "p", "ok\n")
	if want := "\n项目 p:\nok\n"; buf.String() != want {
		t.Fatalf("output = %q, want %q", buf.String(), want)
	}

	buf.Reset()
	rebaseWriteOutput(&buf, "p", "no-newline")
	if want := "\n项目 p:\nno-newline\n"; buf.String() != want {
		t.Fatalf("output = %q, want %q", buf.String(), want)
	}

	buf.Reset()
	rebaseWriteOutput(&buf, "p", "")
	if buf.Len() != 0 {
		t.Fatalf("empty output = %q, want none", buf.String())
	}
}

// TestRebaseInvoke 验证交互模式走 RunInteractive（stdin/stdout 直连），
// 非交互模式仍走 RunCommand。
func TestRebaseInvoke(t *testing.T) {
	t.Run("交互模式直连终端", func(t *testing.T) {
		runner := newRebaseRecordingRunner()
		proj := newRebaseTestProject(runner)
		args := []string{"rebase", "--interactive", "origin/main"}

		out, err := rebaseInvoke(proj, args, true)
		if err != nil {
			t.Fatalf("rebaseInvoke() error = %v", err)
		}
		if out != nil {
			t.Errorf("interactive output = %q, want nil", out)
		}
		if len(runner.interactiveCalls) != 1 || strings.Join(runner.interactiveCalls[0], " ") != strings.Join(args, " ") {
			t.Fatalf("interactiveCalls = %v, want %v", runner.interactiveCalls, args)
		}
		if len(runner.runInDirCalls) != 0 {
			t.Errorf("interactive must not use RunCommand, calls = %v", runner.runInDirCalls)
		}
	})

	t.Run("非交互模式捕获输出", func(t *testing.T) {
		runner := newRebaseRecordingRunner()
		runner.responses["rebase origin/main"] = mockGitResp{out: []byte("ok\n")}
		proj := newRebaseTestProject(runner)

		out, err := rebaseInvoke(proj, []string{"rebase", "origin/main"}, false)
		if err != nil {
			t.Fatalf("rebaseInvoke() error = %v", err)
		}
		if string(out) != "ok\n" {
			t.Errorf("output = %q, want %q", out, "ok\n")
		}
		if len(runner.interactiveCalls) != 0 {
			t.Errorf("non-interactive must not use RunInteractive, calls = %v", runner.interactiveCalls)
		}
	})
}

// TestRebaseCurrentBranchUsesSymbolicRef 验证当前分支用完整 symbolic-ref HEAD
// 再剥离 refs/heads/（--short 在同名 tag 下会返回 heads/main 歧义形式）。
func TestRebaseCurrentBranchUsesSymbolicRef(t *testing.T) {
	tests := []struct {
		name string
		resp mockGitResp
		want string
	}{
		{name: "普通分支", resp: mockGitResp{out: []byte("refs/heads/main\n")}, want: "main"},
		{name: "detached HEAD", resp: mockGitResp{err: pruneTestError("not a symbolic ref")}, want: ""},
		{name: "非 heads ref", resp: mockGitResp{out: []byte("refs/remotes/origin/main\n")}, want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runner := newRebaseRecordingRunner()
			runner.responses["symbolic-ref HEAD"] = tt.resp
			proj := newRebaseTestProject(runner)

			if got := rebaseCurrentBranch(proj); got != tt.want {
				t.Errorf("rebaseCurrentBranch() = %q, want %q", got, tt.want)
			}
			joined := strings.Join(runner.commands, "\n")
			if !strings.Contains(joined, "symbolic-ref HEAD") {
				t.Errorf("must use full symbolic-ref HEAD, commands = %v", runner.commands)
			}
			if strings.Contains(joined, "symbolic-ref --short HEAD") {
				t.Errorf("must not use symbolic-ref --short, commands = %v", runner.commands)
			}
		})
	}
}
