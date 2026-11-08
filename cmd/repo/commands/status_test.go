package commands

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/leopardxu/repo-go/internal/git"
	"github.com/leopardxu/repo-go/internal/logger"
)

// TestComputeOrphanDirs 验证 --orphans 的递归孤儿识别：
//   - 项目目录（含嵌套项目路径）整体视为已覆盖；
//   - 顶层普通文件、项目祖先目录下的其它文件/目录均需上报；
//   - 目录条目带尾部 "/"；".repo" 始终跳过；
//   - 结果按路径排序。
func TestComputeOrphanDirs(t *testing.T) {
	root := t.TempDir()

	// 项目目录（内容不应上报）
	mustMkdir(t, filepath.Join(root, "art"))
	mustWriteFile(t, filepath.Join(root, "art", "inside.txt"))
	mustMkdir(t, filepath.Join(root, "build"))
	// 嵌套项目：external 仅是祖先目录，需继续深入
	mustMkdir(t, filepath.Join(root, "external", "foo"))
	// 祖先目录下的孤儿文件与目录
	mustMkdir(t, filepath.Join(root, "external", "other"))
	mustWriteFile(t, filepath.Join(root, "external", "bar.txt"))
	// 多层嵌套：dir/subdir 是项目祖先，proj3 未被覆盖
	mustMkdir(t, filepath.Join(root, "dir", "subdir", "proj1"))
	mustMkdir(t, filepath.Join(root, "dir", "subdir", "proj2"))
	mustMkdir(t, filepath.Join(root, "dir", "subdir", "proj3"))
	mustWriteFile(t, filepath.Join(root, "dir", "subdir", "loose.txt"))
	// 顶层孤儿目录与文件
	mustMkdir(t, filepath.Join(root, "leftover"))
	mustWriteFile(t, filepath.Join(root, "README.md"))
	// 路径前缀相同但不是完整路径段：不应视为被项目 app 覆盖
	mustMkdir(t, filepath.Join(root, "apple"))
	// .repo 始终跳过
	mustMkdir(t, filepath.Join(root, ".repo"))

	relPaths := []string{"art", "external/foo", "build", "dir/subdir/proj1", "dir/subdir/proj2", "app"}

	got, err := computeOrphanDirs(root, relPaths)
	if err != nil {
		t.Fatalf("computeOrphanDirs 失败: %v", err)
	}

	want := []string{
		"README.md",
		"apple/",
		"dir/subdir/loose.txt",
		"dir/subdir/proj3/",
		"external/bar.txt",
		"external/other/",
		"leftover/",
	}
	if !sort.StringsAreSorted(got) {
		t.Errorf("结果未排序: %v", got)
	}
	if !equalStringSlice(got, want) {
		t.Errorf("computeOrphanDirs = %v, want %v", got, want)
	}
}

// TestComputeOrphanDirs_SymlinkProjectNotOrphan 验证指向目录的符号链接项目不被
// 误报为孤儿文件（DirEntry.IsDir 对符号链接返回 false）。
func TestComputeOrphanDirs_SymlinkProjectNotOrphan(t *testing.T) {
	root := t.TempDir()
	external := t.TempDir()
	mustWriteFile(t, filepath.Join(external, "inside.txt"))
	mustSymlink(t, external, filepath.Join(root, "linked"))
	// 指向不存在目标的符号链接按普通条目上报
	mustSymlink(t, filepath.Join(root, "missing-target"), filepath.Join(root, "broken"))

	got, err := computeOrphanDirs(root, []string{"linked"})
	if err != nil {
		t.Fatalf("computeOrphanDirs 失败: %v", err)
	}
	want := []string{"broken"}
	if !equalStringSlice(got, want) {
		t.Errorf("computeOrphanDirs = %v, want %v", got, want)
	}
}

// TestComputeOrphanDirs_SymlinkParentDir 验证符号链接父目录仍会深入查找嵌套项目。
func TestComputeOrphanDirs_SymlinkParentDir(t *testing.T) {
	root := t.TempDir()
	external := t.TempDir()
	mustMkdir(t, filepath.Join(external, "proj1"))
	mustMkdir(t, filepath.Join(external, "other"))
	mustSymlink(t, external, filepath.Join(root, "external"))

	got, err := computeOrphanDirs(root, []string{"external/proj1"})
	if err != nil {
		t.Fatalf("computeOrphanDirs 失败: %v", err)
	}
	want := []string{"external/other/"}
	if !equalStringSlice(got, want) {
		t.Errorf("computeOrphanDirs = %v, want %v", got, want)
	}
}

