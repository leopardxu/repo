package project

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"github.com/leopardxu/repo-go/internal/config"
	"github.com/leopardxu/repo-go/internal/git"
	"github.com/leopardxu/repo-go/internal/logger"
	"github.com/leopardxu/repo-go/internal/manifest"
	"golang.org/x/sync/errgroup"
)

// Manager 管理项目列表
type Manager struct {
	Projects     []*Project
	ManifestURL  string
	ManifestName string
	RepoDir      string
	TopDir       string // repo 根目录（工作区顶层），用于路径参数解析
	GitRunner    git.Runner
	mu           sync.RWMutex // 添加锁保护并发访问
}

// NewManager 创建项目管理器
func NewManager(manifestURL, manifestName, repoDir string, gitRunner git.Runner) *Manager {
	logger.Debug("创建项目管理器: manifestURL=%s, manifestName=%s, repoDir=%s", manifestURL, manifestName, repoDir)
	return &Manager{
		Projects:     make([]*Project, 0),
		ManifestURL:  manifestURL,
		ManifestName: manifestName,
		RepoDir:      repoDir,
		GitRunner:    gitRunner,
	}
}

// NewManagerFromManifest 从清单创建项目管理器
func NewManagerFromManifest(m *manifest.Manifest, cfg *config.Config) *Manager {
	logger.Info("开始创建项目管理器")

	// 创建项目管理器
	manager := &Manager{
		Projects:     make([]*Project, 0),
		ManifestURL:  "",
		ManifestName: "default.xml", // 默认清单名称
		RepoDir:      m.RepoDir,
		GitRunner:    git.NewRunner(),
	}
	// 解析 repo 根目录：调用方（各命令）已 chdir 至根目录，GetRepoRoot 可直接命中；
	// 失败时回退为 RepoDir 的父目录（RepoDir 通常为 ".repo"）
	if topDir, terr := config.GetRepoRoot(); terr == nil && topDir != "" {
		manager.TopDir = topDir
	} else if d := filepath.Dir(m.RepoDir); d != "" && d != "." {
		manager.TopDir = d
	} else {
		manager.TopDir = "."
	}
	// 清单服务器 URL（manifest-server 标签）优先；无标签时回退清单仓库 URL
	// （config.json 的 manifest_url），相对 fetch 路径（如 ".."）需要基于它解析
	if m.ManifestServer != nil && m.ManifestServer.URL != "" {
		manager.ManifestURL = m.ManifestServer.URL
	} else if cfg != nil {
		manager.ManifestURL = cfg.ManifestURL
	}

	// 从清单中加载项目
	// 在镜像模式下，需要去重
	isMirror := cfg != nil && cfg.Mirror
	seenPaths := make(map[string]bool) // 用于跟踪已处理的路径，避免重复

	for _, p := range m.Projects {
		// 获取远程信息
		var remoteName, remoteFetch string
		if p.Remote != "" {
			remoteName = p.Remote
		} else if m.Default.Remote != "" {
			remoteName = m.Default.Remote
		}

		// 查找远程配置（这里保存的是 remote 的 fetch 基地址，不含项目名）
		for _, r := range m.Remotes {
			if r.Name == remoteName {
				remoteFetch = r.Fetch
				break
			}
		}

		// 获取修订版本
		revision := p.Revision
		if revision == "" {
			revision = m.Default.Revision
		}

		// 创建项目路径
		var projectPath string
		if isMirror {
			// 在镜像模式下，根据远程URL的路径结构确定项目路径
			// 注意：这里传入的是 fetch 基地址，保持原有镜像布局不变
			projectPath = manager.getMirrorProjectPath(p.Path, remoteFetch, p.Name)
		} else {
			// 普通模式下，使用清单中的路径
			projectPath = filepath.Join(m.RepoDir, p.Path)
		}

		// 在镜像模式下进行去重检查
		if isMirror {
			if seenPaths[projectPath] {
				// 路径已存在，跳过这个项目
				logger.Debug("镜像模式下去重，跳过项目: %s (路径: %s)", p.Name, projectPath)
				continue
			}
			// 标记路径已处理
			seenPaths[projectPath] = true
		}

		// 创建项目对象
		// 对齐上游 repo（manifest_xml.py 的 ToRemoteSpec）：
		// 项目 URL = remote fetch 去掉尾部 '/' 后拼接项目名（name，非 path）。
		// 若不拼接，形如 ssh://host:29418 的无路径 fetch 会导致
		// `git clone` 报 "未指定路径" 错误。
		// 相对 fetch（..、../、./）保持原样，由同步引擎基于 manifest
		// 服务器 URL 解析相对路径时再拼接项目名，避免此处二次拼接。
		project := NewProject(
			p.Name,
			projectPath,
			remoteName,
			projectURLFromFetch(remoteFetch, p.Name),
			revision,
			p.EffectiveGroups(),
			git.NewRunner(),
		)

		// 转换并赋值 Linkfiles 字段
		if len(p.Linkfiles) > 0 {
			project.Linkfiles = make([]LinkFile, len(p.Linkfiles))
			for i, lf := range p.Linkfiles {
				project.Linkfiles[i] = LinkFile{
					Src:  lf.Src,
					Dest: lf.Dest,
				}
			}
		}

		// 转换并赋值 Copyfiles 字段
		if len(p.Copyfiles) > 0 {
			project.Copyfiles = make([]CopyFile, len(p.Copyfiles))
			for i, cf := range p.Copyfiles {
				project.Copyfiles[i] = CopyFile{
					Src:  cf.Src,
					Dest: cf.Dest,
				}
			}
		}

		// 映射项目级属性（对齐上游 repo manifest project 属性）
		project.SyncS = p.SyncS
		project.SyncC = p.SyncC
		// SyncTags 取最终值：项目显式声明优先，缺省继承 default（上游默认 true）
		project.SyncTags = p.SyncTags.Get(true)
		project.CloneDepth = p.CloneDepth
		project.DestBranch = p.DestBranch
		project.Upstream = p.Upstream

		// 添加项目到管理器
		manager.AddProject(project)
	}

	logger.Info("项目管理器创建完成，共加载%d 个项目", len(manager.Projects))
	return manager
}

