package repo_sync

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/leopardxu/repo-go/internal/git"
	"github.com/leopardxu/repo-go/internal/logger"
	"github.com/leopardxu/repo-go/internal/manifest"
	"github.com/leopardxu/repo-go/internal/project"
)

// TestFetchRetries 验证 fetch/clone 重试次数取自 Options.RetryFetches。
// 上游 repo 的 --retry-fetches 默认值为 0（不额外重试），0 是合法配置，
// 不能被替换成哨兵默认值；实际执行时 effectiveFetchRetries 会应用
// 上游 _Fetch 的 max(retry_fetches, 2) 最小尝试预算。
func TestFetchRetries(t *testing.T) {
	tests := []struct {
		name string
		opts *Options
		want int
	}{
		{"zero means no retries", &Options{RetryFetches: 0}, 0},
		{"negative clamped to zero", &Options{RetryFetches: -1}, 0},
		{"explicit value", &Options{RetryFetches: 5}, 5},
		{"explicit one", &Options{RetryFetches: 1}, 1},
		{"explicit ten", &Options{RetryFetches: 10}, 10},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := &Engine{options: tt.opts}
			got := e.fetchRetries()
			if got != tt.want {
				t.Errorf("fetchRetries() = %d, want %d (RetryFetches=%d)", got, tt.want, tt.opts.RetryFetches)
			}
		})
	}
}

// TestEffectiveFetchRetries 验证实际重试预算对齐上游 _Fetch：
// retry_fetches = max(--retry-fetches, 2) 次尝试，即至少重试 1 次。
func TestEffectiveFetchRetries(t *testing.T) {
	tests := []struct {
		name string
		opts *Options
		want int
	}{
		{"zero gets minimum retry", &Options{RetryFetches: 0}, 1},
		{"negative gets minimum retry", &Options{RetryFetches: -1}, 1},
		{"one stays one", &Options{RetryFetches: 1}, 1},
		{"two stays two", &Options{RetryFetches: 2}, 2},
		{"explicit value preserved", &Options{RetryFetches: 5}, 5},
		{"nil options get minimum", nil, 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := &Engine{options: tt.opts}
			if got := e.effectiveFetchRetries(); got != tt.want {
				t.Errorf("effectiveFetchRetries() = %d, want %d", got, tt.want)
			}
		})
	}
}

// TestIsBareGitRepo 验证裸仓库判定：HEAD + objects + refs 齐全才认定为有效裸仓库。
// 这是 mirror 模式下"已存在镜像不再被误判为缺失/残留"的关键判定。
func TestIsBareGitRepo(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T, dir string)
		want  bool
	}{
		{
			name: "complete bare repo (mirror layout)",
			setup: func(t *testing.T, dir string) {
				writeMirrorLayout(t, dir, true)
			},
			want: true,
		},
		{
			name: "missing HEAD is incomplete",
			setup: func(t *testing.T, dir string) {
				writeMirrorLayout(t, dir, false) // 布局本身跳过 HEAD 文件
			},
			want: false,
		},
		{
			name: "missing objects is incomplete",
			setup: func(t *testing.T, dir string) {
				writeMirrorLayout(t, dir, true)
				if err := os.RemoveAll(filepath.Join(dir, "objects")); err != nil {
					t.Fatalf("remove objects failed: %v", err)
				}
			},
			want: false,
		},
		{
			name: "missing refs is incomplete",
			setup: func(t *testing.T, dir string) {
				writeMirrorLayout(t, dir, true)
				if err := os.RemoveAll(filepath.Join(dir, "refs")); err != nil {
					t.Fatalf("remove refs failed: %v", err)
				}
			},
			want: false,
		},
		{
			name: "empty directory is not a repo",
			setup: func(t *testing.T, dir string) {
				if err := os.MkdirAll(dir, 0755); err != nil {
					t.Fatalf("mkdir failed: %v", err)
				}
			},
			want: false,
		},
		{
			name:  "nonexistent directory is not a repo",
			setup: func(_ *testing.T, _ string) {},
			want:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), tt.name)
			tt.setup(t, dir)
			if got := isBareGitRepo(dir); got != tt.want {
				t.Errorf("isBareGitRepo(%q) = %v, want %v", dir, got, tt.want)
			}
		})
	}
}

// writeMirrorLayout 在 dir 下写入镜像克隆产物的标准裸仓库布局。
// withHead=false 时跳过 HEAD 文件，用于构造不完整克隆场景。
func writeMirrorLayout(t *testing.T, dir string, withHead bool) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "objects"), 0755); err != nil {
		t.Fatalf("mkdir objects failed: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "refs"), 0755); err != nil {
		t.Fatalf("mkdir refs failed: %v", err)
	}
	if withHead {
		if err := os.WriteFile(filepath.Join(dir, "HEAD"), []byte("ref: refs/heads/main\n"), 0644); err != nil {
			t.Fatalf("write HEAD failed: %v", err)
		}
	}
}

