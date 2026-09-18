package commands

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
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
			wantCmds: []string{
				"fetch --all",
				"rev-parse --verify --quiet refs/remotes/origin/main",
				"rev-parse --verify --quiet refs/heads/main",
				"checkout main",
				"reset --hard refs/remotes/origin/main",
			},
		},
		{
			name: "branch only on remote: fetch by refspec then create",
			cfg:  &RepoConfig{ManifestBranch: "feature", ManifestURL: "https://example.com/manifest.git"},
			responses: map[string]mockGitResp{
				"rev-parse --verify --quiet refs/remotes/origin/feature": {err: errors.New("exit 1")},
				"rev-parse --verify --quiet refs/heads/feature":          {err: errors.New("exit 1")},
			},
			wantCmds: []string{
				"fetch --all",
				"rev-parse --verify --quiet refs/remotes/origin/feature",
				"fetch origin refs/heads/feature:refs/remotes/origin/feature",
				"rev-parse --verify --quiet refs/heads/feature",
				"checkout -b feature refs/remotes/origin/feature",
			},
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
			wantCmds: []string{
				"fetch --all",
				"rev-parse --verify --quiet refs/remotes/origin/nosuchbranch",
				"fetch origin refs/heads/nosuchbranch:refs/remotes/origin/nosuchbranch",
			},
		},
		{
			name:     "mirror mode: fetch only",
			cfg:      &RepoConfig{ManifestBranch: "main", Mirror: true},
			wantCmds: []string{"fetch --all"},
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
				"rev-parse --verify --quiet refs/remotes/origin/main": {out: []byte("abc123\n")},
				"rev-parse --verify --quiet refs/heads/main":          {out: []byte("abc123\n")},
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