// projectURLFromFetch 对齐上游 repo（manifest_xml.py 的 ToRemoteSpec）：
// 项目 URL = resolvedFetchUrl.rstrip('/') + '/' + name。
// 空 fetch 或相对 fetch（..、../、./）原样返回：前者由同步引擎按 manifest URL
// 回退处理，后者由引擎解析相对路径时拼接项目名，此处不能提前拼接。
func projectURLFromFetch(fetch, projectName string) string {
	if fetch == "" || isRelativeFetchURL(fetch) {
		return fetch
	}
	return strings.TrimSuffix(fetch, "/") + "/" + projectName
}

// isRelativeFetchURL 判断 remote fetch 是否为相对路径（..、../、./ 前缀），
// 这类 fetch 需要基于 manifest 服务器 URL 解析（对齐上游 urljoin 语义）。
func isRelativeFetchURL(fetch string) bool {
	return fetch == ".." ||
		strings.HasPrefix(fetch, "../") ||
		strings.HasPrefix(fetch, "./")
}

// GetProjectsInGroups 获取指定组中的项目
func (m *Manager) GetProjectsInGroups(groups []string) ([]*Project, error) {
	// 如果没有指定组，返回所有项目
	if len(groups) == 0 {
		logger.Debug("未指定项目组，返回所有项目")
		return m.GetProjects(), nil
	}

	// 记录过滤操作
	logger.Info("过滤项目组: %v", groups)

	// 获取在指定组中的项目
	projects := m.GetProjectsInAnyGroup(groups)

	// 如果没有找到项目，返回空列表而不是错误，让调用者决定如何处理
	if len(projects) == 0 {
		logger.Warn("在指定组 %v 中未找到项目，返回空列表", groups)
	}

	logger.Info("找到 %d 个匹配项目", len(projects))
	return projects, nil
}

// AddProject 添加项目
func (m *Manager) AddProject(p *Project) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// logger.Info("添加项目: %s (路径: %s, 修订版本: %s)", p.Name, p.Path, p.Revision)
	m.Projects = append(m.Projects, p)
}

