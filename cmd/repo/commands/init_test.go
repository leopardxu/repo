package commands

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/leopardxu/repo-go/internal/logger"
	"github.com/leopardxu/repo-go/internal/manifest"
	"github.com/spf13/cobra"
)

// TestDiffInitConfig 验证 init 关键配置变化的判定逻辑：
// 仅标识性字段（URL/分支/清单名/groups/platform）变化才视为需确认，
// 克隆调优参数（如 depth）变化不应触发确认。
func TestDiffInitConfig(t *testing.T) {
	// 基线：现有配置与新选项的关键标识字段完全一致
	base := RepoConfig{
		ManifestURL:    "https://a.example/manifest.git",
		ManifestBranch: "main",
		ManifestName:   "default.xml",
		Groups:         "default",
		Platform:       "auto",
	}
	baseOpts := func() *InitOptions {
		return &InitOptions{
			ManifestURL:    base.ManifestURL,
			ManifestBranch: base.ManifestBranch,
			ManifestName:   base.ManifestName,
			Groups:         base.Groups,
			Platform:       base.Platform,
		}
	}

	tests := []struct {
		name      string
		mutate    func(o *InitOptions)
		wantDiffs int
	}{
		{"identical refresh", func(_ *InitOptions) {}, 0},
		{"manifest-url changed", func(o *InitOptions) { o.ManifestURL = "https://b.example/m.git" }, 1},
		{"manifest-branch changed", func(o *InitOptions) { o.ManifestBranch = "dev" }, 1},
		{"manifest-name changed", func(o *InitOptions) { o.ManifestName = "other.xml" }, 1},
		{"groups changed", func(o *InitOptions) { o.Groups = "all" }, 1},
		{"platform changed", func(o *InitOptions) { o.Platform = "linux" }, 1},
		{"multiple changed", func(o *InitOptions) {
			o.ManifestURL = "https://b.example/m.git"
			o.Groups = "all"
			o.Platform = "linux"
		}, 3},
		{"clone-tuning only (depth) ignored", func(o *InitOptions) { o.Depth = 5 }, 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts := baseOpts()
			tt.mutate(opts)
			diffs := diffInitConfig(base, opts)
			if len(diffs) != tt.wantDiffs {
				t.Fatalf("diffInitConfig got %d diffs, want %d: %+v", len(diffs), tt.wantDiffs, diffs)
			}
		})
	}
}

// TestValidateOptions 验证 init 选项校验逻辑：
// - ManifestURL 为空时报错
// - mirror 与 partial-clone / worktree 互斥
// - 正反 flag 同时设置时报错
func TestValidateOptions(t *testing.T) {
	validOpts := func() *InitOptions {
		return &InitOptions{
			ManifestURL: "https://example.com/manifest.git",
		}
	}

	tests := []struct {
		name    string
		mutate  func(o *InitOptions)
		wantErr bool
	}{
		{"valid opts", func(_ *InitOptions) {}, false},
		{"empty manifest url", func(o *InitOptions) { o.ManifestURL = "" }, true},
		{"mirror + archive", func(o *InitOptions) { o.Mirror = true; o.Archive = true }, true},
		{"mirror + partial-clone", func(o *InitOptions) { o.Mirror = true; o.PartialClone = true }, true},
		{"mirror + worktree", func(o *InitOptions) { o.Mirror = true; o.Worktree = true }, true},
		{"mirror + use-superproject", func(o *InitOptions) { o.Mirror = true; o.UseSuperproject = true }, true},
		{"current-branch + no-current-branch", func(o *InitOptions) { o.CurrentBranch = true; o.NoCurrentBranch = true }, true},
		{"tags + no-tags", func(o *InitOptions) { o.Tags = true; o.NoTags = true }, true},
		{"standalone-manifest + manifest-branch", func(o *InitOptions) {
			o.StandaloneManifest = true
			o.ManifestBranch = "main"
		}, true},
		{"manifest-upstream-branch without manifest-branch", func(o *InitOptions) {
			o.ManifestUpstreamBranch = "main"
		}, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts := validOpts()
			tt.mutate(opts)
			err := validateOptions(opts)
			if tt.wantErr && err == nil {
				t.Fatalf("validateOptions expected error, got nil")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("validateOptions unexpected error: %v", err)
			}
		})
	}
}

// mockGitResp 是 mockGitRunner 对单条命令的预设响应
type mockGitResp struct {
	out []byte
	err error
}

// mockGitRunner 脚本化 git.Runner：按完整命令行（args 空格拼接）返回预设
// 响应，未预设的命令视为成功；同时记录调用顺序供断言。测试不产生真实子进程。
type mockGitRunner struct {
	responses map[string]mockGitResp
	commands  []string
}

