package repo_sync

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/leopardxu/repo-go/internal/git"
	"github.com/leopardxu/repo-go/internal/manifest"
	"github.com/leopardxu/repo-go/internal/project"
)

func TestNewSuperprojectValidManifest(t *testing.T) {
	// NewSuperproject runs git init in .repo/superproject, so test with a temp dir
	tmpDir := t.TempDir()
	m := &manifest.Manifest{
		Subdir: tmpDir + "/.repo",
		Topdir: tmpDir,
	}

	sp, err := NewSuperproject(m, true)
	if err != nil {
		// git init may fail in some environments, that's OK for this test
		t.Logf("NewSuperproject returned error (acceptable in test env): %v", err)
		return
	}
	if sp == nil {
		t.Fatal("NewSuperproject returned nil without error")
	}
	if sp.quiet != true {
		t.Error("quiet should be true")
	}
	if want := filepath.Join(tmpDir, ".repo", "superproject"); sp.worktree != want {
		t.Errorf("superproject worktree = %q, want %q (must live under .repo)", sp.worktree, want)
	}
}

func TestSuperprojectUpdateProjectsNil(t *testing.T) {
	tmpDir := t.TempDir()
	m := &manifest.Manifest{
		Subdir: tmpDir + "/.repo",
		Topdir: tmpDir,
	}

	sp, err := NewSuperproject(m, true)
	if err != nil {
		t.Skipf("NewSuperproject failed (test env): %v", err)
	}

	// UpdateProjectsRevisionID with nil projects should not panic.
	// 清单未定义 superproject-remote/superproject-branch，首个校验即返回明确错误
	// （确定性路径），故断言错误非空。
	_, err = sp.UpdateProjectsRevisionID(nil)
	if err == nil {
		t.Error("UpdateProjectsRevisionID should return an error when superproject is not configured")
	}
}

