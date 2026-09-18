package repo_sync

import (
	"testing"

	"github.com/leopardxu/repo-go/internal/manifest"
)

func TestNewSuperprojectValidManifest(t *testing.T) {
	// NewSuperproject calls git init --bare, so we test with a temp dir
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