func (m *mockGitRunner) respond(args []string) ([]byte, error) {
	key := strings.Join(args, " ")
	m.commands = append(m.commands, key)
	if resp, ok := m.responses[key]; ok {
		return resp.out, resp.err
	}
	return nil, nil
}

func (m *mockGitRunner) Run(args ...string) ([]byte, error) {
	return m.respond(args)
}

func (m *mockGitRunner) RunInDir(_ string, args ...string) ([]byte, error) {
	return m.respond(args)
}

func (m *mockGitRunner) RunWithTimeout(_ time.Duration, args ...string) ([]byte, error) {
	return m.respond(args)
}

func (m *mockGitRunner) RunInDirWithTimeout(_ string, _ time.Duration, args ...string) ([]byte, error) {
	return m.respond(args)
}

func (m *mockGitRunner) SetVerbose(_ bool)             {}
func (m *mockGitRunner) SetQuiet(_ bool)               {}
func (m *mockGitRunner) SetMaxRetries(_ int)           {}
func (m *mockGitRunner) SetRetryDelay(_ time.Duration) {}
func (m *mockGitRunner) SetConcurrency(_ int)          {}

// TestUpdateExistingManifestRepo 验证已存在清单仓库的更新语义：
//   - 本地分支存在：checkout 后必须 reset --hard 到远程顶端（陈旧清单回归）
//   - 仅远程存在：按显式 refspec 补取后从远程跟踪分支创建
//   - 远程无此分支：报错，不得静默沿用旧分支导致配置与检出不一致
//   - mirror/未指定分支：仅 fetch，不做 checkout
func TestUpdateExistingManifestRepo(t *testing.T) {
	urlCommands := func(url string) []string {
		return []string{
			"config --get remote.origin.url",
			"remote add origin " + url,
		}
	}
	branchTail := func(branch string) []string {
		remoteRef := "refs/remotes/origin/" + branch
		return []string{
			"rev-parse --verify --quiet " + remoteRef,
			"rev-parse --verify --quiet refs/heads/" + branch,
			"checkout " + branch,
			"reset --hard " + remoteRef,
		}
	}

	tests := []struct {
		name      string
		cfg       *RepoConfig
		responses map[string]mockGitResp
		wantErr   bool
		wantCmds  []string
	}{
		{
			name: "local branch exists: checkout then reset to remote tip",
			cfg:  &RepoConfig{ManifestBranch: "main", ManifestURL: "https://example.com/manifest.git"},
			responses: map[string]mockGitResp{
				"rev-parse --verify --quiet refs/remotes/origin/main": {out: []byte("abc123\n")},
				"rev-parse --verify --quiet refs/heads/main":          {out: []byte("abc123\n")},
			},
			wantCmds: append(append(urlCommands("https://example.com/manifest.git"), "fetch --all"), branchTail("main")...),
		},
		{
			name: "origin url changed: set-url before fetch",
			cfg:  &RepoConfig{ManifestBranch: "main", ManifestURL: "https://new.example/manifest.git"},
			responses: map[string]mockGitResp{
				"config --get remote.origin.url": {out: []byte("https://old.example/manifest.git\n")},
			},
			wantCmds: append(append([]string{
				"config --get remote.origin.url",
				"remote set-url origin https://new.example/manifest.git",
			}, "fetch --all"), branchTail("main")...),
		},
		{
			name: "branch only on remote: fetch by refspec then create",
			cfg:  &RepoConfig{ManifestBranch: "feature", ManifestURL: "https://example.com/manifest.git"},
			responses: map[string]mockGitResp{
				"rev-parse --verify --quiet refs/remotes/origin/feature": {err: errors.New("exit 1")},
				"rev-parse --verify --quiet refs/heads/feature":          {err: errors.New("exit 1")},
			},
			wantCmds: append(append(urlCommands("https://example.com/manifest.git"), "fetch --all"),
				"rev-parse --verify --quiet refs/remotes/origin/feature",
				"fetch origin refs/heads/feature:refs/remotes/origin/feature",
				"rev-parse --verify --quiet refs/heads/feature",
				"checkout -b feature refs/remotes/origin/feature",
			),
		},
		{
			name: "branch missing on remote: error instead of silent stale fallback",
			cfg:  &RepoConfig{ManifestBranch: "nosuchbranch", ManifestURL: "https://example.com/manifest.git"},
			responses: map[string]mockGitResp{
				"rev-parse --verify --quiet refs/remotes/origin/nosuchbranch": {err: errors.New("exit 1")},
				"fetch origin refs/heads/nosuchbranch:refs/remotes/origin/nosuchbranch": {
					err: errors.New("fatal: couldn't find remote ref nosuchbranch"),
				},
			},
			wantErr: true,
			wantCmds: append(append(urlCommands("https://example.com/manifest.git"), "fetch --all"),
				"rev-parse --verify --quiet refs/remotes/origin/nosuchbranch",
				"fetch origin refs/heads/nosuchbranch:refs/remotes/origin/nosuchbranch",
			),
		},
		{
			name: "-b HEAD: resolve remote default branch then checkout",
			cfg:  &RepoConfig{ManifestBranch: "HEAD"},
			responses: map[string]mockGitResp{
				"symbolic-ref --short refs/remotes/origin/HEAD": {out: []byte("origin/trunk\n")},
			},
			wantCmds: append([]string{
				"fetch --all",
				"symbolic-ref --short refs/remotes/origin/HEAD",
			}, branchTail("trunk")...),
		},
		{
			name: "-b HEAD with URL: resolve via ls-remote --symref after fetch",
			cfg:  &RepoConfig{ManifestBranch: "HEAD", ManifestURL: "https://new.example/manifest.git"},
			responses: map[string]mockGitResp{
				"ls-remote --symref https://new.example/manifest.git HEAD": {
					out: []byte("ref: refs/heads/trunk\tHEAD\n"),
				},
			},
			wantCmds: []string{
				"config --get remote.origin.url",
				"remote add origin https://new.example/manifest.git",
				"fetch --all",
				"ls-remote --symref https://new.example/manifest.git HEAD",
				"rev-parse --verify --quiet refs/remotes/origin/trunk",
				"rev-parse --verify --quiet refs/heads/trunk",
				"checkout trunk",
				"reset --hard refs/remotes/origin/trunk",
			},
		},
		{
			name: "-b <commit SHA>: fetch then checkout detached commit",
			cfg:  &RepoConfig{ManifestBranch: "0123456789abcdef0123456789abcdef01234567"},
			wantCmds: []string{
				"fetch --all",
				"checkout 0123456789abcdef0123456789abcdef01234567",
			},
		},
		{
			name:     "mirror still updates manifest worktree (manifest repo is never bare)",
			cfg:      &RepoConfig{ManifestBranch: "main", Mirror: true},
			wantCmds: append([]string{"fetch --all"}, branchTail("main")...),
		},
		{
			name:     "no branch specified: fetch only",
			cfg:      &RepoConfig{},
			wantCmds: []string{"fetch --all"},
		},
		{
			name: "fetch failure propagates",
			cfg:  &RepoConfig{ManifestBranch: "main"},
			responses: map[string]mockGitResp{
				"fetch --all": {err: errors.New("network unreachable")},
			},
			wantErr:  true,
			wantCmds: []string{"fetch --all"},
		},
		{
			name: "checkout failure propagates",
			cfg:  &RepoConfig{ManifestBranch: "main"},
			responses: map[string]mockGitResp{
				"checkout main": {err: errors.New("checkout conflict")},
			},
			wantErr: true,
			wantCmds: []string{
				"fetch --all",
				"rev-parse --verify --quiet refs/remotes/origin/main",
				"rev-parse --verify --quiet refs/heads/main",
				"checkout main",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runner := &mockGitRunner{responses: tt.responses}
			err := updateExistingManifestRepo(runner, ".repo/manifests", tt.cfg)
			if tt.wantErr && err == nil {
				t.Fatalf("updateExistingManifestRepo expected error, got nil")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("updateExistingManifestRepo unexpected error: %v", err)
			}
			if !reflect.DeepEqual(runner.commands, tt.wantCmds) {
				t.Errorf("git command sequence = %v, want %v", runner.commands, tt.wantCmds)
			}
		})
	}
}

