package repo_sync

import (
	"fmt"
	"testing"

	"github.com/leopardxu/repo-go/internal/logger"
	"github.com/leopardxu/repo-go/internal/manifest"
	"github.com/leopardxu/repo-go/internal/project"
)

func TestAbandonTopicsEmpty(t *testing.T) {
	engine := NewEngine(&Options{Jobs: 1}, &manifest.Manifest{}, logger.NewDefaultLogger())
	results := engine.AbandonTopics(nil, "test-branch")
	if len(results) != 0 {
		t.Errorf("AbandonTopics(nil) = %d results, want 0", len(results))
	}
}

func TestPrintAbandonSummaryEmpty(_ *testing.T) {
	log := logger.NewDefaultLogger()
	// 空结果不应 panic
	PrintAbandonSummary(nil, false, log)
}

func TestPrintAbandonSummaryWithResults(_ *testing.T) {
	log := logger.NewDefaultLogger()
	log.SetLevel(logger.LogLevelInfo)

	proj := &project.Project{Name: "proj-a"}
	results := []AbandonResult{
		{Project: proj, Branch: "feature", Success: true},
		{Project: proj, Branch: "feature", Success: false, Error: fmt.Errorf("branch not found")},
		{Project: proj, Branch: "gone", NotFound: true},
	}
	// dry-run 与真实模式都不应 panic
	PrintAbandonSummary(results, false, log)
	PrintAbandonSummary(results, true, log)
}

func TestAbandonResultStructure(t *testing.T) {
	r := AbandonResult{
		Branch:  "dev",
		Success: true,
	}
	if r.Branch != "dev" {
		t.Errorf("Branch = %q", r.Branch)
	}
	if !r.Success {
		t.Error("Success should be true")
	}
}
