package project

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/leopardxu/repo-go/internal/git"
)

// recordingRunner 记录 git 命令参数，避免测试依赖真实 git/网络。
type recordingRunner struct {
	mu    sync.Mutex
	calls [][]string
}

func (r *recordingRunner) record(args []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cp := make([]string, len(args))
	copy(cp, args)
	r.calls = append(r.calls, cp)
}

func (r *recordingRunner) lastCall() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.calls) == 0 {
		return nil
	}
	return r.calls[len(r.calls)-1]
}

// snapshot 返回目前记录到的全部命令调用的副本。
func (r *recordingRunner) snapshot() [][]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([][]string, len(r.calls))
	for i, call := range r.calls {
		cp := make([]string, len(call))
		copy(cp, call)
		out[i] = cp
	}
	return out
}

func (r *recordingRunner) Run(args ...string) ([]byte, error) {
	r.record(args)
	return nil, nil
}

func (r *recordingRunner) RunInDir(_ string, args ...string) ([]byte, error) {
	r.record(args)
	return nil, nil
}

func (r *recordingRunner) RunWithTimeout(_ time.Duration, args ...string) ([]byte, error) {
	r.record(args)
	return nil, nil
}

func (r *recordingRunner) RunInDirWithTimeout(_ string, _ time.Duration, args ...string) ([]byte, error) {
	r.record(args)
	return nil, nil
}

func (r *recordingRunner) SetVerbose(bool)             {}
func (r *recordingRunner) SetQuiet(bool)               {}
func (r *recordingRunner) SetMaxRetries(int)           {}
func (r *recordingRunner) SetRetryDelay(time.Duration) {}
func (r *recordingRunner) SetConcurrency(int)          {}