// TestProjectExistsMirrorLayout 验证三种目录形态的同步路径判定：
// 裸镜像仓库与标准工作树都应判为"存在"（走 fetch 更新），
// 只有普通目录判为"不存在"（走 clone）。
// 回归背景：mirror 模式下裸仓库无 .git 子目录，曾被误判为不存在，
// 导致"已存在目录"克隆失败后整镜像被删除重下。
func TestProjectExistsMirrorLayout(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T, dir string)
		want  bool
	}{
		{
			name: "bare mirror repo exists",
			setup: func(t *testing.T, dir string) {
				writeMirrorLayout(t, dir, true)
			},
			want: true,
		},
		{
			name: "standard worktree with .git exists",
			setup: func(t *testing.T, dir string) {
				if err := os.MkdirAll(filepath.Join(dir, ".git"), 0755); err != nil {
					t.Fatalf("mkdir .git failed: %v", err)
				}
			},
			want: true,
		},
		{
			name: "incomplete clone dir does not exist",
			setup: func(t *testing.T, dir string) {
				writeMirrorLayout(t, dir, false) // 有 objects/refs 但无 HEAD
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), tt.name)
			tt.setup(t, dir)
			e := &Engine{logger: logger.NewDefaultLogger()}
			p := &project.Project{Name: tt.name, Worktree: dir}
			got, err := e.projectExists(p)
			if err != nil {
				t.Fatalf("projectExists() error = %v", err)
			}
			if got != tt.want {
				t.Errorf("projectExists(%q) = %v, want %v", dir, got, tt.want)
			}
		})
	}
}

// TestResolveRevisionID 验证 manifest revision 到提交 SHA 的解析顺序
// (对齐上游 GetRevisionId:远端跟踪引用优先,本地分支回退,tag 不隐式回退)。
// 回归背景:revision 分支名曾被直接传给 git checkout,在 toolchain/gcc
// 这类工作树内存在同名文件的项目上触发引用/路径歧义。
func TestResolveRevisionID(t *testing.T) {
	const remoteSha = "1111111111111111111111111111111111111111"
	const localSha = "2222222222222222222222222222222222222222"

	tests := []struct {
		name     string
		revision string
		remote   string
		wantSha  string
		wantErr  string
	}{
		{
			name:     "remote tracking ref wins",
			revision: "cix_master",
			remote:   "cix",
			wantSha:  remoteSha,
		},
		{
			name:     "remote name empty falls back to local branch",
			revision: "cix_master",
			remote:   "",
			wantSha:  localSha,
		},
		{
			name:     "refs heads prefix strips",
			revision: "refs/heads/cix_master",
			remote:   "cix",
			wantSha:  remoteSha,
		},
		{
			name:     "hex branch name resolves via remote tracking ref",
			revision: "deadbeef",
			remote:   "cix",
			wantSha:  remoteSha,
		},
		{
			name:     "sha revision resolves directly",
			revision: "6f692f73cb2043b4a0b0446539cd8c15b3dd9220",
			remote:   "cix",
			wantSha:  remoteSha,
		},
		{
			name:     "tags ref resolves as-is",
			revision: "refs/tags/v1.6.0",
			remote:   "cix",
			wantSha:  remoteSha,
		},
		{
			name:     "missing revision reports upstream error text",
			revision: "nope_missing",
			remote:   "cix",
			wantErr:  "revision nope_missing in proj not found",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := &Engine{
				logger: logger.NewDefaultLogger(),
				gitRunner: &funcRunner{run: func(_ string, args []string) ([]byte, error) {
					// resolveRevisionID 的探测参数末尾为 <ref>^0
					if len(args) > 0 && args[0] == "rev-parse" {
						ref := strings.TrimSuffix(args[len(args)-1], "^0")
						switch ref {
						case "refs/remotes/cix/cix_master", "refs/remotes/cix/deadbeef":
							return []byte(remoteSha + "\n"), nil
						case "refs/heads/cix_master":
							return []byte(localSha + "\n"), nil
						case "refs/tags/v1.6.0", "6f692f73cb2043b4a0b0446539cd8c15b3dd9220":
							return []byte(remoteSha + "\n"), nil
						}
						return nil, errors.New("fatal: ambiguous argument")
					}
					return nil, nil
				}},
			}
			p := &project.Project{
				Name:       "proj",
				RemoteName: tt.remote,
				Worktree:   t.TempDir(),
				Revision:   tt.revision,
			}

			got, err := e.resolveRevisionID(p)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("resolveRevisionID() = %q, want error %q", got, tt.wantErr)
				}
				if err.Error() != tt.wantErr {
					t.Errorf("resolveRevisionID() error = %q, want %q", err.Error(), tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveRevisionID() unexpected error: %v", err)
			}
			if got != tt.wantSha {
				t.Errorf("resolveRevisionID() = %q, want %q", got, tt.wantSha)
			}
		})
	}
}

// TestCheckoutProjectDetachedToSha 验证 checkout 输入是解析后的提交 SHA
// 且以 --detach 方式检出(游离 HEAD 场景),绝不把分支名交给 git checkout。
func TestCheckoutProjectDetachedToSha(t *testing.T) {
	const revid = "7353ca0399ae65ad89051dc3d4db2a70284d6881"

	var gotArgs []string
	e := NewEngine(&Options{Jobs: 1, Quiet: true}, nil, logger.NewDefaultLogger())
	e.gitRunner = &funcRunner{run: func(_ string, args []string) ([]byte, error) {
		if len(args) > 0 && args[0] == "rev-parse" {
			// HEAD 指向另一提交(未就位);revision 探测返回 revid
			if args[len(args)-1] == "HEAD" {
				return []byte("2222222222222222222222222222222222222222\n"), nil
			}
			return []byte(revid + "\n"), nil
		}
		if len(args) > 0 && args[0] == "symbolic-ref" {
			return nil, errors.New("fatal: not a symbolic ref")
		}
		if len(args) > 2 && args[2] == "checkout" {
			gotArgs = args
			return nil, nil
		}
		return nil, nil
	}}

	p := &project.Project{
		Name:       "toolchain/gcc",
		RemoteName: "cix",
		Worktree:   filepath.Join(t.TempDir(), "gcc"),
		Revision:   "arm-gnu-toolchain-12.3.rel1-x86_64-aarch64-none-linux-gnu",
	}

	if err := e.checkoutProject(context.Background(), p); err != nil {
		t.Fatalf("checkoutProject() error = %v", err)
	}
	want := []string{
		"-C", p.Worktree, "checkout", "--detach", revid, "--",
	}
	if !reflect.DeepEqual(gotArgs, want) {
		t.Errorf("checkout args = %v, want %v", gotArgs, want)
	}
}

