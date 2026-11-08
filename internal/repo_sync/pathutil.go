package repo_sync

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/leopardxu/repo-go/internal/manifest"
)

// resolveRepoMetadataDir 返回 .repo 元数据目录的路径。
// RepoDir 为相对路径时以 Topdir 为基准，确保输出始终落在仓库根的 .repo 下，
// 而不是当前工作目录（CLI 之外调用时 cwd 未必是仓库根）。
func resolveRepoMetadataDir(m *manifest.Manifest) string {
	if m == nil {
		return ".repo"
	}
	if m.RepoDir != "" {
		if filepath.IsAbs(m.RepoDir) {
			return m.RepoDir
		}
		if m.Topdir != "" {
			return filepath.Join(m.Topdir, m.RepoDir)
		}
		return m.RepoDir
	}
	if m.Topdir != "" {
		return filepath.Join(m.Topdir, ".repo")
	}
	return ".repo"
}

// IsSafeToDelete 检查路径是否可以安全删除
// 返回 nil 表示安全，返回 error 表示不安全，包含原因
func IsSafeToDelete(worktreePath string, repoRoot string) error {
	if worktreePath == "" {
		return fmt.Errorf("working directory路径为空")
	}
	if repoRoot == "" {
		return fmt.Errorf("repo 根目录为空，无法校验删除范围，deletion refused")
	}

	// 获取绝对路径
	absWorktree, err := filepath.Abs(worktreePath)
	if err != nil {
		return fmt.Errorf("无法获取working directory绝对路径: %w", err)
	}
	absRepoRoot, err := filepath.Abs(repoRoot)
	if err != nil {
		return fmt.Errorf("无法获取 repo 根目录绝对路径: %w", err)
	}

	// 检查是否为危险路径
	if isDangerousPath(worktreePath, absWorktree) {
		return fmt.Errorf("working directory %s is a system critical directory, deletion is forbidden", worktreePath)
	}

	// 目标就是 repo 根目录本身：删除它会销毁整个客户端，必须拒绝
	if absWorktree == absRepoRoot {
		return fmt.Errorf("working directory %s 是 repo 根目录本身，deletion refused", worktreePath)
	}

	// 使用 filepath.Rel 做分隔符感知的包含判断，避免 HasPrefix 把
	// /repo 与 /repo-sibling 这类兄弟目录误判为位于 repo 根目录下。
	rel, err := filepath.Rel(absRepoRoot, absWorktree)
	if err != nil {
		return fmt.Errorf("无法计算 working directory 与 repo 根目录的相对路径: %w", err)
	}
	if rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("working directory %s 不在 repo 根目录 %s 下，deletion refused", worktreePath, repoRoot)
	}

	return nil
}

// isDangerousPath 检查给定路径是否为危险路径（不应该被删除的系统目录）
func isDangerousPath(rawPath, absPath string) bool {
	// 检查常见的危险相对路径
	if rawPath == "." || rawPath == ".." ||
		strings.HasPrefix(rawPath, "../") || strings.HasPrefix(rawPath, "..\\") {
		return true
	}

	// 检查根目录
	if absPath == "/" || absPath == "\\" {
		return true
	}

	// Windows 根目录检查
	if filepath.VolumeName(absPath) == absPath {
		return true
	}

	// 检查 Windows 卷根 (如 "C:\")
	if len(absPath) == 3 && absPath[1] == ':' && (absPath[2] == '\\' || absPath[2] == '/') {
		return true
	}

	return false
}
