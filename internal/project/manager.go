package project

import (
	"context"
	"fmt"
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
	// 解析 repo 根目录：RepoDir 为绝对路径时其父目录就是工作区根（最可靠，
	// 不受 CWD 影响）；否则调用方通常已 chdir 至根目录，GetRepoRoot 可命中；
	// 最后回退为 RepoDir 的父目录（RepoDir 通常为 ".repo"）。
	if m.Topdir != "" {
		manager.TopDir = m.Topdir
	} else if m.RepoDir != "" && filepath.IsAbs(m.RepoDir) {
		manager.TopDir = filepath.Dir(m.RepoDir)
	} else if topDir, terr := config.GetRepoRoot(); terr == nil && topDir != "" {
		manager.TopDir = topDir
	} else if d := filepath.Dir(m.RepoDir); d != "" && d != "." {
		manager.TopDir = d
	} else {
		manager.TopDir = "."
	}
	// 相对 fetch 路径（如 ".."）只能基于清单仓库 URL（config.json 的
	// manifest_url）解析。manifest-server 是清单服务器地址，不是仓库
	// urljoin 基准，绝不能参与相对 fetch 解析（对齐上游 GetManifestProject 语义）。
	if cfg != nil {
		manager.ManifestURL = cfg.ManifestURL
	}

	// 从清单中加载项目
	// 镜像模式下以生成的镜像路径（<topdir>/<name>.git）为唯一键去重；
	// 普通模式不合并，不同工作树路径即使同名也各自保留。
	isMirror := cfg != nil && cfg.Mirror
	seenProjects := make(map[string]string, len(m.Projects))

	for _, p := range m.Projects {
		// 获取远程信息（remote name 可能是 alias，匹配时一并考虑）
		var remoteName, remoteFetch, remotePushURL, remoteReview string
		if p.Remote != "" {
			remoteName = p.Remote
		} else if m.Default.Remote != "" {
			remoteName = m.Default.Remote
		}

		// 查找远程配置（这里保存的是 remote 的 fetch 基地址，不含项目名）。
		// 先按 name 精确匹配，未命中再按 alias 匹配。
		// 对齐上游 ToRemoteSpec：remote 声明了 alias 时，RemoteName 使用 alias，
		// 而 fetch/pushurl 始终取自该 remote。
		foundRemote := false
		for _, r := range m.Remotes {
			if r.Name == remoteName {
				remoteFetch = r.Fetch
				remotePushURL = r.PushURL
				remoteReview = r.Review
				if r.Alias != "" {
					remoteName = r.Alias
				}
				foundRemote = true
				break
			}
		}
		if !foundRemote {
			for _, r := range m.Remotes {
				if r.Alias != "" && r.Alias == remoteName {
					remoteFetch = r.Fetch
					remotePushURL = r.PushURL
					remoteReview = r.Review
					break
				}
			}
		}

		// 获取修订版本
		revision := p.Revision
		if revision == "" {
			revision = m.Default.Revision
		}

		// 清单路径为空时退回项目名；去掉尾部斜杠（对齐上游 GetProjectPaths
		// 的 rstrip("/")），保证工作区相对路径与镜像 name 无歧义。
		relPath := strings.TrimRight(strings.TrimSpace(p.Path), "/")
		if relPath == "" {
			relPath = strings.TrimRight(strings.TrimSpace(p.Name), "/")
		}

		// 创建项目路径。镜像模式对齐上游 GetProjectPaths：
		// worktree=None，gitdir=objdir=<topdir>/<name>.git（name 含嵌套前缀）。
		// 去重键使用生成的镜像路径：同名项目映射到同一目录，必须显式报告
		// 碰撞，不能静默丢弃。普通模式 Path 是工作区相对路径，不能拼接 .repo。
		projectPath := relPath
		if isMirror {
			projectPath = manager.getMirrorProjectPath(relPath, p.Name)
			if prev, exists := seenProjects[projectPath]; exists {
				logger.Error("镜像路径冲突：项目 %s 与 %s 均映射到 %s，跳过后者", prev, p.Name, projectPath)
				continue
			}
			seenProjects[projectPath] = p.Name
		}

		// 创建项目对象
		// 对齐上游 repo（manifest_xml.py 的 ToRemoteSpec）：
		// 原始 fetch 基址保存在 FetchBase，完整项目 URL = 基址 urljoin 后
		// 无条件拼接 "/"+项目名（name，非 path）。若不拼接，形如
		// ssh://host:29418 的无路径 fetch 会导致 `git clone` 报 "未指定路径"。
		project := NewProject(
			p.Name,
			projectPath,
			remoteName,
			"",
			revision,
			p.EffectiveGroups(),
			manager.GitRunner,
		)
		// 工作树与仓库目录（对齐上游 GetProjectPaths）：
		//   - 镜像：worktree=None，gitdir=objdir=<topdir>/<name>.git；
		//   - 普通：worktree=<topdir>/<relpath>，gitdir=<worktree>/.git。
		project.ManifestURL = manager.ManifestURL
		project.Relpath = relPath
		project.FetchBase = remoteFetch
		project.RemoteURL = manager.ResolveProjectURL(project)
		project.PushURL = manager.resolveProjectPushURL(remotePushURL, remoteFetch, p.Name)
		project.ReviewURL = ResolveReviewURL(remoteReview, p.Name)
		if isMirror {
			// 镜像的 Path 仍是清单相对路径；仓库目录只走 Gitdir/Objdir，
			// 调用方不能再用 Path 定位镜像仓库。
			project.Path = relPath
			project.Mirror = true
			project.Worktree = ""
			project.Gitdir = projectPath
			project.Objdir = projectPath
			project.GitRepo = git.NewRepository(projectPath, manager.GitRunner)
		} else {
			project.Worktree = filepath.Join(manager.TopDir, relPath)
			project.Gitdir = filepath.Join(project.Worktree, ".git")
			// GitRepo 必须指向工作树（非镜像项目 Path 是 workspace 相对路径，
			// 而工作树已基于 TopDir 展开；两者不一致时以 Worktree 为准）。
			project.GitRepo = git.NewRepository(project.Worktree, manager.GitRunner)
		}

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
		project.SyncS = p.SyncS.Get(false)
		project.SyncC = p.SyncC.Get(false)
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

// resolveProjectPushURL 计算项目的完整推送地址（薄包装）：urljoin 基准取
// 清单仓库 URL，实际拼接由 project.ResolvePushURL 统一完成。
func (m *Manager) resolveProjectPushURL(pushURL, fetchURL, projectName string) string {
	return ResolvePushURL(&config.Config{ManifestURL: m.ManifestURL}, pushURL, fetchURL, projectName)
}

// isRelativeFetchURL 判断 remote fetch 是否为相对路径（"."、".."、"../"、"./"）。
// 这类 fetch 需要基于 manifest 仓库 URL 解析（对齐上游 urljoin 语义）。
func isRelativeFetchURL(fetch string) bool {
	if fetch == "." || fetch == ".." {
		return true
	}
	return strings.HasPrefix(fetch, "../") || strings.HasPrefix(fetch, "./")
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

// GetProjectsByNames 根据名称列表返回全部匹配项目。同名项目可能有多个
// 工作树路径（如 cherry-pick 需要全部命中），因此不能遇到第一个就停止。
func (m *Manager) GetProjectsByNames(names []string) ([]*Project, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	projects := make([]*Project, 0, len(names))
	for _, name := range names {
		found := false
		for _, p := range m.Projects {
			if p.Name == name {
				projects = append(projects, p)
				found = true
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

// ResolveProjectURL 返回项目的完整 clone/fetch URL：以清单原始 fetch 基址
// （Project.FetchBase）经 urljoin 解析后，无条件拼接一次 "/"+项目名
// （对齐上游 ToRemoteSpec：fetchUrl.rstrip('/') + '/' + name）。
// 没有 FetchBase 的直接构造项目（RemoteURL 已是完整地址）原样返回 RemoteURL，
// 这是唯一保留的幂等路径；绝不按项目名后缀猜测是否已经拼接。
func (m *Manager) ResolveProjectURL(p *Project) string {
	if p == nil {
		return ""
	}
	return p.resolveFetchURL(m.ManifestURL)
}

// ResolveRemoteURL 把 remoteURL 视为 fetch/push 基址，经 urljoin 解析后
// 无条件拼接一次项目名（对齐上游 ToRemoteSpec）。空入参返回空串；
// 不做"已包含项目名"的后缀判断，避免把末段与项目名同名的基址误当成完整 URL。
func (m *Manager) ResolveRemoteURL(remoteURL string, projectName ...string) string {
	logger.Debug("解析远程URL: %s", remoteURL)

	if remoteURL == "" {
		return ""
	}

	name := ""
	if len(projectName) > 0 {
		name = strings.TrimSpace(projectName[0])
	}

	resolved := joinProjectName(resolveBaseURL(m.ManifestURL, remoteURL), name)
	logger.Debug("解析后的URL: %s", resolved)
	return resolved
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
	projects := m.GetProjects()

	// DryRun 只打印将要执行的动作，不 clone/fetch/checkout
	if opts.DryRun {
		for _, p := range projects {
			logger.Info("dry-run: 将同步项目 %s (%s)", p.Name, p.RemoteURL)
		}
		return nil
	}

	logger.Info("开始同步%d 个项目", len(projects))

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
	projects := m.GetProjects()

	// DryRun 只打印将要执行的动作，不 clone/fetch/checkout
	if opts.DryRun {
		for _, p := range projects {
			logger.Info("dry-run: 将同步项目 %s (%s)", p.Name, p.RemoteURL)
		}
		return nil
	}

	logger.Info("开始同%d 个项目，并发 %d", len(projects), concurrency)

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

// getMirrorProjectPath 生成镜像模式下的仓库路径。
// 对齐上游 GetProjectPaths：gitdir=objdir=<topdir>/<name>.git，其中 name
// 含嵌套前缀（如 platform/frameworks/base -> <topdir>/platform/frameworks/base.git），
// 镜像没有工作树。
//   - name 仅去掉全部尾部斜杠，不剥离 ".git" 后缀（上游同款行为）；
//   - 使用 filepath.Clean 规范化后生成镜像路径；
//   - 返回绝对路径，作为裸仓库 Gitdir/Objdir；
//   - name 缺失或不安全（绝对路径、.. 越界）时退回清单 path，再不行使用固定名。
func (m *Manager) getMirrorProjectPath(manifestPath, projectName string) string {
	name := filepath.Clean(strings.TrimRight(strings.TrimSpace(projectName), "/"))
	if name == "" || name == "." || !filepath.IsLocal(name) {
		name = filepath.Clean(strings.TrimRight(strings.TrimSpace(manifestPath), "/"))
	}
	if name == "" || name == "." || !filepath.IsLocal(name) {
		name = "project"
	}

	mirrorPath := filepath.Join(m.TopDir, name+".git")
	if abs, err := filepath.Abs(mirrorPath); err == nil {
		mirrorPath = abs
	}
	return mirrorPath
}