// GetProjectsByNames 根据名称列表failed to get project
func (m *Manager) GetProjectsByNames(names []string) ([]*Project, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var projects []*Project
	for _, name := range names {
		found := false
		for _, p := range m.Projects {
			if p.Name == name {
				projects = append(projects, p)
				found = true
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("未找到项 %s", name)
		}
	}

	return projects, nil
}

// GetProjectsByArgs 按上游 repo（command.py GetProjects）语义解析参数列表：
//  1. 先按项目名精确匹配（同名多 worktree 时全部命中）；
//  2. 名字未命中时按路径解析：从 baseDir 起逐级向上查找包含该路径的项目工作树，
//     直到 repo 根目录为止（支持 "."、子目录与相对路径，含多级父目录回溯）；
//  3. 仍未命中返回错误（对应上游 NoSuchProjectError）。
func (m *Manager) GetProjectsByArgs(args []string, baseDir string) ([]*Project, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if baseDir == "" {
		var err error
		baseDir, err = os.Getwd()
		if err != nil {
			return nil, fmt.Errorf("failed to get current directory: %w", err)
		}
	}

	projects := make([]*Project, 0, len(args))
	for _, arg := range args {
		matched := m.getProjectsByName(arg)
		if len(matched) == 0 {
			if p := m.getProjectByPath(arg, baseDir); p != nil {
				matched = append(matched, p)
			}
		}
		if len(matched) == 0 {
			return nil, fmt.Errorf("no project found: %s", arg)
		}
		projects = append(projects, matched...)
	}
	return projects, nil
}

// getProjectsByName 返回与项目名精确匹配的全部项目（同名多 worktree 全命中，调用方需持读锁）
func (m *Manager) getProjectsByName(name string) []*Project {
	var matched []*Project
	for _, p := range m.Projects {
		if p.Name == name {
			matched = append(matched, p)
		}
	}
	return matched
}

// getProjectByPath 按路径解析项目（调用方需持读锁）。对齐上游 _GetProjectByPath：
//   - 相对参数基于 baseDir（命令 chdir 前的原始目录）求绝对路径；
//   - 路径存在（是项目工作树或其子目录）时逐级向上回溯到 repo 根目录（topdir），
//     命中第一个包含该路径的项目；
//   - 路径不存在时仅做精确匹配（不回溯），未命中返回 nil。
func (m *Manager) getProjectByPath(arg, baseDir string) *Project {
	if m.TopDir == "" {
		return nil
	}

	absPath := arg
	if !filepath.IsAbs(absPath) {
		absPath = filepath.Join(baseDir, arg)
	}
	absPath = filepath.Clean(absPath)

	byPath := make(map[string]*Project, len(m.Projects))
	for _, p := range m.Projects {
		wt := p.Worktree
		if wt == "" {
			continue
		}
		if !filepath.IsAbs(wt) {
			wt = filepath.Join(m.TopDir, wt)
		}
		byPath[filepath.Clean(wt)] = p
	}

	// 路径不存在：仅精确匹配（上游语义，避免把任意字符串误解析成上级项目）
	if _, serr := os.Stat(absPath); serr != nil {
		return byPath[absPath]
	}

	dir := absPath
	// 路径存在：先查自身，再逐级向上回溯
	for {
		if p, ok := byPath[dir]; ok {
			return p
		}
		if dir == m.TopDir || dir == filepath.Dir(dir) {
			return nil
		}
		dir = filepath.Dir(dir)
	}
}

// GetProject failed to get project
func (m *Manager) GetProject(name string) *Project {
	m.mu.RLock()
	defer m.mu.RUnlock()

	for _, p := range m.Projects {
		if p.Name == name {
			return p
		}
	}

	logger.Debug("未找到项 %s", name)
	return nil
}

// GetProjects 获取所有项目
func (m *Manager) GetProjects() []*Project {
	m.mu.RLock()
	defer m.mu.RUnlock()

	// 创建副本以避免并发修改
	projects := make([]*Project, len(m.Projects))
	copy(projects, m.Projects)

	logger.Debug("获取所有项目，共%d 个", len(projects))
	return projects
}

// GetProjectsInGroup 获取指定组中的项目
func (m *Manager) GetProjectsInGroup(group string) []*Project {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var projects []*Project

	for _, p := range m.Projects {
		if p.IsInGroup(group) {
			projects = append(projects, p)
		}
	}

	if len(projects) > 0 {
		logger.Info("在%s 中找到%d 个项目", group, len(projects))
	} else {
		logger.Debug("在%s 中未找到项目", group)
	}
	return projects
}

// GetProjectsInAnyGroup 获取在任意指定组中的项目
func (m *Manager) GetProjectsInAnyGroup(groups []string) []*Project {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if len(groups) == 0 || manifest.MatchesAllGroups(groups) {
		// 创建副本以避免并发修改
		projects := make([]*Project, len(m.Projects))
		copy(projects, m.Projects)
		return projects
	}

	logger.Debug("获取在任意组 %v 中的项目", groups)
	var projects []*Project

	for _, p := range m.Projects {
		if p.IsInAnyGroup(groups) {
			projects = append(projects, p)
		}
	}

	logger.Debug("在指定组中找到%d 个项目", len(projects))
	return projects
}

// ResolveRemoteURL 解析远程URL。
// 相对 fetch 路径（如 "../foo"）基于 manifest URL 解析（对齐上游 urljoin 语义，
// 会规范化 "." / ".." 段），绝对 URL 原样返回。
func (m *Manager) ResolveRemoteURL(remoteURL string) string {
	logger.Debug("解析远程URL: %s", remoteURL)

	// 如果URL为空，返回空字符串
	if remoteURL == "" {
		return ""
	}

	// 绝对 URL（带 scheme 或 scp 风式 git@host:path）直接返回
	if isAbsoluteRemoteURL(remoteURL) {
		return remoteURL
	}

	// 相对路径：基于 manifest URL 解析（urljoin 语义，规范化 .. / . 段）
	if m.ManifestURL != "" {
		if base, err := url.Parse(m.ManifestURL); err == nil {
			if ref, rerr := url.Parse(remoteURL); rerr == nil {
				resolved := base.ResolveReference(ref)
				// Go 的 ResolveReference 不处理 scp 风式 URL（无 scheme），
				// 解析结果不含 scheme 时退回字符串拼接
				if resolved.Scheme != "" {
					logger.Debug("解析后的URL: %s", resolved.String())
					return resolved.String()
				}
			}
		}
	}

	// 回退：字符串拼接（含 scp 风式 manifest URL 的场景）
	baseURL := m.extractBaseURL(m.ManifestURL)
	if baseURL == "" {
		logger.Warn("无法%s 提取基础URL", m.ManifestURL)
		return remoteURL
	}

	resolvedURL := baseURL
	if !strings.HasSuffix(resolvedURL, "/") {
		resolvedURL += "/"
	}
	resolvedURL += remoteURL

	logger.Debug("解析后的URL: %s", resolvedURL)
	return resolvedURL
}

// isAbsoluteRemoteURL 判断是否为绝对远程 URL：
// 带 scheme（http(s)/git/ssh/file 等）或 scp 风式（user@host:path）
func isAbsoluteRemoteURL(remoteURL string) bool {
	if strings.HasPrefix(remoteURL, "http://") ||
		strings.HasPrefix(remoteURL, "https://") ||
		strings.HasPrefix(remoteURL, "git://") ||
		strings.HasPrefix(remoteURL, "ssh://") ||
		strings.HasPrefix(remoteURL, "file://") ||
		strings.HasPrefix(remoteURL, "persistent-") {
		return true
	}
	// scp 风式：user@host:path（无 scheme，含 : 且含 @）
	return strings.Contains(remoteURL, "@") && strings.Contains(remoteURL, ":")
}

// extractBaseURL 提取基础URL
func (m *Manager) extractBaseURL(url string) string {
	logger.Debug("%s 提取基础URL", url)

	// 处理不同格式的URL

	// HTTP/HTTPS URL
	if strings.HasPrefix(url, "http://") || strings.HasPrefix(url, "https://") {
		// 移除最后一个路径组件
		var lastSlash = strings.LastIndex(url, "/")
		if lastSlash > 8 { // 确保不是协议后的第一个斜杠
			return url[:lastSlash]
		}
		return url
	}

	// SSH URL (git@github.com:user/repo.git)
	if strings.Contains(url, "@") && strings.Contains(url, ":") {
		parts := strings.Split(url, ":")
		if len(parts) == 2 {
			host := parts[0]
			path := parts[1]

			// 移除最后一个路径组件
			lastSlash := strings.LastIndex(path, "/")
			if lastSlash >= 0 {
				path = path[:lastSlash]
			} else {
				// 如果没有斜杠，可能是直接的仓库名
				path = ""
			}

			if path == "" {
				return host + ":"
			}
			return host + ":" + path
		}
	}

	// 文件URL
	if strings.HasPrefix(url, "file://") {
		path := strings.TrimPrefix(url, "file://")
		dir := filepath.Dir(path)
		return "file://" + dir
	}

	// unrecognized URL format
	logger.Warn("unrecognized URL format: %s", url)
	return ""
}

// ForEach executes an operation on each project with bounded concurrency.
// Uses errgroup for cancellation propagation. The default concurrency limit
// is runtime.NumCPU() to prevent unbounded goroutine creation.
func (m *Manager) ForEach(fn func(*Project) error) error {
	return m.ForEachWithJobs(fn, runtime.NumCPU())
}

// ForEachWithJobs 使用指定数量的并发任务对每个项目执行操作
func (m *Manager) ForEachWithJobs(fn func(*Project) error, jobs int) error {
	m.mu.RLock()
	projects := make([]*Project, len(m.Projects))
	copy(projects, m.Projects)
	m.mu.RUnlock()

	logger.Debug("使用 %d 个并发任务对 %d 个项目执行操作", jobs, len(projects))

	if len(projects) == 0 {
		logger.Warn("没有项目可执行操作")
		return nil
	}

	// 如果jobs <= 0，使用项目数量作为并发数
	if jobs <= 0 {
		jobs = len(projects)
		logger.Debug("未指定并发数，使用项目数量%d 作为并发数", jobs)
	}

	// 创建任务通道
	taskChan := make(chan *Project, len(projects))

	// 创建错误通道
	errChan := make(chan error, len(projects))

	// 创建等待组
	var wg sync.WaitGroup

	// 启动工作协程
	for i := 0; i < jobs; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()

			logger.Debug("启动工作协程 #%d", workerID)
			for p := range taskChan {
				logger.Debug("工作协程 #%d 处理项目 %s", workerID, p.Name)
				err := fn(p)
				if err != nil {
					logger.Error("项目 %s 操作failed: %v", p.Name, err)
					errChan <- fmt.Errorf("项目 %s: %w", p.Name, err)
				} else {
					logger.Debug("项目 %s 操作成功", p.Name)
				}
			}
			logger.Debug("工作协程 #%d 完成", workerID)
		}(i)
	}

	// 发送任务
	for _, p := range projects {
		taskChan <- p
	}
	close(taskChan)

	// 等待所有工作协程完成
	wg.Wait()
	close(errChan)

	// 收集错误
	var errors []error
	for err := range errChan {
		errors = append(errors, err)
	}

	if len(errors) > 0 {
		logger.Error("有%d 个项目操作failed", len(errors))
		return fmt.Errorf("有%d 个项目操作failed", len(errors))
	}

	logger.Debug("所有项目操作完成")
	return nil
}