// TestCheckoutProjectAlreadyAtRevision 验证 HEAD 已指向 revid 时不执行
// checkout 命令(对齐上游 "No changes" 分支)；但仍会探测 index/HEAD tree，
// 用于修复 network-only 遗留的空工作树。
func TestCheckoutProjectAlreadyAtRevision(t *testing.T) {
	const revid = "7353ca0399ae65ad89051dc3d4db2a70284d6881"

	var gitCalls int
	var checkoutCalled bool
	e := &Engine{
		logger:  logger.NewDefaultLogger(),
		options: &Options{Jobs: 1},
		gitRunner: &funcRunner{run: func(_ string, args []string) ([]byte, error) {
			gitCalls++
			if len(args) > 0 && args[0] == "checkout" {
				checkoutCalled = true
			}
			if len(args) > 0 && args[0] == "rev-parse" {
				return []byte(revid + "\n"), nil
			}
			return nil, nil
		}},
	}
	p := &project.Project{
		Name:       "proj",
		RemoteName: "cix",
		Worktree:   filepath.Join(t.TempDir(), "w"),
		Revision:   "cix_master",
	}

	if err := e.checkoutProject(context.Background(), p); err != nil {
		t.Fatalf("checkoutProject() error = %v", err)
	}
	if checkoutCalled {
		t.Errorf("HEAD already at revision must not run checkout")
	}
	// resolveRevisionID + gitHeadSha + gitHeadBranch + ls-files + ls-tree
	if gitCalls > 5 {
		t.Errorf("unexpected git calls beyond revision/head/worktree probes: %d", gitCalls)
	}
}

// TestCheckoutProjectFastForward 验证当前分支就是 revision 分支时走
// merge --ff-only 快进而非 checkout(对齐上游 _FastForward)。
func TestCheckoutProjectFastForward(t *testing.T) {
	const revid = "1111111111111111111111111111111111111111"

	var gotArgs []string
	e := NewEngine(&Options{Jobs: 1, Quiet: true}, nil, logger.NewDefaultLogger())
	e.gitRunner = &funcRunner{run: func(_ string, args []string) ([]byte, error) {
		if len(args) > 0 && args[0] == "rev-parse" {
			// HEAD 指向另一提交(落后);revision 探测返回 revid
			if args[len(args)-1] == "HEAD" {
				return []byte("2222222222222222222222222222222222222222\n"), nil
			}
			return []byte(revid + "\n"), nil
		}
		if len(args) > 0 && args[0] == "symbolic-ref" {
			return []byte("refs/heads/cix_master\n"), nil
		}
		if len(args) > 2 && args[2] == "merge" {
			gotArgs = args
			return nil, nil
		}
		return nil, nil
	}}

	p := &project.Project{
		Name:       "proj",
		RemoteName: "cix",
		Worktree:   filepath.Join(t.TempDir(), "w"),
		Revision:   "cix_master",
	}

	if err := e.checkoutProject(context.Background(), p); err != nil {
		t.Fatalf("checkoutProject() error = %v", err)
	}
	want := []string{"-C", p.Worktree, "merge", "--no-stat", "--ff-only", revid}
	if !reflect.DeepEqual(gotArgs, want) {
		t.Errorf("merge args = %v, want %v", gotArgs, want)
	}
}

// TestRevisionFetchRefspec 验证 fetch 阶段追加的精确 revision refspec
// (对齐上游 Sync_NetworkHalf):分支走 refs/heads 映射,refs/ 前缀原样,
// 提交 ID 为空(依赖通配 refspec)。
func TestRevisionFetchRefspec(t *testing.T) {
	tests := []struct {
		name     string
		remote   string
		revision string
		wantSpec string
	}{
		{
			name:     "branch maps to remote tracking ref",
			remote:   "cix",
			revision: "cix_master",
			wantSpec: "+refs/heads/cix_master:refs/remotes/cix/cix_master",
		},
		{
			name:     "refs heads prefix strips",
			remote:   "cix",
			revision: "refs/heads/cix_master",
			wantSpec: "+refs/heads/cix_master:refs/remotes/cix/cix_master",
		},
		{
			name:     "tag ref maps as-is",
			remote:   "cix",
			revision: "refs/tags/v1.6.0",
			wantSpec: "+refs/tags/v1.6.0:refs/tags/v1.6.0",
		},
		{
			name:     "hex branch maps to remote tracking ref",
			remote:   "cix",
			revision: "deadbeef",
			wantSpec: "+refs/heads/deadbeef:refs/remotes/cix/deadbeef",
		},
		{
			name:     "commit id maps to empty",
			remote:   "cix",
			revision: "6f692f73cb2043b4a0b0446539cd8c15b3dd9220",
			wantSpec: "",
		},
		{
			name:     "empty remote maps to empty",
			remote:   "",
			revision: "cix_master",
			wantSpec: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &project.Project{
				Name:       "proj",
				RemoteName: tt.remote,
				Revision:   tt.revision,
			}
			if got := revisionFetchRefspec(p); got != tt.wantSpec {
				t.Errorf("revisionFetchRefspec() = %q, want %q", got, tt.wantSpec)
			}
		})
	}
}

