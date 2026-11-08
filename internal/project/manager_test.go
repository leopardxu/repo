package project

import (
	"os"
	"path/filepath"
	"strings"
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
			{Name: "a", Path: "a", SyncS: manifest.OptBool{Set: true, Value: true}, CloneDepth: 5, DestBranch: "dev", Upstream: "main"},
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

// TestManagerResolveProjectURL 验证完整项目 URL 对齐上游 repo
// （manifest_xml.py 的 ToRemoteSpec：url = resolvedFetchUrl.rstrip('/') + '/' + name）。
// FetchBase 是清单原始 fetch 基址：可 urljoin 的地址（含不带点号的相对值）
// 先按清单 URL 解析，再无条件拼接一次项目名；绝不按项目名后缀去重。
func TestManagerResolveProjectURL(t *testing.T) {
	tests := []struct {
		name        string
		manifestURL string
		fetchBase   string
		projectName string
		remoteURL   string // 无 FetchBase 时直接构造项目使用的 RemoteURL
		want        string
	}{
		{
			// 实际踩坑场景：无路径的 ssh fetch 直接传给 git clone 会报
			// "未指定路径，执行 'git help pull' 查看有效的 url 语法"
			name:        "ssh fetch without path",
			manifestURL: "ssh://host/linux_repo/cix-manifest",
			fetchBase:   "ssh://git@gitmirror.cixcomputing.com:29418",
			projectName: "cix_opensource/release/edk2-non-osi",
			want:        "ssh://git@gitmirror.cixcomputing.com:29418/cix_opensource/release/edk2-non-osi",
		},
		{
			name:        "https fetch with trailing slash",
			fetchBase:   "https://example.com/",
			projectName: "platform/build",
			want:        "https://example.com/platform/build",
		},
		{
			name:        "https fetch without trailing slash",
			fetchBase:   "https://example.com",
			projectName: "platform/build",
			want:        "https://example.com/platform/build",
		},
		{
			name:        "https fetch with double trailing slash",
			fetchBase:   "https://example.com//",
			projectName: "platform/build",
			want:        "https://example.com/platform/build",
		},
		{
			name:        "scp style fetch",
			fetchBase:   "git@host:base",
			projectName: "a",
			want:        "git@host:base/a",
		},
		{
			// 基址末段与项目名相同也必须无条件再拼一次
			name:        "project name equals last path segment",
			fetchBase:   "https://example.com/proj",
			projectName: "proj",
			want:        "https://example.com/proj/proj",
		},
		{
			// 相对但不带点号的 fetch 同样按 urljoin 解析
			name:        "dotless relative fetch is urljoined",
			manifestURL: "ssh://host/manifests/main",
			fetchBase:   "platform",
			projectName: "proj",
			want:        "ssh://host/manifests/platform/proj",
		},
		{
			name:        "relative parent fetch is urljoined",
			manifestURL: "ssh://host/linux_repo/cix-manifest",
			fetchBase:   "..",
			projectName: "a",
			want:        "ssh://host/a",
		},
		{
			name:        "relative parent path fetch is urljoined",
			manifestURL: "ssh://host/linux_repo/cix-manifest",
			fetchBase:   "../platform",
			projectName: "a",
			want:        "ssh://host/platform/a",
		},
		{
			name:        "relative current dir fetch is urljoined",
			manifestURL: "ssh://host/linux_repo/cix-manifest",
			fetchBase:   "./foo",
			projectName: "a",
			want:        "ssh://host/linux_repo/foo/a",
		},
		{
			name:        "empty fetch kept empty",
			manifestURL: "ssh://host/linux_repo/cix-manifest",
			fetchBase:   "",
			projectName: "a",
			want:        "",
		},
		{
			// 无 FetchBase 的直接构造项目：RemoteURL 已是完整地址，幂等返回
			name:        "no fetch base returns complete remote url",
			projectName: "proj",
			remoteURL:   "https://example.com/base/proj",
			want:        "https://example.com/base/proj",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mgr := NewManager(tt.manifestURL, "default.xml", ".repo", nil)
			p := &Project{
				Name:      tt.projectName,
				FetchBase: tt.fetchBase,
				RemoteURL: tt.remoteURL,
			}
			if got := mgr.ResolveProjectURL(p); got != tt.want {
				t.Errorf("ResolveProjectURL(fetchBase=%q, manifest=%q, project=%q) = %q, want %q",
					tt.fetchBase, tt.manifestURL, tt.projectName, got, tt.want)
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
	// 无 manifest URL 无法 urljoin，保留相对基址并拼接一次项目名
	if want := "../rel/proj"; p3.RemoteURL != want {
		t.Errorf("relative fetch RemoteURL = %q, want %q", p3.RemoteURL, want)
	}
	if p3.FetchBase != "../" {
		t.Errorf("relative fetch FetchBase = %q, want ../", p3.FetchBase)
	}
}

// TestNewManagerFromManifestRemoteAlias 验证 remote 声明 alias 时 RemoteName
// 使用 alias，而 RemoteURL/FetchBase 仍取自该 remote 的 fetch。
func TestNewManagerFromManifestRemoteAlias(t *testing.T) {
	m := &manifest.Manifest{
		RepoDir: "/tmp/repo",
		Remotes: []manifest.Remote{
			{Name: "origin", Alias: "cix", Fetch: "ssh://git@host:29418", PushURL: "ssh://git@host:29418/push", Review: "https://review.example.com"},
			{Name: "upstream", Alias: "up", Fetch: "https://example.com/base", Review: "ssh://review/up"},
		},
		Default: manifest.Default{Remote: "origin", Revision: "main"},
		Projects: []manifest.Project{
			{Name: "proj-a", Path: "a"},
			{Name: "proj-b", Path: "b", Remote: "up"},
		},
	}
	m.Default.CustomAttrs = make(map[string]string)

	mgr := NewManagerFromManifest(m, &config.Config{ManifestURL: "ssh://host/manifests/main"})
	if len(mgr.Projects) != 2 {
		t.Fatalf("projects = %d, want 2", len(mgr.Projects))
	}

	tests := []struct {
		index      int
		wantName   string
		wantFetch  string
		wantRemote string
		wantPush   string
		wantReview string
	}{
		{index: 0, wantName: "proj-a", wantFetch: "ssh://git@host:29418", wantRemote: "ssh://git@host:29418/proj-a", wantPush: "ssh://git@host:29418/push/proj-a", wantReview: "https://review.example.com/proj-a"},
		{index: 1, wantName: "proj-b", wantFetch: "https://example.com/base", wantRemote: "https://example.com/base/proj-b", wantPush: "https://example.com/base/proj-b", wantReview: "ssh://review/up/proj-b"},
	}
	for _, tt := range tests {
		p := mgr.Projects[tt.index]
		if p.Name != tt.wantName {
			t.Errorf("project[%d].Name = %q, want %q", tt.index, p.Name, tt.wantName)
		}
		if p.FetchBase != tt.wantFetch {
			t.Errorf("project %s FetchBase = %q, want %q", p.Name, p.FetchBase, tt.wantFetch)
		}
		if p.RemoteURL != tt.wantRemote {
			t.Errorf("project %s RemoteURL = %q, want %q", p.Name, p.RemoteURL, tt.wantRemote)
		}
		if p.PushURL != tt.wantPush {
			t.Errorf("project %s PushURL = %q, want %q", p.Name, p.PushURL, tt.wantPush)
		}
		if p.ReviewURL != tt.wantReview {
			t.Errorf("project %s ReviewURL = %q, want %q", p.Name, p.ReviewURL, tt.wantReview)
		}
	}
	if mgr.Projects[0].RemoteName != "cix" {
		t.Errorf("default-remote alias = %q, want cix", mgr.Projects[0].RemoteName)
	}
	if mgr.Projects[1].RemoteName != "up" {
		t.Errorf("explicit-remote alias = %q, want up", mgr.Projects[1].RemoteName)
	}
}

// TestNewManagerFromManifestDefaultRemoteAlias 验证清单 default remote 写成
// alias 时仍能解析到对应 remote（name/alias 双向匹配），RemoteName 保留 alias。
func TestNewManagerFromManifestDefaultRemoteAlias(t *testing.T) {
	m := &manifest.Manifest{
		RepoDir: "/tmp/repo",
		Remotes: []manifest.Remote{
			{Name: "origin", Alias: "up", Fetch: "ssh://host/base", PushURL: "ssh://host/push", Review: "https://review.example.com"},
		},
		Default:  manifest.Default{Remote: "up", Revision: "main"},
		Projects: []manifest.Project{{Name: "proj", Path: "p"}},
	}
	m.Default.CustomAttrs = make(map[string]string)

	mgr := NewManagerFromManifest(m, &config.Config{ManifestURL: "ssh://host/manifests/main"})
	if len(mgr.Projects) != 1 {
		t.Fatalf("projects = %d, want 1", len(mgr.Projects))
	}
	p := mgr.Projects[0]
	if p.RemoteName != "up" {
		t.Errorf("RemoteName = %q, want alias up", p.RemoteName)
	}
	if got, want := p.FetchBase, "ssh://host/base"; got != want {
		t.Errorf("FetchBase = %q, want %q", got, want)
	}
	if got, want := p.RemoteURL, "ssh://host/base/proj"; got != want {
		t.Errorf("RemoteURL = %q, want %q", got, want)
	}
	if got, want := p.PushURL, "ssh://host/push/proj"; got != want {
		t.Errorf("PushURL = %q, want %q", got, want)
	}
	if got, want := p.ReviewURL, "https://review.example.com/proj"; got != want {
		t.Errorf("ReviewURL = %q, want %q", got, want)
	}
}

// TestNewManagerFromManifestPushAndReviewURL 验证 Project.PushURL/ReviewURL：
// pushurl 优先、缺省回退 fetch、相对地址先解析再拼项目名；review 保留原值。
func TestNewManagerFromManifestPushAndReviewURL(t *testing.T) {
	m := &manifest.Manifest{
		RepoDir: "/tmp/repo",
		Remotes: []manifest.Remote{
			{Name: "origin", Fetch: "ssh://git@host:29418/base", PushURL: "ssh://git@host:29418/push", Review: "https://review.example.com"},
			{Name: "nopush", Fetch: "ssh://git@host/base"},
			{Name: "rel", Fetch: "../platform", PushURL: "../push", Review: "ssh://review/base"},
			{Name: "full", Fetch: "ssh://git@host/base", PushURL: "ssh://git@host/base/proj"},
		},
		Default: manifest.Default{Remote: "origin", Revision: "main"},
		Projects: []manifest.Project{
			{Name: "proj", Path: "p", Remote: "origin"},
			{Name: "proj", Path: "p2", Remote: "nopush"},
			{Name: "proj", Path: "p3", Remote: "rel"},
			{Name: "proj", Path: "p4", Remote: "full"},
		},
	}
	m.Default.CustomAttrs = make(map[string]string)

	mgr := NewManagerFromManifest(m, &config.Config{ManifestURL: "ssh://host/linux_repo/cix-manifest"})
	if len(mgr.Projects) != 4 {
		t.Fatalf("projects = %d, want 4", len(mgr.Projects))
	}

	tests := []struct {
		remote     string
		wantPush   string
		wantReview string
	}{
		{remote: "origin", wantPush: "ssh://git@host:29418/push/proj", wantReview: "https://review.example.com/proj"},
		{remote: "nopush", wantPush: "ssh://git@host/base/proj"},
		{remote: "rel", wantPush: "ssh://host/push/proj", wantReview: "ssh://review/base/proj"},
		{remote: "full", wantPush: "ssh://git@host/base/proj/proj"},
	}
	for i, tt := range tests {
		p := mgr.Projects[i]
		if p.PushURL != tt.wantPush {
			t.Errorf("%s PushURL = %q, want %q", tt.remote, p.PushURL, tt.wantPush)
		}
		if p.ReviewURL != tt.wantReview {
			t.Errorf("%s ReviewURL = %q, want %q", tt.remote, p.ReviewURL, tt.wantReview)
		}
	}
}

// TestNormalizeReviewURL 验证 review 端点规范化：补 scheme、去 Gerrit/ssh_info
// 尾缀、统一尾部斜杠，且不拼接项目名。
func TestNormalizeReviewURL(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{name: "empty", raw: "", want: ""},
		{name: "missing scheme", raw: "review.example.com", want: "http://review.example.com/"},
		{name: "https keeps scheme", raw: "https://review.example.com", want: "https://review.example.com/"},
		{name: "ssh keeps scheme", raw: "ssh://git@host:29418", want: "ssh://git@host:29418/"},
		{name: "sso keeps scheme", raw: "sso://host/gerrit", want: "sso://host/gerrit/"},
		{name: "strip Gerrit suffix", raw: "https://review.example.com/Gerrit", want: "https://review.example.com/"},
		{name: "strip ssh_info suffix", raw: "https://review.example.com/ssh_info", want: "https://review.example.com/"},
		{name: "strip persistent prefix first", raw: "persistent-https://review.example.com/Gerrit", want: "https://review.example.com/"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := NormalizeReviewURL(tt.raw); got != tt.want {
				t.Errorf("NormalizeReviewURL(%q) = %q, want %q", tt.raw, got, tt.want)
			}
		})
	}
}

// TestResolveReviewURL 验证 ReviewURL = 规范化端点到项目名的完整 push 目标：
// 补 scheme、去 Gerrit/ssh_info 尾缀、单个尾斜杠，再拼一次项目名。
func TestResolveReviewURL(t *testing.T) {
	tests := []struct {
		name        string
		raw         string
		projectName string
		want        string
	}{
		{name: "empty", raw: "", projectName: "proj", want: ""},
		{name: "missing scheme", raw: "review.example.com", projectName: "proj", want: "http://review.example.com/proj"},
		{name: "https endpoint", raw: "https://review.example.com", projectName: "proj", want: "https://review.example.com/proj"},
		{name: "ssh endpoint keeps port", raw: "ssh://git@host:29418", projectName: "proj", want: "ssh://git@host:29418/proj"},
		{name: "strip Gerrit suffix", raw: "https://review.example.com/Gerrit", projectName: "proj", want: "https://review.example.com/proj"},
		{name: "strip ssh_info suffix", raw: "https://review.example.com/ssh_info", projectName: "proj", want: "https://review.example.com/proj"},
		{name: "strip persistent prefix", raw: "persistent-https://review.example.com/Gerrit", projectName: "proj", want: "https://review.example.com/proj"},
		{name: "collapse repeated trailing slashes", raw: "https://review.example.com///", projectName: "proj", want: "https://review.example.com/proj"},
		{name: "name equal to last segment is still appended", raw: "https://review.example.com/proj", projectName: "proj", want: "https://review.example.com/proj/proj"},
		{name: "empty project name", raw: "https://review.example.com", projectName: "", want: "https://review.example.com/"},
		{name: "nested project name", raw: "https://review.example.com", projectName: "platform/build", want: "https://review.example.com/platform/build"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ResolveReviewURL(tt.raw, tt.projectName); got != tt.want {
				t.Errorf("ResolveReviewURL(%q, %q) = %q, want %q", tt.raw, tt.projectName, got, tt.want)
			}
		})
	}
}

// TestResolvePushURL 验证 pushurl 优先/回退 fetch、相对基址按 cfg 做
// urljoin、绝对/scp 地址原样保留，最后无条件拼接一次项目名。
func TestResolvePushURL(t *testing.T) {
	cfg := &config.Config{ManifestURL: "ssh://host/manifests/main"}
	tests := []struct {
		name     string
		cfg      *config.Config
		pushURL  string
		fetchURL string
		projName string
		want     string
	}{
		{name: "pushurl 优先", cfg: cfg, pushURL: "ssh://host/push", fetchURL: "ssh://host/fetch", projName: "proj", want: "ssh://host/push/proj"},
		{name: "缺少 pushurl 回退 fetch", cfg: cfg, fetchURL: "ssh://host/fetch", projName: "proj", want: "ssh://host/fetch/proj"},
		{name: "两者皆空", cfg: cfg},
		{name: "点前缀相对 pushurl 先 urljoin", cfg: cfg, pushURL: "../platform", projName: "proj", want: "ssh://host/platform/proj"},
		{name: "不带点相对 fetch 原样保留", cfg: cfg, fetchURL: "platform", projName: "proj", want: "platform/proj"},
		{name: "scp 风式原样保留", cfg: cfg, pushURL: "git@host:base", projName: "proj", want: "git@host:base/proj"},
		{name: "无 cfg 保留相对基址并拼名", cfg: nil, fetchURL: "../foo", projName: "proj", want: "../foo/proj"},
		{name: "基址已含项目名也再拼一次", cfg: cfg, pushURL: "ssh://host/base/proj", projName: "proj", want: "ssh://host/base/proj/proj"},
		{name: "尾部斜杠折叠", cfg: cfg, pushURL: "ssh://host/base///", projName: "proj", want: "ssh://host/base/proj"},
		{name: "嵌套项目名", cfg: cfg, pushURL: "ssh://host/base", projName: "platform/build", want: "ssh://host/base/platform/build"},
		{name: "空项目名原样返回基址", cfg: cfg, pushURL: "ssh://host/base", want: "ssh://host/base"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ResolvePushURL(tt.cfg, tt.pushURL, tt.fetchURL, tt.projName); got != tt.want {
				t.Errorf("ResolvePushURL(%q, %q, %q) = %q, want %q", tt.pushURL, tt.fetchURL, tt.projName, got, tt.want)
			}
		})
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
		projectName string
		want        string
	}{
		{name: "相对 fetch 基于 manifest URL 解析", manifestURL: "ssh://host/linux_repo/cix-manifest", remoteURL: "../platform/foo", want: "ssh://host/platform/foo"},
		{name: "相对 fetch 解析后拼接项目名", manifestURL: "ssh://host/linux_repo/cix-manifest", remoteURL: "../platform", projectName: "proj", want: "ssh://host/platform/proj"},
		{name: "相对无点 fetch 按 urljoin 解析", manifestURL: "ssh://host/manifests/main", remoteURL: "platform", projectName: "proj", want: "ssh://host/manifests/platform/proj"},
		{name: "scp 风式原样作为基址", manifestURL: "ssh://host/manifest", remoteURL: "git@host:repo.git", want: "git@host:repo.git"},
		{name: "绝对 URL 无条件拼接项目名", manifestURL: "ssh://host/manifest", remoteURL: "ssh://host/platform/proj", projectName: "proj", want: "ssh://host/platform/proj/proj"},
		{name: "点号 fetch 解析到清单目录", manifestURL: "ssh://host/linux_repo/cix-manifest", remoteURL: ".", projectName: "proj", want: "ssh://host/linux_repo/proj"},
		{name: "基址已含项目名也再拼一次", manifestURL: "ssh://host/manifests/main", remoteURL: "../platform/proj", projectName: "proj", want: "ssh://host/platform/proj/proj"},
		{name: "绝对文件路径拼项目名", manifestURL: "ssh://host/manifest", remoteURL: "/srv/git/base", projectName: "proj", want: "/srv/git/base/proj"},
		{name: "绝对文件路径无项目名原样", manifestURL: "ssh://host/manifest", remoteURL: "/srv/git/base", want: "/srv/git/base"},
		{name: "空 URL", manifestURL: "ssh://host/manifest", remoteURL: "", want: ""},
		{name: "点号段规范化", manifestURL: "https://host/a/b/manifest", remoteURL: "../..", want: "https://host/"},
		{name: "无 manifest URL 时保留相对形式并拼接项目名", manifestURL: "", remoteURL: "../foo", projectName: "proj", want: "../foo/proj"},
		{name: "无 manifest URL 且无项目名时原样返回", manifestURL: "", remoteURL: "../foo", want: "../foo"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mgr := NewManager(tt.manifestURL, "default.xml", ".repo", nil)
			if got := mgr.ResolveRemoteURL(tt.remoteURL, tt.projectName); got != tt.want {
				t.Errorf("ResolveRemoteURL(%q, manifest=%q, project=%q) = %q, want %q",
					tt.remoteURL, tt.manifestURL, tt.projectName, got, tt.want)
			}
		})
	}
}

