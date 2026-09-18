package project

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/leopardxu/repo-go/internal/config"
	"github.com/leopardxu/repo-go/internal/manifest"
)

func TestNewManagerFromManifestWithSyncS(t *testing.T) {
	m := &manifest.Manifest{
		Remotes: []manifest.Remote{
			{Name: "origin", Fetch: "https://example.com/"},
		},
		Default: manifest.Default{Remote: "origin", Revision: "main"},
		Projects: []manifest.Project{
			{Name: "a", Path: "a", SyncS: true, CloneDepth: 5, DestBranch: "dev", Upstream: "main"},
			{Name: "b", Path: "b"},
		},
	}
	m.Default.CustomAttrs = make(map[string]string)

	cfg := &config.Config{}
	mgr := NewManagerFromManifest(m, cfg)

	if len(mgr.Projects) != 2 {
		t.Fatalf("Projects = %d, want 2", len(mgr.Projects))
	}

	// Verify SyncS, CloneDepth, DestBranch, Upstream are mapped
	p0 := mgr.Projects[0]
	if !p0.SyncS {
		t.Error("Project[0].SyncS should be true")
	}
	if p0.CloneDepth != 5 {
		t.Errorf("Project[0].CloneDepth = %d, want 5", p0.CloneDepth)
	}
	if p0.DestBranch != "dev" {
		t.Errorf("Project[0].DestBranch = %q, want dev", p0.DestBranch)
	}
	if p0.Upstream != "main" {
		t.Errorf("Project[0].Upstream = %q, want main", p0.Upstream)
	}

	// Project without attributes should have zero values
	p1 := mgr.Projects[1]
	if p1.SyncS {
		t.Error("Project[1].SyncS should be false")
	}
	if p1.CloneDepth != 0 {
		t.Errorf("Project[1].CloneDepth = %d, want 0", p1.CloneDepth)
	}
}

func TestManagerGetProjectsInGroupFromManager(t *testing.T) {
	mgr := NewManager("url", "default.xml", ".repo", nil)
	p1 := NewProject("a", "a", "origin", "url", "main", []string{"linux"}, nil)
	p2 := NewProject("b", "b", "origin", "url", "main", []string{"arm"}, nil)
	p3 := NewProject("c", "c", "origin", "url", "main", []string{"linux", "arm"}, nil)
	mgr.AddProject(p1)
	mgr.AddProject(p2)
	mgr.AddProject(p3)

	linuxProjects := mgr.GetProjectsInGroup("linux")
	if len(linuxProjects) != 2 {
		t.Errorf("GetProjectsInGroup(linux) = %d, want 2", len(linuxProjects))
	}

	armProjects := mgr.GetProjectsInGroup("arm")
	if len(armProjects) != 2 {
		t.Errorf("GetProjectsInGroup(arm) = %d, want 2", len(armProjects))
	}
}

func TestManagerForEachEmpty(t *testing.T) {
	mgr := NewManager("url", "default.xml", ".repo", nil)
	err := mgr.ForEach(func(_ *Project) error {
		t.Error("ForEach should not call fn on empty manager")
		return nil
	})
	if err != nil {
		t.Errorf("ForEach on empty manager should not error: %v", err)
	}
}