// TestMergePlatformGroups 验证 --platform 按上游语义合并进有效组：
// auto 追加宿主平台组（mirror 时不追加）、all 追加三个平台组、none 不追加、
// 具体平台追加对应组、非法值报错；重复 init 时先剔除旧的 platform-* 组。
func TestMergePlatformGroups(t *testing.T) {
	hostGroup := "platform-" + runtime.GOOS
	tests := []struct {
		name     string
		groups   string
		platform string
		mirror   bool
		preserve bool
		want     string
		wantErr  bool
	}{
		{"auto adds host platform", "default", "auto", false, false, "default," + hostGroup, false},
		{"auto mirror skips platform", "default", "auto", true, false, "default", false},
		{"all adds every platform", "default", "all", false, false, "default,platform-linux,platform-darwin,platform-windows", false},
		{"none adds nothing", "default", "none", false, false, "default", false},
		{"specific platform", "default", "linux", false, false, "default,platform-linux", false},
		{"empty platform means auto", "default", "", false, false, "default," + hostGroup, false},
		{"invalid platform", "default", "solaris", false, false, "", true},
		{"stale inherited platform groups removed before merge", "default,platform-windows,cix", "linux", false, false, "default,cix,platform-linux", false},
		{"explicit platform groups preserved", "default,platform-custom", "none", false, true, "default,platform-custom", false},
		{"explicit platform groups kept alongside added", "platform-windows", "linux", false, true, "platform-windows,platform-linux", false},
		{"explicit groups preserved", "cix,plfm", "none", false, false, "cix,plfm", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := mergePlatformGroups(tt.groups, tt.platform, tt.mirror, tt.preserve)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("mergePlatformGroups(%q,%q) expected error", tt.groups, tt.platform)
				}
				return
			}
			if err != nil {
				t.Fatalf("mergePlatformGroups unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("mergePlatformGroups(%q,%q,mirror=%v,preserve=%v) = %q, want %q",
					tt.groups, tt.platform, tt.mirror, tt.preserve, got, tt.want)
			}
		})
	}
}

