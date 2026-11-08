package repo_sync

import (
	"strings"
	"testing"

	"github.com/leopardxu/repo-go/internal/git"
	"github.com/leopardxu/repo-go/internal/logger"
	"github.com/leopardxu/repo-go/internal/manifest"
	"github.com/leopardxu/repo-go/internal/project"
)

// recordReporter 记录每次 Update 的绝对计数，用于验证进度计数语义。
type recordReporter struct {
	currents []int
}

func (r *recordReporter) Start(int)                    {}
func (r *recordReporter) Update(current int, _ string) { r.currents = append(r.currents, current) }
func (r *recordReporter) Finish()                      {}

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
	// 零可用项目对齐上游 "no project has branch"：必须返回错误，不能静默成功。
	err := engine.CheckoutBranch(nil)
	if err == nil {
		t.Fatal("CheckoutBranch(nil) = nil, want no project has branch error")
	}
	if !strings.Contains(err.Error(), "no project has branch") {
		t.Errorf("error = %q, want no project has branch", err)
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

// TestCheckoutBranchDeduplicatesProjects 验证重复的项目只检出一次，
// 统计也只计一次，避免同一项目被并发重复检出。
func TestCheckoutBranchDeduplicatesProjects(t *testing.T) {
	worktree := t.TempDir()
	checkoutCalls := 0
	runner := &funcRunner{run: func(_ string, args []string) ([]byte, error) {
		switch strings.Join(args, " ") {
		case "branch --list topic":
			return []byte("topic\n"), nil
		case "checkout topic":
			checkoutCalls++
			return nil, nil
		default:
			return nil, nil
		}
	}}
	proj := &project.Project{
		Name:     "p",
		Path:     "p",
		Worktree: worktree,
		GitRepo:  git.NewRepository(worktree, runner),
	}
	engine := NewEngine(&Options{Jobs: 1, JobsCheckout: 1, Quiet: true}, &manifest.Manifest{}, logger.NewDefaultLogger())
	engine.SetBranchName("topic")

	if err := engine.CheckoutBranch([]*project.Project{proj, proj}); err != nil {
		t.Fatalf("CheckoutBranch() error = %v", err)
	}
	success, failed := engine.GetCheckoutStats()
	if success != 1 || failed != 0 {
		t.Fatalf("stats = %d success, %d failed, want 1/0", success, failed)
	}
	if checkoutCalls != 1 {
		t.Fatalf("checkout calls = %d, want 1", checkoutCalls)
	}
}

// TestProcessCheckoutResultCumulativeProgress 验证进度 Update 收到的是累计
// 已处理项目数（绝对值），而非固定 1。
func TestProcessCheckoutResultCumulativeProgress(t *testing.T) {
	e := &Engine{
		options:       &Options{},
		logger:        logger.NewDefaultLogger(),
		checkoutStats: &checkoutStats{},
	}
	pm := &recordReporter{}

	e.processCheckoutResult(CheckoutResult{Success: true, Project: &project.Project{Name: "a"}}, pm)
	e.processCheckoutResult(CheckoutResult{Project: &project.Project{Name: "b"}}, pm)
	e.processCheckoutResult(CheckoutResult{BranchMissing: true, Project: &project.Project{Name: "c"}}, pm)

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