// Sync 同步所有项目
func (m *Manager) Sync(opts SyncOptions) error {
	logger.Info("开始同步%d 个项目", len(m.Projects))

	// 如果指定了并发数，使用ForEachWithJobs
	if opts.Jobs > 0 {
		logger.Debug("使用 %d 个并发任务同步项目", opts.Jobs)
		return m.ForEachWithJobs(func(p *Project) error {
			if !opts.Quiet {
				logger.Info("同步项目 %s", p.Name)
			}
			return p.Sync(opts)
		}, opts.Jobs)
	}

	// 否则使用ForEach
	return m.ForEach(func(p *Project) error {
		if !opts.Quiet {
			logger.Info("同步项目 %s", p.Name)
		}
		return p.Sync(opts)
	})
}

// SyncOptions 同步选项
type SyncOptions struct {
	Force       bool   // 强制同步，覆盖本地修改
	DryRun      bool   // 仅显示将要执行的操作，不实际执行
	Quiet       bool   // 静默模式，减少输出
	Detach      bool   // 分离模式，不检出工作区
	Jobs        int    // 并发任务数
	Current     bool   // 仅同步当前分支
	Depth       int    // 克隆深度
	LocalOnly   bool   // 仅执行本地同步
	NetworkOnly bool   // 仅执行网络同步
	Prune       bool   // 修剪远程跟踪分支
	Tags        bool   // 获取标签
	Group       string // 指定要同步的组
	NoGC        bool   // 不执行垃圾回收
}