// TestProjectURLFromFetch 验证项目 URL 拼接对齐上游 repo
// （manifest_xml.py 的 ToRemoteSpec：url = fetchUrl.rstrip('/') + '/' + name）。
// 相对 fetch（..、../、./）与空 fetch 原样返回，由同步引擎基于 manifest
// 服务器 URL 解析后再拼接项目名。
func TestProjectURLFromFetch(t *testing.T) {
	tests := []struct {
		name        string
		fetch       string
		projectName string
		want        string
	}{
		{
			// 实际踩坑场景：无路径的 ssh fetch 直接传给 git clone 会报
			// "未指定路径，执行 'git help pull' 查看有效的 url 语法"
			name:        "ssh fetch without path",
			fetch:       "ssh://git@gitmirror.cixcomputing.com:29418",
			projectName: "cix_opensource/release/edk2-non-osi",
			want:        "ssh://git@gitmirror.cixcomputing.com:29418/cix_opensource/release/edk2-non-osi",
		},
		{
			name:        "https fetch with trailing slash",
			fetch:       "https://example.com/",
			projectName: "platform/build",
			want:        "https://example.com/platform/build",
		},
		{
			name:        "https fetch without trailing slash",
			fetch:       "https://example.com",
			projectName: "platform/build",
			want:        "https://example.com/platform/build",
		},
		{
			name:        "scp style fetch",
			fetch:       "git@host:base",
			projectName: "a",
			want:        "git@host:base/a",
		},
		{
			name:        "relative parent fetch kept as-is",
			fetch:       "..",
			projectName: "a",
			want:        "..",
		},
		{
			name:        "relative parent path fetch kept as-is",
			fetch:       "../foo",
			projectName: "a",
			want:        "../foo",
		},
		{
			name:        "relative current dir fetch kept as-is",
			fetch:       "./foo",
			projectName: "a",
			want:        "./foo",
		},
		{
			name:        "empty fetch kept as-is",
			fetch:       "",
			projectName: "a",
			want:        "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := projectURLFromFetch(tt.fetch, tt.projectName); got != tt.want {
				t.Errorf("projectURLFromFetch(%q, %q) = %q, want %q",
					tt.fetch, tt.projectName, got, tt.want)
			}
		})
	}
}

// TestNewManagerFromManifestJoinsProjectURL 是本次修复的核心回归测试：
// manager 构造项目时必须把 remote fetch 基地址拼接上项目名，
// 否则无路径 fetch（如 ssh://host:29418）会导致 git clone 报 "未指定路径"。
func TestNewManagerFromManifestJoinsProjectURL(t *testing.T) {
	m := &manifest.Manifest{
		RepoDir: "/tmp/repo",
		Remotes: []manifest.Remote{
			{Name: "cix", Fetch: "ssh://git@gitmirror.cixcomputing.com:29418"},
			{Name: "rel", Fetch: "../"},
		},
		Default: manifest.Default{Remote: "cix", Revision: "main"},
		Projects: []manifest.Project{
			// 默认 remote 的项目
			{Name: "cix_opensource/release/edk2-non-osi", Path: "bsp/uefi_release/edk2-non-osi"},
			// 显式指定 remote 的项目
			{Name: "cix_opensource/release/edk2", Remote: "cix", Path: "bsp/uefi_release/edk2"},
			// 相对 fetch 的项目：保持相对形式，由引擎解析
			{Name: "rel/proj", Remote: "rel", Path: "rel_proj"},
		},
	}
	m.Default.CustomAttrs = make(map[string]string)

	mgr := NewManagerFromManifest(m, nil)

	byName := make(map[string]*Project, len(mgr.Projects))
	for _, p := range mgr.Projects {
		byName[p.Name] = p
	}

	p1 := byName["cix_opensource/release/edk2-non-osi"]
	if p1 == nil {
		t.Fatal("project cix_opensource/release/edk2-non-osi not found")
	}
	if want := "ssh://git@gitmirror.cixcomputing.com:29418/cix_opensource/release/edk2-non-osi"; p1.RemoteURL != want {
		t.Errorf("default-remote RemoteURL = %q, want %q", p1.RemoteURL, want)
	}

	p2 := byName["cix_opensource/release/edk2"]
	if p2 == nil {
		t.Fatal("project cix_opensource/release/edk2 not found")
	}
	if want := "ssh://git@gitmirror.cixcomputing.com:29418/cix_opensource/release/edk2"; p2.RemoteURL != want {
		t.Errorf("explicit-remote RemoteURL = %q, want %q", p2.RemoteURL, want)
	}

	p3 := byName["rel/proj"]
	if p3 == nil {
		t.Fatal("project rel/proj not found")
	}
	if p3.RemoteURL != "../" {
		t.Errorf("relative fetch RemoteURL = %q, want %q", p3.RemoteURL, "../")
	}
}

