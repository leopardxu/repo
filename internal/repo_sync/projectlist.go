package repo_sync

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// projectListFile 是 .repo 下记录上次同步项目路径的文件名（对齐上游 repo）。
const projectListFile = "project.list"

// projectListPath 返回当前 manifest 对应的 project.list 路径。
func (e *Engine) projectListPath() string {
	return filepath.Join(resolveRepoMetadataDir(e.manifest), projectListFile)
}

// updateProjectList 对齐上游 subcmds/sync.py 的 UpdateProjectList：
//   - 把当前清单选中的项目路径写回 .repo/project.list；
//   - 删除旧列表中存在、当前清单已不存在的项目检出；
//   - 工作树脏/损坏且未指定 --force-remove-dirty 时报错，绝不静默丢弃改动；
//   - 镜像/归档仓库没有工作树，跳过。
//
// 返回的错误由调用方计入同步失败；存在任何错误时不写回新列表，保持旧状态，
// 使下次同步仍能重新发现并处理这些过期项目（与上游行为一致）。
func (e *Engine) updateProjectList() []error {
	if e == nil || e.options == nil || e.options.DryRun {
		return nil
	}
	if e.options.Config != nil && e.options.Config.Mirror {
		return nil
	}
	if e.manifest != nil && e.manifest.IsArchive {
		return nil
	}
	// 空项目列表通常是过滤异常而非“清单已清空”，避免据此删除全部检出。
	if len(e.projects) == 0 {
		return nil
	}

	selected := make(map[string]bool, len(e.projects))
	newPaths := make([]string, 0, len(e.projects))
	for _, p := range e.projects {
		if p == nil || p.Relpath == "" {
			continue
		}
		rel := normalizeProjectPath(p.Relpath)
		if rel == "" || selected[rel] {
			continue
		}
		selected[rel] = true
		newPaths = append(newPaths, rel)
	}
	if len(newPaths) == 0 {
		return nil
	}

	oldPaths, err := readProjectList(e.projectListPath())
	if err != nil {
		return []error{err}
	}

	var errs []error
	retained := make([]string, 0)
	// 逆序处理：先删子目录，再删父目录。
	for i := len(oldPaths) - 1; i >= 0; i-- {
		rel := oldPaths[i]
		if selected[rel] {
			continue
		}
		if hasSelectedDescendant(rel, selected) {
			// 父路径已不在清单中，但其下仍有有效项目：删除会连带销毁
			// 这些项目的检出，保守保留并继续记录在列表中。
			e.logger.Warn("跳过删除过期项目 %s：其下仍有清单中的项目", rel)
			retained = append(retained, rel)
			continue
		}
		if removeErr := e.removeObsoleteProject(rel); removeErr != nil {
			errs = append(errs, removeErr)
		}
	}

	if len(errs) > 0 {
		return errs
	}

	newPaths = append(newPaths, retained...)
	sort.Strings(newPaths)
	if writeErr := writeProjectList(e.projectListPath(), newPaths); writeErr != nil {
		return []error{writeErr}
	}
	return nil
}

// removeObsoleteProject 删除单个已从清单移除的项目工作树。
// 只处理带 .git 的检出；路径删除前经 IsSafeToDelete 校验，确保不会越出 repoRoot。
func (e *Engine) removeObsoleteProject(rel string) error {
	worktree := filepath.Join(e.repoRoot, filepath.FromSlash(rel))
	gitDir := filepath.Join(worktree, ".git")
	if _, err := os.Stat(gitDir); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// 工作树已不存在，无需处理。
			return nil
		}
		return fmt.Errorf("检查过期项目 %s 失败: %w", rel, err)
	}

	dirty, corrupted := e.projectWorktreeDirty(worktree)
	if dirty || corrupted {
		if !e.options.ForceRemoveDirty {
			if corrupted {
				return fmt.Errorf("项目 %s 已损坏，无法删除；如需强制删除请使用 --force-remove-dirty", rel)
			}
			return fmt.Errorf("项目 %s 有未提交改动，无法删除；如需强制删除请使用 --force-remove-dirty", rel)
		}
		if corrupted {
			e.logger.Warn("强制删除已损坏的过期项目 %s", rel)
		} else {
			e.logger.Warn("强制删除含未提交改动的过期项目 %s（改动将丢失）", rel)
		}
	} else {
		e.logger.Info("删除清单中已不存在的项目 %s", rel)
	}

	if err := IsSafeToDelete(worktree, e.repoRoot); err != nil {
		return fmt.Errorf("删除过期项目 %s 被拒绝: %w", rel, err)
	}
	if err := os.RemoveAll(worktree); err != nil {
		return fmt.Errorf("删除过期项目 %s 失败: %w", rel, err)
	}
	return nil
}

// projectWorktreeDirty 返回工作树是否有未提交改动（含未跟踪文件），
// 以及仓库是否已损坏（git 命令执行失败，无法判定状态）。
func (e *Engine) projectWorktreeDirty(worktree string) (dirty, corrupted bool) {
	out, err := e.runGitInDir(worktree, "status", "--porcelain")
	if err != nil {
		return false, true
	}
	return strings.TrimSpace(string(out)) != "", false
}

// readProjectList 读取 project.list，忽略空行与越界/绝对路径。
func readProjectList(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return []string{}, nil
		}
		return nil, fmt.Errorf("读取 %s 失败: %w", path, err)
	}

	paths := make([]string, 0)
	for _, line := range strings.Split(string(data), "\n") {
		rel := normalizeProjectPath(line)
		if rel == "" {
			continue
		}
		paths = append(paths, rel)
	}
	return paths, nil
}

// writeProjectList 原子写入 project.list（同目录临时文件 + rename）。
func writeProjectList(path string, paths []string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("创建 %s 失败: %w", dir, err)
	}

	content := ""
	if len(paths) > 0 {
		content = strings.Join(paths, "\n") + "\n"
	}

	tmp, err := os.CreateTemp(dir, projectListFile+".tmp-*")
	if err != nil {
		return fmt.Errorf("创建临时 project.list 失败: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		_ = os.Remove(tmpName) // rename 成功后该路径已不存在，失败时清理残留
	}()

	if _, err := tmp.WriteString(content); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("写入 %s 失败: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("关闭 %s 失败: %w", tmpName, err)
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		return fmt.Errorf("设置 %s 权限失败: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("更新 %s 失败: %w", path, err)
	}
	return nil
}

// normalizeProjectPath 把列表/清单中的相对路径规范化为斜杠分隔的形式；
// 空路径、绝对路径和越界路径返回空串，调用方据此跳过。
func normalizeProjectPath(raw string) string {
	clean := filepath.ToSlash(filepath.Clean(strings.TrimSpace(raw)))
	if clean == "" || clean == "." || clean == ".." ||
		strings.HasPrefix(clean, "../") || filepath.IsAbs(clean) {
		return ""
	}
	return clean
}

// hasSelectedDescendant 判断 rel 是否是 selected 中某个项目路径的祖先目录。
func hasSelectedDescendant(rel string, selected map[string]bool) bool {
	prefix := rel + "/"
	for path := range selected {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}
	return false
}