// TestLooksLikeCommitID 验证提交 ID 形态判定：只认 40/64 位完整小写十六进制，
// 短 SHA / 大写 / 7-40 位 hex 分支名都按分支（不可变 ID 以外）处理。
func TestLooksLikeCommitID(t *testing.T) {
	tests := []struct {
		name string
		rev  string
		want bool
	}{
		{"40 hex", "6f692f73cb2043b4a0b0446539cd8c15b3dd9220", true},
		{"64 hex", "6f692f73cb2043b4a0b0446539cd8c15b3dd9220" + "6f692f73cb2043b4a0b04465", true},
		{"short hex branch is not immutable id", "7353ca0", false},
		{"7-40 hex branch is not immutable id", "deadbeef", false},
		{"uppercase hex is not accepted", "6F692F73CB2043B4A0B0446539CD8C15B3DD9220", false},
		{"too short", "7353ca", false},
		{"branch name", "cix_master", false},
		{"mixed alnum", "abc123xyz", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := looksLikeCommitID(tt.rev); got != tt.want {
				t.Errorf("looksLikeCommitID(%q) = %v, want %v", tt.rev, got, tt.want)
			}
		})
	}
}

// TestFetchProjectExplicitRefspec 验证 fetch 命令显式携带通配 refspec 与
// 精确 revision refspec(对齐上游 Sync_NetworkHalf):命令行给出 refspec 后
// remote.<name>.fetch 配置不再参与,通配必须显式补上;精确 refspec 使远端
// 缺少 manifest 分支时 fetch 阶段即报错。--current-branch 时只取 revision 分支。
func TestFetchProjectExplicitRefspec(t *testing.T) {
	tests := []struct {
		name          string
		currentBranch bool
		wantArgs      []string
	}{
		{
			name:          "default fetch keeps wildcard and adds revision refspec",
			currentBranch: false,
			wantArgs: []string{
				"+refs/heads/*:refs/remotes/cix/*",
				"+refs/heads/cix_master:refs/remotes/cix/cix_master",
			},
		},
		{
			name:          "current branch only drops wildcard",
			currentBranch: true,
			wantArgs: []string{
				"+refs/heads/cix_master:refs/remotes/cix/cix_master",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var fetchArgs []string
			e := NewEngine(&Options{
				Jobs:          1,
				Quiet:         true,
				CurrentBranch: tt.currentBranch,
			}, &manifest.Manifest{}, logger.NewDefaultLogger())
			e.gitRunner = &funcRunner{run: func(_ string, args []string) ([]byte, error) {
				// fetchProject 构造的参数形如 [-C <worktree> fetch ... <remote> <refspec>...]
				if len(args) >= 3 && args[0] == "-C" && args[2] == "fetch" {
					fetchArgs = args
					return nil, nil
				}
				return nil, nil
			}}
			p := &project.Project{
				Name:       "proj",
				RemoteName: "cix",
				RemoteURL:  "ssh://example.com/proj",
				Worktree:   filepath.Join(t.TempDir(), "w"),
				Revision:   "cix_master",
			}
			if err := e.fetchProject(context.Background(), p); err != nil {
				t.Fatalf("fetchProject() error = %v", err)
			}
			if fetchArgs == nil {
				t.Fatal("fetch 命令未执行")
			}
			// fetchArgs[0]=="fetch" 之后为旗标与远端名,refspec 在其后
			var specs []string
			remoteIdx := -1
			for i, a := range fetchArgs {
				if a == "cix" {
					remoteIdx = i
				}
			}
			if remoteIdx < 0 {
				t.Fatalf("fetch args 缺少远端名: %v", fetchArgs)
			}
			specs = append(specs, fetchArgs[remoteIdx+1:]...)
			if !reflect.DeepEqual(specs, tt.wantArgs) {
				t.Errorf("fetch refspecs = %v, want %v", specs, tt.wantArgs)
			}
		})
	}
}

// TestCloneProjectNetworkOnlySkipsCheckout 验证 --network-only 克隆非镜像项目时
// 传 --no-checkout，且克隆后不执行 checkout/linkfile/copyfile/submodule。
func TestCloneProjectNetworkOnlySkipsCheckout(t *testing.T) {
	base := t.TempDir()
	var cloneArgs []string
	var checkoutCalls int

	e := NewEngine(&Options{Jobs: 1, Quiet: true, NetworkOnly: true},
		&manifest.Manifest{Topdir: base, RepoDir: filepath.Join(base, ".repo")},
		logger.NewDefaultLogger())
	e.gitRunner = &funcRunner{run: func(_ string, args []string) ([]byte, error) {
		switch {
		case len(args) > 0 && args[0] == "clone":
			cloneArgs = args
		case len(args) > 2 && args[2] == "checkout":
			checkoutCalls++
		}
		return nil, nil
	}}
	p := &project.Project{
		Name:       "platform/build",
		RemoteName: "origin",
		RemoteURL:  "https://example.com/platform/build",
		Worktree:   filepath.Join(base, "build"),
		Revision:   "main",
	}

	if err := e.cloneProject(context.Background(), p); err != nil {
		t.Fatalf("cloneProject() error = %v", err)
	}
	if cloneArgs == nil {
		t.Fatal("clone 未执行")
	}
	if !containsArg(cloneArgs, "--no-checkout") {
		t.Errorf("network-only 克隆应包含 --no-checkout: %v", cloneArgs)
	}
	if checkoutCalls != 0 {
		t.Errorf("network-only 不得执行 checkout，实际 %d 次", checkoutCalls)
	}
}

