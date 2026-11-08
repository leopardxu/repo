package repo_sync

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/leopardxu/repo-go/internal/git"
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

func TestPrintAbandonSummaryEmpty(t *testing.T) {
	var buf bytes.Buffer
	// 空结果不应 panic
	PrintAbandonSummary(&buf, nil, false)
	if !strings.Contains(buf.String(), "共处理项 0") {
		t.Errorf("summary = %q, want zero counts", buf.String())
	}
}

func TestPrintAbandonSummaryWithResults(t *testing.T) {
	proj := &project.Project{Name: "proj-a"}
	results := []AbandonResult{
		{Project: proj, Branch: "feature", Success: true},
		{Project: proj, Branch: "feature", Success: false, Error: fmt.Errorf("branch not found")},
		{Project: proj, Branch: "gone", NotFound: true},
	}
	// dry-run 与真实模式都写同一个 writer，且 dry-run 输出"将删除"。
	var buf bytes.Buffer
	PrintAbandonSummary(&buf, results, false)
	out := buf.String()
	for _, want := range []string{"[OK]    proj-a: 成功删除分支 feature", "[FAIL]", "共处理项 2", "未找到分支: 1"} {
		if !strings.Contains(out, want) {
			t.Errorf("summary %q missing %q", out, want)
		}
	}
	if strings.Contains(out, "gone") {
		t.Errorf("NotFound branch must not be printed: %q", out)
	}

	buf.Reset()
	PrintAbandonSummary(&buf, results, true)
	if !strings.Contains(buf.String(), "将删除分支") {
		t.Errorf("dry-run summary = %q, want 将删除分支", buf.String())
	}
}