func TestManagerForEachWithJobsEmpty(t *testing.T) {
	mgr := NewManager("url", "default.xml", ".repo", nil)
	err := mgr.ForEachWithJobs(func(_ *Project) error {
		t.Error("ForEachWithJobs should not call fn on empty manager")
		return nil
	}, 4)
	if err != nil {
		t.Errorf("ForEachWithJobs on empty manager should not error: %v", err)
	}
}

func TestManagerSyncProjectsEmpty(t *testing.T) {
	mgr := NewManager("url", "default.xml", ".repo", nil)
	err := mgr.SyncProjects(SyncOptions{Jobs: 2}, 2)
	if err != nil {
		t.Errorf("SyncProjects on empty manager should not error: %v", err)
	}
}

func TestManagerForEachProjectEmpty(t *testing.T) {
	mgr := NewManager("url", "default.xml", ".repo", nil)
	err := mgr.ForEachProject(func(_ *Project) error {
		t.Error("ForEachProject should not call fn on empty manager")
		return nil
	}, 4)
	if err != nil {
		t.Errorf("ForEachProject on empty manager should not error: %v", err)
	}
}

func TestManagerResolveRemoteURL(t *testing.T) {
	tests := []struct {
		name        string
		manifestURL string
		remoteURL   string
		want        string
	}{
		{name: "相对 fetch 基于 manifest URL 解析", manifestURL: "ssh://host/linux_repo/cix-manifest", remoteURL: "../platform/foo", want: "ssh://host/platform/foo"},
		{name: "scp 风式透传", manifestURL: "ssh://host/manifest", remoteURL: "git@host:repo.git", want: "git@host:repo.git"},
		{name: "空 URL", manifestURL: "ssh://host/manifest", remoteURL: "", want: ""},
		{name: "点号段规范化", manifestURL: "https://host/a/b/manifest", remoteURL: "../..", want: "https://host/"},
		{name: "无 manifest URL 时原样返回", manifestURL: "", remoteURL: "../foo", want: "../foo"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mgr := NewManager(tt.manifestURL, "default.xml", ".repo", nil)
			if got := mgr.ResolveRemoteURL(tt.remoteURL); got != tt.want {
				t.Errorf("ResolveRemoteURL(%q, manifest=%q) = %q, want %q", tt.remoteURL, tt.manifestURL, got, tt.want)
			}
		})
	}
}