// FindTopLevelRepoDir 查找包含.repo目录的顶层目录
func FindTopLevelRepoDir(startDir string) string {
	logger.Debug("从%s 开始查找顶层仓库目录", startDir)

	// 从当前目录开始向上查找，直到找到包含.repo目录的目录
	dir := startDir
	for {
		// 检查当前目录是否包含.repo目录
		repoDir := filepath.Join(dir, ".repo")
		if _, err := os.Stat(repoDir); err == nil {
			// 找到.repo目录
			logger.Debug("找到顶层仓库目录: %s", dir)
			return dir
		}

		// 获取父目录
		parent := filepath.Dir(dir)
		if parent == dir {
			// 已经到达根目录，没有找到.repo目录
			logger.Warn("未找到顶层仓库目录")
			return ""
		}
		dir = parent
	}
}

// ForEachProject 对每个项目执行操作，支持并发执行
func (m *Manager) ForEachProject(fn func(*Project) error, concurrency int) error {
	projects := m.GetProjects()

	// 如果并发数为1，则串行执行
	if concurrency <= 1 {
		for _, p := range projects {
			if err := fn(p); err != nil {
				return err
			}
		}
		return nil
	}

	// 使用errgroup来处理并发
	ctx := context.Background()
	g, ctx := errgroup.WithContext(ctx)
	g.SetLimit(concurrency)

	for _, p := range projects {
		// 捕获循环变量
		proj := p
		g.Go(func() error {
			// 检查上下文是否被取消
			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
			}
			return fn(proj)
		})
	}

	return g.Wait()
}

