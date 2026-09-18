package repo_sync

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/leopardxu/repo-go/internal/logger"
	"github.com/leopardxu/repo-go/internal/manifest"
	"github.com/leopardxu/repo-go/internal/project"
)

// TestFetchRetries 验证 fetch/clone 重试次数取自 Options.RetryFetches，默认 3。
// 覆盖阶段 1.4 的 RetryFetches 接线逻辑（替换原硬编码 3）。
func TestFetchRetries(t *testing.T) {
	tests := []struct {
		name string
		opts *Options
		want int
	}{
		{"default when zero", &Options{RetryFetches: 0}, 3},
		{"default when negative", &Options{RetryFetches: -1}, 3},
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
						case "refs/remotes/cix/cix_master":
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
// 任何 checkout 命令(对齐上游 "No changes" 分支)。
func TestCheckoutProjectAlreadyAtRevision(t *testing.T) {
	const revid = "7353ca0399ae65ad89051dc3d4db2a70284d6881"

	var gitCalls int
	e := &Engine{
		logger:  logger.NewDefaultLogger(),
		options: &Options{Jobs: 1},
		gitRunner: &funcRunner{run: func(_ string, args []string) ([]byte, error) {
			gitCalls++
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
	// 只允许发生 rev-parse 探测,不允许 checkout
	if gitCalls > 2 {
		t.Errorf("unexpected git calls beyond rev-parse probes: %d", gitCalls)
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

// TestLooksLikeCommitID 验证提交 ID 形态判定(clone --branch 与
// revision 解析共用)。
func TestLooksLikeCommitID(t *testing.T) {
	tests := []struct {
		name string
		rev  string
		want bool
	}{
		{"40 hex", "6f692f73cb2043b4a0b0446539cd8c15b3dd9220", true},
		{"short hex", "7353ca0", true},
		{"too short", "7353ca", false},
		{"branch name", "cix_master", false},
		{"branch like hex but short", "abcdef", false},
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
