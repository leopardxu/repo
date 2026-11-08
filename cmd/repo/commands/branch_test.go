package commands

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/leopardxu/repo-go/internal/project"
)

// TestFilterSyncedProjects 验证无项目参数时的未同步过滤（对齐上游 GetProjects
// 的 Exists 判定）：工作树目录不存在或为空的项目被跳过。
func TestFilterSyncedProjects(t *testing.T) {
	root := t.TempDir()
	synced := &project.Project{Name: "synced", Worktree: root}
	missing := &project.Project{Name: "missing", Worktree: filepath.Join(root, "not-checked-out")}
	empty := &project.Project{Name: "empty", Worktree: ""}

	got := filterSyncedProjects([]*project.Project{synced, missing, empty})

	if len(got) != 1 || got[0] != synced {
		t.Fatalf("filterSyncedProjects() = %v, want only %q", got, synced.Name)
	}
	if got := filterSyncedProjects(nil); len(got) != 0 {
		t.Errorf("filterSyncedProjects(nil) = %v, want empty", got)
	}
}

// TestPrintBranchReport 验证分支清单写入给定 writer（stdout payload）：
// 当前分支 * 标记、全项目/单项目标注、--current 过滤。
func TestPrintBranchReport(t *testing.T) {
	branchInfo := map[string][]string{
		"main":  {"p1", "p2"},
		"topic": {"p1"},
	}
	current := map[string]bool{"main": true}

	var buf bytes.Buffer
	printBranchReport(&buf, branchInfo, current, 2, &BranchOptions{Quiet: true})
	out := buf.String()
	for _, want := range []string{"分支信息汇总", "* main", "topic", "所有项目", "在项目 p1", "共有 2 个分支"} {
		if !strings.Contains(out, want) {
			t.Errorf("output %q missing %q", out, want)
		}
	}

	buf.Reset()
	printBranchReport(&buf, branchInfo, current, 2, &BranchOptions{Current: true})
	out = buf.String()
	if strings.Contains(out, "topic") {
		t.Errorf("--current output %q must not contain non-current branch", out)
	}
	if !strings.Contains(out, "* main") {
		t.Errorf("--current output %q missing current branch", out)
	}
}

// TestParseBranchListLine 验证 git branch --list 的 *、+、空格前缀处理，
// 以及 detached HEAD 伪条目被跳过。
func TestParseBranchListLine(t *testing.T) {
	tests := []struct {
		line string
		want string
	}{
		{line: "* main", want: "main"},
		{line: "+ worktree-topic", want: "worktree-topic"},
		{line: "   topic", want: "topic"},
		{line: "*  spaced", want: "spaced"},
		{line: "  remotes/origin/main", want: "remotes/origin/main"},
		{line: "* (HEAD detached at abc1234)", want: ""},
		// 仅有 "+ " 才是 worktree 标记；分支名本身以 + 开头时不能被误剥。
		{line: "  +foo", want: "+foo"},
		{line: "", want: ""},
		{line: "   ", want: ""},
	}
	for _, tt := range tests {
		if got := parseBranchListLine(tt.line); got != tt.want {
			t.Errorf("parseBranchListLine(%q) = %q, want %q", tt.line, got, tt.want)
		}
	}
}

// TestDedupeProjects 验证重复的项目参数按 名称+路径 去重且保留首次顺序。
func TestDedupeProjects(t *testing.T) {
	a := &project.Project{Name: "a", Path: "a"}
	b := &project.Project{Name: "b", Path: "b"}
	aAgain := &project.Project{Name: "a", Path: "a"}
	// 同名不同路径是合法项目，不能被误去重。
	aOtherPath := &project.Project{Name: "a", Path: "other/a"}

	got := dedupeProjects([]*project.Project{a, b, aAgain, aOtherPath, nil})
	want := []*project.Project{a, b, aOtherPath}
	if len(got) != len(want) {
		t.Fatalf("dedupeProjects() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("dedupeProjects()[%d] = %v, want %v", i, got[i], want[i])
		}
	}
}

// TestRequireSyncedProjects 验证显式指定未同步项目时返回包含名称/路径的清晰错误，
// 已同步项目与空列表不报错。
func TestRequireSyncedProjects(t *testing.T) {
	root := t.TempDir()
	synced := &project.Project{Name: "synced", Worktree: root}
	missingPath := filepath.Join(root, "not-checked-out")
	missing := &project.Project{Name: "missing", Worktree: missingPath}

	if err := requireSyncedProjects([]*project.Project{synced}); err != nil {
		t.Fatalf("requireSyncedProjects(synced) error = %v", err)
	}
	if err := requireSyncedProjects(nil); err != nil {
		t.Fatalf("requireSyncedProjects(nil) error = %v", err)
	}

	err := requireSyncedProjects([]*project.Project{synced, missing})
	if err == nil {
		t.Fatal("requireSyncedProjects() = nil, want error for unsynced project")
	}
	msg := err.Error()
	if !strings.Contains(msg, "missing") || !strings.Contains(msg, missingPath) {
		t.Errorf("error = %q, want project name and worktree path", msg)
	}
}