// TestFetchProjectOptimizedFetchOnlyForCommitIDs 验证 --optimized-fetch 仅在
// revision 形如提交 ID 且对象已存在时才跳过 fetch。分支/标签的远端可能已前进，
// 必须正常 fetch（修复前对所有非空 RevisionID 都会跳过）。
func TestFetchProjectOptimizedFetchOnlyForCommitIDs(t *testing.T) {
	const sha = "6f692f73cb2043b4a0b0446539cd8c15b3dd9220"
	tests := []struct {
		name       string
		revision   string
		revisionID string
		wantFetch  bool
	}{
		{"commit id present locally skips fetch", sha, "", false},
		{"branch must fetch", "cix_master", "", true},
		{"hex-like branch must fetch", "deadbeef", "", true},
		{"tag ref must fetch", "refs/tags/v1.0", "", true},
		{"full sha revision id with branch revision must fetch", "cix_master", sha, true},
		{"branch revision id must fetch", "cix_master", "cix_master", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var fetched bool
			e := NewEngine(&Options{Jobs: 1, Quiet: true, OptimizedFetch: true},
				&manifest.Manifest{}, logger.NewDefaultLogger())
			e.gitRunner = &funcRunner{run: func(_ string, args []string) ([]byte, error) {
				switch {
				case len(args) > 0 && args[0] == "cat-file":
					return nil, nil // 对象已存在
				case len(args) >= 3 && args[0] == "-C" && args[2] == "fetch":
					fetched = true
				}
				return nil, nil
			}}
			p := &project.Project{
				Name:       "proj",
				RemoteName: "cix",
				RemoteURL:  "ssh://example.com/proj",
				Worktree:   filepath.Join(t.TempDir(), "w"),
				Revision:   tt.revision,
				RevisionID: tt.revisionID,
			}
			if err := e.fetchProject(context.Background(), p); err != nil {
				t.Fatalf("fetchProject() error = %v", err)
			}
			if fetched != tt.wantFetch {
				t.Errorf("fetch executed = %v, want %v (revision=%q revisionID=%q)",
					fetched, tt.wantFetch, tt.revision, tt.revisionID)
			}
		})
	}
}

// TestSyncProjectLocalOnlyMissingDoesNotClone 验证 -l/--local-only 下缺失项目
// 绝不 clone（网络操作），并返回上游同款错误提示。
func TestSyncProjectLocalOnlyMissingDoesNotClone(t *testing.T) {
	var cloneCalls int
	e := newFailFastEngine(t, func(_ string, args []string) ([]byte, error) {
		if len(args) > 0 && args[0] == "clone" {
			cloneCalls++
		}
		return nil, nil
	})
	e.options.LocalOnly = true

	p := &project.Project{
		Name:       "platform/build",
		RemoteName: "origin",
		RemoteURL:  "https://example.com/platform/build",
		Worktree:   filepath.Join(t.TempDir(), "missing", "build"),
	}

	err := e.syncProject(context.Background(), p)
	if err == nil {
		t.Fatal("local-only 下缺失项目应返回错误")
	}
	if !strings.Contains(err.Error(), "Cannot checkout platform/build due to missing network sync") {
		t.Errorf("错误应提示先执行 repo sync -n，实际: %v", err)
	}
	if cloneCalls != 0 {
		t.Errorf("local-only 不得 clone，实际调用 clone %d 次", cloneCalls)
	}
}

// TestBuildCloneArgsNetworkOnlyAndNoCloneBundle 验证：
//   - --network-only 的非镜像克隆追加 --no-checkout，镜像克隆不追加；
//   - --no-clone-bundle 永不透传给 git clone（repo-go 保持 no-op）。
func TestBuildCloneArgsNetworkOnlyAndNoCloneBundle(t *testing.T) {
	tests := []struct {
		name           string
		networkOnly    bool
		mirror         bool
		wantNoCheckout bool
	}{
		{"network-only worktree", true, false, true},
		{"network-only mirror", true, true, false},
		{"normal worktree", false, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := &Engine{
				logger: logger.NewDefaultLogger(),
				options: &Options{
					NetworkOnly:   tt.networkOnly,
					NoCloneBundle: true,
					Quiet:         true,
				},
			}
			p := &project.Project{Name: "proj", RemoteName: "origin", Worktree: "/tmp/w"}
			args := e.buildCloneArgs(p, "https://example.com/proj", tt.mirror)

			if containsArg(args, "--no-clone-bundle") {
				t.Errorf("clone args 不应包含 --no-clone-bundle: %v", args)
			}
			if got := containsArg(args, "--no-checkout"); got != tt.wantNoCheckout {
				t.Errorf("--no-checkout present = %v, want %v (args: %v)", got, tt.wantNoCheckout, args)
			}
		})
	}
}

