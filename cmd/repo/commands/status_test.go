package commands

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// TestComputeOrphanDirs 验证 --orphans 的孤儿目录识别：
// 顶层目录被某项目相对路径覆盖（相等或为前缀段）则非孤儿；.repo 始终跳过；
// 普通文件不纳入；结果按目录名排序。
func TestComputeOrphanDirs(t *testing.T) {
	root := t.TempDir()

	// 构造工作树顶层结构
	mustMkdir(t, filepath.Join(root, "art"))           // 项目
	mustMkdir(t, filepath.Join(root, "external"))      // 嵌套项目 external/foo 的前缀段
	mustMkdir(t, filepath.Join(root, "build"))         // 项目
	mustMkdir(t, filepath.Join(root, "leftover"))      // 孤儿
	mustMkdir(t, filepath.Join(root, "junk"))          // 孤儿
	mustMkdir(t, filepath.Join(root, ".repo"))         // 始终跳过
	mustWriteFile(t, filepath.Join(root, "README.md")) // 普通文件，忽略

	// 项目相对路径（含嵌套）
	relPaths := []string{"art", "external/foo", "build"}

	got, err := computeOrphanDirs(root, relPaths)
	if err != nil {
		t.Fatalf("computeOrphanDirs 失败: %v", err)
	}

	want := []string{"junk", "leftover"}
	if !sort.StringsAreSorted(got) {
		t.Errorf("结果未排序: %v", got)
	}
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
