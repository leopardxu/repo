package repo_sync

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/leopardxu/repo-go/internal/config"
	"github.com/leopardxu/repo-go/internal/git"
	"github.com/leopardxu/repo-go/internal/logger"
	"github.com/leopardxu/repo-go/internal/manifest"
	"github.com/leopardxu/repo-go/internal/project"
)

// mustGit 执行 git 命令；测试内使用 internal/git.Runner，不直接 exec。
func mustGit(t *testing.T, runner git.Runner, dir string, args ...string) string {
	t.Helper()
	out, err := runner.RunInDir(dir, args...)
	if err != nil {
		t.Fatalf("git %v (dir=%s) failed: %v", args, dir, err)
	}
	return strings.TrimSpace(string(out))
}

// setupRemoteWithCommit 创建带一个提交的裸远程仓库与可继续提交的工作仓库。
func setupRemoteWithCommit(t *testing.T) (root, bare, work string, runner git.Runner) {
	t.Helper()
	root = t.TempDir()
	runner = git.NewRunner()
	runner.SetMaxRetries(0)

	bare = filepath.Join(root, "remote.git")
	if _, err := runner.RunInDir(root, "init", "--bare", bare); err != nil {
		t.Fatalf("init bare: %v", err)
	}

	work = filepath.Join(root, "src")
	if err := os.MkdirAll(work, 0755); err != nil {
		t.Fatal(err)
	}
	mustGit(t, runner, work, "init")
	mustGit(t, runner, work, "config", "user.email", "test@example.com")
	mustGit(t, runner, work, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(work, "file.txt"), []byte("hello\n"), 0644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, runner, work, "add", "file.txt")
	mustGit(t, runner, work, "commit", "-m", "c1")
	mustGit(t, runner, work, "remote", "add", "origin", bare)
	mustGit(t, runner, work, "push", "origin", "HEAD:refs/heads/main")
	// 让裸仓库 HEAD 指向 main，clone 才能正常检出。
	mustGit(t, runner, bare, "symbolic-ref", "HEAD", "refs/heads/main")
	return root, bare, work, runner
}

