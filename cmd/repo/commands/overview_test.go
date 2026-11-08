package commands

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/leopardxu/repo-go/internal/git"
	"github.com/leopardxu/repo-go/internal/logger"
	"github.com/leopardxu/repo-go/internal/manifest"
	"github.com/leopardxu/repo-go/internal/project"
)

// TestOverviewResolveBase 验证 overview 基准解析先远端跟踪引用、再本地回退，
// SHA 原样，且无法解析时返回 ok=false 供调用方标注。
func TestOverviewResolveBase(t *testing.T) {
	dir := t.TempDir()
	localID := diffTestInitRepo(t, dir)
	remoteID := diffTestCommit(t, dir, "remote")

	runner := git.NewRunner()
	runGit := func(args ...string) {
		t.Helper()
		if out, err := runner.RunInDir(dir, args...); err != nil {
			t.Fatalf("git %v failed: %v: %s", args, err, out)
		}
	}
	runGit("branch", "dev", localID)
	runGit("update-ref", "refs/remotes/origin/dev", remoteID)

	proj := &project.Project{
		Name:       "foo",
		RemoteName: "origin",
		Revision:   "dev",
		GitRepo:    git.NewRepository(dir, runner),
	}
	log := logger.NewDefaultLogger()

	base, ok := overviewResolveBase(proj, nil, log)
	if !ok || base != remoteID {
		t.Errorf("overviewResolveBase(dev) = (%q, %v), want (%q, true)", base, ok, remoteID)
	}

	// remote 声明 alias 时，清单 remote name 要映射到本地 alias 引用。
	runGit("update-ref", "refs/remotes/up/dev", remoteID)
	aliasProj := &project.Project{
		Name:       "foo",
		RemoteName: "upstream",
		Revision:   "dev",
		GitRepo:    git.NewRepository(dir, runner),
	}
	base, ok = overviewResolveBase(aliasProj, []manifest.Remote{{Name: "upstream", Alias: "up"}}, log)
	if !ok || base != remoteID {
		t.Errorf("overviewResolveBase(alias) = (%q, %v), want (%q, true)", base, ok, remoteID)
	}

	runGit("update-ref", "-d", "refs/remotes/origin/dev")
	base, ok = overviewResolveBase(proj, nil, log)
	if !ok || base != localID {
		t.Errorf("远端缺失时 overviewResolveBase(dev) = (%q, %v), want (%q, true)", base, ok, localID)
	}

	proj.Revision = localID
	base, ok = overviewResolveBase(proj, nil, log)
	if !ok || base != localID {
		t.Errorf("overviewResolveBase(SHA) = (%q, %v), want (%q, true)", base, ok, localID)
	}

	// 裸 revision 只按分支名处理；只存在同名 tag 时不得解析成 tag。
	runGit("tag", "v-only-tag", remoteID)
	proj.Revision = "v-only-tag"
	if _, ok := overviewResolveBase(proj, nil, log); ok {
		t.Error("裸 tag 名不应被当作 tag 解析")
	}
	proj.Revision = "refs/tags/v-only-tag"
	base, ok = overviewResolveBase(proj, nil, log)
	if !ok || base != remoteID {
		t.Errorf("显式 refs/tags 应解析为 tag = (%q, %v), want (%q, true)", base, ok, remoteID)
	}

	proj.Revision = "missing-branch"
	if _, ok := overviewResolveBase(proj, nil, log); ok {
		t.Error("无法解析的 revision 应返回 ok=false")
	}
}

// TestRunOverviewAnnotatesUnresolvedRevision 验证 overview 在 revision 无法解析
// 时显式标注而不是静默当作 0 个未推送提交。
func TestRunOverviewAnnotatesUnresolvedRevision(t *testing.T) {
	repoRoot := readCmdTestRepoRoot(t, `  <project name="foo" path="foo" revision="missing-branch" />`)
	dir := filepath.Join(repoRoot, "foo")
	mustMkdir(t, dir)
	diffTestInitRepo(t, dir)

	readCmdWithCwd(t, repoRoot, func() {
		stdout, _, err := readCmdRunCaptured(t, func() error {
			return runOverview(&OverviewOptions{}, nil)
		})
		if err != nil {
			t.Fatalf("runOverview failed: %v", err)
		}
		for _, want := range []string{"project foo", "无法解析 manifest revision", "missing-branch"} {
			if !strings.Contains(stdout, want) {
				t.Errorf("stdout 缺少 %q，实际输出:\n%s", want, stdout)
			}
		}
	})
}
