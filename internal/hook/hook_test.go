package hook

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/leopardxu/repo-go/internal/logger"
)

func TestInitHooks(t *testing.T) {
	tmpDir := t.TempDir()

	err := InitHooks(tmpDir)
	if err != nil {
		t.Fatalf("InitHooks: %v", err)
	}

	// Verify .repo/hooks directory was created
	hooksDir := filepath.Join(tmpDir, ".repo", "hooks")
	if _, err := os.Stat(hooksDir); os.IsNotExist(err) {
		t.Error(".repo/hooks directory was not created")
	}

	// Verify some hook files were created
	preCommit := filepath.Join(hooksDir, "pre-commit")
	if _, err := os.Stat(preCommit); os.IsNotExist(err) {
		t.Error("pre-commit hook was not created")
	}
}

func TestCreateRepoGitConfig(t *testing.T) {
	tmpDir := t.TempDir()

	err := CreateRepoGitConfig(tmpDir)
	if err != nil {
		t.Fatalf("CreateRepoGitConfig: %v", err)
	}

	// Verify config file was created
	configPath := filepath.Join(tmpDir, ".repo", "repo.git")
	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		t.Error("repo.git config file was not created")
	}
}

func TestCreateRepoGitconfig(t *testing.T) {
	tmpDir := t.TempDir()

	err := CreateRepoGitconfig(tmpDir)
	if err != nil {
		t.Fatalf("CreateRepoGitconfig: %v", err)
	}

	// Verify config file was created
	configPath := filepath.Join(tmpDir, ".repo", "repo.gitconfig")
	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		t.Error("repo.gitconfig file was not created")
	}
}

func TestHookError(t *testing.T) {
	err := &HookError{
		Op:   "init",
		Path: "/path/to/hook",
		Err:  nil,
	}

	msg := err.Error()
	if msg == "" {
		t.Error("HookError.Error() should not be empty")
	}
}

func TestHookErrorWithoutPath(t *testing.T) {
	err := &HookError{
		Op:   "init",
		Path: "",
		Err:  nil,
	}

	msg := err.Error()
	if msg == "" {
		t.Error("HookError.Error() without path should not be empty")
	}
}

func TestSetLogger(_ *testing.T) {
	// Should not panic with nil
	SetLogger(nil)

	// Should not panic with a real logger
	originalLog := log
	defer func() { log = originalLog }()
	SetLogger(logger.NewDefaultLogger())
}
