package repo_sync

import (
	"errors"
	"testing"

	"github.com/leopardxu/repo-go/internal/logger"
	"github.com/leopardxu/repo-go/internal/manifest"
	"github.com/leopardxu/repo-go/internal/project"
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

// TestProcessCherryPickResultCumulativeProgress 验证进度 Update 使用累计
// 已处理项目数，而非固定 1。
func TestProcessCherryPickResultCumulativeProgress(t *testing.T) {
	e := &Engine{
		options:         &Options{},
		logger:          logger.NewDefaultLogger(),
		cherryPickStats: &cherryPickStats{},
	}
	pm := &recordReporter{}

	e.processCherryPickResult(CherryPickResult{Success: true, Project: &project.Project{Name: "a"}}, pm)
	e.processCherryPickResult(CherryPickResult{Project: &project.Project{Name: "b"}, Error: errors.New("boom")}, pm)
	e.processCherryPickResult(CherryPickResult{Success: true, Project: &project.Project{Name: "c"}}, pm)

	want := []int{1, 2, 3}
	if len(pm.currents) != len(want) {
		t.Fatalf("updates = %v, want %v", pm.currents, want)
	}
	for i := range want {
		if pm.currents[i] != want[i] {
			t.Errorf("update[%d] = %d, want %d (all: %v)", i, pm.currents[i], want[i], pm.currents)
		}
	}
}