// SyncProjects 同步所有项目，支持并发
func (m *Manager) SyncProjects(opts SyncOptions, concurrency int) error {
	logger.Info("开始同%d 个项目，并发 %d", len(m.Projects), concurrency)

	// 使用 ForEachProject 并发执行同步
	err := m.ForEachProject(func(p *Project) error {
		return p.Sync(opts)
	}, concurrency)

	if err != nil {
		logger.Error("项目同步过程中发生错 %v", err)
		return err
	}

	// Run garbage collection after sync
	if !opts.NoGC {
		logger.Info("running project garbage collection")
		if err := m.ForEachProject(func(p *Project) error {
			return p.GC()
		}, concurrency); err != nil {
			logger.Warn("garbage collection completed with errors: %v", err)
		}
	}

	logger.Info("所有项目同步完成")
	return nil
}

// FilterProjects 根据条件过滤项目
func (m *Manager) FilterProjects(filter func(*Project) bool) []*Project {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var filtered []*Project
	for _, p := range m.Projects {
		if filter(p) {
			filtered = append(filtered, p)
		}
	}

	return filtered
}

// getMirrorProjectPath 根据远程URL的路径结构确定镜像模式下的项目路径
func (m *Manager) getMirrorProjectPath(manifestPath, remoteURL, projectName string) string {
	// 如果remote URL is empty，使用清单中的路径
	if remoteURL == "" {
		// 在镜像模式下，路径需要以.git结尾
		path := filepath.Join(m.RepoDir, manifestPath)
		if !strings.HasSuffix(path, ".git") {
			path += ".git"
		}
		return path
	}

	// 解析远程URL，提取路径部分
	var path string

	// 处理SSH格式: user@host:path/to/repo.git
	if strings.Contains(remoteURL, "@") && strings.Contains(remoteURL, ":") {
		parts := strings.Split(remoteURL, ":")
		if len(parts) == 2 {
			path = parts[1]
		}
	} else if strings.HasPrefix(remoteURL, "ssh://") ||
		strings.HasPrefix(remoteURL, "http://") ||
		strings.HasPrefix(remoteURL, "https://") {
		// 处理标准URL格式
		u, err := url.Parse(remoteURL)
		if err == nil {
			path = u.Path
		}
	} else {
		// 其他格式，直接使用URL作为路径
		path = remoteURL
	}

	// 确保返回的路径是安全的，基于.repo目录。
	// URL 解析出的路径为绝对形式（/foo/bar.git），先中和前导分隔符再校验；
	// filepath.IsLocal 拒绝绝对路径与含 ".." 会逃逸出本目录的路径（参照
	// internal/repo_sync/checkout.go 的同一守卫写法）。
	cleanPath := strings.TrimPrefix(filepath.Clean(path), "/")
	if !filepath.IsLocal(cleanPath) {
		// 非本地安全路径时，使用项目名称作为路径
		cleanPath = projectName
	}

	// 在镜像模式下，路径需要以.git结尾
	if !strings.HasSuffix(cleanPath, ".git") {
		cleanPath += ".git"
	}

	// 确保返回绝对路径，避免相对路径导致的安全问题
	absPath := filepath.Join(m.RepoDir, cleanPath)
	return absPath
}
