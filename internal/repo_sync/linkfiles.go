package repo_sync

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/leopardxu/repo-go/internal/project"
)

// splitPathComponents 按 "/" 与当前平台的路径分隔符拆分路径组件。
// 上游清单的作者始终使用 "/"，但 Windows 上 os 层会把 "/" 规范成 "\"，
// 因此必须同时识别两种分隔符，避免绕过后续的安全检查。
func splitPathComponents(subpath string) []string {
	return strings.FieldsFunc(subpath, func(r rune) bool {
		return r == '/' || r == os.PathSeparator
	})
}

// safeExpandPath 等价于上游 _SafeExpandPath：确保 subpath 完全落在 base 之内。
// 规则：
//   - 绝对路径（含 Windows 卷名与前导分隔符）一律按 base 下的相对路径处理，绝不逃逸；
//   - 拒绝任何 "."、".." 组件；
//   - 逐级 Lstat，禁止穿越符号链接；
//   - 中间路径只允许普通文件或目录；
//   - skipFinal 为 true 时由调用方自行处理最终组件（linkfile 的最终目标可以已存在）。
func safeExpandPath(base, subpath string, skipFinal bool) (string, error) {
	// 去掉卷名（如 Windows 的 "C:"），绝对路径因此退化为相对 base 的路径
	if volume := filepath.VolumeName(subpath); volume != "" {
		subpath = strings.TrimPrefix(subpath, volume)
	}
	components := splitPathComponents(subpath)

	finalPart := ""
	if skipFinal {
		if len(components) == 0 {
			return "", fmt.Errorf("%q: 路径为空，无法确定最终组件", subpath)
		}
		finalPart = components[len(components)-1]
		components = components[:len(components)-1]
		// 最终组件也必须是普通名字，防止通过 ".." 把目标解析到 base 之外
		if finalPart == "." || finalPart == ".." {
			return "", fmt.Errorf("%q: 路径中不允许出现 %q 组件", subpath, finalPart)
		}
	}

	path := base
	for _, part := range components {
		if part == "." || part == ".." {
			return "", fmt.Errorf("%q: 路径中不允许出现 %q 组件", subpath, part)
		}

		path = filepath.Join(path, part)

		info, err := os.Lstat(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return "", fmt.Errorf("检查路径 %s failed: %w", path, err)
		}
		// 禁止穿越符号链接
		if info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("%s: 禁止穿越符号链接", path)
		}
		// 只允许普通文件与目录（拒绝 socket、fifo、设备文件等特殊文件）
		if !info.IsDir() && !info.Mode().IsRegular() {
			return "", fmt.Errorf("%s: 只允许普通文件或目录", path)
		}
	}

	if skipFinal {
		path = filepath.Join(path, finalPart)
	}
	return path, nil
}

// expandProjectSourcePath 解析项目内的源路径（copyfile/linkfile 的 src）。
// src="." 是上游允许的特例，表示项目 worktree 根目录本身。
func expandProjectSourcePath(worktree, src string) (string, error) {
	if src == "." {
		return worktree, nil
	}
	return safeExpandPath(worktree, src, false)
}