// TestManagerResolveProjectURLStable 验证从原始 FetchBase 重复解析
// 完整项目 URL 始终一致（不会因 RemoteURL 已含项目名而再次拼接）。
func TestManagerResolveProjectURLStable(t *testing.T) {
	mgr := NewManager("ssh://host/linux_repo/cix-manifest", "default.xml", ".repo", nil)
	p := &Project{Name: "proj", FetchBase: "../platform"}
	p.RemoteURL = mgr.ResolveProjectURL(p)
	if want := "ssh://host/platform/proj"; p.RemoteURL != want {
		t.Fatalf("ResolveProjectURL = %q, want %q", p.RemoteURL, want)
	}
	if second := mgr.ResolveProjectURL(p); second != p.RemoteURL {
		t.Errorf("second ResolveProjectURL = %q, want stable %q", second, p.RemoteURL)
	}
}

// TestNewManagerFromManifestUsesManifestRepoURL 验证相对 fetch 的 urljoin
// 基准只取清单仓库 URL（cfg.ManifestURL），manifest-server 不参与解析。
func TestNewManagerFromManifestUsesManifestRepoURL(t *testing.T) {
	m := &manifest.Manifest{
		RepoDir:        "/tmp/repo/.repo",
		ManifestServer: &manifest.ManifestServer{URL: "https://manifest-server.example"},
		Remotes:        []manifest.Remote{{Name: "origin", Fetch: "../platform"}},
		Default:        manifest.Default{Remote: "origin", Revision: "main"},
		Projects:       []manifest.Project{{Name: "proj", Path: "proj", Remote: "origin"}},
	}
	m.Default.CustomAttrs = make(map[string]string)

	mgr := NewManagerFromManifest(m, &config.Config{ManifestURL: "ssh://host/manifests/main"})
	if want := "ssh://host/manifests/main"; mgr.ManifestURL != want {
		t.Fatalf("ManifestURL = %q, want manifest repo URL %q", mgr.ManifestURL, want)
	}

	// 相对 fetch 以清单仓库 URL 为基准解析，再拼接项目名
	if got, want := mgr.ResolveRemoteURL("../platform", "proj"), "ssh://host/platform/proj"; got != want {
		t.Errorf("ResolveRemoteURL = %q, want %q", got, want)
	}
	if len(mgr.Projects) != 1 {
		t.Fatalf("projects = %d, want 1", len(mgr.Projects))
	}
	if got, want := mgr.Projects[0].RemoteURL, "ssh://host/platform/proj"; got != want {
		t.Errorf("project RemoteURL = %q, want %q", got, want)
	}

	// 无 config 时不得退回 manifest-server
	mgr = NewManagerFromManifest(m, nil)
	if mgr.ManifestURL != "" {
		t.Errorf("ManifestURL = %q, want empty without cfg.ManifestURL", mgr.ManifestURL)
	}
}