// TestNetworkOnlyCloneThenNormalSyncPopulatesWorktree 回归测试：
// --network-only 使用 clone --no-checkout 后，普通 sync 在 HEAD==revid 时
// 必须补检出，而不是因早退留下空工作树。
func TestNetworkOnlyCloneThenNormalSyncPopulatesWorktree(t *testing.T) {
	root, bare, _, _ := setupRemoteWithCommit(t)
	worktree := filepath.Join(root, "proj")
	m := &manifest.Manifest{Topdir: root, RepoDir: filepath.Join(root, ".repo")}

	networkOpts := &Options{Jobs: 1, Quiet: true, NetworkOnly: true}
	networkEngine := NewEngine(networkOpts, m, logger.NewDefaultLogger())
	p := &project.Project{
		Name:       "proj",
		RemoteName: "origin",
		RemoteURL:  bare,
		Worktree:   worktree,
		Revision:   "main",
	}
	if err := networkEngine.cloneProject(context.Background(), p); err != nil {
		t.Fatalf("network-only cloneProject() error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(worktree, "file.txt")); !os.IsNotExist(err) {
		// clone --no-checkout 可能因 git 版本留下空 index 但不应有工作树文件
		if err == nil {
			t.Fatal("network-only 后不应存在工作树文件")
		}
	}

	localOpts := &Options{Jobs: 1, Quiet: true}
	localEngine := NewEngine(localOpts, m, logger.NewDefaultLogger())
	p2 := &project.Project{
		Name:       "proj",
		RemoteName: "origin",
		RemoteURL:  bare,
		Worktree:   worktree,
		Revision:   "main",
	}
	if err := localEngine.checkoutProject(context.Background(), p2); err != nil {
		t.Fatalf("checkoutProject() error = %v", err)
	}
	data, err := os.ReadFile(filepath.Join(worktree, "file.txt"))
	if err != nil {
		t.Fatalf("普通 sync 后工作树仍为空: %v", err)
	}
	if string(data) != "hello\n" {
		t.Errorf("file.txt = %q, want %q", data, "hello\n")
	}
}

// TestRecoverIncompleteCloneCheckout 验证 clone 在 checkout 阶段失败留下的
// 仓库（对象和 HEAD 已存在）可被恢复，而不是直接放弃。
func TestRecoverIncompleteCloneCheckout(t *testing.T) {
	root, bare, _, runner := setupRemoteWithCommit(t)
	worktree := filepath.Join(root, "proj")
	// 模拟 clone --no-checkout 留下的残留：HEAD 已指向提交，工作树为空。
	if _, err := runner.RunInDir(root, "clone", "--no-checkout", bare, worktree); err != nil {
		t.Fatalf("clone --no-checkout: %v", err)
	}

	e := NewEngine(&Options{Jobs: 1, Quiet: true},
		&manifest.Manifest{Topdir: root}, logger.NewDefaultLogger())
	p := &project.Project{Name: "proj", Worktree: worktree}
	if !e.recoverIncompleteCloneCheckout(p) {
		t.Fatal("recoverIncompleteCloneCheckout() = false, want true")
	}
	if _, err := os.Stat(filepath.Join(worktree, "file.txt")); err != nil {
		t.Errorf("恢复后工作树应有 file.txt: %v", err)
	}
}

// TestUpdateManifestRepoFetchFailureFatal 验证 manifest 仓库 fetch origin 与
// fetch --all 都失败时必须返回错误（runSync 的致命逻辑依赖此错误）。
func TestUpdateManifestRepoFetchFailureFatal(t *testing.T) {
	root := t.TempDir()
	manifests := filepath.Join(root, ".repo", "manifests")
	if err := os.MkdirAll(manifests, 0755); err != nil {
		t.Fatal(err)
	}
	runner := git.NewRunner()
	runner.SetMaxRetries(0)
	if _, err := runner.RunInDir(manifests, "init"); err != nil {
		t.Fatalf("git init: %v", err)
	}
	mustGit(t, runner, manifests, "config", "user.email", "test@example.com")
	mustGit(t, runner, manifests, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(manifests, "default.xml"), []byte("<manifest/>"), 0644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, runner, manifests, "add", "default.xml")
	mustGit(t, runner, manifests, "commit", "-m", "init")
	mustGit(t, runner, manifests, "remote", "add", "origin", filepath.Join(root, "does-not-exist.git"))

	e := &Engine{
		repoRoot:  root,
		options:   &Options{},
		config:    &config.Config{ManifestBranch: "main"},
		logger:    logger.NewDefaultLogger(),
		gitRunner: runner,
	}
	if err := e.UpdateManifestRepo(); err == nil {
		t.Fatal("fetch origin 与 fetch --all 都失败时应返回错误")
	}
}

// TestUpdateManifestRepoCommitRevision 验证 -b <commit SHA> 时 fetch 并 reset
// 到该 SHA，绝不回退到默认分支顶端。
func TestUpdateManifestRepoCommitRevision(t *testing.T) {
	root, bare, work, runner := setupRemoteWithCommit(t)
	manifests := filepath.Join(root, ".repo", "manifests")
	if err := os.MkdirAll(filepath.Dir(manifests), 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := runner.RunInDir(root, "clone", bare, manifests); err != nil {
		t.Fatalf("clone manifests: %v", err)
	}
	firstSHA := mustGit(t, runner, manifests, "rev-parse", "HEAD")

	// 远程前进到第二个提交，本地 manifest 仓库也更新到最新，之后要求回退到 firstSHA。
	if err := os.WriteFile(filepath.Join(work, "file.txt"), []byte("second\n"), 0644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, runner, work, "commit", "-am", "c2")
	mustGit(t, runner, work, "push", "origin", "HEAD:refs/heads/main")
	mustGit(t, runner, manifests, "fetch", "origin")
	mustGit(t, runner, manifests, "reset", "--hard", "origin/main")
	if got := mustGit(t, runner, manifests, "rev-parse", "HEAD"); got == firstSHA {
		t.Fatal("测试前置失败：本地 manifest 仓库仍在第一个提交")
	}

	e := &Engine{
		repoRoot:  root,
		options:   &Options{},
		config:    &config.Config{ManifestBranch: firstSHA},
		logger:    logger.NewDefaultLogger(),
		gitRunner: runner,
	}
	if err := e.UpdateManifestRepo(); err != nil {
		t.Fatalf("UpdateManifestRepo() error = %v", err)
	}
	if got := mustGit(t, runner, manifests, "rev-parse", "HEAD"); got != firstSHA {
		t.Errorf("manifest HEAD = %s, want %s (must not reset to default branch)", got, firstSHA)
	}
}

// TestPreSyncMissingSuperprojectIsNonFatal 验证清单没有 <superproject> 时
// --use-superproject 告警后跳过，不中断同步。
func TestPreSyncMissingSuperprojectIsNonFatal(t *testing.T) {
	e := NewEngine(&Options{Jobs: 1, Quiet: true, UseSuperproject: true},
		&manifest.Manifest{}, logger.NewDefaultLogger())
	if err := e.preSync(context.Background()); err != nil {
		t.Fatalf("preSync() error = %v, want nil", err)
	}
}

// TestEnsureWorktreePopulatedBareRepoSkips 验证裸仓库（无工作树）时
// ensureWorktreePopulated 直接返回 nil，不执行 reset --hard、不改写 HEAD。
func TestEnsureWorktreePopulatedBareRepoSkips(t *testing.T) {
	_, bare, _, runner := setupRemoteWithCommit(t)
	headBefore := mustGit(t, runner, bare, "rev-parse", "HEAD")

	e := NewEngine(&Options{Jobs: 1, Quiet: true},
		&manifest.Manifest{}, logger.NewDefaultLogger())
	p := &project.Project{Name: "bareproj", Worktree: bare}
	if err := e.ensureWorktreePopulated(context.Background(), p, headBefore); err != nil {
		t.Fatalf("ensureWorktreePopulated(bare) error = %v", err)
	}
	if got := mustGit(t, runner, bare, "rev-parse", "HEAD"); got != headBefore {
		t.Errorf("bare repo HEAD changed: got %s, want %s", got, headBefore)
	}
}

// TestEnsureWorktreePopulatedMirrorSkips 验证镜像项目（裸仓库布局）同样跳过，
// 不会因 reset --hard 失败而中断同步。
func TestEnsureWorktreePopulatedMirrorSkips(t *testing.T) {
	_, bare, _, runner := setupRemoteWithCommit(t)
	headBefore := mustGit(t, runner, bare, "rev-parse", "HEAD")

	e := NewEngine(&Options{Jobs: 1, Quiet: true},
		&manifest.Manifest{}, logger.NewDefaultLogger())
	p := &project.Project{Name: "mirrorproj", Mirror: true, Gitdir: bare, Worktree: ""}
	if err := e.ensureWorktreePopulated(context.Background(), p, headBefore); err != nil {
		t.Fatalf("ensureWorktreePopulated(mirror) error = %v", err)
	}
	if got := mustGit(t, runner, bare, "rev-parse", "HEAD"); got != headBefore {
		t.Errorf("mirror repo HEAD changed: got %s, want %s", got, headBefore)
	}
}

// TestEnsureWorktreePopulatedEmptyWorktreeResets 验证 clone --no-checkout
// 留下的空工作树会被 reset --hard 补齐。
func TestEnsureWorktreePopulatedEmptyWorktreeResets(t *testing.T) {
	root, bare, _, runner := setupRemoteWithCommit(t)
	worktree := filepath.Join(root, "proj")
	if _, err := runner.RunInDir(root, "clone", "--no-checkout", bare, worktree); err != nil {
		t.Fatalf("clone --no-checkout: %v", err)
	}
	revid := mustGit(t, runner, worktree, "rev-parse", "HEAD")

	e := NewEngine(&Options{Jobs: 1, Quiet: true},
		&manifest.Manifest{Topdir: root}, logger.NewDefaultLogger())
	p := &project.Project{Name: "proj", Worktree: worktree}
	if err := e.ensureWorktreePopulated(context.Background(), p, revid); err != nil {
		t.Fatalf("ensureWorktreePopulated() error = %v", err)
	}
	data, err := os.ReadFile(filepath.Join(worktree, "file.txt"))
	if err != nil {
		t.Fatalf("工作树未被补齐: %v", err)
	}
	if string(data) != "hello\n" {
		t.Errorf("file.txt = %q, want %q", data, "hello\n")
	}
}

// TestEnsureWorktreePopulatedKeepsExistingFiles 验证 index 为空但工作树文件
// 仍存在时不做 reset --hard，避免覆盖本地内容。
func TestEnsureWorktreePopulatedKeepsExistingFiles(t *testing.T) {
	root, bare, _, runner := setupRemoteWithCommit(t)
	worktree := filepath.Join(root, "proj")
	if _, err := runner.RunInDir(root, "clone", "--no-checkout", bare, worktree); err != nil {
		t.Fatalf("clone --no-checkout: %v", err)
	}
	revid := mustGit(t, runner, worktree, "rev-parse", "HEAD")
	// 模拟 index 丢失但工作树仍有本地内容。
	const local = "local-edit\n"
	if err := os.WriteFile(filepath.Join(worktree, "file.txt"), []byte(local), 0644); err != nil {
		t.Fatal(err)
	}

	e := NewEngine(&Options{Jobs: 1, Quiet: true},
		&manifest.Manifest{Topdir: root}, logger.NewDefaultLogger())
	p := &project.Project{Name: "proj", Worktree: worktree}
	if err := e.ensureWorktreePopulated(context.Background(), p, revid); err != nil {
		t.Fatalf("ensureWorktreePopulated() error = %v", err)
	}
	data, err := os.ReadFile(filepath.Join(worktree, "file.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != local {
		t.Errorf("本地文件被 reset 覆盖: got %q, want %q", data, local)
	}
}

// TestSetupRemotePushURLReviewAndMirror 覆盖 setupRemote 的 remote 配置语义：
//   - pushurl 只取清单 pushurl 基址并“无条件”拼接 "/<项目名>"（含已是完整
//     URL 的值，对齐上游 pushUrl + "/" + projectname）；
//   - review 原值写入 remote.<name>.review，绝不冒充 pushurl；
//   - 字段消失时 unset-all 清理旧值；
//   - mirror 用 --replace-all 而非 --add，重复值被替换为单一 true。
func TestSetupRemotePushURLReviewAndMirror(t *testing.T) {
	tests := []struct {
		name          string
		remote        manifest.Remote
		manifestURL   string
		mirror        bool
		wantPushURL   string
		wantReviewURL string
	}{
		{
			name:        "manifest pushurl base appends project",
			remote:      manifest.Remote{Name: "origin", Fetch: "https://example.com/manifests", PushURL: "ssh://git@example.com:29418/push"},
			wantPushURL: "ssh://git@example.com:29418/push/proj",
		},
		{
			name:        "full pushurl still appends project unconditionally",
			remote:      manifest.Remote{Name: "origin", Fetch: "https://example.com/manifests", PushURL: "ssh://git@example.com:29418/push/proj"},
			wantPushURL: "ssh://git@example.com:29418/push/proj/proj",
		},
		{
			name:        "dot relative pushurl resolves then appends",
			remote:      manifest.Remote{Name: "origin", Fetch: "https://example.com/manifests", PushURL: "../push"},
			manifestURL: "https://example.com/manifests",
			wantPushURL: "https://example.com/push/proj",
		},
		{
			name:        "non-dot relative pushurl resolves then appends",
			remote:      manifest.Remote{Name: "origin", Fetch: "https://example.com/manifests", PushURL: "push"},
			manifestURL: "https://example.com/manifests",
			wantPushURL: "https://example.com/push/proj",
		},
		{
			name:          "review written to review not pushurl",
			remote:        manifest.Remote{Name: "origin", Fetch: "https://example.com/manifests", Review: "ssh://git@example.com:29418/review"},
			wantPushURL:   "",
			wantReviewURL: "ssh://git@example.com:29418/review",
		},
		{
			name:          "pushurl and review both preserved separately",
			remote:        manifest.Remote{Name: "origin", Fetch: "https://example.com/manifests", PushURL: "ssh://git@example.com:29418/push", Review: "ssh://git@example.com:29418/review/raw"},
			wantPushURL:   "ssh://git@example.com:29418/push/proj",
			wantReviewURL: "ssh://git@example.com:29418/review/raw",
		},
		{
			name:   "fields absent are unset",
			remote: manifest.Remote{Name: "origin", Fetch: "https://example.com/manifests"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			runner := git.NewRunner()
			runner.SetMaxRetries(0)
			worktree := filepath.Join(root, "proj")
			if err := os.MkdirAll(worktree, 0755); err != nil {
				t.Fatal(err)
			}
			mustGit(t, runner, worktree, "init")
			// 预置多值残留，验证字段消失时 unset-all 清空、存在时 replace-all 收敛。
			mustGit(t, runner, worktree, "config", "--add", "remote.origin.pushurl", "stale-push-1")
			mustGit(t, runner, worktree, "config", "--add", "remote.origin.pushurl", "stale-push-2")
			mustGit(t, runner, worktree, "config", "--add", "remote.origin.review", "stale-review-1")
			mustGit(t, runner, worktree, "config", "--add", "remote.origin.review", "stale-review-2")

			m := &manifest.Manifest{Remotes: []manifest.Remote{tt.remote}}
			e := &Engine{
				manifest:  m,
				options:   &Options{Quiet: true},
				logger:    logger.NewDefaultLogger(),
				gitRunner: runner,
			}
			p := &project.Project{
				Name:        "proj",
				RemoteName:  "origin",
				Worktree:    worktree,
				ManifestURL: tt.manifestURL,
			}
			if err := e.setupRemote(p, "https://example.com/proj"); err != nil {
				t.Fatalf("setupRemote() error = %v", err)
			}

			assertGitConfigValue(t, runner, worktree, "remote.origin.pushurl", tt.wantPushURL)
			assertGitConfigValue(t, runner, worktree, "remote.origin.review", tt.wantReviewURL)
		})
	}
}

// assertGitConfigValue 断言 key 的最终唯一值；want 为空时断言 key 不存在。
func assertGitConfigValue(t *testing.T, runner git.Runner, dir, key, want string) {
	t.Helper()
	out, err := runner.RunInDir(dir, "config", "--get-all", key)
	if want == "" {
		if err == nil {
			t.Errorf("%s should be unset, got %q", key, strings.TrimSpace(string(out)))
		}
		return
	}
	if err != nil {
		t.Fatalf("%s unset, want %q", key, want)
	}
	if got := strings.TrimSpace(string(out)); got != want {
		t.Errorf("%s = %q, want %q (must not accumulate values)", key, got, want)
	}
}

// TestSetupRemoteMirrorReplacesNotAdds 验证 mirror 使用 replace，不累积多值。
func TestSetupRemoteMirrorReplacesNotAdds(t *testing.T) {
	root := t.TempDir()
	runner := git.NewRunner()
	runner.SetMaxRetries(0)
	worktree := filepath.Join(root, "proj")
	if err := os.MkdirAll(worktree, 0755); err != nil {
		t.Fatal(err)
	}
	mustGit(t, runner, worktree, "init")
	mustGit(t, runner, worktree, "config", "--add", "remote.origin.mirror", "false")
	mustGit(t, runner, worktree, "config", "--add", "remote.origin.mirror", "false")

	e := &Engine{
		manifest:  &manifest.Manifest{},
		options:   &Options{Quiet: true, Config: &config.Config{Mirror: true}},
		logger:    logger.NewDefaultLogger(),
		gitRunner: runner,
	}
	p := &project.Project{Name: "proj", RemoteName: "origin", Worktree: worktree}
	if err := e.setupRemote(p, "https://example.com/proj"); err != nil {
		t.Fatalf("setupRemote() error = %v", err)
	}

	got := mustGit(t, runner, worktree, "config", "--get-all", "remote.origin.mirror")
	if got != "true" {
		t.Errorf("remote.origin.mirror = %q, want single value true", got)
	}
}

// TestCheckoutProjectPopulatesEmptyWorktreeBeforeFastForward 回归：
// sync -n 之后远端前进时，checkoutProject 必须先补齐空工作树再执行
// merge --ff-only，否则会因“本地 tracked 文件被删除”失败。
func TestCheckoutProjectPopulatesEmptyWorktreeBeforeFastForward(t *testing.T) {
	root, bare, work, runner := setupRemoteWithCommit(t)
	worktree := filepath.Join(root, "proj")
	// sync -n 的产物：.git 与 HEAD 已就位，但 index/工作树为空。
	if _, err := runner.RunInDir(root, "clone", "--no-checkout", bare, worktree); err != nil {
		t.Fatalf("clone --no-checkout: %v", err)
	}

	// 远端前进到第二个提交。
	if err := os.WriteFile(filepath.Join(work, "file.txt"), []byte("second\n"), 0644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, runner, work, "commit", "-am", "c2")
	mustGit(t, runner, work, "push", "origin", "HEAD:refs/heads/main")
	mustGit(t, runner, worktree, "fetch", "origin")

	e := NewEngine(&Options{Jobs: 1, Quiet: true},
		&manifest.Manifest{Topdir: root}, logger.NewDefaultLogger())
	p := &project.Project{
		Name:       "proj",
		RemoteName: "origin",
		RemoteURL:  bare,
		Worktree:   worktree,
		Revision:   "main",
	}
	if err := e.checkoutProject(context.Background(), p); err != nil {
		t.Fatalf("checkoutProject() error = %v (empty worktree must be populated before ff)", err)
	}
	data, err := os.ReadFile(filepath.Join(worktree, "file.txt"))
	if err != nil {
		t.Fatalf("工作树未补齐到远端最新提交: %v", err)
	}
	if string(data) != "second\n" {
		t.Errorf("file.txt = %q, want %q", data, "second\n")
	}
}

// TestCheckoutProjectKeepsEmptyWorktreeLocalContent 验证补齐逻辑只填充空工作树：
// 空 index 但 tracked 文件仍在（用户本地内容）时不做 reset，ff 失败而非覆盖。
func TestCheckoutProjectKeepsEmptyWorktreeLocalContent(t *testing.T) {
	root, bare, work, runner := setupRemoteWithCommit(t)
	worktree := filepath.Join(root, "proj")
	if _, err := runner.RunInDir(root, "clone", "--no-checkout", bare, worktree); err != nil {
		t.Fatalf("clone --no-checkout: %v", err)
	}
	const local = "local-content\n"
	if err := os.WriteFile(filepath.Join(worktree, "file.txt"), []byte(local), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "file.txt"), []byte("second\n"), 0644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, runner, work, "commit", "-am", "c2")
	mustGit(t, runner, work, "push", "origin", "HEAD:refs/heads/main")
	mustGit(t, runner, worktree, "fetch", "origin")

	e := NewEngine(&Options{Jobs: 1, Quiet: true},
		&manifest.Manifest{Topdir: root}, logger.NewDefaultLogger())
	p := &project.Project{
		Name:       "proj",
		RemoteName: "origin",
		RemoteURL:  bare,
		Worktree:   worktree,
		Revision:   "main",
	}
	_ = e.checkoutProject(context.Background(), p)
	data, err := os.ReadFile(filepath.Join(worktree, "file.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != local {
		t.Errorf("本地内容被覆盖: got %q, want %q", data, local)
	}
}

// TestPreSyncLocalOnlyReusesSuperprojectManifest 验证 --local-only 下不执行任何
// git fetch，而是复用 .repo/superproject-manifest.xml 中的 revision。
func TestPreSyncLocalOnlyReusesSuperprojectManifest(t *testing.T) {
	root := t.TempDir()
	repoDir := filepath.Join(root, ".repo")
	if err := os.MkdirAll(repoDir, 0755); err != nil {
		t.Fatal(err)
	}
	const rev = "1111111111111111111111111111111111111111"
	content := `<?xml version="1.0" encoding="UTF-8"?>
<manifest>
  <remote name="superproject" fetch="https://example.com" />
  <default remote="superproject" revision="main" />
  <project name="proj" path="proj" revision="` + rev + `" />
</manifest>
`
	if err := os.WriteFile(filepath.Join(repoDir, "superproject-manifest.xml"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	m := &manifest.Manifest{
		Topdir:       root,
		RepoDir:      repoDir,
		Remotes:      []manifest.Remote{{Name: "origin", Fetch: "https://example.com"}},
		Superproject: &manifest.Superproject{Name: "super", Remote: "origin", Revision: "main"},
	}
	e := NewEngine(&Options{Jobs: 1, Quiet: true, UseSuperproject: true, LocalOnly: true},
		m, logger.NewDefaultLogger())
	e.gitRunner = &funcRunner{run: func(_ string, _ []string) ([]byte, error) {
		t.Fatal("--local-only preSync 不得执行任何 git 命令（含 superproject fetch）")
		return nil, nil
	}}

	p := &project.Project{Name: "proj", Path: "proj"}
	e.SetProjects([]*project.Project{p})
	if err := e.preSync(context.Background()); err != nil {
		t.Fatalf("preSync() error = %v", err)
	}
	if p.RevisionID != rev {
		t.Errorf("RevisionID = %q, want %q from existing superproject manifest", p.RevisionID, rev)
	}
}

// TestPreSyncLocalOnlyWithoutSuperprojectManifestSkips 验证文件缺失时告警跳过，
// 不发起网络 fetch，也不中断同步。
func TestPreSyncLocalOnlyWithoutSuperprojectManifestSkips(t *testing.T) {
	root := t.TempDir()
	m := &manifest.Manifest{
		Topdir:       root,
		RepoDir:      filepath.Join(root, ".repo"),
		Remotes:      []manifest.Remote{{Name: "origin", Fetch: "https://example.com"}},
		Superproject: &manifest.Superproject{Name: "super", Remote: "origin", Revision: "main"},
	}
	e := NewEngine(&Options{Jobs: 1, Quiet: true, UseSuperproject: true, LocalOnly: true},
		m, logger.NewDefaultLogger())
	e.gitRunner = &funcRunner{run: func(_ string, _ []string) ([]byte, error) {
		t.Fatal("--local-only preSync 不得执行任何 git 命令")
		return nil, nil
	}}
	if err := e.preSync(context.Background()); err != nil {
		t.Fatalf("preSync() error = %v, want nil", err)
	}
}

// TestSyncLocalOnlySkipsHyperSyncHTTP 验证 --local-only 与 --hyper-sync 同用时
// 不向清单服务器发 HTTP 请求，退化为全量本地同步。
func TestSyncLocalOnlySkipsHyperSyncHTTP(t *testing.T) {
	var requests int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&requests, 1)
		http.Error(w, "unexpected request", http.StatusInternalServerError)
	}))
	defer srv.Close()

	m := &manifest.Manifest{ManifestServer: &manifest.ManifestServer{URL: srv.URL}}
	e := NewEngine(&Options{Jobs: 1, Quiet: true, HyperSync: true, LocalOnly: true},
		m, logger.NewDefaultLogger())
	// 缺失项目 + local-only 必然失败：说明已退化为全量本地同步（而非 HyperSync
	// 早退为“无需更新”而静默成功）。
	p := &project.Project{
		Name:       "proj",
		RemoteName: "origin",
		RemoteURL:  "https://example.com/proj",
		Worktree:   filepath.Join(t.TempDir(), "missing"),
		Revision:   "main",
	}
	e.SetProjects([]*project.Project{p})

	err := e.Sync()
	if err == nil {
		t.Fatal("local-only 全量同步缺失项目应返回错误（HyperSync 不得早退）")
	}
	if !strings.Contains(err.Error(), "missing network sync") {
		t.Errorf("error = %v, want contains %q", err, "missing network sync")
	}
	if got := atomic.LoadInt32(&requests); got != 0 {
		t.Errorf("--local-only 不得请求清单服务器，实际 %d 次", got)
	}
}

// TestApplyDefaultRemoteFillsMissingRemote 验证 --default-remote 在项目 remote
// 名缺失时生效，并从清单补齐 fetch 基址与完整 RemoteURL。
func TestApplyDefaultRemoteFillsMissingRemote(t *testing.T) {
	tests := []struct {
		name          string
		defaultName   string
		fetch         string
		p             *project.Project
		wantRemote    string
		wantURL       string
		wantFetchBase string
	}{
		{
			name:          "absolute base appends project name",
			defaultName:   "cix",
			fetch:         "https://example.com/base",
			p:             &project.Project{Name: "proj"},
			wantRemote:    "cix",
			wantURL:       "https://example.com/base/proj",
			wantFetchBase: "https://example.com/base",
		},
		{
			name:          "relative base kept for later urljoin",
			defaultName:   "platform",
			fetch:         "platform",
			p:             &project.Project{Name: "proj"},
			wantRemote:    "platform",
			wantURL:       "platform",
			wantFetchBase: "platform",
		},
		{
			name:        "existing remote name preserved",
			defaultName: "cix",
			fetch:       "https://example.com/base",
			p:           &project.Project{Name: "proj", RemoteName: "keep", RemoteURL: "https://keep.example.com/proj"},
			wantRemote:  "keep",
			wantURL:     "https://keep.example.com/proj",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := &manifest.Manifest{Remotes: []manifest.Remote{{Name: tt.defaultName, Fetch: tt.fetch}}}
			e := NewEngine(&Options{Jobs: 1, Quiet: true, DefaultRemote: tt.defaultName},
				m, logger.NewDefaultLogger())
			e.applyDefaultRemote(tt.p)
			if tt.p.RemoteName != tt.wantRemote {
				t.Errorf("RemoteName = %q, want %q", tt.p.RemoteName, tt.wantRemote)
			}
			if tt.p.RemoteURL != tt.wantURL {
				t.Errorf("RemoteURL = %q, want %q", tt.p.RemoteURL, tt.wantURL)
			}
			if tt.wantFetchBase != "" && tt.p.FetchBase != tt.wantFetchBase {
				t.Errorf("FetchBase = %q, want %q", tt.p.FetchBase, tt.wantFetchBase)
			}
		})
	}
}