// TestCheckoutProjectDetachWhenAlreadyAtRevision 验证 HEAD 已等于 revid 但
// 仍附着在分支上时，--detach 必须执行 checkout --detach（修复前提前 return）。
func TestCheckoutProjectDetachWhenAlreadyAtRevision(t *testing.T) {
	const revid = "7353ca0399ae65ad89051dc3d4db2a70284d6881"

	var checkoutArgs []string
	e := NewEngine(&Options{Jobs: 1, Quiet: true, Detach: true}, nil, logger.NewDefaultLogger())
	e.gitRunner = &funcRunner{run: func(_ string, args []string) ([]byte, error) {
		switch {
		case len(args) > 0 && args[0] == "rev-parse":
			return []byte(revid + "\n"), nil
		case len(args) > 0 && args[0] == "symbolic-ref":
			return []byte("refs/heads/cix_master\n"), nil
		case len(args) > 2 && args[2] == "checkout":
			checkoutArgs = args
			return nil, nil
		}
		return nil, nil
	}}
	p := &project.Project{
		Name:       "proj",
		RemoteName: "cix",
		Worktree:   filepath.Join(t.TempDir(), "w"),
		Revision:   "cix_master",
	}

	if err := e.checkoutProject(context.Background(), p); err != nil {
		t.Fatalf("checkoutProject() error = %v", err)
	}
	want := []string{"-C", p.Worktree, "checkout", "--detach", revid, "--"}
	if !reflect.DeepEqual(checkoutArgs, want) {
		t.Errorf("checkout args = %v, want %v", checkoutArgs, want)
	}
}

// TestCheckoutProjectDetachAlreadyDetachedSkips 验证已处于游离 HEAD 且
// HEAD == revid 时，--detach 无需再次 checkout。
func TestCheckoutProjectDetachAlreadyDetachedSkips(t *testing.T) {
	const revid = "7353ca0399ae65ad89051dc3d4db2a70284d6881"

	var checkoutCalls int
	e := NewEngine(&Options{Jobs: 1, Quiet: true, Detach: true}, nil, logger.NewDefaultLogger())
	e.gitRunner = &funcRunner{run: func(_ string, args []string) ([]byte, error) {
		switch {
		case len(args) > 0 && args[0] == "rev-parse":
			return []byte(revid + "\n"), nil
		case len(args) > 0 && args[0] == "symbolic-ref":
			return nil, errors.New("fatal: not a symbolic ref")
		case len(args) > 2 && args[2] == "checkout":
			checkoutCalls++
			return nil, nil
		}
		return nil, nil
	}}
	p := &project.Project{
		Name:       "proj",
		RemoteName: "cix",
		Worktree:   filepath.Join(t.TempDir(), "w"),
		Revision:   "cix_master",
	}

	if err := e.checkoutProject(context.Background(), p); err != nil {
		t.Fatalf("checkoutProject() error = %v", err)
	}
	if checkoutCalls != 0 {
		t.Errorf("已 detached 且 HEAD 就位时不应 checkout，实际 %d 次", checkoutCalls)
	}
}

// TestCheckoutRevisionForceOnlyWhenRequested 验证重试绝不在最后一轮偷偷加
// --force：仅 --force-sync/--force-overwrite 显式请求时才允许强制覆盖。
func TestCheckoutRevisionForceOnlyWhenRequested(t *testing.T) {
	tests := []struct {
		name           string
		forceSync      bool
		forceOverwrite bool
		wantForce      bool
	}{
		{"default never forces", false, false, false},
		{"force-sync forces", true, false, true},
		{"force-overwrite forces", false, true, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			var calls [][]string
			e := NewEngine(&Options{
				Jobs:           1,
				Quiet:          true,
				ForceSync:      tt.forceSync,
				ForceOverwrite: tt.forceOverwrite,
			}, nil, logger.NewDefaultLogger())
			e.gitRunner = &funcRunner{run: func(_ string, args []string) ([]byte, error) {
				if len(args) > 2 && args[2] == "checkout" {
					calls = append(calls, args)
					cancel() // 避免重试退避拖慢测试
					return nil, errors.New("error: Your local changes would be overwritten")
				}
				return nil, nil
			}}
			p := &project.Project{Name: "proj", Worktree: t.TempDir(), Revision: "cix_master"}

			err := e.checkoutRevision(ctx, p, "7353ca0399ae65ad89051dc3d4db2a70284d6881", true)
			if err == nil {
				t.Fatal("checkoutRevision() 应在重试耗尽/取消后返回错误")
			}
			if len(calls) == 0 {
				t.Fatal("checkout 未执行")
			}
			for _, args := range calls {
				if got := containsArg(args, "--force"); got != tt.wantForce {
					t.Errorf("--force present = %v, want %v (args: %v)", got, tt.wantForce, args)
				}
			}
		})
	}
}

// TestFastForwardToFailureReturnsError 验证 merge --ff-only 失败不再返回 nil
// 冒充成功，而是返回带 stderr 的错误（项目计为失败）。
func TestFastForwardToFailureReturnsError(t *testing.T) {
	e := NewEngine(&Options{Jobs: 1, Quiet: true}, nil, logger.NewDefaultLogger())
	e.gitRunner = &funcRunner{run: func(_ string, args []string) ([]byte, error) {
		if len(args) > 2 && args[2] == "merge" {
			return nil, &git.GitCommandError{
				Command: "merge", Dir: "/w", Err: errors.New("exit status 1"),
				Stderr: "CONFLICT: local changes", ExitCode: 1,
			}
		}
		return nil, nil
	}}
	p := &project.Project{Name: "proj", Worktree: filepath.Join(t.TempDir(), "w")}

	err := e.fastForwardTo(p, "1111111111111111111111111111111111111111")
	if err == nil {
		t.Fatal("fastForwardTo() 在 merge --ff-only 失败时应返回错误")
	}
	var syncErr *SyncError
	if !errors.As(err, &syncErr) {
		t.Fatalf("错误应为 *SyncError，实际 %T: %v", err, err)
	}
	if syncErr.Phase != "checkout" {
		t.Errorf("SyncError.Phase = %q, want checkout", syncErr.Phase)
	}
	if !strings.Contains(syncErr.Output, "CONFLICT") {
		t.Errorf("SyncError.Output 应包含 stderr，实际: %q", syncErr.Output)
	}
}