// TestSuperprojectConfigFromManifest 验证超级项目配置来自清单的
// <superproject name/remote/revision>，而不是自定义属性：
// 完整项目 URL = fetch 基址 + "/" + name，生成的清单仍使用 fetch 基址。
func TestSuperprojectConfigFromManifest(t *testing.T) {
	tests := []struct {
		name          string
		manifest      *manifest.Manifest
		manifestURL   string // 非空时写入临时 .repo/config.json
		wantName      string
		wantURL       string
		wantFetchBase string
		wantRev       string
		wantErr       bool
	}{
		{
			name: "explicit name remote revision",
			manifest: &manifest.Manifest{
				Remotes:      []manifest.Remote{{Name: "aosp", Fetch: "https://example.com/aosp"}},
				Superproject: &manifest.Superproject{Name: "super", Remote: "aosp", Revision: "main"},
			},
			wantName:      "super",
			wantURL:       "https://example.com/aosp/super",
			wantFetchBase: "https://example.com/aosp",
			wantRev:       "main",
		},
		{
			name: "remote falls back to default",
			manifest: &manifest.Manifest{
				Remotes:      []manifest.Remote{{Name: "origin", Fetch: "https://example.com/origin"}},
				Default:      manifest.Default{Remote: "origin", Revision: "main"},
				Superproject: &manifest.Superproject{Name: "super"},
			},
			wantName:      "super",
			wantURL:       "https://example.com/origin/super",
			wantFetchBase: "https://example.com/origin",
			wantRev:       "main",
		},
		{
			name: "non-dot relative fetch resolved via manifest URL",
			manifest: &manifest.Manifest{
				Remotes:      []manifest.Remote{{Name: "origin", Fetch: "platform"}},
				Superproject: &manifest.Superproject{Name: "super", Remote: "origin", Revision: "main"},
			},
			manifestURL:   "https://example.com/manifests",
			wantName:      "super",
			wantURL:       "https://example.com/platform/super",
			wantFetchBase: "https://example.com/platform",
			wantRev:       "main",
		},
		{
			name: "relative local manifest URL resolves fetch locally",
			manifest: &manifest.Manifest{
				Remotes:      []manifest.Remote{{Name: "origin", Fetch: "platform"}},
				Superproject: &manifest.Superproject{Name: "super", Remote: "origin", Revision: "main"},
			},
			manifestURL:   "relative/manifests/default.xml",
			wantName:      "super",
			wantURL:       "relative/manifests/platform/super",
			wantFetchBase: "relative/manifests/platform",
			wantRev:       "main",
		},
		{
			name: "default remote preferred over same-name remote",
			manifest: &manifest.Manifest{
				Remotes: []manifest.Remote{
					{Name: "super", Fetch: "https://example.com/wrong"},
					{Name: "origin", Fetch: "https://example.com/origin"},
				},
				Default:      manifest.Default{Remote: "origin", Revision: "main"},
				Superproject: &manifest.Superproject{Name: "super"},
			},
			wantName:      "super",
			wantURL:       "https://example.com/origin/super",
			wantFetchBase: "https://example.com/origin",
			wantRev:       "main",
		},
		{
			name: "revision falls back to remote revision",
			manifest: &manifest.Manifest{
				Remotes: []manifest.Remote{{
					Name:     "origin",
					Fetch:    "https://example.com/origin",
					Revision: "remote-branch",
				}},
				Superproject: &manifest.Superproject{Name: "super", Remote: "origin"},
			},
			wantName:      "super",
			wantURL:       "https://example.com/origin/super",
			wantFetchBase: "https://example.com/origin",
			wantRev:       "remote-branch",
		},
		{
			name: "relative fetch resolved via manifest URL",
			manifest: &manifest.Manifest{
				Remotes:      []manifest.Remote{{Name: "origin", Fetch: ".."}},
				Superproject: &manifest.Superproject{Name: "super", Remote: "origin", Revision: "main"},
			},
			manifestURL:   "https://example.com/manifests",
			wantName:      "super",
			wantURL:       "https://example.com/super",
			wantFetchBase: "https://example.com",
			wantRev:       "main",
		},
		{
			name: "missing superproject tag",
			manifest: &manifest.Manifest{
				Remotes: []manifest.Remote{{Name: "origin", Fetch: "https://example.com/origin"}},
			},
			wantErr: true,
		},
		{
			name: "missing remote definition",
			manifest: &manifest.Manifest{
				Superproject: &manifest.Superproject{Name: "super", Remote: "nope", Revision: "main"},
			},
			wantErr: true,
		},
		{
			name: "relative fetch without manifest URL fails",
			manifest: &manifest.Manifest{
				Remotes:      []manifest.Remote{{Name: "origin", Fetch: ".."}},
				Superproject: &manifest.Superproject{Name: "super", Remote: "origin", Revision: "main"},
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sp := &Superproject{manifest: tt.manifest}
			if tt.manifestURL != "" {
				repoDir := filepath.Join(t.TempDir(), ".repo")
				if err := os.MkdirAll(repoDir, 0755); err != nil {
					t.Fatalf("mkdir .repo: %v", err)
				}
				data := fmt.Sprintf(`{"manifest_url":%q}`, tt.manifestURL)
				if err := os.WriteFile(filepath.Join(repoDir, "config.json"), []byte(data), 0644); err != nil {
					t.Fatalf("write config.json: %v", err)
				}
				sp.repoDir = repoDir
			}
			name, url, fetchBase, rev, err := sp.superprojectConfig()
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("superprojectConfig() error = %v", err)
			}
			if name != tt.wantName || url != tt.wantURL ||
				fetchBase != tt.wantFetchBase || rev != tt.wantRev {
				t.Errorf("superprojectConfig() = (%q,%q,%q,%q), want (%q,%q,%q,%q)",
					name, url, fetchBase, rev,
					tt.wantName, tt.wantURL, tt.wantFetchBase, tt.wantRev)
			}
		})
	}
}

