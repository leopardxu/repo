package commands

import (
	"path/filepath"
	"testing"

	"github.com/leopardxu/repo-go/internal/config"
	"github.com/leopardxu/repo-go/internal/manifest"
	"github.com/leopardxu/repo-go/internal/project"
)

// TestListProjectOutput 验证 -p/-n/-u/--full-name 及组合的输出（对齐上游 list.py）。
func TestListProjectOutput(t *testing.T) {
	proj := &project.Project{Name: "platform/foo", Path: "src/foo", RemoteName: "origin"}

	tests := []struct {
		name string
		opts ListOptions
		want string
	}{
		{
			name: "默认输出 path : name",
			opts: ListOptions{},
			want: "src/foo : platform/foo",
		},
		{
			name: "仅 -p 输出 path",
			opts: ListOptions{Path: true},
			want: "src/foo",
		},
		{
			name: "仅 -n 输出 name",
			opts: ListOptions{Name: true},
			want: "platform/foo",
		},
		{
			name: "-p 与 -n 同用仍输出 path : name",
			opts: ListOptions{Path: true, Name: true},
			want: "src/foo : platform/foo",
		},
		{
			name: "-u 输出 URL",
			opts: ListOptions{URL: true},
			want: "https://example.com/platform/foo",
		},
		{
			name: "--full-name 输出 name : path",
			opts: ListOptions{FullName: true},
			want: "platform/foo : src/foo",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := listProjectOutput(proj, "src/foo", "https://example.com/platform/foo", &tt.opts)
			if got != tt.want {
				t.Errorf("listProjectOutput() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestListResolveProjectURL 验证 list --url 使用的 Manager.ResolveProjectURL：
// 基址无条件拼接项目名，绝对与相对 fetch 都解析正确。
func TestListResolveProjectURL(t *testing.T) {
	newManager := func(fetch string) *project.Manager {
		m := &manifest.Manifest{
			Default: manifest.Default{Remote: "origin", Revision: "main"},
			Remotes: []manifest.Remote{{Name: "origin", Fetch: fetch}},
			Projects: []manifest.Project{
				{Name: "platform/foo", Path: "foo", Remote: "origin", Revision: "main"},
			},
			Topdir: t.TempDir(),
		}
		return project.NewManagerFromManifest(m, &config.Config{ManifestURL: "https://example.com/manifest"})
	}

	tests := []struct {
		name  string
		fetch string
		want  string
	}{
		{
			name:  "绝对基址无条件拼项目名",
			fetch: "https://example.com/base",
			want:  "https://example.com/base/platform/foo",
		},
		{
			name:  "相对 fetch 基于 manifest URL 解析",
			fetch: "../repos",
			want:  "https://example.com/repos/platform/foo",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mgr := newManager(tt.fetch)
			if len(mgr.Projects) != 1 {
				t.Fatalf("项目数量 = %d, want 1", len(mgr.Projects))
			}
			if got := mgr.ResolveProjectURL(mgr.Projects[0]); got != tt.want {
				t.Errorf("ResolveProjectURL() = %q, want %q", got, tt.want)
			}
		})
	}

	// 无 FetchBase 的直接构造项目保留完整 RemoteURL。
	direct := &project.Project{Name: "bar", RemoteURL: "https://example.com/bar"}
	if got := newManager("https://example.com/base").ResolveProjectURL(direct); got != direct.RemoteURL {
		t.Errorf("无 FetchBase 时 = %q, want %q", got, direct.RemoteURL)
	}
}

// TestRunListURLStdout 验证 list -u 走 ResolveProjectURL 输出基址+项目名。
func TestRunListURLStdout(t *testing.T) {
	repoRoot := readCmdTestRepoRoot(t, `  <project name="foo" path="foo" revision="main" />`)
	mustMkdir(t, filepath.Join(repoRoot, "foo"))

	readCmdWithCwd(t, repoRoot, func() {
		stdout, _, err := readCmdRunCaptured(t, func() error {
			return runList(&ListOptions{URL: true}, nil)
		})
		if err != nil {
			t.Fatalf("runList failed: %v", err)
		}
		if want := "https://example.com/foo\n"; stdout != want {
			t.Errorf("runList --url stdout = %q, want %q", stdout, want)
		}
	})
}

// TestListProjectAbsPath 验证项目绝对路径 = TopDir + 清单 Path。
func TestListProjectAbsPath(t *testing.T) {
	tests := []struct {
		name        string
		topDir      string
		projectPath string
		want        string
	}{
		{
			name:        "相对路径拼接 TopDir",
			topDir:      "/repo",
			projectPath: "foo/bar",
			want:        "/repo/foo/bar",
		},
		{
			name:        "已经是绝对路径",
			topDir:      "/repo",
			projectPath: "/other/x",
			want:        "/other/x",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := listProjectAbsPath(tt.topDir, tt.projectPath); got != tt.want {
				t.Errorf("listProjectAbsPath() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestListRelativePath 验证 --relative-to 使用绝对路径时结果正确。
func TestListRelativePath(t *testing.T) {
	tests := []struct {
		name        string
		topDir      string
		relativeTo  string
		projectPath string
		want        string
	}{
		{
			name:        "绝对 relative-to 指向顶层",
			topDir:      "/repo",
			relativeTo:  "/repo",
			projectPath: "foo/bar",
			want:        "foo/bar",
		},
		{
			name:        "绝对 relative-to 指向子目录",
			topDir:      "/repo",
			relativeTo:  "/repo/foo",
			projectPath: "foo/bar",
			want:        "bar",
		},
		{
			name:        "项目绝对路径已经解析",
			topDir:      "/repo",
			relativeTo:  "/other",
			projectPath: "/other/x",
			want:        "x",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := listRelativePath(tt.topDir, tt.relativeTo, tt.projectPath)
			if err != nil {
				t.Fatalf("listRelativePath() error = %v", err)
			}
			if filepath.Clean(got) != filepath.Clean(tt.want) {
				t.Errorf("listRelativePath() = %q, want %q", got, tt.want)
			}
		})
	}
}