// TestSyncNetworkHalfFetchPassesRemoteName 验证 fetch 把 remote name 原样交给
// git，不能被当作 URL 解析（旧实现对不带 "://" 的 remote name 会拼成 https://origin）。
func TestSyncNetworkHalfFetchPassesRemoteName(t *testing.T) {
	worktree := t.TempDir()
	if err := os.MkdirAll(filepath.Join(worktree, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}
	runner := &recordingRunner{}
	p := NewProject("proj", worktree, "origin", "https://example.com/base/proj", "main", nil, runner)

	if !p.SyncNetworkHalf(SyncNetworkOptions{}) {
		t.Fatal("SyncNetworkHalf should succeed")
	}
	args := runner.lastCall()
	if len(args) == 0 || args[0] != "fetch" {
		t.Fatalf("expected fetch command, got %v", args)
	}
	if args[len(args)-1] != "origin" {
		t.Errorf("fetch target = %q, want remote name origin", args[len(args)-1])
	}
	for _, a := range args {
		if strings.Contains(a, "://") {
			t.Errorf("remote name must not be resolved as URL, got arg %q", a)
		}
	}
}

// TestFetchTargetUsesFetchBase 验证通过清单创建的项目的 clone/fetch 目标
// 由原始 FetchBase 解析得到：绝对基址无条件拼项目名，相对基址按 urljoin
// 解析后再拼名；无 FetchBase 的直接构造项目沿用 RemoteURL。
func TestFetchTargetUsesFetchBase(t *testing.T) {
	tests := []struct {
		name        string
		fetchBase   string
		manifestURL string
		remoteURL   string
		want        string
	}{
		{
			name:      "绝对基址无条件拼项目名",
			fetchBase: "https://example.com/base",
			want:      "https://example.com/base/proj",
		},
		{
			name:        "相对无点基址按 urljoin 解析",
			fetchBase:   "platform",
			manifestURL: "ssh://host/manifests/main",
			want:        "ssh://host/manifests/platform/proj",
		},
		{
			name:        "相对父目录基址按 urljoin 解析",
			fetchBase:   "../platform",
			manifestURL: "ssh://host/manifests/main",
			want:        "ssh://host/platform/proj",
		},
		{
			name:        "无 FetchBase 保留完整 RemoteURL",
			manifestURL: "ssh://host/manifests/main",
			remoteURL:   "https://example.com/proj",
			want:        "https://example.com/proj",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := NewProject("proj", t.TempDir(), "", tt.remoteURL, "main", nil, nil)
			p.FetchBase = tt.fetchBase
			p.ManifestURL = tt.manifestURL

			if got := p.cloneURL(); got != tt.want {
				t.Errorf("cloneURL() = %q, want %q", got, tt.want)
			}
			if got := p.fetchTarget(); got != tt.want {
				t.Errorf("fetchTarget() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestSyncNetworkHalfResolvesRelativeFetch 验证相对 fetch 基于清单 URL 解析
// 并拼接项目名。
func TestSyncNetworkHalfResolvesRelativeFetch(t *testing.T) {
	worktree := t.TempDir()
	if err := os.MkdirAll(filepath.Join(worktree, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}
	runner := &recordingRunner{}
	p := NewProject("proj", worktree, "", "../platform", "main", nil, runner)
	p.ManifestURL = "ssh://host/linux_repo/cix-manifest"

	if !p.SyncNetworkHalf(SyncNetworkOptions{}) {
		t.Fatal("SyncNetworkHalf should succeed")
	}
	args := runner.lastCall()
	if len(args) == 0 || args[0] != "fetch" {
		t.Fatalf("expected fetch command, got %v", args)
	}
	const want = "ssh://host/platform/proj"
	if got := args[len(args)-1]; got != want {
		t.Errorf("relative fetch target = %q, want %q", got, want)
	}
}

// TestSyncNetworkHalfRetryFetchesNotDepth 验证 RetryFetches 不再被误用为深度。
func TestSyncNetworkHalfRetryFetchesNotDepth(t *testing.T) {
	worktree := t.TempDir()
	if err := os.MkdirAll(filepath.Join(worktree, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}
	runner := &recordingRunner{}
	p := NewProject("proj", worktree, "origin", "https://example.com/proj", "main", nil, runner)

	if !p.SyncNetworkHalf(SyncNetworkOptions{RetryFetches: 5, Depth: 2}) {
		t.Fatal("SyncNetworkHalf should succeed")
	}
	args := runner.lastCall()
	if !containsArg(args, "--depth") {
		t.Fatalf("expected --depth from Depth option, got %v", args)
	}
	if !containsArgPair(args, "--depth", "2") {
		t.Errorf("--depth should be 2 from Depth, not RetryFetches: %v", args)
	}
}

// TestSyncNetworkHalfMirrorCloneUsesMirror 验证镜像项目按裸仓库布局判定存在性
// 并执行 git clone --mirror，且克隆后把 origin 改名为清单 remote 名。
func TestSyncNetworkHalfMirrorCloneUsesMirror(t *testing.T) {
	topDir := t.TempDir()
	mirrorDir := filepath.Join(topDir, "platform", "a.git")
	runner := &recordingRunner{}
	p := NewProject("platform/a", "", "cix", "https://example.com/platform/a", "main", nil, runner)
	p.Mirror = true
	p.Worktree = ""
	p.Gitdir = mirrorDir
	p.Objdir = mirrorDir
	p.GitRepo = git.NewRepository(mirrorDir, runner)

	if !p.SyncNetworkHalf(SyncNetworkOptions{Quiet: true}) {
		t.Fatal("SyncNetworkHalf(mirror) should succeed")
	}

	calls := runner.snapshot()
	if len(calls) != 2 {
		t.Fatalf("expected clone + remote rename, got %v", calls)
	}
	cloneCall := calls[0]
	if len(cloneCall) < 2 || cloneCall[0] != "clone" || !containsArg(cloneCall, "--mirror") {
		t.Fatalf("first call must be git clone --mirror, got %v", cloneCall)
	}
	if got := cloneCall[len(cloneCall)-1]; got != mirrorDir {
		t.Errorf("clone destination = %q, want mirror dir %q", got, mirrorDir)
	}
	renameCall := calls[1]
	if len(renameCall) != 4 || renameCall[0] != "remote" || renameCall[1] != "rename" ||
		renameCall[2] != "origin" || renameCall[3] != "cix" {
		t.Errorf("expected remote rename origin cix, got %v", renameCall)
	}
	// 镜像没有工作树，不能出现 checkout
	for _, call := range calls {
		if len(call) > 0 && call[0] == "checkout" {
			t.Errorf("mirror clone must not checkout, got %v", call)
		}
	}
}

// TestSyncNetworkHalfShaRevisionCheckout 验证 SHA revision 克隆后显式检出到
// 该 SHA，不能静默停在默认分支；非 origin 的 remote 名需重命名。
func TestSyncNetworkHalfShaRevisionCheckout(t *testing.T) {
	sha := "0123456789abcdef0123456789abcdef01234567"
	worktree := filepath.Join(t.TempDir(), "proj")
	runner := &recordingRunner{}
	p := NewProject("proj", worktree, "cix", "https://example.com/proj", sha, nil, runner)

	if !p.SyncNetworkHalf(SyncNetworkOptions{Quiet: true}) {
		t.Fatal("SyncNetworkHalf(sha revision) should succeed")
	}

	calls := runner.snapshot()
	if len(calls) != 3 {
		t.Fatalf("expected clone + rename + checkout, got %v", calls)
	}
	if calls[0][0] != "clone" || containsArg(calls[0], "--mirror") {
		t.Errorf("non-mirror clone expected, got %v", calls[0])
	}
	checkout := calls[2]
	if len(checkout) != 3 || checkout[0] != "checkout" || checkout[1] != sha || checkout[2] != "--" {
		t.Errorf("expected checkout %s --, got %v", sha, checkout)
	}
}

// TestProjectExistsMirrorBareLayout 验证镜像 Exists 以 HEAD/objects/refs 判定，
// 而不是工作树下的 .git 子目录。
func TestProjectExistsMirrorBareLayout(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "a.git")
	if err := os.MkdirAll(filepath.Join(dir, "objects"), 0o755); err != nil {
		t.Fatalf("mkdir objects: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "refs"), 0o755); err != nil {
		t.Fatalf("mkdir refs: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "HEAD"), []byte("ref: refs/heads/main\n"), 0o644); err != nil {
		t.Fatalf("write HEAD: %v", err)
	}
	p := &Project{Name: "a", Mirror: true, Gitdir: dir, Objdir: dir}

	exists, err := p.exists()
	if err != nil {
		t.Fatalf("exists() error = %v", err)
	}
	if !exists {
		t.Fatal("bare mirror repository should be detected as existing")
	}

	if err := os.RemoveAll(filepath.Join(dir, "objects")); err != nil {
		t.Fatalf("remove objects: %v", err)
	}
	exists, err = p.exists()
	if err != nil {
		t.Fatalf("exists() error = %v", err)
	}
	if exists {
		t.Error("incomplete bare repository (no objects) must not count as existing")
	}
}

// TestProjectSyncDryRun 验证 DryRun 在 Project.Sync 入口生效：不执行任何 git 命令。
func TestProjectSyncDryRun(t *testing.T) {
	runner := &recordingRunner{}
	worktree := filepath.Join(t.TempDir(), "proj")
	p := NewProject("proj", worktree, "origin", "https://example.com/proj", "main", nil, runner)

	if err := p.Sync(SyncOptions{DryRun: true}); err != nil {
		t.Fatalf("Project.Sync(DryRun) error = %v", err)
	}
	if calls := runner.snapshot(); len(calls) != 0 {
		t.Errorf("DryRun must not run git commands, got %v", calls)
	}
	if _, err := os.Stat(worktree); !os.IsNotExist(err) {
		t.Errorf("DryRun must not create worktree %s, stat err = %v", worktree, err)
	}
}

// outputRunner 按命令输出脚本化结果，用于驱动 SyncLocalHalf 的分支判定。
// errs 可为指定命令返回错误，用于验证 revision 解析的回退顺序。
type outputRunner struct {
	recordingRunner
	outputs map[string][]byte
	errs    map[string]error
}

func (r *outputRunner) RunInDir(_ string, args ...string) ([]byte, error) {
	r.record(args)
	key := strings.Join(args, " ")
	if err, ok := r.errs[key]; ok {
		return nil, err
	}
	return r.outputs[key], nil
}

// TestSyncLocalHalfCheckoutSemantics 验证本地同步：仅当前分支跟踪 manifest
// revision 时 fast-forward，其余情况 detach 到解析后的 revID；--force-sync
// 强制检出 revID（不移动其他分支）。
func TestSyncLocalHalfCheckoutSemantics(t *testing.T) {
	const oldSha = "1111111111111111111111111111111111111111"
	const newSha = "2222222222222222222222222222222222222222"

	tests := []struct {
		name        string
		branch      string
		merge       string
		forceSync   bool
		wantCommand []string
	}{
		{
			name:        "分支跟踪 manifest revision 时 fast-forward",
			branch:      "main",
			merge:       "refs/heads/main\n",
			wantCommand: []string{"merge", "--ff-only", newSha},
		},
		{
			name:        "分支不跟踪 manifest revision 时 detach",
			branch:      "feature",
			merge:       "refs/heads/feature\n",
			wantCommand: []string{"checkout", newSha, "--"},
		},
		{
			name:        "游离 HEAD 时 detach",
			branch:      "HEAD",
			wantCommand: []string{"checkout", newSha, "--"},
		},
		{
			name:        "强制同步检出解析后的 revID",
			branch:      "feature",
			merge:       "refs/heads/feature\n",
			forceSync:   true,
			wantCommand: []string{"checkout", "--force", newSha, "--"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runner := &outputRunner{outputs: map[string][]byte{
				"status --porcelain": []byte(""),
				// 分支 revision 优先解析远程跟踪引用（refs/remotes/origin/main），
				// 其次才是本地分支。
				"rev-parse --verify refs/remotes/origin/main^{commit}": []byte(newSha + "\n"),
				"rev-parse --verify refs/heads/main^{commit}":          []byte(newSha + "\n"),
				"rev-parse --verify main^{commit}":                     []byte(newSha + "\n"),
				"rev-parse --verify HEAD":                              []byte(oldSha + "\n"),
				"rev-parse --abbrev-ref HEAD":                          []byte(tt.branch + "\n"),
				"config --get branch.feature.merge":                    []byte(tt.merge),
				"config --get branch.main.merge":                       []byte(tt.merge),
			}}
			worktree := filepath.Join(t.TempDir(), "proj")
			p := NewProject("proj", worktree, "origin", "https://example.com/proj", "main", nil, runner)

			if !p.SyncLocalHalf(false, tt.forceSync, false) {
				t.Fatal("SyncLocalHalf should succeed")
			}
			got := runner.lastCall()
			if len(got) != len(tt.wantCommand) {
				t.Fatalf("last command = %v, want %v", got, tt.wantCommand)
			}
			for i := range got {
				if got[i] != tt.wantCommand[i] {
					t.Fatalf("last command = %v, want %v", got, tt.wantCommand)
				}
			}
		})
	}
}

// TestResolveRevisionIDOrder 验证分支 revision 的解析顺序：
// 远程跟踪引用优先，本地分支次之，最后直接解析（tag/提交 ID/显式 refs/）。
func TestResolveRevisionIDOrder(t *testing.T) {
	const remoteSha = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const localSha = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	const rawSha = "cccccccccccccccccccccccccccccccccccccccc"

	tests := []struct {
		name      string
		revision  string
		outputs   map[string][]byte
		errs      map[string]error
		want      string
		wantFirst string
		forbidden string
		wantErr   bool
	}{
		{
			name:      "远程跟踪优先于本地分支",
			revision:  "main",
			outputs:   map[string][]byte{"rev-parse --verify refs/remotes/origin/main^{commit}": []byte(remoteSha + "\n"), "rev-parse --verify refs/heads/main^{commit}": []byte(localSha + "\n")},
			want:      remoteSha,
			wantFirst: "rev-parse --verify refs/remotes/origin/main^{commit}",
		},
		{
			name:      "远程缺失回退本地分支",
			revision:  "main",
			errs:      map[string]error{"rev-parse --verify refs/remotes/origin/main^{commit}": errors.New("missing")},
			outputs:   map[string][]byte{"rev-parse --verify refs/heads/main^{commit}": []byte(localSha + "\n")},
			want:      localSha,
			wantFirst: "rev-parse --verify refs/remotes/origin/main^{commit}",
		},
		{
			name:     "裸名不回退同名 tag",
			revision: "v1.0",
			errs: map[string]error{
				"rev-parse --verify refs/remotes/origin/v1.0^{commit}": errors.New("missing"),
				"rev-parse --verify refs/heads/v1.0^{commit}":          errors.New("missing"),
			},
			outputs:   map[string][]byte{"rev-parse --verify v1.0^{commit}": []byte(rawSha + "\n")},
			wantErr:   true,
			wantFirst: "rev-parse --verify refs/remotes/origin/v1.0^{commit}",
			forbidden: "rev-parse --verify v1.0^{commit}",
		},
		{
			name:      "40 位提交 ID 直接解析",
			revision:  rawSha,
			outputs:   map[string][]byte{"rev-parse --verify " + rawSha + "^{commit}": []byte(rawSha + "\n")},
			want:      rawSha,
			wantFirst: "rev-parse --verify " + rawSha + "^{commit}",
		},
		{
			name:      "64 位提交 ID 直接解析",
			revision:  strings.Repeat("d", 64),
			outputs:   map[string][]byte{"rev-parse --verify " + strings.Repeat("d", 64) + "^{commit}": []byte(strings.Repeat("d", 64) + "\n")},
			want:      strings.Repeat("d", 64),
			wantFirst: "rev-parse --verify " + strings.Repeat("d", 64) + "^{commit}",
		},
		{
			name:      "短十六进制按分支解析",
			revision:  "deadbeef",
			outputs:   map[string][]byte{"rev-parse --verify refs/remotes/origin/deadbeef^{commit}": []byte(remoteSha + "\n")},
			want:      remoteSha,
			wantFirst: "rev-parse --verify refs/remotes/origin/deadbeef^{commit}",
		},
		{
			name:      "显式 refs/heads 前缀同样远端优先",
			revision:  "refs/heads/main",
			outputs:   map[string][]byte{"rev-parse --verify refs/remotes/origin/main^{commit}": []byte(remoteSha + "\n"), "rev-parse --verify refs/heads/main^{commit}": []byte(localSha + "\n")},
			want:      remoteSha,
			wantFirst: "rev-parse --verify refs/remotes/origin/main^{commit}",
		},
		{
			name:      "显式 refs/tags 不追加候选",
			revision:  "refs/tags/v1.0",
			outputs:   map[string][]byte{"rev-parse --verify refs/tags/v1.0^{commit}": []byte(rawSha + "\n")},
			want:      rawSha,
			wantFirst: "rev-parse --verify refs/tags/v1.0^{commit}",
		},
		{
			name:     "空 revision 不执行命令",
			revision: "",
			want:     "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runner := &outputRunner{outputs: tt.outputs, errs: tt.errs}
			p := NewProject("proj", t.TempDir(), "origin", "https://example.com/proj", tt.revision, nil, runner)

			got, err := p.resolveRevisionID(tt.revision)
			if (err != nil) != tt.wantErr {
				t.Fatalf("resolveRevisionID(%q) error = %v, wantErr %v", tt.revision, err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("resolveRevisionID(%q) = %q, want %q", tt.revision, got, tt.want)
			}
			if tt.wantFirst != "" {
				calls := runner.snapshot()
				if len(calls) == 0 || strings.Join(calls[0], " ") != tt.wantFirst {
					t.Errorf("first rev-parse = %v, want %s", calls, tt.wantFirst)
				}
			}
			if tt.forbidden != "" {
				for _, call := range runner.snapshot() {
					if strings.Join(call, " ") == tt.forbidden {
						t.Errorf("must not fall back to %s (tag fallback removed)", tt.forbidden)
					}
				}
			}
		})
	}
}

// TestListLocalBranchesFullRefname 验证用完整 refname 输出并剥离 refs/heads/，
// 避免分支与同名 tag 共存时得到 heads/foo 这类有歧义的名字。
func TestListLocalBranchesFullRefname(t *testing.T) {
	runner := &outputRunner{outputs: map[string][]byte{
		"for-each-ref --format=%(refname) refs/heads/": []byte("refs/heads/main\nrefs/heads/feature/x\n"),
	}}
	p := NewProject("proj", t.TempDir(), "origin", "https://example.com/proj", "main", nil, runner)

	branches, err := p.ListLocalBranches()
	if err != nil {
		t.Fatalf("ListLocalBranches error = %v", err)
	}
	want := []string{"main", "feature/x"}
	if len(branches) != len(want) {
		t.Fatalf("branches = %v, want %v", branches, want)
	}
	for i := range want {
		if branches[i] != want[i] {
			t.Errorf("branches[%d] = %q, want %q", i, branches[i], want[i])
		}
	}
}

func containsArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

func containsArgPair(args []string, flag, value string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag && args[i+1] == value {
			return true
		}
	}
	return false
}

// TestCloneBranch 验证 --branch 参数清洗：去 refs 前缀、提交 ID 不传。
func TestCloneBranch(t *testing.T) {
	tests := []struct {
		name     string
		revision string
		want     string
	}{
		{name: "plain branch", revision: "main", want: "main"},
		{name: "refs heads", revision: "refs/heads/main", want: "main"},
		{name: "refs tags", revision: "refs/tags/v1.0", want: "v1.0"},
		{name: "sha1", revision: "0123456789abcdef0123456789abcdef01234567", want: ""},
		{name: "sha256", revision: strings.Repeat("a", 64), want: ""},
		{name: "empty", revision: "", want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := cloneBranch(tt.revision); got != tt.want {
				t.Errorf("cloneBranch(%q) = %q, want %q", tt.revision, got, tt.want)
			}
		})
	}
}

// TestDeleteWorktreeRejectsUnsafePaths 验证删除工作树的安全边界：
// repo 根目录、.repo 自身、越界目录与前缀兄弟目录都必须拒绝。
func TestDeleteWorktreeRejectsUnsafePaths(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".repo"), 0o755); err != nil {
		t.Fatalf("mkdir .repo: %v", err)
	}
	prefixSibling := root + "-sibling"
	outside := filepath.Join(filepath.Dir(root), filepath.Base(root)+"-outside")
	for _, d := range []string{prefixSibling, outside, filepath.Join(root, "proj")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	t.Cleanup(func() {
		os.RemoveAll(prefixSibling)
		os.RemoveAll(outside)
	})

	origDir, _ := os.Getwd()
	defer os.Chdir(origDir)
	if err := os.Chdir(root); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	tests := []struct {
		name     string
		worktree string
		wantErr  bool
	}{
		{name: "repo root itself", worktree: root, wantErr: true},
		{name: ".repo itself", worktree: filepath.Join(root, ".repo"), wantErr: true},
		{name: "outside repo", worktree: outside, wantErr: true},
		{name: "prefix sibling", worktree: prefixSibling, wantErr: true},
		{name: "safe project", worktree: filepath.Join(root, "proj"), wantErr: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := NewProject("p", tt.worktree, "origin", "url", "main", nil, git.NewRunner())
			err := p.DeleteWorktree(true, true)
			if (err != nil) != tt.wantErr {
				t.Fatalf("DeleteWorktree(%q) error = %v, wantErr %v", tt.worktree, err, tt.wantErr)
			}
			if !tt.wantErr {
				if _, statErr := os.Stat(tt.worktree); !os.IsNotExist(statErr) {
					t.Errorf("safe worktree %s should be removed, stat err = %v", tt.worktree, statErr)
				}
			}
		})
	}
}

func TestNewProject(t *testing.T) {
	runner := git.NewRunner()
	p := NewProject("test-proj", "path/to/proj", "origin", "https://example.com/repo.git", "main", []string{"default"}, runner)

	if p.Name != "test-proj" {
		t.Errorf("Name = %q, want test-proj", p.Name)
	}
	// filepath.Clean normalizes separators per OS
	wantPath := filepath.Clean("path/to/proj")
	if p.Path != wantPath {
		t.Errorf("Path = %q, want %q", p.Path, wantPath)
	}
	if p.RemoteName != "origin" {
		t.Errorf("RemoteName = %q, want origin", p.RemoteName)
	}
	if p.RemoteURL != "https://example.com/repo.git" {
		t.Errorf("RemoteURL = %q", p.RemoteURL)
	}
	if p.Revision != "main" {
		t.Errorf("Revision = %q, want main", p.Revision)
	}
	if p.Remote != "origin" {
		t.Errorf("Remote = %q, want origin", p.Remote)
	}
	if p.RevisionID != "main" {
		t.Errorf("RevisionID = %q, want main", p.RevisionID)
	}
}

func TestProjectIsInGroup(t *testing.T) {
	p := NewProject("test", "test", "origin", "url", "main", []string{"linux", "arm"}, git.NewRunner())

	if !p.IsInGroup("linux") {
		t.Error("IsInGroup(linux) should be true")
	}
	if !p.IsInGroup("arm") {
		t.Error("IsInGroup(arm) should be true")
	}
	if p.IsInGroup("windows") {
		t.Error("IsInGroup(windows) should be false")
	}
	if !p.IsInGroup("all") {
		t.Error("IsInGroup(all) should always be true")
	}
	if !p.IsInGroup("") {
		t.Error("IsInGroup(empty) should be true")
	}
}

func TestProjectIsInAnyGroup(t *testing.T) {
	p := NewProject("test", "test", "origin", "url", "main", []string{"linux", "arm"}, git.NewRunner())

	if !p.IsInAnyGroup([]string{"linux"}) {
		t.Error("IsInAnyGroup([linux]) should be true")
	}
	if !p.IsInAnyGroup([]string{"linux", "windows"}) {
		t.Error("IsInAnyGroup([linux, windows]) should be true (matches linux)")
	}
	if p.IsInAnyGroup([]string{"windows"}) {
		t.Error("IsInAnyGroup([windows]) should be false")
	}
	if !p.IsInAnyGroup([]string{}) {
		t.Error("IsInAnyGroup(empty) should be true")
	}
	if !p.IsInAnyGroup([]string{"all"}) {
		t.Error("IsInAnyGroup([all]) should be true")
	}
}

func TestProjectGetSetRemoteURL(t *testing.T) {
	p := NewProject("test", "test", "origin", "url", "main", nil, git.NewRunner())

	p.SetRemoteURL("https://new-url.com/repo.git")
	if p.GetRemoteURL() != "https://new-url.com/repo.git" {
		t.Errorf("GetRemoteURL() = %q, want https://new-url.com/repo.git", p.GetRemoteURL())
	}
}

func TestProjectGetSetRevision(t *testing.T) {
	p := NewProject("test", "test", "origin", "url", "main", nil, git.NewRunner())

	p.SetRevision("dev")
	if p.GetRevision() != "dev" {
		t.Errorf("GetRevision() = %q, want dev", p.GetRevision())
	}
	if p.RevisionID != "dev" {
		t.Errorf("RevisionID = %q, want dev (should sync with Revision)", p.RevisionID)
	}
}

func TestProjectSetNeedGC(t *testing.T) {
	p := NewProject("test", "test", "origin", "url", "main", nil, git.NewRunner())

	p.SetNeedGC(true)
	if !p.NeedGC {
		t.Error("NeedGC should be true after SetNeedGC(true)")
	}

	p.SetNeedGC(false)
	if p.NeedGC {
		t.Error("NeedGC should be false after SetNeedGC(false)")
	}
}

func TestFindTopLevelRepoDir(t *testing.T) {
	// When not in a repo, should return empty string
	result := FindTopLevelRepoDir("/nonexistent/path/that/does/not/exist")
	if result != "" {
		t.Errorf("FindTopLevelRepoDir with nonexistent path should return empty, got %q", result)
	}
}

func TestManagerNewManager(t *testing.T) {
	runner := git.NewRunner()
	mgr := NewManager("https://example.com/manifest.git", "default.xml", ".repo", runner)

	if mgr == nil {
		t.Fatal("NewManager() returned nil")
	}
	if mgr.ManifestURL != "https://example.com/manifest.git" {
		t.Errorf("ManifestURL = %q", mgr.ManifestURL)
	}
	if mgr.ManifestName != "default.xml" {
		t.Errorf("ManifestName = %q", mgr.ManifestName)
	}
	if len(mgr.Projects) != 0 {
		t.Errorf("Projects should be empty, got %d", len(mgr.Projects))
	}
}

func TestManagerAddProject(t *testing.T) {
	mgr := NewManager("url", "default.xml", ".repo", git.NewRunner())
	p := NewProject("test", "test", "origin", "url", "main", nil, git.NewRunner())

	mgr.AddProject(p)

	if len(mgr.Projects) != 1 {
		t.Fatalf("Projects count = %d, want 1", len(mgr.Projects))
	}
	if mgr.Projects[0].Name != "test" {
		t.Errorf("Project[0].Name = %q, want test", mgr.Projects[0].Name)
	}
}

func TestManagerGetProject(t *testing.T) {
	mgr := NewManager("url", "default.xml", ".repo", git.NewRunner())
	p := NewProject("test", "test", "origin", "url", "main", nil, git.NewRunner())
	mgr.AddProject(p)

	got := mgr.GetProject("test")
	if got == nil {
		t.Fatal("GetProject(test) returned nil")
	}
	if got.Name != "test" {
		t.Errorf("GetProject(test).Name = %q, want test", got.Name)
	}

	// Nonexistent
	if mgr.GetProject("nonexistent") != nil {
		t.Error("GetProject(nonexistent) should return nil")
	}
}

func TestManagerGetProjects(t *testing.T) {
	mgr := NewManager("url", "default.xml", ".repo", git.NewRunner())
	p1 := NewProject("a", "a", "origin", "url", "main", nil, git.NewRunner())
	p2 := NewProject("b", "b", "origin", "url", "main", nil, git.NewRunner())
	mgr.AddProject(p1)
	mgr.AddProject(p2)

	projects := mgr.GetProjects()
	if len(projects) != 2 {
		t.Errorf("GetProjects() count = %d, want 2", len(projects))
	}
}

func TestManagerGetProjectsByNames(t *testing.T) {
	mgr := NewManager("url", "default.xml", ".repo", git.NewRunner())
	p1 := NewProject("a", "a", "origin", "url", "main", nil, git.NewRunner())
	p2 := NewProject("b", "b", "origin", "url", "main", nil, git.NewRunner())
	// 同名多 path：两者都必须返回（cherry-pick 等命令需要全部命中）
	p3 := NewProject("a", "a-alias", "origin", "url", "main", nil, git.NewRunner())
	mgr.AddProject(p1)
	mgr.AddProject(p2)
	mgr.AddProject(p3)

	projects, err := mgr.GetProjectsByNames([]string{"a", "b"})
	if err != nil {
		t.Fatalf("GetProjectsByNames error: %v", err)
	}
	if len(projects) != 3 {
		t.Fatalf("GetProjectsByNames count = %d, want 3 (same-name paths all returned)", len(projects))
	}
	paths := make(map[string]bool, len(projects))
	for _, p := range projects {
		paths[p.Path] = true
	}
	if !paths["a"] || !paths["a-alias"] {
		t.Errorf("GetProjectsByNames paths = %v, want both a and a-alias", paths)
	}

	// Nonexistent name should return error
	_, err = mgr.GetProjectsByNames([]string{"nonexistent"})
	if err == nil {
		t.Error("GetProjectsByNames with nonexistent name should return error")
	}
}

func TestManagerFilterProjects(t *testing.T) {
	mgr := NewManager("url", "default.xml", ".repo", git.NewRunner())
	p1 := NewProject("a", "a", "origin", "url", "main", []string{"linux"}, git.NewRunner())
	p2 := NewProject("b", "b", "origin", "url", "main", []string{"arm"}, git.NewRunner())
	mgr.AddProject(p1)
	mgr.AddProject(p2)

	filtered := mgr.FilterProjects(func(p *Project) bool {
		return p.IsInGroup("linux")
	})

	if len(filtered) != 1 {
		t.Errorf("FilterProjects count = %d, want 1", len(filtered))
	}
	if filtered[0].Name != "a" {
		t.Errorf("Filtered[0].Name = %q, want a", filtered[0].Name)
	}
}

func TestManagerGetProjectsInGroup(t *testing.T) {
	mgr := NewManager("url", "default.xml", ".repo", git.NewRunner())
	p1 := NewProject("a", "a", "origin", "url", "main", []string{"linux"}, git.NewRunner())
	p2 := NewProject("b", "b", "origin", "url", "main", []string{"arm"}, git.NewRunner())
	p3 := NewProject("c", "c", "origin", "url", "main", []string{"linux", "arm"}, git.NewRunner())
	mgr.AddProject(p1)
	mgr.AddProject(p2)
	mgr.AddProject(p3)

	linuxProjects := mgr.GetProjectsInGroup("linux")
	if len(linuxProjects) != 2 {
		t.Errorf("GetProjectsInGroup(linux) count = %d, want 2", len(linuxProjects))
	}
}

// TestProjectGetCurrentBranchShortName 验证 GetCurrentBranch 把
// Repository.CurrentBranch 返回的完整 symbolic-ref（refs/heads/X）归一为短名，
// 保证与 ListLocalBranches/DeleteBranch 的分支名比较一致。
func TestProjectGetCurrentBranchShortName(t *testing.T) {
	runner := &outputRunner{outputs: map[string][]byte{
		"symbolic-ref HEAD": []byte("refs/heads/main\n"),
	}}
	p := NewProject("proj", t.TempDir(), "origin", "https://example.com/proj", "main", nil, runner)

	got, err := p.GetCurrentBranch()
	if err != nil {
		t.Fatalf("GetCurrentBranch() error = %v", err)
	}
	if got != "main" {
		t.Errorf("GetCurrentBranch() = %q, want main", got)
	}
}
