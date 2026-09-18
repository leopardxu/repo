package manifest

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/leopardxu/repo-go/internal/logger"
)

// Merger 负责合并多个清单
type Merger struct {
	Parser  *Parser
	BaseDir string // 清单文件的基础目录
}

// NewMerger 创建清单合并器
func NewMerger(parser *Parser, baseDir string) *Merger {
	return &Merger{
		Parser:  parser,
		BaseDir: baseDir,
	}
}

// Merge 合并多个清单
func (m *Merger) Merge(manifests []*Manifest) (*Manifest, error) {
	if len(manifests) == 0 {
		return nil, fmt.Errorf("没有清单可合并")
	}

	if len(manifests) == 1 {
		return manifests[0], nil
	}

	// 使用第一个清单作为基础
	result := manifests[0]

	// 合并其他清单
	for i := 1; i < len(manifests); i++ {
		if err := m.mergeManifest(result, manifests[i]); err != nil {
			logger.Error("合并第%d 个清单failed: %v", i+1, err)
			return nil, err
		}
	}

	return result, nil
}

// mergeManifest 将src清单合并到dst清单
func (m *Merger) mergeManifest(dst, src *Manifest) error {
	if dst == nil || src == nil {
		return fmt.Errorf("源清单或目标清单为空")
	}

	// 合并远程
	remoteCount := 0
	for _, remote := range src.Remotes {
		// 检查是否已存在同名远程
		exists := false
		for _, r := range dst.Remotes {
			if r.Name == remote.Name {
				exists = true
				break
			}
		}

		// 如果不存在，添加到目标清单
		if !exists {
			dst.Remotes = append(dst.Remotes, remote)
			remoteCount++
		}
	}

	// 远程配置合并完成

	// 合并项目
	addedProjects := 0
	updatedProjects := 0
	skippedProjects := 0

	for _, project := range src.Projects {
		// 检查是否需要移除该项目
		skip := false
		for _, rp := range dst.RemoveProjects {
			if rp.Name == project.Name {
				skip = true
				// 跳过已标记为移除的项目
				break
			}
		}

		if skip {
			skippedProjects++
			continue
		}

		// 使用 name + path 组合作为唯一标识，支持同一仓库拉取到不同目录
		exists := false
		projectPath := project.Path
		if projectPath == "" {
			projectPath = project.Name // 如果 path 为空，使用 name 作为 path
		}

		for i, p := range dst.Projects {
			dstPath := p.Path
			if dstPath == "" {
				dstPath = p.Name
			}

			// 比较 name 和 path 的组合
			if p.Name == project.Name && dstPath == projectPath {
				// 更新现有项目（相同 name 和 path）
				logger.Debug("合并: 更新项目 name=%s, path=%s", project.Name, projectPath)
				dst.Projects[i] = project
				exists = true
				updatedProjects++
				break
			}
		}

		// 如果不存在，添加到目标清单（支持同名不同路径）
		if !exists {
			logger.Debug("合并: 添加新项目 name=%s, path=%s", project.Name, projectPath)
			dst.Projects = append(dst.Projects, project)
			addedProjects++
		}
	}

	// 项目合并完成
	logger.Info("合并项目统计: 添加=%d, 更新=%d, 跳过=%d", addedProjects, updatedProjects, skippedProjects)

	// 合并移除项目
	removedCount := 0
	addedRemoveProjects := 0

	for _, removeProject := range src.RemoveProjects {
		// 检查是否已存在同名移除项目
		exists := false
		for _, rp := range dst.RemoveProjects {
			if rp.Name == removeProject.Name {
				exists = true
				break
			}
		}

		// 如果不存在，添加到目标清
		if !exists {
			dst.RemoveProjects = append(dst.RemoveProjects, removeProject)
			addedRemoveProjects++
			// 添加移除项目标记
		}

		// 从项目列表中移除该项（注意：remove-project 仅根据 name 匹配，不考虑 path）
		for i := 0; i < len(dst.Projects); i++ {
			if dst.Projects[i].Name == removeProject.Name {
				// 移除项目
				dst.Projects = append(dst.Projects[:i], dst.Projects[i+1:]...)
				removedCount++
				i-- // 调整索引，因为删除了元素
				// 继续检查，因为可能有多个同名不同 path 的项目
			}
		}
	}

	// 移除项目处理完成

	return nil
}

// ProcessIncludes 处理清单中的include标签
func (m *Merger) ProcessIncludes(manifest *Manifest, groups []string) (*Manifest, error) {
	if manifest == nil {
		return nil, fmt.Errorf("清单不能为空")
	}

	if len(manifest.Includes) == 0 {
		return manifest, nil
	}

	// 处理包含的子清单

	// 收集所有需要合并的清单
	manifests := []*Manifest{manifest}

	// 处理包含的清单文
	for _, include := range manifest.Includes {
		includePath := filepath.Join(m.BaseDir, include.Name)
		// 处理包含的清单文件

		// 检查文件是否存
		if _, err := os.Stat(includePath); os.IsNotExist(err) {
			logger.Error("included manifest file does not exist: %s", includePath)
			return nil, fmt.Errorf("included manifest file does not exist: %s", includePath)
		}

		// 使用组过滤

		// 解析包含的清单文
		includeManifest, err := m.Parser.ParseFromFile(includePath, groups)
		if err != nil {
			logger.Error("failed to parse included manifest失 %s, 错误: %v", includePath, err)
			return nil, fmt.Errorf("failed to parse included manifest失 %w", err)
		}

		// 递归处理包含的清单中的include标签
		processedInclude, err := m.ProcessIncludes(includeManifest, groups)
		if err != nil {
			logger.Error("处理包含的清单中的包含标签failed: %v", err)
			return nil, err
		}

		manifests = append(manifests, processedInclude)
	}

	// 合并所有清单
	return m.Merge(manifests)
}
