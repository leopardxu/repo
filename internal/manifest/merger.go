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

// mergeManifest 将 src 清单合并到 dst 清单。合并语义：
//   - remote/default/项目只补空或追加，不覆盖 dst 已有的非空字段；
//   - remove-project 使用 path 匹配语义；optional 条目仅表示“目标缺失可接受”，
//     不得作为后续清单项目的过滤条件，也不向外传播；
//   - 非 optional 的 remove 未命中时追加到 dst 供外层执行。
func (m *Merger) mergeManifest(dst, src *Manifest) error {
	if dst == nil || src == nil {
		return fmt.Errorf("源清单或目标清单为空")
	}

	// 合并远程（同名 remote 只补空字段，不覆盖）
	for _, remote := range src.Remotes {
		idx := -1
		for i := range dst.Remotes {
			if dst.Remotes[i].Name == remote.Name {
				idx = i
				break
			}
		}
		if idx < 0 {
			dst.Remotes = append(dst.Remotes, remote)
			continue
		}
		mergeRemoteValues(&dst.Remotes[idx], remote)
	}

	// 合并 default（只补空）
	mergeDefaultValues(&dst.Default, src.Default)

	// 合并项目（相同 name+path 只补空，不覆盖）
	addedProjects := 0
	updatedProjects := 0
	skippedProjects := 0

	for _, project := range src.Projects {
		// 检查是否需要移除该项目（沿用 remove-project 的 path 语义）。
		// optional 的悬空 remove 仅表示“目标缺失可接受”，不得作为后续
		// 清单的过滤条件；仅非 optional 的 remove 参与过滤。
		skip := false
		for _, rp := range dst.RemoveProjects {
			if rp.Optional.Get(false) {
				continue
			}
			if removeProjectMatchesProject(rp, project) {
				skip = true
				break
			}
		}
		if skip {
			skippedProjects++
			continue
		}

		projectPath := normalizeManifestPath(project.Path)
		if projectPath == "" {
			projectPath = normalizeManifestPath(project.Name)
		}

		exists := false
		for i := range dst.Projects {
			dstPath := normalizeManifestPath(dst.Projects[i].Path)
			if dstPath == "" {
				dstPath = normalizeManifestPath(dst.Projects[i].Name)
			}
			if normalizeManifestPath(dst.Projects[i].Name) == normalizeManifestPath(project.Name) && dstPath == projectPath {
				mergeProjectValues(&dst.Projects[i], project)
				exists = true
				updatedProjects++
				break
			}
		}
		if !exists {
			dst.Projects = append(dst.Projects, project)
			addedProjects++
		}
	}

	logger.Info("合并项目统计: 添加=%d, 更新=%d, 跳过=%d", addedProjects, updatedProjects, skippedProjects)

	// 合并移除项目
	for _, removeProject := range src.RemoveProjects {
		// 先尝试在目标项目上执行（path/optional 语义与解析期一致）
		matched, err := removeMatches(dst, &dst.Projects, removeProject, true)
		if err != nil {
			return err
		}
		if matched {
			// 已在本层生效：不再保留该条目。否则后续清单合并会把它当成
			// 仍待执行的 remove 而误跳过项目；ToXML 写回不存在的
			// remove-project 也会导致回环解析失败。
			continue
		}

		// 未命中：仅非 optional 条目保留供更外层执行（按 name+path 去重）。
		// optional 未命中表示目标缺失可接受，既不作为过滤条件，也不传播。
		if removeProject.Optional.Get(false) {
			continue
		}
		exists := false
		for _, rp := range dst.RemoveProjects {
			if rp.Name == removeProject.Name && rp.Path == removeProject.Path {
				exists = true
				break
			}
		}
		if !exists {
			dst.RemoveProjects = append(dst.RemoveProjects, removeProject)
		}
	}

	// 合并其余元数据
	if dst.Notice == "" {
		dst.Notice = src.Notice
	}
	if dst.RepoHooks == nil && src.RepoHooks != nil {
		hooks := *src.RepoHooks
		dst.RepoHooks = &hooks
	}
	if dst.ManifestServer == nil && src.ManifestServer != nil {
		server := *src.ManifestServer
		dst.ManifestServer = &server
	}
	if dst.Superproject == nil && src.Superproject != nil {
		superproject := *src.Superproject
		dst.Superproject = &superproject
	}
	if dst.CustomAttrs == nil {
		dst.CustomAttrs = make(map[string]string)
	}
	for k, v := range src.CustomAttrs {
		if _, ok := dst.CustomAttrs[k]; !ok {
			dst.CustomAttrs[k] = v
		}
	}

	return nil
}

// removeProjectMatchesProject 判断 remove-project 是否匹配项目（path 为空时
// 项目 Path 按 Name 视为隐式 path）。
func removeProjectMatchesProject(rp RemoveProject, proj Project) bool {
	rpName := normalizeManifestPath(rp.Name)
	rpPath := normalizeManifestPath(rp.Path)
	if rpName == "" && rpPath == "" {
		return false
	}
	projName := normalizeManifestPath(proj.Name)
	if rpPath == "" {
		return projName == rpName
	}
	projectPath := normalizeManifestPath(proj.Path)
	if projectPath == "" {
		projectPath = projName
	}
	return projectPath == rpPath && (rpName == "" || projName == rpName)
}