// TestResolveRevisionIDPrefersFullRevisionID 验证超级项目写入的完整 SHA
// RevisionID 优先于 manifest revision（使 --use-superproject 覆盖生效）。
func TestResolveRevisionIDPrefersFullRevisionID(t *testing.T) {
	const override = "3333333333333333333333333333333333333333"
	e := &Engine{
		logger: logger.NewDefaultLogger(),
		gitRunner: &funcRunner{run: func(_ string, _ []string) ([]byte, error) {
			t.Fatal("RevisionID 为完整 SHA 时不应再探测 git")
			return nil, nil
		}},
	}
	p := &project.Project{
		Name:       "proj",
		Worktree:   t.TempDir(),
		Revision:   "cix_master",
		RevisionID: override,
	}
	got, err := e.resolveRevisionID(p)
	if err != nil {
		t.Fatalf("resolveRevisionID() error = %v", err)
	}
	if got != override {
		t.Errorf("resolveRevisionID() = %q, want override %q", got, override)
	}
}

// TestCleanupIncompleteCloneForceSyncGate 验证克隆残留清理：
// 非空目录未开启 --force-sync 时拒绝删除；本次运行前的空目录可直接清理；
// 开启 --force-sync 后仅在安全范围内删除。
func TestCleanupIncompleteCloneForceSyncGate(t *testing.T) {
	t.Run("without force-sync refuses non-empty target", func(t *testing.T) {
		root := t.TempDir()
		work := filepath.Join(root, "build")
		if err := os.MkdirAll(work, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(work, "user-data"), []byte("keep"), 0644); err != nil {
			t.Fatal(err)
		}
		e := &Engine{
			logger:   logger.NewDefaultLogger(),
			options:  &Options{},
			repoRoot: root,
		}
		p := &project.Project{Name: "build", Worktree: work}
		err := e.cleanupIncompleteClone(p)
		if err == nil || !strings.Contains(err.Error(), "without --force-sync") {
			t.Fatalf("期望拒绝删除错误，实际: %v", err)
		}
		if _, statErr := os.Stat(work); statErr != nil {
			t.Errorf("目录不应被删除: %v", statErr)
		}
	})

	t.Run("empty target can be cleaned without force-sync", func(t *testing.T) {
		root := t.TempDir()
		work := filepath.Join(root, "build")
		if err := os.MkdirAll(work, 0755); err != nil {
			t.Fatal(err)
		}
		e := &Engine{
			logger:   logger.NewDefaultLogger(),
			options:  &Options{},
			repoRoot: root,
		}
		p := &project.Project{Name: "build", Worktree: work}
		if err := e.cleanupIncompleteCloneState(p, cloneTargetState{existed: true, empty: true}); err != nil {
			t.Fatalf("空目录应可无 --force-sync 清理: %v", err)
		}
		if _, statErr := os.Stat(work); !os.IsNotExist(statErr) {
			t.Errorf("空目录应被删除，stat err = %v", statErr)
		}
	})

	t.Run("force-sync removes safe residual", func(t *testing.T) {
		root := t.TempDir()
		work := filepath.Join(root, "build")
		if err := os.MkdirAll(work, 0755); err != nil {
			t.Fatal(err)
		}
		e := &Engine{
			logger:   logger.NewDefaultLogger(),
			options:  &Options{ForceSync: true},
			repoRoot: root,
		}
		p := &project.Project{Name: "build", Worktree: work}
		if err := e.cleanupIncompleteClone(p); err != nil {
			t.Fatalf("cleanupIncompleteClone() error = %v", err)
		}
		if _, statErr := os.Stat(work); !os.IsNotExist(statErr) {
			t.Errorf("残留目录应被删除，stat err = %v", statErr)
		}
	})

	t.Run("force-sync refuses repo root", func(t *testing.T) {
		root := t.TempDir()
		e := &Engine{
			logger:   logger.NewDefaultLogger(),
			options:  &Options{ForceSync: true},
			repoRoot: root,
		}
		p := &project.Project{Name: "root", Worktree: root}
		err := e.cleanupIncompleteClone(p)
		if err == nil {
			t.Fatal("删除 repo 根目录本身必须被拒绝")
		}
	})
}

