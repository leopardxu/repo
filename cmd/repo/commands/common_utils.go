package commands

import (
	"fmt"
	"os"

	"github.com/leopardxu/repo-go/internal/config"
	"github.com/leopardxu/repo-go/internal/logger"
	"github.com/leopardxu/repo-go/internal/project"
)

// dedupeProjects 按项目身份（名称 + 工作区相对路径）去重，保留首次出现顺序。
// 重复的项目参数不应导致同一项目被重复处理（例如并发重复删除分支）。
// abandon/branch/checkout/prune/rebase 共用此实现，避免同包重复定义。
func dedupeProjects(projects []*project.Project) []*project.Project {
	seen := make(map[string]bool, len(projects))
	out := make([]*project.Project, 0, len(projects))
	for _, p := range projects {
		if p == nil {
			continue
		}
		key := p.Name + "\x00" + p.Path
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, p)
	}
	return out
}

// EnsureRepoRoot 确保当前working directory在repo根目录下
// 如果不在，则切换到repo根目录
// 返回原始working directory，以便在需要时恢复
func EnsureRepoRoot(log logger.Logger) (string, error) {
	// 获取当前working directory
	cwd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("failed to get current directory: %w", err)
	}

	// 查找repo根目录
	repoRoot, err := config.GetRepoRoot()
	if err != nil {
		return "", fmt.Errorf("failed to locate repo root: %w", err)
	}

	// 如果当前目录不是repo根目录，切换到repo根目录
	if cwd != repoRoot {
		log.Debug("切换working directory: %s -> %s", cwd, repoRoot)
		if err := os.Chdir(repoRoot); err != nil {
			return "", fmt.Errorf("failed to change to repo root: %w", err)
		}
	}

	return cwd, nil
}

// RestoreWorkDir 恢复到原始working directory
func RestoreWorkDir(originalDir string, log logger.Logger) error {
	if originalDir == "" {
		return nil
	}

	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("failed to get current directory: %w", err)
	}

	if cwd != originalDir {
		log.Debug("恢复working directory: %s -> %s", cwd, originalDir)
		if err := os.Chdir(originalDir); err != nil {
			return fmt.Errorf("failed to restore working directory: %w", err)
		}
	}

	return nil
}