// mergeRemoteValues 用 src 补齐 dst 的空字段，不覆盖已有值。
func mergeRemoteValues(dst *Remote, src Remote) {
	if dst.Fetch == "" {
		dst.Fetch = src.Fetch
	}
	if dst.Review == "" {
		dst.Review = src.Review
	}
	if dst.Revision == "" {
		dst.Revision = src.Revision
	}
	if dst.Alias == "" {
		dst.Alias = src.Alias
	}
	if dst.PushURL == "" {
		dst.PushURL = src.PushURL
	}
	if dst.CustomAttrs == nil {
		dst.CustomAttrs = make(map[string]string)
	}
	for k, v := range src.CustomAttrs {
		if _, ok := dst.CustomAttrs[k]; !ok {
			dst.CustomAttrs[k] = v
		}
	}
	if len(src.Annotations) > 0 {
		dst.Annotations = append(dst.Annotations, src.Annotations...)
	}
}

// mergeDefaultValues 用 src 补齐 dst 的空字段，不覆盖已有值。
func mergeDefaultValues(dst *Default, src Default) {
	if dst.Remote == "" {
		dst.Remote = src.Remote
	}
	if dst.Revision == "" {
		dst.Revision = src.Revision
	}
	if dst.DestBranch == "" {
		dst.DestBranch = src.DestBranch
	}
	if dst.Upstream == "" {
		dst.Upstream = src.Upstream
	}
	if dst.SyncJ == 0 {
		dst.SyncJ = src.SyncJ
	}
	if !dst.SyncC.Set {
		dst.SyncC = src.SyncC
	}
	if !dst.SyncS.Set {
		dst.SyncS = src.SyncS
	}
	if !dst.SyncTags.Set {
		dst.SyncTags = src.SyncTags
	}
	if dst.Sync == "" {
		dst.Sync = src.Sync
	}
	if dst.CustomAttrs == nil {
		dst.CustomAttrs = make(map[string]string)
	}
	for k, v := range src.CustomAttrs {
		if _, ok := dst.CustomAttrs[k]; !ok {
			dst.CustomAttrs[k] = v
		}
	}
}

// mergeProjectValues 用 src 补齐 dst 的空字段并追加列表，不覆盖非空字段。
func mergeProjectValues(dst *Project, src Project) {
	if dst.Path == "" {
		dst.Path = src.Path
	}
	if dst.Remote == "" {
		dst.Remote = src.Remote
	}
	if dst.Revision == "" {
		dst.Revision = src.Revision
	}
	if dst.Upstream == "" {
		dst.Upstream = src.Upstream
	}
	if dst.DestBranch == "" {
		dst.DestBranch = src.DestBranch
	}
	if dst.Groups == "" {
		dst.Groups = src.Groups
	}
	if !dst.SyncC.Set {
		dst.SyncC = src.SyncC
	}
	if !dst.SyncS.Set {
		dst.SyncS = src.SyncS
	}
	if !dst.SyncTags.Set {
		dst.SyncTags = src.SyncTags
	}
	if dst.CloneDepth == 0 {
		dst.CloneDepth = src.CloneDepth
	}
	if !dst.ForcePath.Set {
		dst.ForcePath = src.ForcePath
	}
	if dst.References == "" {
		dst.References = src.References
	}
	if dst.CustomAttrs == nil {
		dst.CustomAttrs = make(map[string]string)
	}
	for k, v := range src.CustomAttrs {
		if _, ok := dst.CustomAttrs[k]; !ok {
			dst.CustomAttrs[k] = v
		}
	}
	dst.Copyfiles = appendProjectCopyfiles(dst.Copyfiles, src.Copyfiles)
	dst.Linkfiles = appendProjectLinkfiles(dst.Linkfiles, src.Linkfiles)
	if len(src.Annotations) > 0 {
		dst.Annotations = append(dst.Annotations, src.Annotations...)
	}
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

		// 解析包含的清单文件。以子清单上下文解析：其中悬空的 remove-project
		// 目标可能在外层清单，不应在单独解析时硬报错；组过滤统一由调用方
		// 在完整清单上执行，故此处传 nil。
		m.Parser.includeDepth++
		includeManifest, err := m.Parser.ParseFromFile(includePath, nil)
		m.Parser.includeDepth--
		if err != nil {
			logger.Error("failed to parse included manifest失 %s, 错误: %v", includePath, err)
			return nil, fmt.Errorf("failed to parse included manifest失 %w", err)
		}

		// ParseFromFile 已递归展开 include 并合并其项目，无需再次递归处理，
		// 避免同一清单被重复解析/合并。
		manifests = append(manifests, includeManifest)
	}

	// 合并所有清单
	return m.Merge(manifests)
}