// TestComputeOrphanDirs_SymlinkLoop 验证符号链接循环不会导致无限递归。
func TestComputeOrphanDirs_SymlinkLoop(t *testing.T) {
	root := t.TempDir()
	mustMkdir(t, filepath.Join(root, "dir", "sub"))
	mustSymlink(t, filepath.Join(root, "dir"), filepath.Join(root, "dir", "sub", "loop"))

	done := make(chan struct{})
	var (
		got []string
		err error
	)
	go func() {
		got, err = computeOrphanDirs(root, []string{"dir/sub/loop/proj"})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("computeOrphanDirs 未在符号链接循环中终止")
	}
	if err != nil {
		t.Fatalf("computeOrphanDirs 失败: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("computeOrphanDirs = %v, want empty", got)
	}
}

// TestComputeOrphanDirs_NestedProjectParentOnly 验证仅祖先目录被覆盖时不漏报更深层孤儿。
func TestComputeOrphanDirs_NestedProjectParentOnly(t *testing.T) {
	root := t.TempDir()
	mustMkdir(t, filepath.Join(root, "dir", "subdir", "proj1"))
	mustMkdir(t, filepath.Join(root, "dir", "subdir", "proj3"))

	got, err := computeOrphanDirs(root, []string{"dir/subdir/proj1"})
	if err != nil {
		t.Fatalf("computeOrphanDirs 失败: %v", err)
	}
	want := []string{"dir/subdir/proj3/"}
	if !equalStringSlice(got, want) {
		t.Errorf("computeOrphanDirs = %v, want %v", got, want)
	}
}

// TestComputeOrphanDirs_EmptyRepoRoot 空目录、无项目时无孤儿（.repo 仍跳过）。
func TestComputeOrphanDirs_EmptyRepoRoot(t *testing.T) {
	root := t.TempDir()
	mustMkdir(t, filepath.Join(root, ".repo"))

	got, err := computeOrphanDirs(root, nil)
	if err != nil {
		t.Fatalf("computeOrphanDirs 失败: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("computeOrphanDirs = %v, want empty", got)
	}
}

// TestComputeOrphanDirs_MissingRoot 根目录不存在时返回错误而非空列表。
func TestComputeOrphanDirs_MissingRoot(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	got, err := computeOrphanDirs(missing, nil)
	if err == nil {
		t.Fatalf("期望错误，实际 got=%v nil", got)
	}
}

func mustMkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0755); err != nil {
		t.Fatalf("MkdirAll %s: %v", path, err)
	}
}

func mustWriteFile(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("x"), 0644); err != nil {
		t.Fatalf("WriteFile %s: %v", path, err)
	}
}

func mustSymlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("当前环境不支持符号链接: %v", err)
	}
}

func equalStringSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestStatusProjectHeaderLine 验证上游项目头格式：project 填充到 40 列后
// branch 紧接同一行；detached 显示 (*** NO BRANCH ***)；quiet 只保留项目头。
func TestStatusProjectHeaderLine(t *testing.T) {
	tests := []struct {
		name   string
		path   string
		branch string
		quiet  bool
		want   string
	}{
		{name: "branch same line", path: "foo", branch: "main", want: fmt.Sprintf("project %-40s", "foo/ ") + "branch main"},
		{name: "detached", path: "foo", branch: "", want: fmt.Sprintf("project %-40s", "foo/ ") + "(*** NO BRANCH ***)"},
		{name: "quiet header only", path: "foo", branch: "main", quiet: true, want: fmt.Sprintf("project %-40s", "foo/ ")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := statusProjectHeaderLine(tt.path, tt.branch, tt.quiet); got != tt.want {
				t.Errorf("statusProjectHeaderLine(%q,%q,%v) = %q, want %q", tt.path, tt.branch, tt.quiet, got, tt.want)
			}
		})
	}
}

// TestRunStatusCleanBranchAndDetached 验证用户侧语义：
//   - 干净且在分支上的项目仍输出 project/branch 头；
//   - 干净 detached 项目静默并输出 nothing to commit 汇总；
//   - quiet 模式只输出项目头/不输出汇总。
func TestRunStatusCleanBranchAndDetached(t *testing.T) {
	repoRoot, _, _ := readCmdStdoutRepo(t)
	dir := filepath.Join(repoRoot, "foo")
	runner := git.NewRunner()
	runGit := func(args ...string) string {
		t.Helper()
		out, err := runner.RunInDir(dir, args...)
		if err != nil {
			t.Fatalf("git %v failed: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	runGit("checkout", "--", "version.txt")
	branchOut, err := runner.RunInDir(dir, "symbolic-ref", "-q", "HEAD")
	if err != nil {
		t.Fatalf("symbolic-ref failed: %v", err)
	}
	branch := strings.TrimPrefix(strings.TrimSpace(string(branchOut)), "refs/heads/")
	header := fmt.Sprintf("project %-40s", "foo/ ") + "branch " + branch

	readCmdWithCwd(t, repoRoot, func() {
		stdout, _, err := readCmdRunCaptured(t, func() error {
			return runStatus(&StatusOptions{Jobs: 1}, nil, logger.NewDefaultLogger())
		})
		if err != nil {
			t.Fatalf("runStatus clean branch failed: %v", err)
		}
		if !strings.Contains(stdout, header) {
			t.Errorf("clean branch status 缺少项目头 %q:\n%s", header, stdout)
		}

		quietOut, _, err := readCmdRunCaptured(t, func() error {
			return runStatus(&StatusOptions{Jobs: 1, Quiet: true}, nil, logger.NewDefaultLogger())
		})
		if err != nil {
			t.Fatalf("runStatus quiet clean branch failed: %v", err)
		}
		if !strings.Contains(quietOut, fmt.Sprintf("project %-40s", "foo/ ")) ||
			strings.Contains(quietOut, "branch ") {
			t.Errorf("quiet clean branch 应仅输出项目头:\n%s", quietOut)
		}

		runGit("checkout", "-q", "--detach", "HEAD")
		detachedOut, _, err := readCmdRunCaptured(t, func() error {
			return runStatus(&StatusOptions{Jobs: 1}, nil, logger.NewDefaultLogger())
		})
		if err != nil {
			t.Fatalf("runStatus detached clean failed: %v", err)
		}
		if strings.TrimSpace(detachedOut) != "nothing to commit (working directory clean)" {
			t.Errorf("detached clean status = %q, want nothing-to-commit summary", detachedOut)
		}

		detachedQuiet, _, err := readCmdRunCaptured(t, func() error {
			return runStatus(&StatusOptions{Jobs: 1, Quiet: true}, nil, logger.NewDefaultLogger())
		})
		if err != nil {
			t.Fatalf("runStatus quiet detached clean failed: %v", err)
		}
		if strings.TrimSpace(detachedQuiet) != "" {
			t.Errorf("quiet detached clean status 应为空，实际 %q", detachedQuiet)
		}
	})
}