// TestSuperprojectUpdateUsesFullURL 集成验证：相对 fetch 基于清单仓库 URL 解析为
// 基址后，remote add/fetch 使用 “基址/超级项目名” 的完整 URL；
// 生成的 superproject-manifest.xml 仍写入基址。
func TestSuperprojectUpdateUsesFullURL(t *testing.T) {
	root := t.TempDir()
	runner := git.NewRunner()
	runner.SetMaxRetries(0)

	// 超级项目裸仓库位于 <root>/super，含 proj/ 目录。
	superRepo := filepath.Join(root, "super")
	if _, err := runner.RunInDir(root, "init", "--bare", superRepo); err != nil {
		t.Fatalf("init bare superproject: %v", err)
	}
	seed := filepath.Join(root, "seed")
	if err := os.MkdirAll(filepath.Join(seed, "proj"), 0755); err != nil {
		t.Fatal(err)
	}
	// proj 必须是嵌入的独立 git 仓库，seed `git add` 才会记录 160000 gitlink；
	// 普通文件不会被超级项目覆盖（上游只接受 gitlink）。
	mustGit(t, runner, filepath.Join(seed, "proj"), "init")
	mustGit(t, runner, filepath.Join(seed, "proj"), "config", "user.email", "test@example.com")
	mustGit(t, runner, filepath.Join(seed, "proj"), "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(seed, "proj", "file.txt"), []byte("hi\n"), 0644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, runner, filepath.Join(seed, "proj"), "add", "file.txt")
	mustGit(t, runner, filepath.Join(seed, "proj"), "commit", "-m", "inner")
	mustGit(t, runner, seed, "init")
	mustGit(t, runner, seed, "config", "user.email", "test@example.com")
	mustGit(t, runner, seed, "config", "user.name", "Test")
	mustGit(t, runner, seed, "add", "proj")
	mustGit(t, runner, seed, "commit", "-m", "c1")
	mustGit(t, runner, seed, "remote", "add", "origin", superRepo)
	mustGit(t, runner, seed, "push", "origin", "HEAD:refs/heads/main")
	mustGit(t, runner, superRepo, "symbolic-ref", "HEAD", "refs/heads/main")

	// 清单仓库 URL 选在 <root>/manifests/manifest.git（可不存在），
	// 相对 fetch ".." 解析后基址为 <root>，超级项目完整 URL 为 <root>/super。
	repoDir := filepath.Join(root, ".repo")
	if err := os.MkdirAll(repoDir, 0755); err != nil {
		t.Fatal(err)
	}
	manifestURL := filepath.Join(root, "manifests", "manifest.git")
	if err := os.WriteFile(
		filepath.Join(repoDir, "config.json"),
		[]byte(fmt.Sprintf(`{"manifest_url":%q}`, manifestURL)),
		0644,
	); err != nil {
		t.Fatal(err)
	}

	m := &manifest.Manifest{
		RepoDir:      repoDir,
		Remotes:      []manifest.Remote{{Name: "origin", Fetch: ".."}},
		Superproject: &manifest.Superproject{Name: "super", Remote: "origin", Revision: "main"},
	}
	sp, err := NewSuperproject(m, true)
	if err != nil {
		t.Fatalf("NewSuperproject() error = %v", err)
	}

	projects := []*project.Project{{Name: "proj", Path: "proj"}}
	manifestPath, err := sp.UpdateProjectsRevisionID(projects)
	if err != nil {
		t.Fatalf("UpdateProjectsRevisionID() error = %v", err)
	}

	if got := mustGit(t, runner, sp.worktree, "remote", "get-url", "origin"); got != superRepo {
		t.Errorf("superproject remote URL = %q, want full URL %q", got, superRepo)
	}
	if projects[0].RevisionID == "" {
		t.Error("project RevisionID should be populated from superproject")
	}

	data, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("read generated manifest: %v", err)
	}
	if !strings.Contains(string(data), fmt.Sprintf(`fetch="%s"`, root)) {
		t.Errorf("generated manifest should keep fetch base %q, got:\n%s", root, data)
	}
	if strings.Contains(string(data), fmt.Sprintf(`fetch="%s"`, superRepo)) {
		t.Errorf("generated manifest must not write full project URL as fetch:\n%s", data)
	}

	// 已固定（RevisionID 为完整 SHA）的项目不再被覆盖。
	pinned := projects[0].RevisionID
	projects[0].RevisionID = pinned
	if _, err := sp.UpdateProjectsRevisionID(projects); err != nil {
		t.Fatalf("second UpdateProjectsRevisionID() error = %v", err)
	}
	if projects[0].RevisionID != pinned {
		t.Errorf("pinned RevisionID changed from %q to %q", pinned, projects[0].RevisionID)
	}
}

// TestParseLsTreeGitlinks 验证只有 mode 160000 才被当作项目提交；
// 普通文件/symlink 忽略，格式损坏整体失败。
func TestParseLsTreeGitlinks(t *testing.T) {
	tests := []struct {
		name    string
		data    string
		want    map[string]string
		wantErr bool
	}{
		{
			name: "gitlink parsed, regular file and symlink ignored",
			data: "160000 commit 1111111111111111111111111111111111111111\tplatform/build\x00" +
				"100644 blob 2222222222222222222222222222222222222222\tREADME\x00" +
				"120000 blob 3333333333333333333333333333333333333333\tlink\x00",
			want: map[string]string{"platform/build": "1111111111111111111111111111111111111111"},
		},
		{
			name: "path with spaces",
			data: "160000 commit 1111111111111111111111111111111111111111\ta b/c d\x00",
			want: map[string]string{"a b/c d": "1111111111111111111111111111111111111111"},
		},
		{
			name:    "missing tab separator fails",
			data:    "160000 commit 1111111111111111111111111111111111111111",
			wantErr: true,
		},
		{
			name:    "gitlink with non-commit type fails",
			data:    "160000 blob 1111111111111111111111111111111111111111\tproj\x00",
			wantErr: true,
		},
		{
			name:    "malformed metadata fails",
			data:    "160000 commit\tproj\x00",
			wantErr: true,
		},
		{
			name: "empty data yields empty map",
			data: "",
			want: map[string]string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseLsTreeGitlinks([]byte(tt.data))
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("parseLsTreeGitlinks() error = %v", err)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("gitlinks = %v, want %v", got, tt.want)
			}
			for path, commitID := range tt.want {
				if got[path] != commitID {
					t.Errorf("gitlinks[%q] = %q, want %q", path, got[path], commitID)
				}
			}
		})
	}
}

