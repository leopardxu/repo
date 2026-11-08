package commands

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/leopardxu/repo-go/internal/config"
	"github.com/leopardxu/repo-go/internal/git"
	"github.com/leopardxu/repo-go/internal/manifest"
	"github.com/leopardxu/repo-go/internal/project"
)

// TestBuildDiffArgs 验证 diff 基础比较对象为 HEAD，--cached 比较索引与 HEAD。
func TestBuildDiffArgs(t *testing.T) {
	tests := []struct {
		name string
		opts *DiffOptions
		want []string
	}{
		{
			name: "默认工作树+索引对 HEAD",
			opts: &DiffOptions{},
			want: []string{"diff", "HEAD", "--"},
		},
		{
			name: "cached 索引对 HEAD",
			opts: &DiffOptions{Cached: true},
			want: []string{"diff", "--cached", "HEAD", "--"},
		},
		{
			name: "格式选项透传",
			opts: &DiffOptions{Unified: 3, NameOnly: true, NameStatus: true, Stat: true},
			want: []string{"diff", "--unified=3", "--name-only", "--name-status", "--stat", "HEAD", "--"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := buildDiffArgs(tt.opts); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("buildDiffArgs() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestDiffStagedChangesVisible 验证 git add 之后 repo diff 仍能看到改动
// （基础命令比较 HEAD，包含索引中的暂存内容）。
func TestDiffStagedChangesVisible(t *testing.T) {
	dir := t.TempDir()
	runner := git.NewRunner()
	run := func(args ...string) {
		t.Helper()
		if _, err := runner.RunInDir(dir, args...); err != nil {
			t.Fatalf("git %v 失败: %v", args, err)
		}
	}

	run("init", "-q")
	run("config", "user.email", "test@example.com")
	run("config", "user.name", "Test")

	file := filepath.Join(dir, "version.txt")
	if err := os.WriteFile(file, []byte("first\n"), 0644); err != nil {
		t.Fatalf("写入初始文件失败: %v", err)
	}
	run("add", ".")
	run("commit", "-q", "-m", "initial")

	if err := os.WriteFile(file, []byte("first\nsecond\n"), 0644); err != nil {
		t.Fatalf("修改文件失败: %v", err)
	}
	run("add", "version.txt")

	for _, opts := range []*DiffOptions{{}, {Cached: true}} {
		t.Run(map[bool]string{true: "cached", false: "worktree"}[opts.Cached], func(t *testing.T) {
			out, err := runner.RunInDir(dir, buildDiffArgs(opts)...)
			if err != nil {
				t.Fatalf("git diff 失败 (cached=%v): %v", opts.Cached, err)
			}
			if !strings.Contains(string(out), "+second") {
				t.Errorf("cached=%v 时暂存改动不可见，输出:\n%s", opts.Cached, out)
			}
		})
	}
}

// TestReadCmdProjectMissing 验证未检出项目判定。
func TestReadCmdProjectMissing(t *testing.T) {
	root := t.TempDir()
	mustMkdir(t, filepath.Join(root, "present"))

	tests := []struct {
		name string
		proj *project.Project
		want bool
	}{
		{"nil 项目", nil, true},
		{"空 worktree", &project.Project{Name: "a"}, true},
		{"已检出", &project.Project{Name: "a", Worktree: filepath.Join(root, "present")}, false},
		{"目录缺失", &project.Project{Name: "a", Worktree: filepath.Join(root, "absent")}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := readCmdProjectMissing(tt.proj); got != tt.want {
				t.Errorf("readCmdProjectMissing() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestReadCmdEffectiveGroups 验证显式 --groups 优先，空配置回退 EffectiveGroups。
func TestReadCmdEffectiveGroups(t *testing.T) {
	tests := []struct {
		name     string
		explicit string
		cfg      *config.Config
		want     []string
	}{
		{"显式组优先", "cix,all", &config.Config{Groups: "default"}, []string{"cix", "all"}},
		{"回退配置组", "", &config.Config{Groups: "default,cix"}, []string{"default", "cix"}},
		{"空组回退 default", "", &config.Config{}, []string{"default"}},
		{"无配置", "", nil, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := readCmdEffectiveGroups(tt.explicit, tt.cfg); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("readCmdEffectiveGroups() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestReadCmdRevisionCandidates 验证 revision 解析候选顺序：
// SHA/refs/tags 原样；分支名与 refs/heads/X 先远端跟踪引用再本地回退。
func TestReadCmdRevisionCandidates(t *testing.T) {
	tests := []struct {
		name         string
		revision     string
		remote       string
		includeLocal bool
		want         []string
	}{
		{
			name:         "SHA 原样",
			revision:     "0123456789abcdef0123456789abcdef01234567",
			remote:       "origin",
			includeLocal: true,
			want:         []string{"0123456789abcdef0123456789abcdef01234567"},
		},
		{
			name:         "refs/tags 原样",
			revision:     "refs/tags/v1.0",
			remote:       "origin",
			includeLocal: true,
			want:         []string{"refs/tags/v1.0"},
		},
		{
			name:         "裸 tag 名按分支处理",
			revision:     "v1.0",
			remote:       "origin",
			includeLocal: true,
			want:         []string{"refs/remotes/origin/v1.0", "refs/heads/v1.0"},
		},
		{
			name:         "分支名先远端再本地",
			revision:     "dev",
			remote:       "aosp",
			includeLocal: true,
			want:         []string{"refs/remotes/aosp/dev", "refs/heads/dev"},
		},
		{
			name:         "refs/heads 先远端再本地",
			revision:     "refs/heads/dev",
			remote:       "origin",
			includeLocal: true,
			want:         []string{"refs/remotes/origin/dev", "refs/heads/dev"},
		},
		{
			name:         "includeLocal=false 只有远端",
			revision:     "dev",
			remote:       "origin",
			includeLocal: false,
			want:         []string{"refs/remotes/origin/dev"},
		},
		{
			name:         "remote 为空回退 origin",
			revision:     "main",
			remote:       "",
			includeLocal: true,
			want:         []string{"refs/remotes/origin/main", "refs/heads/main"},
		},
		{
			name:         "其它完整引用原样",
			revision:     "refs/changes/01/123/1",
			remote:       "origin",
			includeLocal: true,
			want:         []string{"refs/changes/01/123/1"},
		},
		{
			name:         "空 revision 无候选",
			revision:     "  ",
			remote:       "origin",
			includeLocal: true,
			want:         []string{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := readCmdRevisionCandidates(tt.revision, tt.remote, tt.includeLocal)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("readCmdRevisionCandidates() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestReadCmdRemoteRefName 验证 remote name/alias 到实际 git remote 名的映射：
// 声明 alias 的 remote 以 alias 作为本地 remote 名。
func TestReadCmdRemoteRefName(t *testing.T) {
	remotes := []manifest.Remote{
		{Name: "upstream", Alias: "up", Fetch: "../up"},
		{Name: "origin", Fetch: "../origin"},
	}
	tests := []struct {
		name    string
		input   string
		remotes []manifest.Remote
		want    string
	}{
		{"name 映射为 alias", "upstream", remotes, "up"},
		{"alias 原样", "up", remotes, "up"},
		{"无 alias 用 name", "origin", remotes, "origin"},
		{"未声明原样", "custom", remotes, "custom"},
		{"空值", "  ", remotes, ""},
		{"无 remotes 原样", "upstream", nil, "upstream"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := readCmdRemoteRefName(tt.input, tt.remotes); got != tt.want {
				t.Errorf("readCmdRemoteRefName(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}
