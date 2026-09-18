package repo_sync

import (
	"testing"

	"github.com/leopardxu/repo-go/internal/logger"
	"github.com/leopardxu/repo-go/internal/manifest"
)

func TestSetBranchName(t *testing.T) {
	engine := NewEngine(&Options{Jobs: 1}, &manifest.Manifest{}, logger.NewDefaultLogger())
	engine.SetBranchName("test-branch")
	if engine.branchName != "test-branch" {
		t.Errorf("branchName = %q, want test-branch", engine.branchName)
	}
}

func TestGetCheckoutStats(t *testing.T) {
	engine := NewEngine(&Options{Jobs: 1}, &manifest.Manifest{}, logger.NewDefaultLogger())
	engine.checkoutStats = &checkoutStats{Success: 5, Failed: 2}
	success, failed := engine.GetCheckoutStats()
	if success != 5 {
		t.Errorf("Success = %d, want 5", success)
	}
	if failed != 2 {
		t.Errorf("Failed = %d, want 2", failed)
	}
}

func TestCheckoutBranchEmptyName(t *testing.T) {
	engine := NewEngine(&Options{Jobs: 1}, &manifest.Manifest{}, logger.NewDefaultLogger())
	// branchName is empty by default
	err := engine.CheckoutBranch(nil)
	if err == nil {
		t.Error("CheckoutBranch with empty branch name should return error")
	}
}

func TestCheckoutBranchNilProjects(t *testing.T) {
	engine := NewEngine(&Options{Jobs: 1}, &manifest.Manifest{}, logger.NewDefaultLogger())
	engine.SetBranchName("test")
	err := engine.CheckoutBranch(nil)
	if err != nil {
		t.Errorf("CheckoutBranch(nil) should not error: %v", err)
	}
}

func TestSetCommitHash(t *testing.T) {
	engine := NewEngine(&Options{Jobs: 1}, &manifest.Manifest{}, logger.NewDefaultLogger())
	engine.SetCommitHash("abc123")
	if engine.commitHash != "abc123" {
		t.Errorf("commitHash = %q, want abc123", engine.commitHash)
	}
}

func TestGetCherryPickStats(t *testing.T) {
	engine := NewEngine(&Options{Jobs: 1}, &manifest.Manifest{}, logger.NewDefaultLogger())
	engine.cherryPickStats = &cherryPickStats{Success: 3, Failed: 1}
	success, failed := engine.GetCherryPickStats()
	if success != 3 {
		t.Errorf("Success = %d, want 3", success)
	}
	if failed != 1 {
		t.Errorf("Failed = %d, want 1", failed)
	}
}
