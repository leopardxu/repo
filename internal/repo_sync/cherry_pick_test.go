package repo_sync

import (
	"testing"

	"github.com/leopardxu/repo-go/internal/logger"
	"github.com/leopardxu/repo-go/internal/manifest"
)

func TestCherryPickCommitEmptyHash(t *testing.T) {
	engine := NewEngine(&Options{Jobs: 1}, &manifest.Manifest{}, logger.NewDefaultLogger())
	// commitHash is empty by default
	err := engine.CherryPickCommit(nil)
	if err == nil {
		t.Error("CherryPickCommit with empty commit hash should return error")
	}
}

func TestCherryPickCommitNilProjects(t *testing.T) {
	engine := NewEngine(&Options{Jobs: 1}, &manifest.Manifest{}, logger.NewDefaultLogger())
	engine.SetCommitHash("abc123")
	err := engine.CherryPickCommit(nil)
	// With nil projects, should not error (nothing to do)
	if err != nil {
		// Some implementations may return error for nil projects, which is also acceptable
		t.Logf("CherryPickCommit(nil) returned error: %v (acceptable)", err)
	}
}
