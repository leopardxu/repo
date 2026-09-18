package repo_sync

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/leopardxu/repo-go/internal/project"
)

// processLinkAndCopyFiles 处理项目中的 linkfile copyfile
func (e *Engine) processLinkAndCopyFiles(p *project.Project) error {
	if p == nil {
		return fmt.Errorf("project object is nil")
	}

	e.logger.Info("开始处理项目 %s 的 linkfile 和 copyfile", p.Name)

	// 首先确保 repoRoot 已正确设置，使用更可靠的初始化逻辑
	if e.repoRoot == "" {
		// 优先使用清单中的顶级目录
		if e.manifest != nil && e.manifest.Topdir != "" {
			e.repoRoot = e.manifest.Topdir
			e.logger.Info("从清单中获取仓库根目录: %s", e.repoRoot)
		} else {
			// 如果清单中没有顶级目录，尝试从当前working directory推断
			cwd, err := os.Getwd()
			if err != nil {
				return fmt.Errorf("failed to get current directory: %w", err)
			}

			// 查找顶层仓库目录
			topDir := project.FindTopLevelRepoDir(cwd)
			if topDir != "" {
				e.repoRoot = topDir
				e.logger.Info("从当前working directory推断仓库根目录: %s", e.repoRoot)
			} else {
				// 如果找不到顶层目录，使用当前目录
				e.repoRoot = cwd
				e.logger.Info("使用当前working directory作为仓库根目录: %s", e.repoRoot)
			}
		}
	}

	// 记录项目的 Linkfiles 和 Copyfiles 数量
	e.logger.Info("项目 %s 有 %d 个 linkfile 和 %d 个 copyfile 需要处理",
		p.Name, len(p.Linkfiles), len(p.Copyfiles))

	// 确定项目根目录
	var projectRoot string
	if filepath.IsAbs(p.Worktree) {
		// 如果工作树是绝对路径，直接使用
		projectRoot = p.Worktree
		e.logger.Debug("使用绝对路径作为项目根目录: %s", projectRoot)
	} else {
		// 如果工作树是相对路径，相对于仓库根目录
		projectRoot = filepath.Join(e.repoRoot, p.Worktree)
		e.logger.Debug("使用相对路径作为项目根目录: %s", projectRoot)
	}

	// 处理 Copyfile
	for i, cpFile := range p.Copyfiles {
		e.logger.Info("处理第 %d 个 copyfile: src=%s, dest=%s", i+1, cpFile.Src, cpFile.Dest)

		// 源文件路径处理
		var sourcePath string
		if cpFile.Src == "." {
			// 如果源是当前目录，使用项目根目录
			sourcePath = projectRoot
			e.logger.Info("源路径是当前目录，使用项目根目录: %s", sourcePath)
		} else {
			// 否则，源文件在项目内部
			sourcePath = filepath.Join(projectRoot, cpFile.Src)
			e.logger.Info("源路径: %s", sourcePath)
		}

		// 目标文件路径处理
		var destPath string
		if filepath.IsAbs(cpFile.Dest) {
			// 如果目标是绝对路径，直接使用
			destPath = cpFile.Dest
			e.logger.Info("目标路径是绝对路径: %s", destPath)
		} else {
			// 如果目标是相对路径，相对于仓库根目录
			destPath = filepath.Join(e.repoRoot, cpFile.Dest)
			e.logger.Info("目标路径是相对路径，相对于仓库根目录: %s", destPath)
		}

		e.logger.Info("复制文件: 从 %s 到 %s", sourcePath, destPath)

		// 检查源文件是否存在
		if _, err := os.Stat(sourcePath); os.IsNotExist(err) {
			e.logger.Error("源文件 %s 不存在，跳过复制", sourcePath)
			continue
		}

		// failed to read source file
		input, err := os.ReadFile(sourcePath)
		if err != nil {
			e.logger.Error("failed to read source file %s failed: %v", sourcePath, err)
			return fmt.Errorf("failed to read source file %s failed: %w", sourcePath, err)
		}

		// 确保目标目录存在
		destDir := filepath.Dir(destPath)
		e.logger.Info("failed to create target directory: %s", destDir)
		if err := os.MkdirAll(destDir, 0755); err != nil {
			e.logger.Error("failed to create target directory %s failed: %v", destDir, err)
			return fmt.Errorf("failed to create target directory %s failed: %w", destDir, err)
		}

		// failed to write target file
		if err := os.WriteFile(destPath, input, 0644); err != nil {
			e.logger.Error("failed to write target file %s failed: %v", destPath, err)
			return fmt.Errorf("failed to write target file %s failed: %w", destPath, err)
		}

		e.logger.Info("成功复制文件: 从 %s 到 %s", sourcePath, destPath)
	}

	// 处理 Linkfile
	for i, lnFile := range p.Linkfiles {
		e.logger.Info("处理第 %d 个 linkfile: src=%s, dest=%s", i+1, lnFile.Src, lnFile.Dest)

		// 源文件路径处理
		var targetPath string
		if lnFile.Src == "." {
			// 如果源是当前目录，使用项目根目录
			targetPath = projectRoot
			e.logger.Info("链接源路径是当前目录，使用项目根目录: %s", targetPath)
		} else {
			// 否则，源文件在项目内部
			targetPath = filepath.Join(projectRoot, lnFile.Src)
			e.logger.Info("链接源路径: %s", targetPath)
		}

		// 检查源路径是否存在
		if _, err := os.Stat(targetPath); os.IsNotExist(err) {
			e.logger.Error("链接源路径 %s 不存在，跳过创建链接", targetPath)
			continue
		}

		// 链接文件路径处理
		var linkPath string
		if filepath.IsAbs(lnFile.Dest) {
			// 如果目标是绝对路径，直接使用
			linkPath = lnFile.Dest
			e.logger.Info("链接目标路径是绝对路径: %s", linkPath)
		} else {
			// 如果目标是相对路径，相对于仓库根目录
			linkPath = filepath.Join(e.repoRoot, lnFile.Dest)
			e.logger.Info("链接目标路径是相对路径，相对于仓库根目录: %s", linkPath)
		}

		e.logger.Info("创建链接: 从 %s 指向 %s", linkPath, targetPath)

		// 创建链接前，确保目标目录存在
		linkDir := filepath.Dir(linkPath)
		e.logger.Info("failed to create link target directory: %s", linkDir)
		if err := os.MkdirAll(linkDir, 0755); err != nil {
			e.logger.Error("failed to create link target directory %s failed: %v", linkDir, err)
			return fmt.Errorf("failed to create link target directory %s failed: %w", linkDir, err)
		}

		// 如果链接已存在，先删除
		if _, err := os.Lstat(linkPath); err == nil {
			e.logger.Info("链接 %s 已存在，将删除", linkPath)
			if err := os.Remove(linkPath); err != nil {
				e.logger.Error("failed to remove existing link %s failed: %v", linkPath, err)
				return fmt.Errorf("failed to remove existing link %s failed: %w", linkPath, err)
			}
		}

		// 计算相对路径
		linkDir = filepath.Dir(linkPath) // 重新获取，确保准确性
		relTargetPath, err := filepath.Rel(linkDir, targetPath)
		if err != nil {
			// 如果无法计算相对路径，则直接使用绝对路径
			relTargetPath = targetPath
			e.logger.Info("无法计算相对路径，将为链接 %s 使用绝对目标路径 %s: %v", linkPath, targetPath, err)
		} else {
			e.logger.Info("计算得到相对路径: %s", relTargetPath)
		}

		// failed to create symlink
		e.logger.Info("failed to create symlink: %s -> %s", linkPath, relTargetPath)
		if err := os.Symlink(relTargetPath, linkPath); err != nil {
			e.logger.Error("failed to create symlink从 %s 到 %s failed: %v", linkPath, relTargetPath, err)
			return fmt.Errorf("failed to create symlink从 %s 到 %s failed: %w", linkPath, relTargetPath, err)
		}

		e.logger.Info("成功创建链接: %s -> %s", linkPath, relTargetPath)
	}

	return nil
}
