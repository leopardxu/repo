package repo_sync

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/leopardxu/repo-go/internal/git"
	"github.com/leopardxu/repo-go/internal/manifest"
	"github.com/leopardxu/repo-go/internal/project"
)

// Superproject 表示超级项目
type Superproject struct {
	manifest *manifest.Manifest
	quiet    bool
	gitdir   string
	worktree string
	gitRepo  *git.Repository
}

// NewSuperproject 创建一个新的超级项目
func NewSuperproject(manifest *manifest.Manifest, quiet bool) (*Superproject, error) {
	// 创建超级项目目录
	gitdir := filepath.Join(manifest.Subdir, "superproject")
	worktree := filepath.Join(manifest.Topdir, ".superproject")

	// 创建超级项目
	sp := &Superproject{
		manifest: manifest,
		quiet:    quiet,
		gitdir:   gitdir,
		worktree: worktree,
		// 修复 git.NewRunner() 调用
		gitRepo: git.NewRepository(worktree, git.NewRunner()),
	}

	// 初始化超级项目
	if err := sp.init(); err != nil {
		return nil, err
	}

	return sp, nil
}

// init 初始化超级项目
func (sp *Superproject) init() error {
	// 检查超级项目目录是否存在
	if _, err := os.Stat(sp.gitdir); os.IsNotExist(err) {
		// 创建超级项目目录
		if err := os.MkdirAll(sp.gitdir, 0755); err != nil {
			return fmt.Errorf("创建超级项目目录failed: %w", err)
		}

		// 初始化超级项目
		if _, err := sp.gitRepo.RunCommand("init", "--bare"); err != nil {
			return fmt.Errorf("初始化超级项目failed: %w", err)
		}
	}

	// 检查工作树是否存在
	if _, err := os.Stat(sp.worktree); os.IsNotExist(err) {
		// 创建工作树
		if err := os.MkdirAll(sp.worktree, 0755); err != nil {
			return fmt.Errorf("创建超级项目工作树failed: %w", err)
		}

		// 初始化工作树
		if _, err := sp.gitRepo.RunCommand("checkout", "-f", "HEAD"); err != nil {
			return fmt.Errorf("初始化超级项目工作树failed: %w", err)
		}
	}

	return nil
}

// UpdateProjectsRevisionID 从超级项目更新项目的修订ID
func (sp *Superproject) UpdateProjectsRevisionID(projects []*project.Project) (string, error) {
	// 获取超级项目的远程URL
	// 修复字段名称，使用自定义属性
	superprojectRemote, ok := sp.manifest.GetCustomAttr("superproject-remote")
	if !ok || superprojectRemote == "" {
		return "", fmt.Errorf("清单中未定义超级项目远程仓库")
	}

	// 获取超级项目的分支
	// 修复字段名称，使用自定义属性
	superprojectBranch, ok := sp.manifest.GetCustomAttr("superproject-branch")
	if !ok || superprojectBranch == "" {
		return "", fmt.Errorf("清单中未定义超级项目分支")
	}

	// 添加远程仓库
	if _, err := sp.gitRepo.RunCommand("remote", "add", "origin", superprojectRemote); err != nil {
		// 如果远程仓库已存在，则设置URL
		if _, err := sp.gitRepo.RunCommand("remote", "set-url", "origin", superprojectRemote); err != nil {
			return "", fmt.Errorf("设置超级项目远程仓库failed: %w", err)
		}
	}

	// 获取超级项目
	if !sp.quiet {
		fmt.Printf("获取超级项目 %s\n", superprojectRemote)
	}

	// 获取超级项目
	if _, err := sp.gitRepo.RunCommand("fetch", "origin", superprojectBranch); err != nil {
		return "", fmt.Errorf("获取超级项目failed: %w", err)
	}

	// 检出超级项目
	if _, err := sp.gitRepo.RunCommand("checkout", "FETCH_HEAD"); err != nil {
		return "", fmt.Errorf("检出超级项目failed: %w", err)
	}

	// 获取超级项目的提交ID
	superprojectCommitID, err := sp.gitRepo.RunCommand("rev-parse", "HEAD")
	if err != nil {
		return "", fmt.Errorf("获取超级项目提交IDfailed: %w", err)
	}
	superprojectCommitIDStr := strings.TrimSpace(string(superprojectCommitID))
	superprojectCommitID = []byte(superprojectCommitIDStr)

	// 创建超级项目清单
	manifestPath := filepath.Join(sp.manifest.Subdir, "superproject-manifest.xml")

	// 创建清单内容
	manifestContent := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<manifest>
  <remote name="superproject" fetch="%s" />
  <default remote="superproject" revision="%s" />
`, superprojectRemote, superprojectCommitID)

	// 添加项目
	for _, project := range projects {
		// failed to get project在超级项目中的提交ID
		projectPath := project.Path
		projectCommitIDBytes, err := sp.gitRepo.RunCommand("ls-tree", "HEAD", projectPath)
		if err != nil {
			continue
		}
		// 解析git ls-tree输出
		parts := strings.Fields(string(projectCommitIDBytes))
		if len(parts) < 4 {
			continue
		}
		projectCommitID := parts[2]

		// 添加项目到清单
		manifestContent += fmt.Sprintf(`  <project name="%s" path="%s" revision="%s" />
`, project.Name, projectPath, projectCommitID)

		// 更新项目的修订ID
		project.RevisionID = string(projectCommitID)
	}

	// 关闭清单
	manifestContent += `</manifest>
`

	// 写入清单文件
	if err := os.WriteFile(manifestPath, []byte(manifestContent), 0644); err != nil {
		return "", fmt.Errorf("写入超级项目清单failed: %w", err)
	}

	return manifestPath, nil
}