// TestManagerSyncDryRun 验证 DryRun 在 Manager.Sync 入口生效：
// 只打印动作，不执行 clone/fetch/checkout。
func TestManagerSyncDryRun(t *testing.T) {
	runner := &recordingRunner{}
	mgr := NewManager("https://example.com/manifest", "default.xml", ".repo", runner)
	wt := filepath.Join(t.TempDir(), "proj")
	mgr.AddProject(NewProject("proj", wt, "origin", "https://example.com/proj", "main", nil, runner))

	if err := mgr.Sync(SyncOptions{DryRun: true}); err != nil {
		t.Fatalf("Manager.Sync(DryRun) error = %v", err)
	}
	if calls := runner.snapshot(); len(calls) != 0 {
		t.Errorf("DryRun must not run git commands, got %v", calls)
	}
	if _, err := os.Stat(wt); !os.IsNotExist(err) {
		t.Errorf("DryRun must not create worktree %s, stat err = %v", wt, err)
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
	topDir := t.TempDir()
	mgr := NewManager("url", "default.xml", ".repo/mirror", nil)
	mgr.TopDir = topDir

	// 镜像布局（对齐上游 GetProjectPaths）：<topdir>/<name>.git，name 含嵌套前缀
	got := mgr.getMirrorProjectPath("some/path", "platform/bar")
	want := filepath.Join(topDir, "platform", "bar.git")
	if got != want {
		t.Errorf("getMirrorProjectPath(name) = %q, want %q", got, want)
	}
	if strings.Contains(got, "project-objects") {
		t.Errorf("mirror path must not use .repo/project-objects: %q", got)
	}

	// 同一 remote fetch 下的不同项目必须得到不同路径（不再按 fetch 目录碰撞）
	other := mgr.getMirrorProjectPath("some/other", "platform/baz")
	if other == got {
		t.Errorf("different projects must not share mirror path, both = %q", got)
	}

	// name 缺失时退回清单 path
	got = mgr.getMirrorProjectPath("fallback/path", "")
	if !strings.HasSuffix(got, filepath.Join("fallback", "path.git")) {
		t.Errorf("getMirrorProjectPath fallback = %q, want suffix fallback/path.git", got)
	}

	// 不安全 name（越界）不能逃逸出 repo 根目录
	got = mgr.getMirrorProjectPath("safe/path", "../../etc/passwd")
	if strings.Contains(got, "..") {
		t.Errorf("getMirrorProjectPath must not allow escaping path, got %q", got)
	}
	if !strings.HasPrefix(got, topDir) {
		t.Errorf("getMirrorProjectPath must stay under topdir %q, got %q", topDir, got)
	}

	// 全部尾部斜杠都要去掉（双斜杠/多斜杠回归）
	got = mgr.getMirrorProjectPath("ignored", "platform/bar///")
	if want := filepath.Join(topDir, "platform", "bar.git"); got != want {
		t.Errorf("getMirrorProjectPath(trailing slashes) = %q, want %q", got, want)
	}

	// 不剥离 .git 后缀（对齐上游：name + ".git"）
	got = mgr.getMirrorProjectPath("ignored", "foo.git")
	if want := filepath.Join(topDir, "foo.git.git"); got != want {
		t.Errorf("getMirrorProjectPath(.git suffix) = %q, want %q", got, want)
	}

	// 返回绝对路径作为裸仓库目录
	if !filepath.IsAbs(got) {
		t.Errorf("getMirrorProjectPath must return an absolute path, got %q", got)
	}
}

// TestNewManagerFromManifestMirrorKeepsDistinctProjects 回归：镜像模式按项目
// name 生成 <topdir>/<name>.git，同名多 path 映射到同一目录时显式报告碰撞并
// 跳过后者（避免并发 clone 同一目录），不同项目各自独立。
func TestNewManagerFromManifestMirrorKeepsDistinctProjects(t *testing.T) {
	topDir := t.TempDir()
	repoDir := filepath.Join(topDir, ".repo")
	m := &manifest.Manifest{
		RepoDir: repoDir,
		Remotes: []manifest.Remote{{Name: "origin", Fetch: "https://example.com/"}},
		Default: manifest.Default{Remote: "origin", Revision: "main"},
		Projects: []manifest.Project{
			{Name: "platform/a", Path: "a", Remote: "origin"},
			{Name: "platform/b", Path: "b", Remote: "origin"},
			// 同名不同 path：镜像只有一个目录，必须去重，不能并发 clone
			{Name: "platform/a", Path: "a-alias", Remote: "origin"},
		},
	}
	m.Default.CustomAttrs = make(map[string]string)

	mgr := NewManagerFromManifest(m, &config.Config{Mirror: true})
	if len(mgr.Projects) != 2 {
		t.Fatalf("mirror projects = %d, want 2 (same name deduped)", len(mgr.Projects))
	}

	byName := make(map[string]*Project, len(mgr.Projects))
	for _, p := range mgr.Projects {
		byName[p.Name] = p
	}
	for name, relPath := range map[string]string{"platform/a": "a", "platform/b": "b"} {
		p := byName[name]
		if p == nil {
			t.Fatalf("mirror project %s missing", name)
		}
		wantPath := filepath.Join(topDir, filepath.FromSlash(name)+".git")
		if p.Gitdir != wantPath {
			t.Errorf("%s Gitdir = %q, want %q", name, p.Gitdir, wantPath)
		}
		if p.Objdir != wantPath {
			t.Errorf("%s Objdir = %q, want %q", name, p.Objdir, wantPath)
		}
		// 镜像没有工作树（对齐上游 GetProjectPaths worktree=None）
		if p.Worktree != "" {
			t.Errorf("%s Worktree = %q, want empty (mirror has no worktree)", name, p.Worktree)
		}
		if !p.Mirror {
			t.Errorf("%s Mirror = false, want true", name)
		}
		if p.Path != relPath {
			t.Errorf("%s Path = %q, want manifest relpath %q", name, p.Path, relPath)
		}
		if p.Relpath != relPath {
			t.Errorf("%s Relpath = %q, want manifest relpath %q", name, p.Relpath, relPath)
		}
		if p.GitRepo == nil || p.GitRepo.Path != wantPath {
			t.Errorf("%s GitRepo.Path = %v, want %q", name, p.GitRepo, wantPath)
		}
	}
	if byName["platform/a"].Gitdir == byName["platform/b"].Gitdir {
		t.Error("distinct mirror projects must not share the same path")
	}
}

// TestNewManagerFromManifestRegularWorktreePath 回归：普通模式的 Path 是
// 工作区相对路径，不能拼接 .repo；Worktree 基于 workspace 根目录。
func TestNewManagerFromManifestRegularWorktreePath(t *testing.T) {
	topDir := t.TempDir()
	repoDir := filepath.Join(topDir, ".repo")
	m := &manifest.Manifest{
		RepoDir: repoDir,
		Remotes: []manifest.Remote{{Name: "origin", Fetch: "https://example.com/"}},
		Default: manifest.Default{Remote: "origin", Revision: "main"},
		Projects: []manifest.Project{
			{Name: "p", Path: "src/p", Remote: "origin"},
		},
	}
	m.Default.CustomAttrs = make(map[string]string)

	mgr := NewManagerFromManifest(m, &config.Config{})
	if len(mgr.Projects) != 1 {
		t.Fatalf("projects = %d, want 1", len(mgr.Projects))
	}
	p := mgr.Projects[0]
	if p.Path != filepath.FromSlash("src/p") {
		t.Errorf("Path = %q, want workspace-relative src/p", p.Path)
	}
	if strings.Contains(p.Path, ".repo") {
		t.Errorf("Path must not contain .repo: %q", p.Path)
	}
	if want := filepath.Join(topDir, "src", "p"); p.Worktree != want {
		t.Errorf("Worktree = %q, want %q", p.Worktree, want)
	}
}