// TestAbandonTopicsAllNoBranchesNoFakeSuccess 验证 --all 无分支可删时不产出结果
// （不伪造成功行），由上层报 no project has local branch(es)。
func TestAbandonTopicsAllNoBranchesNoFakeSuccess(t *testing.T) {
	engine := NewEngine(&Options{Jobs: 1}, &manifest.Manifest{}, logger.NewDefaultLogger())
	got := engine.AbandonTopics(nil, "")
	if len(got) != 0 {
		t.Fatalf("AbandonTopics(nil, all) = %v, want none", got)
	}
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

// TestAbandonLocalBranchName 验证当前分支名归一化：完整 refs/heads/X 去前缀，
// detached sentinel 原样保留，以便与 ListLocalBranches 的短名比较。
func TestAbandonLocalBranchName(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{in: "refs/heads/main", want: "main"},
		{in: "feature/x", want: "feature/x"},
		{in: "HEAD detached at abc1234", want: "HEAD detached at abc1234"},
		{in: "  refs/heads/dev\n", want: "dev"},
	}
	for _, tt := range tests {
		if got := abandonLocalBranchName(tt.in); got != tt.want {
			t.Errorf("abandonLocalBranchName(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// abandonAllRunner 构造只含一个项目的 Git Runner 回调：
// 本地分支为 branches，当前分支为 current；记录调用顺序。
func abandonAllRunner(branches []string, current string, commands *[]string) *funcRunner {
	return &funcRunner{run: func(_ string, args []string) ([]byte, error) {
		key := strings.Join(args, " ")
		*commands = append(*commands, key)
		switch {
		case key == "for-each-ref --format=%(refname) refs/heads/":
			lines := make([]string, 0, len(branches))
			for _, b := range branches {
				lines = append(lines, "refs/heads/"+b)
			}
			return []byte(strings.Join(lines, "\n") + "\n"), nil
		case key == "symbolic-ref HEAD":
			return []byte("refs/heads/" + current + "\n"), nil
		case strings.HasPrefix(key, "branch --list "):
			return []byte(strings.TrimPrefix(key, "branch --list ") + "\n"), nil
		default:
			return nil, nil
		}
	}}
}

// TestAbandonTopicsAllDeletesCurrentAndManifestBranch 验证 --all 对齐上游：
// 当前分支先 detach 到 manifest revision 再删除，manifest 分支也删除，
// 且当前分支排在首位（避免 manifest 分支先删导致 detach 目标丢失）。
func TestAbandonTopicsAllDeletesCurrentAndManifestBranch(t *testing.T) {
	var commands []string
	runner := abandonAllRunner([]string{"main", "topic"}, "main", &commands)
	proj := &project.Project{
		Name:     "p",
		Path:     "/repo/p",
		Worktree: "/repo/p",
		Revision: "main",
		GitRepo:  git.NewRepository("/repo/p", runner),
	}
	engine := NewEngine(&Options{Jobs: 1, JobsCheckout: 1, Quiet: true}, &manifest.Manifest{}, logger.NewDefaultLogger())

	results := engine.AbandonTopics([]*project.Project{proj}, "")

	if len(results) != 2 {
		t.Fatalf("results = %+v, want 2 deleted branches", results)
	}
	for _, r := range results {
		if !r.Success || r.Error != nil {
			t.Errorf("result %+v, want success", r)
		}
	}
	joined := strings.Join(commands, "\n")
	if !strings.Contains(joined, "checkout --detach main") {
		t.Errorf("current branch must detach before deletion, commands = %v", commands)
	}
	if !strings.Contains(joined, "branch -D main") || !strings.Contains(joined, "branch -D topic") {
		t.Errorf("all local branches must be deleted, commands = %v", commands)
	}
	if idx("branch -D main", commands) > idx("branch -D topic", commands) {
		t.Errorf("current branch must be deleted first, commands = %v", commands)
	}
}

// TestAbandonTopicsAllDeletesCurrentBeforeManifestBranch 验证当前分支不是
// manifest 分支但字母序靠后时仍先删当前分支：当前分支需要 detach 到 manifest
// revision，若先删 manifest 分支，detach 目标已不存在。
func TestAbandonTopicsAllDeletesCurrentBeforeManifestBranch(t *testing.T) {
	var commands []string
	runner := abandonAllRunner([]string{"main", "topic"}, "topic", &commands)
	proj := &project.Project{
		Name:     "p",
		Path:     "/repo/p",
		Worktree: "/repo/p",
		Revision: "main",
		GitRepo:  git.NewRepository("/repo/p", runner),
	}
	engine := NewEngine(&Options{Jobs: 1, JobsCheckout: 1, Quiet: true}, &manifest.Manifest{}, logger.NewDefaultLogger())

	results := engine.AbandonTopics([]*project.Project{proj}, "")

	if len(results) != 2 {
		t.Fatalf("results = %+v, want 2 deleted branches", results)
	}
	detach := idx("checkout --detach main", commands)
	delCurrent := idx("branch -D topic", commands)
	delManifest := idx("branch -D main", commands)
	if detach < 0 || delCurrent < 0 || delManifest < 0 {
		t.Fatalf("missing expected commands, commands = %v", commands)
	}
	if detach > delCurrent {
		t.Errorf("current branch must detach before deletion, commands = %v", commands)
	}
	if delCurrent > delManifest {
		t.Errorf("current branch must be deleted before manifest branch, commands = %v", commands)
	}
}

// TestAbandonTopicsAllOnlyCurrentBranch 验证仅剩当前/manifest 分支时仍删除并成功，
// 不会误报 no project has local branch(es)。
func TestAbandonTopicsAllOnlyCurrentBranch(t *testing.T) {
	var commands []string
	runner := abandonAllRunner([]string{"main"}, "main", &commands)
	proj := &project.Project{
		Name:     "p",
		Path:     "/repo/p",
		Worktree: "/repo/p",
		Revision: "main",
		GitRepo:  git.NewRepository("/repo/p", runner),
	}
	engine := NewEngine(&Options{Jobs: 1, JobsCheckout: 1, Quiet: true}, &manifest.Manifest{}, logger.NewDefaultLogger())

	results := engine.AbandonTopics([]*project.Project{proj}, "")

	if len(results) != 1 || !results[0].Success {
		t.Fatalf("results = %+v, want current branch deleted successfully", results)
	}
	if !strings.Contains(strings.Join(commands, "\n"), "branch -D main") {
		t.Errorf("current branch must be deleted, commands = %v", commands)
	}
}

// idx 返回命令在列表中的位置（不存在返回 -1）。
func idx(cmd string, commands []string) int {
	for i, c := range commands {
		if c == cmd {
			return i
		}
	}
	return -1
}