// processLinkAndCopyFiles 处理项目中的 linkfile copyfile
func (e *Engine) processLinkAndCopyFiles(p *project.Project) error {
	if p == nil {
		return fmt.Errorf("project object is nil")
	}

	e.logger.Info("开始处理项目 %s 的 linkfile 和 copyfile", p.Name)

	// 只读 e.repoRoot，避免并发 worker 之间的数据竞争；为空时退化为局部值
	repoRoot := e.repoRoot
	if repoRoot == "" {
		// 优先使用清单中的顶级目录
		if e.manifest != nil && e.manifest.Topdir != "" {
			repoRoot = e.manifest.Topdir
			e.logger.Info("从清单中获取仓库根目录: %s", repoRoot)
		} else {
			// 如果清单中没有顶级目录，尝试从当前 working directory 推断
			cwd, err := os.Getwd()
			if err != nil {
				return fmt.Errorf("failed to get current directory: %w", err)
			}

			// 查找顶层仓库目录
			topDir := project.FindTopLevelRepoDir(cwd)
			if topDir != "" {
				repoRoot = topDir
				e.logger.Info("从当前 working directory 推断仓库根目录: %s", repoRoot)
			} else {
				// 如果找不到顶层目录，使用当前目录
				repoRoot = cwd
				e.logger.Info("使用当前 working directory 作为仓库根目录: %s", repoRoot)
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
		projectRoot = filepath.Join(repoRoot, p.Worktree)
		e.logger.Debug("使用相对路径作为项目根目录: %s", projectRoot)
	}

	// 处理 Copyfile
	for i, cpFile := range p.Copyfiles {
		e.logger.Info("处理第 %d 个 copyfile: src=%s, dest=%s", i+1, cpFile.Src, cpFile.Dest)

		// 源文件必须在项目 worktree 内
		sourcePath, err := expandProjectSourcePath(projectRoot, cpFile.Src)
		if err != nil {
			return fmt.Errorf("项目 %s 的 copyfile 源路径 %q 非法: %w", p.Name, cpFile.Src, err)
		}

		// 目标必须在 repoRoot 内（绝对路径同样按 repoRoot 下相对路径处理）
		destPath, err := safeExpandPath(repoRoot, cpFile.Dest, false)
		if err != nil {
			return fmt.Errorf("项目 %s 的 copyfile 目标路径 %q 非法: %w", p.Name, cpFile.Dest, err)
		}

		e.logger.Info("复制文件: 从 %s 到 %s", sourcePath, destPath)

		// 检查源文件是否存在
		srcInfo, err := os.Stat(sourcePath)
		if err != nil {
			if os.IsNotExist(err) {
				e.logger.Warn("源文件 %s 不存在，跳过复制", sourcePath)
				continue
			}
			return fmt.Errorf("项目 %s: 无法读取 copyfile 源 %s 信息: %w", p.Name, sourcePath, err)
		}
		if srcInfo.IsDir() {
			return fmt.Errorf("项目 %s 的 copyfile 源 %s 是目录，不支持复制目录", p.Name, sourcePath)
		}

		// 目标已存在时进行校验：目录拒绝覆盖，符号链接拒绝写入（防止写穿）
		if destInfo, statErr := os.Lstat(destPath); statErr == nil {
			if destInfo.IsDir() {
				return fmt.Errorf("项目 %s 的 copyfile 目标 %s 是目录，拒绝复制", p.Name, destPath)
			}
			if destInfo.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("项目 %s 的 copyfile 目标 %s 是符号链接，拒绝写入", p.Name, destPath)
			}
		} else if !os.IsNotExist(statErr) {
			return fmt.Errorf("项目 %s: 无法检查 copyfile 目标 %s: %w", p.Name, destPath, statErr)
		}

		// 所有校验通过后才读取与写入
		input, err := os.ReadFile(sourcePath)
		if err != nil {
			return fmt.Errorf("读取 copyfile 源文件 %s failed: %w", sourcePath, err)
		}

		// 确保目标目录存在
		destDir := filepath.Dir(destPath)
		if err := os.MkdirAll(destDir, 0755); err != nil {
			return fmt.Errorf("创建 copyfile 目标目录 %s failed: %w", destDir, err)
		}

		// 写入前再次确认目标没有被替换为符号链接
		if fi, err := os.Lstat(destPath); err == nil && fi.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("项目 %s 的 copyfile 目标 %s 是符号链接，拒绝写入", p.Name, destPath)
		}

		if err := os.WriteFile(destPath, input, 0644); err != nil {
			return fmt.Errorf("写入 copyfile 目标文件 %s failed: %w", destPath, err)
		}

		e.logger.Info("成功复制文件: 从 %s 到 %s", sourcePath, destPath)
	}

	// 处理 Linkfile
	for i, lnFile := range p.Linkfiles {
		e.logger.Info("处理第 %d 个 linkfile: src=%s, dest=%s", i+1, lnFile.Src, lnFile.Dest)

		// 源文件必须在项目 worktree 内（src="." 表示 worktree 根）
		targetPath, err := expandProjectSourcePath(projectRoot, lnFile.Src)
		if err != nil {
			return fmt.Errorf("项目 %s 的 linkfile 源路径 %q 非法: %w", p.Name, lnFile.Src, err)
		}

		// 上游 _Link 支持 src 通配符：dest 视为目录，逐个链接匹配文件
		if hasGlobMagic(lnFile.Src) {
			if err := e.processGlobLinkfile(p, repoRoot, targetPath, lnFile.Dest); err != nil {
				return err
			}
			continue
		}

		// 检查源路径是否存在
		if _, err := os.Stat(targetPath); err != nil {
			if os.IsNotExist(err) {
				e.logger.Warn("链接源路径 %s 不存在，跳过创建链接", targetPath)
				continue
			}
			return fmt.Errorf("项目 %s: 无法访问 linkfile 源 %s: %w", p.Name, targetPath, err)
		}

		// 链接位置必须在 repoRoot 内；最终组件允许已存在（会在下方安全删除后重建）
		linkPath, err := safeExpandPath(repoRoot, lnFile.Dest, true)
		if err != nil {
			return fmt.Errorf("项目 %s 的 linkfile 目标路径 %q 非法: %w", p.Name, lnFile.Dest, err)
		}

		if err := e.createLink(p, linkPath, targetPath); err != nil {
			return err
		}
	}

	return nil
}