func TestGetProjectsByArgs(t *testing.T) {
	// 构造临时 repo 根目录与两个项目的工作树目录
	topDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(topDir, "component", "group"), 0o755); err != nil {
		t.Fatalf("mkdir failed: %v", err)
	}

	m := &manifest.Manifest{
		Remotes: []manifest.Remote{{Name: "origin", Fetch: "https://example.com/"}},
		Default: manifest.Default{Remote: "origin", Revision: "main"},
		Projects: []manifest.Project{
			{Name: "group/proj-a", Path: "component/group/proj-a", Remote: "origin"},
			{Name: "group/proj-b", Path: "component/group/proj-b", Remote: "origin"},
			{Name: "dup", Path: "dup1", Remote: "origin"},
			{Name: "dup", Path: "dup2", Remote: "origin"},
		},
	}
	cfg := &config.Config{}
	mgr := NewManagerFromManifest(m, cfg)
	mgr.TopDir = topDir

	// 工作树目录映射：路径解析需要实际存在的目录回溯
	for _, p := range mgr.Projects {
		abs := filepath.Join(topDir, filepath.FromSlash(p.Path))
		if err := os.MkdirAll(abs, 0o755); err != nil {
			t.Fatalf("mkdir project %s failed: %v", p.Name, err)
		}
		p.Worktree = abs
	}

	tests := []struct {
		name    string
		args    []string
		baseDir string
		wantLen int
		wantErr bool
	}{
		{name: "按名字命中单个项目", args: []string{"group/proj-a"}, baseDir: topDir, wantLen: 1, wantErr: false},
		{name: "同名项目全部命中", args: []string{"dup"}, baseDir: topDir, wantLen: 2, wantErr: false},
		{name: "点号表示当前目录项目", args: []string{"."}, baseDir: filepath.Join(topDir, "component", "group", "proj-a"), wantLen: 1, wantErr: false},
		{name: "项目内子目录回溯", args: []string{"."}, baseDir: filepath.Join(topDir, "component", "group", "proj-b"), wantLen: 1, wantErr: false},
		{name: "相对路径参数命中项目", args: []string{"component/group/proj-a"}, baseDir: topDir, wantLen: 1, wantErr: false},
		{name: "不存在的名字报错", args: []string{"no-such-project"}, baseDir: filepath.Join(topDir, "component", "group", "proj-a"), wantLen: 0, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := mgr.GetProjectsByArgs(tt.args, tt.baseDir)
			if (err != nil) != tt.wantErr {
				t.Fatalf("GetProjectsByArgs(%v, %s) error = %v, wantErr %v", tt.args, tt.baseDir, err, tt.wantErr)
			}
			if !tt.wantErr && len(got) != tt.wantLen {
				t.Errorf("GetProjectsByArgs(%v, %s) = %d projects, want %d", tt.args, tt.baseDir, len(got), tt.wantLen)
			}
		})
	}
}

func TestGetProjectsByArgsPathBacktrack(t *testing.T) {
	// 从项目内部子目录逐级向上回溯应命中所在项目（对齐上游 GetProjects 路径语义）
	topDir := t.TempDir()
	projDir := filepath.Join(topDir, "component", "proj")
	nested := filepath.Join(projDir, "src", "deep")
	for _, d := range []string{nested, filepath.Join(topDir, "other")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir failed: %v", err)
		}
	}

	m := &manifest.Manifest{
		Remotes: []manifest.Remote{{Name: "origin", Fetch: "https://example.com/"}},
		Default: manifest.Default{Remote: "origin", Revision: "main"},
		Projects: []manifest.Project{
			{Name: "proj", Path: "component/proj", Remote: "origin"},
		},
	}
	mgr := NewManagerFromManifest(m, &config.Config{})
	mgr.TopDir = topDir
	mgr.Projects[0].Worktree = projDir

	got, err := mgr.GetProjectsByArgs([]string{"."}, nested)
	if err != nil {
		t.Fatalf("GetProjectsByArgs from nested dir error: %v", err)
	}
	if len(got) != 1 || got[0].Name != "proj" {
		t.Errorf("GetProjectsByArgs(nested) = %+v, want single project proj", got)
	}

	// 不在项目内的目录返回错误
	if _, err := mgr.GetProjectsByArgs([]string{"."}, filepath.Join(topDir, "other")); err == nil {
		t.Error("GetProjectsByArgs from non-project dir should fail")
	}
}

func TestManagerGetMirrorProjectPath(t *testing.T) {
	mgr := NewManager("url", "default.xml", ".repo/mirror", nil)

	// With remote URL
	path := mgr.getMirrorProjectPath("some/path", "https://example.com/foo/bar.git", "bar")
	if path == "" {
		t.Error("getMirrorProjectPath should not return empty")
	}
	// Should end with .git
	if len(path) < 4 || path[len(path)-4:] != ".git" {
		t.Errorf("getMirrorProjectPath should end with .git, got %q", path)
	}

	// Without remote URL (falls back to manifest path)
	path = mgr.getMirrorProjectPath("some/path", "", "bar")
	if path == "" {
		t.Error("getMirrorProjectPath with empty URL should not return empty")
	}
}
