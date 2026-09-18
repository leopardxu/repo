package commands

import (
	"strings"
	"testing"

	"github.com/leopardxu/repo-go/internal/project"
)

// TestBuildForallEnv_DisablesGitPager 验证 forall 注入 GIT_PAGER=cat，
// 避免 git log/diff/show 等在并发执行时启动交互式分页器（less）等待按键，
// 导致单个项目阻塞、wg.Wait() 无法返回而使整个 forall 永久挂起。
func TestBuildForallEnv_DisablesGitPager(t *testing.T) {
	proj := &project.Project{
		Name:       "test/project",
		Relpath:    "test/project",
		Revision:   "main",
		Worktree:   ".",
		RemoteName: "origin",
	}

	env := buildForallEnv(proj, 1)

	var pager string
	pagerSet := false
	for _, kv := range env {
		if strings.HasPrefix(kv, "GIT_PAGER=") {
			pager = strings.TrimPrefix(kv, "GIT_PAGER=")
			pagerSet = true
		}
	}
	if !pagerSet {
		t.Fatal("期望 buildForallEnv 注入 GIT_PAGER，实际未找到")
	}
	if pager != "cat" {
		t.Errorf("期望 GIT_PAGER=cat，实际 GIT_PAGER=%s", pager)
	}
}

// TestBuildForallEnv_OverridesExistingGitPager 验证即使进程环境中已设置 GIT_PAGER，
// buildForallEnv 也会将其替换为 cat（先过滤再追加，避免重复键导致行为不确定）。
func TestBuildForallEnv_OverridesExistingGitPager(t *testing.T) {
	t.Setenv("GIT_PAGER", "less")

	proj := &project.Project{Name: "p", Worktree: "."}
	env := buildForallEnv(proj, 1)

	count := 0
	for _, kv := range env {
		if strings.HasPrefix(kv, "GIT_PAGER=") {
			count++
		}
	}
	if count != 1 {
		t.Errorf("期望 GIT_PAGER 仅出现 1 次（值为 cat），实际出现 %d 次", count)
	}
}

// TestBuildForallEnv_UpstreamVars 验证 forall 注入上游 repo 约定的 REPO_* 环境变量。
func TestBuildForallEnv_UpstreamVars(t *testing.T) {
	proj := &project.Project{
		Name:       "platform/build",
		Relpath:    "build",
		Revision:   "main",
		Worktree:   ".",
		RemoteName: "origin",
	}

	env := buildForallEnv(proj, 3)

	expected := map[string]string{
		"REPO_PATH":        "build",
		"REPO_PROJECT":     "platform/build",
		"REPO_REMOTE":      "origin",
		"REPO_LREV":        "main",
		"REPO_RREV":        "main",
		"REPO_I":           "3",
		"REPO_UPSTREAM":    "main",
		"REPO_DEST_BRANCH": "main",
	}

	for _, kv := range env {
		for key, want := range expected {
			if kv == key+"="+want {
				delete(expected, key)
			}
		}
	}

	for key, want := range expected {
		t.Errorf("expected env var %s=%s not found", key, want)
	}
}