// TestApplyNegationFlagsBeforeValidate 回归测试：--no-current-branch /
// --no-clone-bundle 必须先在 validateOptions 之前归一化，否则正 flag 的
// 默认 true 会被误判为"正反同时指定"而永久报错。
func TestApplyNegationFlagsBeforeValidate(t *testing.T) {
	tests := []struct {
		name   string
		opts   *InitOptions
		mutate func(o *InitOptions)
		check  func(o *InitOptions) bool
	}{
		{
			name:   "no-current-branch",
			opts:   &InitOptions{ManifestURL: "https://example.com/m.git", CurrentBranch: true},
			mutate: func(o *InitOptions) { o.NoCurrentBranch = true },
			check:  func(o *InitOptions) bool { return !o.CurrentBranch },
		},
		{
			name:   "no-clone-bundle",
			opts:   &InitOptions{ManifestURL: "https://example.com/m.git", CloneBundle: true},
			mutate: func(o *InitOptions) { o.NoCloneBundle = true },
			check:  func(o *InitOptions) bool { return !o.CloneBundle },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.mutate(tt.opts)
			applyNegationFlags(tt.opts)
			if !tt.check(tt.opts) {
				t.Fatalf("negation flag was not normalized: %+v", tt.opts)
			}
			if err := validateOptions(tt.opts); err != nil {
				t.Fatalf("validateOptions after negation failed: %v", err)
			}
		})
	}
}