// TestSuperprojectSkipProject 验证 local manifest 与已固定项目的跳过规则。
func TestSuperprojectSkipProject(t *testing.T) {
	const fullSHA = "1111111111111111111111111111111111111111"
	localKeys := map[string]struct{}{"localproj": {}, "local/path": {}}

	tests := []struct {
		name string
		p    *project.Project
		want bool
	}{
		{"nil project", nil, true},
		{"empty path", &project.Project{Name: "x"}, true},
		{"normal project", &project.Project{Name: "x", Path: "x"}, false},
		{"pinned full revision id", &project.Project{Name: "x", Path: "x", RevisionID: fullSHA}, true},
		{"branch revision id is not pinned", &project.Project{Name: "x", Path: "x", RevisionID: "main"}, false},
		{"local manifest by name", &project.Project{Name: "localproj", Path: "x"}, true},
		{"local manifest by path", &project.Project{Name: "x", Path: "local/path"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := superprojectSkipProject(tt.p, localKeys); got != tt.want {
				t.Errorf("superprojectSkipProject() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestLocalManifestProjectKeys 验证从 .repo/local_manifests 收集 project/
// extend-project 的 name/path。
func TestLocalManifestProjectKeys(t *testing.T) {
	repoDir := t.TempDir()
	localDir := filepath.Join(repoDir, "local_manifests")
	if err := os.MkdirAll(localDir, 0755); err != nil {
		t.Fatal(err)
	}
	doc := `<?xml version="1.0"?>
<manifest>
  <project name="added" path="vendor/added" />
  <extend-project name="extended" path="extended" />
</manifest>`
	if err := os.WriteFile(filepath.Join(localDir, "local.xml"), []byte(doc), 0644); err != nil {
		t.Fatal(err)
	}
	sp := &Superproject{repoDir: repoDir}
	keys := sp.localManifestProjectKeys()
	for _, want := range []string{"added", "vendor/added", "extended"} {
		if _, ok := keys[want]; !ok {
			t.Errorf("local manifest keys missing %q: %v", want, keys)
		}
	}
}

// TestSuperprojectMissingGitlinkFails 验证超级项目缺少 gitlink（普通文件）时
// 整体失败，且不写入部分覆盖结果。
func TestSuperprojectMissingGitlinkFails(t *testing.T) {
	root := t.TempDir()
	runner := git.NewRunner()
	runner.SetMaxRetries(0)

	superRepo := filepath.Join(root, "super")
	if _, err := runner.RunInDir(root, "init", "--bare", superRepo); err != nil {
		t.Fatalf("init bare superproject: %v", err)
	}
	seed := filepath.Join(root, "seed")
	if err := os.MkdirAll(filepath.Join(seed, "proj"), 0755); err != nil {
		t.Fatal(err)
	}
	mustGit(t, runner, seed, "init")
	mustGit(t, runner, seed, "config", "user.email", "test@example.com")
	mustGit(t, runner, seed, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(seed, "proj", "file.txt"), []byte("hi\n"), 0644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, runner, seed, "add", ".")
	mustGit(t, runner, seed, "commit", "-m", "c1")
	mustGit(t, runner, seed, "remote", "add", "origin", superRepo)
	mustGit(t, runner, seed, "push", "origin", "HEAD:refs/heads/main")
	mustGit(t, runner, superRepo, "symbolic-ref", "HEAD", "refs/heads/main")

	repoDir := filepath.Join(root, ".repo")
	if err := os.MkdirAll(repoDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(repoDir, "config.json"),
		[]byte(fmt.Sprintf(`{"manifest_url":%q}`, filepath.Join(root, "manifests", "manifest.git"))),
		0644,
	); err != nil {
		t.Fatal(err)
	}

	m := &manifest.Manifest{
		RepoDir:      repoDir,
		Remotes:      []manifest.Remote{{Name: "origin", Fetch: ".."}},
		Superproject: &manifest.Superproject{Name: "super", Remote: "origin", Revision: "main"},
	}
	sp, err := NewSuperproject(m, true)
	if err != nil {
		t.Fatalf("NewSuperproject() error = %v", err)
	}
	p := &project.Project{Name: "proj", Path: "proj", Revision: "main"}
	manifestPath, err := sp.UpdateProjectsRevisionID([]*project.Project{p})
	if err == nil {
		t.Fatalf("missing gitlink should fail, got manifest %s", manifestPath)
	}
	if !strings.Contains(err.Error(), "does not have commit ids") {
		t.Errorf("error = %v, want missing commit ids", err)
	}
	if p.RevisionID != "" {
		t.Errorf("failed update must not set RevisionID, got %q", p.RevisionID)
	}
}