// hasGlobMagic 判断路径是否包含 filepath.Glob 支持的通配符。
func hasGlobMagic(path string) bool {
	return strings.ContainsAny(path, "*?[")
}

// processGlobLinkfile 处理 src 含通配符的 linkfile（对齐上游 _Link）：
// dest 必须是目录，工作树内每个匹配项以其 basename 链接到 dest 下。
func (e *Engine) processGlobLinkfile(p *project.Project, repoRoot, pattern, dest string) error {
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return fmt.Errorf("项目 %s 的 linkfile 通配符 %q 非法: %w", p.Name, pattern, err)
	}
	if len(matches) == 0 {
		e.logger.Warn("链接源通配符 %s 没有匹配文件，跳过", pattern)
		return nil
	}

	destDir, err := safeExpandPath(repoRoot, dest, false)
	if err != nil {
		return fmt.Errorf("项目 %s 的 linkfile 目标路径 %q 非法: %w", p.Name, dest, err)
	}
	// dest 可以是尚不存在的目录；已存在时必须是目录
	if info, statErr := os.Stat(destDir); statErr == nil {
		if !info.IsDir() {
			return fmt.Errorf("项目 %s: linkfile 通配符目标 %s 不是目录", p.Name, destDir)
		}
	} else if !os.IsNotExist(statErr) {
		return fmt.Errorf("项目 %s: 无法访问 linkfile 目标 %s: %w", p.Name, destDir, statErr)
	}
	if err := os.MkdirAll(destDir, 0755); err != nil {
		return fmt.Errorf("创建 linkfile 目标目录 %s failed: %w", destDir, err)
	}

	sort.Strings(matches)
	for _, match := range matches {
		base := filepath.Base(match)
		if base == "." || base == ".." || base == string(filepath.Separator) {
			continue
		}
		if err := e.createLink(p, filepath.Join(destDir, base), match); err != nil {
			return err
		}
	}
	return nil
}

// createLink 在 linkPath 处创建指向 targetPath 的符号链接（已存在则安全替换）。
func (e *Engine) createLink(p *project.Project, linkPath, targetPath string) error {
	e.logger.Info("创建链接: 从 %s 指向 %s", linkPath, targetPath)

	linkDir := filepath.Dir(linkPath)
	if err := os.MkdirAll(linkDir, 0755); err != nil {
		return fmt.Errorf("创建 linkfile 目标目录 %s failed: %w", linkDir, err)
	}

	if _, err := os.Lstat(linkPath); err == nil {
		e.logger.Info("链接 %s 已存在，将删除", linkPath)
		if err := os.Remove(linkPath); err != nil {
			return fmt.Errorf("删除已存在的链接 %s failed: %w", linkPath, err)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("项目 %s: 无法检查链接路径 %s: %w", p.Name, linkPath, err)
	}

	relTargetPath, err := filepath.Rel(linkDir, targetPath)
	if err != nil {
		relTargetPath = targetPath
		e.logger.Info("无法计算相对路径，将为链接 %s 使用绝对目标路径 %s: %v", linkPath, targetPath, err)
	} else {
		e.logger.Debug("计算得到相对路径: %s", relTargetPath)
	}

	if err := os.Symlink(relTargetPath, linkPath); err != nil {
		return fmt.Errorf("创建符号链接从 %s 到 %s failed: %w", linkPath, relTargetPath, err)
	}
	e.logger.Info("成功创建链接: %s -> %s", linkPath, relTargetPath)
	return nil
}
