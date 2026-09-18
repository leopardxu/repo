package repo_sync

import (
	"fmt"
	"path/filepath"
	"strings"
)

// IsSafeToDelete 检查路径是否可以安全删除
// 返回 nil 表示安全，返回 error 表示不安全，包含原因
func IsSafeToDelete(worktreePath string, repoRoot string) error {
	if worktreePath == "" {
		return fmt.Errorf("working directory路径为空")
	}

	// 获取绝对路径
	absWorktree, err := filepath.Abs(worktreePath)
	if err != nil {
		return fmt.Errorf("无法获取working directory绝对路径: %w", err)
	}

	// 检查是否为危险路径
	if isDangerousPath(worktreePath, absWorktree) {
		return fmt.Errorf("working directory %s is a system critical directory, deletion is forbidden", worktreePath)
	}

	// 如果 repoRoot 不为空，检查路径是否在允许范围内
	if repoRoot != "" {
		absRepoRoot, err := filepath.Abs(repoRoot)
		if err != nil {
			return fmt.Errorf("无法获取 repo 根目录绝对路径: %w", err)
		}

		// 检查是否在 repoRoot 下
		inRepoDir := strings.HasPrefix(absWorktree, absRepoRoot+string(filepath.Separator)) ||
			absWorktree == absRepoRoot
		if !inRepoDir {
			return fmt.Errorf("working directory %s 不在 repo 根目录 %s 下，deletion refused", worktreePath, repoRoot)
		}
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