// TestManifestCloneParamsChanged 验证清单仓库克隆参数变化检测：
// depth/partial-clone/filter/reference/dissociate 变化需触发重克隆。
func TestManifestCloneParamsChanged(t *testing.T) {
	base := RepoConfig{ManifestDepth: 0, PartialClone: false, CloneFilter: "blob:none", Reference: "", Dissociate: false}
	tests := []struct {
		name   string
		mutate func(o *RepoConfig)
		want   bool
	}{
		{"unchanged", func(_ *RepoConfig) {}, false},
		{"depth changed", func(o *RepoConfig) { o.ManifestDepth = 5 }, true},
		{"partial clone changed", func(o *RepoConfig) { o.PartialClone = true }, true},
		{"reference changed", func(o *RepoConfig) { o.Reference = "/mirror" }, true},
		{"dissociate changed", func(o *RepoConfig) { o.Dissociate = true }, true},
		{"non-clone field ignored", func(o *RepoConfig) { o.ManifestBranch = "dev" }, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			next := base
			tt.mutate(&next)
			if got := manifestCloneParamsChanged(base, next); got != tt.want {
				t.Errorf("manifestCloneParamsChanged = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestValidateChangedFlagConflicts 验证正反 flag 同时显式指定时在归一化前报错。
func TestValidateChangedFlagConflicts(t *testing.T) {
	newCmd := func(t *testing.T, flags ...string) *cobra.Command {
		t.Helper()
		cmd := InitCmd()
		for _, f := range flags {
			if err := cmd.Flags().Set(f, "true"); err != nil {
				t.Fatalf("set flag %s: %v", f, err)
			}
		}
		return cmd
	}

	tests := []struct {
		name    string
		flags   []string
		wantErr bool
	}{
		{"no flags", nil, false},
		{"single negative flag", []string{"no-current-branch"}, false},
		{"single positive flag", []string{"current-branch"}, false},
		{"both current-branch flags", []string{"current-branch", "no-current-branch"}, true},
		{"both clone-bundle flags", []string{"clone-bundle", "no-clone-bundle"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateChangedFlagConflicts(newCmd(t, tt.flags...))
			if tt.wantErr && err == nil {
				t.Fatal("expected conflict error")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

// TestApplyRepoConfigInheritance 验证 re-init 时未显式指定的字段从旧配置继承：
// `repo init -u URL` 不得把 mirror/reference/depth/groups 等重置为默认值。
func TestApplyRepoConfigInheritance(t *testing.T) {
	old := &RepoConfig{
		ManifestURL:         "https://old.example/manifest.git",
		ManifestBranch:      "main",
		ManifestName:        "cix.xml",
		Groups:              "default,platform-linux,cix",
		Platform:            "auto",
		Mirror:              true,
		Reference:           "/mirror",
		Depth:               7,
		ManifestDepth:       3,
		PartialClone:        true,
		PartialCloneExclude: "big/*",
		CloneFilter:         "blob:limit=1m",
		Dissociate:          true,
		GitLFS:              true,
		CurrentBranch:       false,
		Tags:                true,
		Submodules:          true,
	}

	t.Run("nothing explicit inherits everything", func(t *testing.T) {
		opts := &InitOptions{
			ManifestName:  "default.xml",
			Groups:        "default",
			Platform:      "auto",
			CloneFilter:   "blob:none",
			CurrentBranch: true,
			changedFlags:  map[string]bool{"manifest-url": true},
		}
		opts.ManifestURL = "https://new.example/manifest.git"
		applyRepoConfigInheritance(opts, old)

		if opts.ManifestBranch != "main" || opts.ManifestName != "cix.xml" || opts.Groups != old.Groups {
			t.Errorf("string fields not inherited: %+v", opts)
		}
		if !opts.Mirror || opts.Reference != "/mirror" || opts.Depth != 7 || opts.ManifestDepth != 3 {
			t.Errorf("clone-tuning fields not inherited: %+v", opts)
		}
		if !opts.PartialClone || opts.CloneFilter != "blob:limit=1m" || !opts.Dissociate || !opts.GitLFS {
			t.Errorf("clone flags not inherited: %+v", opts)
		}
		if opts.CurrentBranch || !opts.Tags || !opts.Submodules {
			t.Errorf("boolean fields not inherited: %+v", opts)
		}
		// 显式 URL 保留
		if opts.ManifestURL != "https://new.example/manifest.git" {
			t.Errorf("ManifestURL = %q, want explicitly changed value", opts.ManifestURL)
		}
	})

	t.Run("explicit flags override old config", func(t *testing.T) {
		opts := &InitOptions{
			ManifestURL:   "https://new.example/manifest.git",
			ManifestName:  "other.xml",
			Groups:        "default",
			Mirror:        false,
			Reference:     "",
			Depth:         0,
			CurrentBranch: true,
			CloneBundle:   true,
			changedFlags: map[string]bool{
				"manifest-url": true, "manifest-name": true, "groups": true,
				"mirror": true, "reference": true, "depth": true,
				"current-branch": true,
			},
		}
		applyRepoConfigInheritance(opts, old)
		if opts.ManifestName != "other.xml" || opts.Groups != "default" || opts.Mirror {
			t.Errorf("explicit flags were overwritten by inheritance: %+v", opts)
		}
		if opts.Reference != "" || opts.Depth != 0 || !opts.CurrentBranch {
			t.Errorf("explicit zero values were overwritten: %+v", opts)
		}
	})

	t.Run("negative flag counts as explicit", func(t *testing.T) {
		opts := &InitOptions{changedFlags: map[string]bool{"no-current-branch": true}, CurrentBranch: false}
		applyRepoConfigInheritance(opts, old)
		if opts.CurrentBranch {
			t.Errorf("--no-current-branch 显式指定时不应继承旧的 true")
		}
	})
}

// TestIsCommitSHA 验证只接受 40/64 位小写十六进制完整 SHA。
func TestIsCommitSHA(t *testing.T) {
	tests := []struct {
		in   string
		want bool
	}{
		{"0123456789abcdef0123456789abcdef01234567", true},
		{"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", true},
		{"deadbeef", false},
		{"0123456789ABCDEF0123456789ABCDEF01234567", false},
		{"0123456789abcdef0123456789abcdef0123456", false},
		{"g123456789abcdef0123456789abcdef01234567", false},
		{"", false},
	}
	for _, tt := range tests {
		if got := isCommitSHA(tt.in); got != tt.want {
			t.Errorf("isCommitSHA(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

// TestNormalizeHeadManifestBranch 验证 -b HEAD 归一化为 origin/HEAD 实际分支，
// 且失败时保持原值（交由 clone 阶段处理）。
func TestNormalizeHeadManifestBranch(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0755); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}
	runner := &mockGitRunner{responses: map[string]mockGitResp{
		"symbolic-ref --short refs/remotes/origin/HEAD": {out: []byte("origin/trunk\n")},
	}}
	if got := normalizeHeadManifestBranch(runner, dir, "HEAD"); got != "trunk" {
		t.Errorf("normalizeHeadManifestBranch(HEAD) = %q, want trunk", got)
	}
	if got := normalizeHeadManifestBranch(runner, dir, "main"); got != "main" {
		t.Errorf("non-HEAD branch changed: %q", got)
	}
	// 仓库不存在时原样返回，不调用 git
	missing := filepath.Join(dir, "missing")
	if got := normalizeHeadManifestBranch(runner, missing, "HEAD"); got != "HEAD" {
		t.Errorf("missing repo should keep HEAD, got %q", got)
	}
}

// TestDiffInitConfigIgnoresInheritedFields 验证启用 changedFlags 后，未显式
// 指定的字段即使值不同也不触发覆盖确认。
func TestDiffInitConfigIgnoresInheritedFields(t *testing.T) {
	existing := RepoConfig{
		ManifestURL:  "https://old.example/m.git",
		ManifestName: "cix.xml",
		Groups:       "default,platform-linux",
		Platform:     "auto",
	}
	opts := &InitOptions{
		ManifestURL:  "https://old.example/m.git",
		ManifestName: "default.xml", // 未显式指定（仅默认值）
		Groups:       "default",
		Platform:     "auto",
		changedFlags: map[string]bool{"manifest-url": true},
	}
	if diffs := diffInitConfig(existing, opts); len(diffs) != 0 {
		t.Fatalf("diffInitConfig = %+v, want no diffs (only explicit same URL)", diffs)
	}
	opts.ManifestName = "other.xml"
	opts.changedFlags["manifest-name"] = true
	if diffs := diffInitConfig(existing, opts); len(diffs) != 1 {
		t.Fatalf("diffInitConfig = %+v, want 1 explicit manifest-name diff", diffs)
	}
}

// TestWriteManifestXMLReplacesSymlink 验证旧 symlink 被替换而不是跟随写入
// 清单仓库，且目标文件内容不被修改。
func TestWriteManifestXMLReplacesSymlink(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".repo"), 0755); err != nil {
		t.Fatalf("mkdir .repo: %v", err)
	}
	prev, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	defer func() { _ = os.Chdir(prev) }()

	target := filepath.Join(dir, ".repo", "manifests", "default.xml")
	if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
		t.Fatalf("mkdir manifests: %v", err)
	}
	const original = "<manifest/>\n"
	if err := os.WriteFile(target, []byte(original), 0644); err != nil {
		t.Fatalf("write target: %v", err)
	}
	link := filepath.Join(dir, ".repo", "manifest.xml")
	if err := os.Symlink(filepath.Join("manifests", "default.xml"), link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	m := &manifest.Manifest{
		Remotes:     []manifest.Remote{},
		Projects:    []manifest.Project{},
		CustomAttrs: map[string]string{},
	}
	if err := writeManifestXML(m, logger.NewDefaultLogger()); err != nil {
		t.Fatalf("writeManifestXML: %v", err)
	}
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("manifest.xml 仍是 symlink（err=%v），应被替换为普通文件", err)
	}
	if data, err := os.ReadFile(target); err != nil || string(data) != original {
		t.Fatalf("清单仓库目标文件被修改: %q (err=%v)", data, err)
	}
}

// TestDiscardInheritedModeConflicts 验证 re-init 时显式选择的检出模式会丢弃
// 与之冲突的继承值（而不是让旧配置把新选择判成非法组合），显式给出的
// 冲突项仍交由 validateOptions 报错。
func TestDiscardInheritedModeConflicts(t *testing.T) {
	tests := []struct {
		name      string
		old       RepoConfig
		opts      *InitOptions
		wantErr   bool
		checkVals func(t *testing.T, o *InitOptions)
	}{
		{
			name: "explicit partial-clone drops inherited mirror",
			old:  RepoConfig{Mirror: true, ManifestURL: "https://old.example/m.git"},
			opts: &InitOptions{ManifestURL: "https://old.example/m.git", PartialClone: true,
				changedFlags: map[string]bool{"partial-clone": true}},
			checkVals: func(t *testing.T, o *InitOptions) {
				if o.Mirror {
					t.Errorf("Mirror = true, want inherited mirror discarded")
				}
			},
		},
		{
			name: "explicit mirror drops inherited partial-clone",
			old:  RepoConfig{PartialClone: true, CloneFilter: "blob:none", ManifestURL: "https://old.example/m.git"},
			opts: &InitOptions{ManifestURL: "https://old.example/m.git", Mirror: true,
				changedFlags: map[string]bool{"mirror": true}},
			checkVals: func(t *testing.T, o *InitOptions) {
				if o.PartialClone {
					t.Errorf("PartialClone = true, want inherited partial-clone discarded")
				}
			},
		},
		{
			name: "explicit archive drops inherited mirror and use-superproject",
			old:  RepoConfig{Mirror: true, UseSuperproject: true, ManifestURL: "https://old.example/m.git"},
			opts: &InitOptions{ManifestURL: "https://old.example/m.git", Archive: true, CloneBundle: true,
				changedFlags: map[string]bool{"archive": true}},
			checkVals: func(t *testing.T, o *InitOptions) {
				if o.Mirror || o.UseSuperproject {
					t.Errorf("Mirror/UseSuperproject = %v/%v, want discarded", o.Mirror, o.UseSuperproject)
				}
			},
		},
		{
			name: "explicit use-local-gitdirs drops inherited mirror/archive/worktree",
			old:  RepoConfig{Mirror: true, Archive: true, Worktree: true, ManifestURL: "https://old.example/m.git"},
			opts: &InitOptions{ManifestURL: "https://old.example/m.git", UseLocalGitdirs: true,
				changedFlags: map[string]bool{"use-local-gitdirs": true}},
			checkVals: func(t *testing.T, o *InitOptions) {
				if o.Mirror || o.Archive || o.Worktree {
					t.Errorf("Mirror/Archive/Worktree = %v/%v/%v, want discarded", o.Mirror, o.Archive, o.Worktree)
				}
			},
		},
		{
			name: "explicit worktree drops inherited mirror",
			old:  RepoConfig{Mirror: true, ManifestURL: "https://old.example/m.git"},
			opts: &InitOptions{ManifestURL: "https://old.example/m.git", Worktree: true,
				changedFlags: map[string]bool{"worktree": true}},
			checkVals: func(t *testing.T, o *InitOptions) {
				if o.Mirror {
					t.Errorf("Mirror = true, want discarded")
				}
			},
		},
		{
			name: "explicit standalone drops inherited branch/name",
			old: RepoConfig{ManifestBranch: "main", ManifestName: "cix.xml",
				ManifestURL: "https://old.example/m.git"},
			opts: &InitOptions{ManifestURL: "https://old.example/m.git", ManifestName: "default.xml",
				StandaloneManifest: true, changedFlags: map[string]bool{"standalone-manifest": true}},
			checkVals: func(t *testing.T, o *InitOptions) {
				if o.ManifestBranch != "" || o.ManifestName != "default.xml" {
					t.Errorf("ManifestBranch/Name = %q/%q, want cleared/default", o.ManifestBranch, o.ManifestName)
				}
			},
		},
		{
			name: "both explicit conflict still errors",
			old:  RepoConfig{ManifestURL: "https://old.example/m.git"},
			opts: &InitOptions{ManifestURL: "https://old.example/m.git", Mirror: true, Archive: true,
				changedFlags: map[string]bool{"mirror": true, "archive": true}},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			applyRepoConfigInheritance(tt.opts, &tt.old)
			discardInheritedModeConflicts(tt.opts)
			err := validateOptions(tt.opts)
			if tt.wantErr {
				if err == nil {
					t.Fatal("validateOptions expected conflict error")
				}
				return
			}
			if err != nil {
				t.Fatalf("validateOptions after discarding inherited conflicts: %v", err)
			}
			if tt.checkVals != nil {
				tt.checkVals(t, tt.opts)
			}
		})
	}
}

// TestResolveEffectiveGroups 验证 platform-* 组只在首次初始化或显式 -p/-g
// 时重建；普通 re-init 原样继承 Groups（保留用户手写的 platform 组）。
func TestResolveEffectiveGroups(t *testing.T) {
	hostGroup := "platform-" + runtime.GOOS
	tests := []struct {
		name        string
		initialized bool
		opts        *InitOptions
		want        string
	}{
		{
			name:        "normal re-init inherits groups as-is",
			initialized: true,
			opts:        &InitOptions{Groups: "default,platform-custom,platform-windows", Platform: "auto", changedFlags: map[string]bool{}},
			want:        "default,platform-custom,platform-windows",
		},
		{
			name:        "fresh init merges platform",
			initialized: false,
			opts:        &InitOptions{Groups: "default", Platform: "auto", changedFlags: map[string]bool{}},
			want:        "default," + hostGroup,
		},
		{
			name:        "explicit platform rebuilds stale groups",
			initialized: true,
			opts:        &InitOptions{Groups: "default,platform-windows", Platform: "linux", changedFlags: map[string]bool{"platform": true}},
			want:        "default,platform-linux",
		},
		{
			name:        "explicit groups preserves handwritten platform groups",
			initialized: true,
			opts:        &InitOptions{Groups: "default,platform-custom", Platform: "none", changedFlags: map[string]bool{"groups": true}},
			want:        "default,platform-custom",
		},
		{
			name:        "normal re-init with empty groups normalizes to default",
			initialized: true,
			opts:        &InitOptions{Groups: "", Platform: "auto", changedFlags: map[string]bool{}},
			want:        "default",
		},
		{
			name:        "fresh init with empty groups normalizes to default",
			initialized: false,
			opts:        &InitOptions{Groups: "", Platform: "none", changedFlags: map[string]bool{}},
			want:        "default",
		},
		{
			name:        "explicit empty groups normalizes to default",
			initialized: true,
			opts:        &InitOptions{Groups: "", Platform: "none", changedFlags: map[string]bool{"groups": true}},
			want:        "default",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveEffectiveGroups(tt.opts, tt.initialized)
			if err != nil {
				t.Fatalf("resolveEffectiveGroups: %v", err)
			}
			if got != tt.want {
				t.Errorf("resolveEffectiveGroups = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestDeriveCloneBundleAfterInheritance 验证继承后重新应用 partial-clone ⇒
// clone-bundle=false 推导，显式 clone-bundle 选择不被覆盖。
func TestDeriveCloneBundleAfterInheritance(t *testing.T) {
	old := RepoConfig{PartialClone: true, CloneBundle: true, ManifestURL: "https://old.example/m.git"}
	opts := &InitOptions{ManifestURL: "https://old.example/m.git", CloneBundle: true, changedFlags: map[string]bool{}}
	applyRepoConfigInheritance(opts, &old)
	deriveCloneBundle(opts)
	if opts.CloneBundle {
		t.Error("CloneBundle = true, want false after inherited partial-clone")
	}

	opts = &InitOptions{ManifestURL: "https://old.example/m.git", CloneBundle: true,
		changedFlags: map[string]bool{"clone-bundle": true}}
	applyRepoConfigInheritance(opts, &old)
	deriveCloneBundle(opts)
	if !opts.CloneBundle {
		t.Error("explicit --clone-bundle must be preserved")
	}
}

// TestInheritEmptyGroups 验证 groups 区分“未指定”（原样继承，含空串）与
// “显式空串”（保持空串，不回落到默认值）。
func TestInheritEmptyGroups(t *testing.T) {
	opts := &InitOptions{Groups: "default", changedFlags: map[string]bool{}}
	applyRepoConfigInheritance(opts, &RepoConfig{Groups: ""})
	if opts.Groups != "" {
		t.Errorf("Groups = %q, want inherited empty string", opts.Groups)
	}

	opts = &InitOptions{Groups: "", changedFlags: map[string]bool{"groups": true}}
	applyRepoConfigInheritance(opts, &RepoConfig{Groups: "default,cix"})
	if opts.Groups != "" {
		t.Errorf("Groups = %q, want explicit empty string preserved", opts.Groups)
	}
}

// TestResolveRemoteDefaultBranch 验证 -b HEAD 使用 ls-remote --symref 直接
// 询问远端，换 URL 时不依赖旧的 refs/remotes/origin/HEAD。
func TestResolveRemoteDefaultBranch(t *testing.T) {
	runner := &mockGitRunner{responses: map[string]mockGitResp{
		"ls-remote --symref https://new.example/m.git HEAD": {
			out: []byte("ref: refs/heads/trunk\tHEAD\n0123456789abcdef0123456789abcdef01234567\tHEAD\n"),
		},
	}}
	got, err := resolveRemoteDefaultBranch(runner, ".repo/manifests", "https://new.example/m.git")
	if err != nil || got != "trunk" {
		t.Fatalf("resolveRemoteDefaultBranch = %q, %v; want trunk", got, err)
	}
}