// TestHexBranchNameClonedAsBranch 验证 7-40 位十六进制分支名（如 deadbeef）
// 不再被误判为提交 ID：clone 使用 --branch，fetch 生成分支 refspec，
// resolveRevisionID 走 refs/remotes/<remote>/<branch>。
func TestHexBranchNameClonedAsBranch(t *testing.T) {
	root, bare, work, runner := setupRemoteWithCommit(t)
	const branch = "deadbeef"
	mustGit(t, runner, work, "branch", branch)
	mustGit(t, runner, work, "push", "origin", branch)
	wantCommit := mustGit(t, runner, bare, "rev-parse", "refs/heads/"+branch)

	worktree := filepath.Join(root, "proj")
	m := &manifest.Manifest{Topdir: root, RepoDir: filepath.Join(root, ".repo")}
	e := NewEngine(&Options{Jobs: 1, Quiet: true}, m, logger.NewDefaultLogger())
	p := &project.Project{
		Name:       "proj",
		RemoteName: "origin",
		RemoteURL:  bare,
		Worktree:   worktree,
		Revision:   branch,
	}

	args := e.buildCloneArgs(p, bare, false)
	if !containsArg(args, "--branch") || !argValue(args, "--branch", branch) {
		t.Fatalf("hex 分支名应按分支克隆（--branch %s），实际参数: %v", branch, args)
	}
	if spec := revisionFetchRefspec(p); spec != "+refs/heads/"+branch+":refs/remotes/origin/"+branch {
		t.Errorf("revisionFetchRefspec() = %q, want branch refspec", spec)
	}

	if err := e.cloneProject(context.Background(), p); err != nil {
		t.Fatalf("cloneProject() error = %v", err)
	}
	got, err := e.resolveRevisionID(p)
	if err != nil {
		t.Fatalf("resolveRevisionID() error = %v", err)
	}
	if got != wantCommit {
		t.Errorf("resolveRevisionID() = %q, want branch commit %q", got, wantCommit)
	}
}

// TestCloneProjectForceSyncRecoversWithZeroRetries 验证 --force-sync 对运行前
// 已存在的损坏/非空目录在第一次 clone 前就执行安全清理，因此
// --retry-fetches 0（不重试）也能恢复。
func TestCloneProjectForceSyncRecoversWithZeroRetries(t *testing.T) {
	root, bare, _, _ := setupRemoteWithCommit(t)
	worktree := filepath.Join(root, "proj")
	if err := os.MkdirAll(worktree, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktree, "stale"), []byte("garbage"), 0644); err != nil {
		t.Fatal(err)
	}

	m := &manifest.Manifest{Topdir: root, RepoDir: filepath.Join(root, ".repo")}
	e := NewEngine(&Options{
		Jobs:         1,
		Quiet:        true,
		ForceSync:    true,
		RetryFetches: 0,
	}, m, logger.NewDefaultLogger())
	p := &project.Project{
		Name:       "proj",
		RemoteName: "origin",
		RemoteURL:  bare,
		Worktree:   worktree,
		Revision:   "main",
	}

	if err := e.cloneProject(context.Background(), p); err != nil {
		t.Fatalf("force-sync clone with zero retries should recover: %v", err)
	}
	if _, err := os.Stat(filepath.Join(worktree, "stale")); !os.IsNotExist(err) {
		t.Errorf("损坏残留应被清理，stat err = %v", err)
	}
	data, err := os.ReadFile(filepath.Join(worktree, "file.txt"))
	if err != nil {
		t.Fatalf("force-sync 后工作树应被重建: %v", err)
	}
	if string(data) != "hello\n" {
		t.Errorf("file.txt = %q, want hello", string(data))
	}
}

// TestCloneProjectNestedWorktreeMergesIntoExistingDir 回归测试：
// 嵌套项目（父项目 worktree 包含子项目 worktree）并发 clone 时，
// 子项目可能先创建父项目目录，直接 git clone 会报“目标已存在且非空”。
// 父项目应改用临时目录 clone 后合并，且不得删除子项目已创建的内容。
func TestCloneProjectNestedWorktreeMergesIntoExistingDir(t *testing.T) {
	root, bare, _, _ := setupRemoteWithCommit(t)
	parentWT := filepath.Join(root, "npu", "ai_demo")
	childWT := filepath.Join(parentWT, "linux", "edge-slm")

	// 模拟并发的子项目先 MkdirAll，使父项目目标目录非空。
	if err := os.MkdirAll(childWT, 0755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(childWT, "marker.txt")
	if err := os.WriteFile(marker, []byte("child"), 0644); err != nil {
		t.Fatal(err)
	}

	parent := &project.Project{
		Name:       "ai_demo",
		RemoteName: "origin",
		RemoteURL:  bare,
		Worktree:   parentWT,
		Revision:   "main",
	}
	child := &project.Project{
		Name:       "edge-slm",
		RemoteName: "origin",
		RemoteURL:  bare,
		Worktree:   childWT,
		Revision:   "main",
	}
	m := &manifest.Manifest{Topdir: root, RepoDir: filepath.Join(root, ".repo")}
	e := NewEngine(&Options{
		Jobs:      1,
		Quiet:     true,
		ForceSync: true, // 构建环境常用；嵌套路径绝不能触发删除子项目目录
	}, m, logger.NewDefaultLogger())
	e.projects = []*project.Project{parent, child}

	if err := e.cloneProject(context.Background(), parent); err != nil {
		t.Fatalf("嵌套项目 clone 应合并到已存在目录，实际: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(parentWT, "file.txt"))
	if err != nil {
		t.Fatalf("父项目工作树应被填充: %v", err)
	}
	if string(data) != "hello\n" {
		t.Errorf("file.txt = %q, want hello", string(data))
	}
	if data, err := os.ReadFile(marker); err != nil || string(data) != "child" {
		t.Fatalf("子项目已创建的内容不应被删除: data=%q err=%v", string(data), err)
	}
	if _, err := os.Stat(filepath.Join(childWT, ".git")); !os.IsNotExist(err) {
		t.Errorf("父项目 clone 不应改写子项目工作树，stat err = %v", err)
	}
}

// argValue 判断 args 中 flag 后紧跟的值为 want。
func argValue(args []string, flag, want string) bool {
	for i := range len(args) - 1 {
		if args[i] == flag && args[i+1] == want {
			return true
		}
	}
	return false
}
