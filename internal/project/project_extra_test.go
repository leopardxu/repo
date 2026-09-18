package project

import (
	"path/filepath"
	"testing"

	"github.com/leopardxu/repo-go/internal/git"
)

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
	mgr.AddProject(p1)
	mgr.AddProject(p2)

	projects, err := mgr.GetProjectsByNames([]string{"a", "b"})
	if err != nil {
		t.Fatalf("GetProjectsByNames error: %v", err)
	}
	if len(projects) != 2 {
		t.Errorf("GetProjectsByNames count = %d, want 2", len(projects))
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
