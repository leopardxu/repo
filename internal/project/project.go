package project

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/leopardxu/repo-go/internal/config"
	"github.com/leopardxu/repo-go/internal/git"
	"github.com/leopardxu/repo-go/internal/logger"
)

// Project 表示一个本地项目
type Project struct {
	Name       string
	Path       string
	RemoteName string
	RemoteURL  string
	Revision   string
	Groups     []string
	GitRepo    *git.Repository

	// 添加与engine.go 兼容的字段
	Relpath    string     // 项目相对路径
	Worktree   string     // 项目working directory
	Gitdir     string     // Git 目录
	RevisionID string     // 修订ID
	Linkfiles  []LinkFile // 链接文件列表
	Copyfiles  []CopyFile // 复制文件列表
	Objdir     string     // 对象目录

	// 添加新的字段
	LastFetch  time.Time // 最后一次获取的时间
	Remote     string    // 远程仓库名称
	References string    // 引用配置(remote:refs格式)
	NeedGC     bool      // 是否需要垃圾回
	SyncS      bool      // 项目级 sync-s：同步子模块
	SyncC      bool      // 项目级 sync-c：仅同步当前分支
	SyncTags   bool      // 项目级 sync-tags：同步标签（含 default 继承，上游缺省 true）
	CloneDepth int       // 项目级 clone-depth：浅克隆深度
	DestBranch string    // 目标分支（用于 upload/code review）
	Upstream   string    // 上游分支（用于 start 跟踪关系）

	// 添加锁，保护并发访问
	mu sync.RWMutex
}

// LinkFile 表示链接文件
type LinkFile struct {
	Src  string // 源文件路
	Dest string // 目标文件路径
}

// CopyFile 表示复制文件
type CopyFile struct {
	Src  string // 源文件路
	Dest string // 目标文件路径
}

// NewProject 创建项目
func NewProject(name, path, remoteName, remoteURL, revision string, groups []string, gitRunner git.Runner) *Project {
	// 确保路径使用正确的分隔符
	path = filepath.Clean(path)

	return &Project{
		Name:       name,
		Path:       path,
		RemoteName: remoteName,
		RemoteURL:  remoteURL,
		Revision:   revision,
		Groups:     groups,
		GitRepo:    git.NewRepository(path, gitRunner),
		Relpath:    path,                        // 设置相对路径
		Worktree:   path,                        // 设置working directory
		Gitdir:     filepath.Join(path, ".git"), // 设置Git目录
		RevisionID: revision,                    // 设置修订ID
		Remote:     remoteName,                  // 设置远程仓库名称
		NeedGC:     false,                       // 默认不需要垃圾回
	}
}

// IsInGroup 检查项目是否在指定组中。
// "all" 隐式包含所有项目（对齐上游 repo）；请求组与项目组比较前均去空白。
func (p *Project) IsInGroup(group string) bool {
	if group == "" {
		return true
	}

	group = strings.TrimSpace(group)
	if group == "all" {
		return true
	}

	p.mu.RLock()
	defer p.mu.RUnlock()

	for _, g := range p.Groups {
		if g == group {
			return true
		}
	}

	return false
}

// IsInAnyGroup 判定项目是否匹配组过滤条件，对齐上游 Project.MatchesGroups：
//   - filter 为空表示不过滤（匹配全部）；
//   - 过滤项按出现顺序求值："-<group>" 命中时置否、"<group>" 命中时置真，
//     最终结果由最后一次命中决定；
//   - "all" 隐式包含所有项目；
//   - "default" 匹配任何不含 "notdefault" 组的项目。
func (p *Project) IsInAnyGroup(groups []string) bool {
	if len(groups) == 0 {
		return true
	}

	p.mu.RLock()
	defer p.mu.RUnlock()

	// 候选集合 = 项目组 ∪ {all} ∪ {default}（不含 notdefault 时）
	candidates := make(map[string]struct{}, len(p.Groups)+2)
	for _, g := range p.Groups {
		candidates[g] = struct{}{}
	}
	candidates["all"] = struct{}{}
	if _, hasNotDefault := candidates["notdefault"]; !hasNotDefault {
		candidates["default"] = struct{}{}
	}

	matched := false
	for _, group := range groups {
		group = strings.TrimSpace(group)
		if group == "" {
			continue
		}
		if strings.HasPrefix(group, "-") {
			if _, ok := candidates[strings.TrimPrefix(group, "-")]; ok {
				matched = false
			}
			continue
		}
		if _, ok := candidates[group]; ok {
			matched = true
		}
	}
	return matched
}

// Sync 同步项目
func (p *Project) Sync(opts SyncOptions) error {
	// 使用结构化日志，减少冗余信息
	logger.Debug("同步项目 [%s]", p.Name)

	// 检查项目目录是否存在
	exists, err := p.GitRepo.Exists()
	if err != nil {
		logger.Error("项目 [%s] 检查失 %v", p.Name, err)
		return fmt.Errorf("failed to check if project exists失 %w", err)
	}

	// 如果不存在，克隆仓库
	if !exists {
		// 确保使用完整的远程URL进行克隆
		cloneURL := p.RemoteURL
		if cloneURL == "" {
			logger.Error("项目 [%s] remote URL is empty", p.Name)
			return fmt.Errorf("failed to clone project %s: remote URL is empty", p.Name)
		}

		// 只在非静默模式下输出信息日志
		if !opts.Quiet {
			logger.Info("克隆 [%s] <- %s", p.Name, cloneURL)
		}

		// 创建父目
		parentDir := filepath.Dir(p.Path)
		if err := os.MkdirAll(parentDir, 0755); err != nil {
			logger.Error("项目 [%s] 创建目录failed: %v", p.Name, err)
			return fmt.Errorf("创建目录failed: %w", err)
		}

		// 克隆仓库
		if err := p.GitRepo.Clone(cloneURL, git.CloneOptions{
			Depth:  opts.Depth,
			Branch: p.Revision,
		}); err != nil {
			logger.Error("项目 [%s] 克隆failed: %v", p.Name, err)
			return fmt.Errorf("克隆项目failed: %w", err)
		}

		if !opts.Quiet {
			logger.Info("项目 [%s] 克隆完成", p.Name)
		}
		return nil
	}

	// 如果存在，获取更新
	if !opts.LocalOnly {
		if !opts.Quiet {
			logger.Debug("failed to get project [%s] 更新", p.Name)
		}

		// 更新最后获取时间
		p.mu.Lock()
		p.LastFetch = time.Now()
		p.mu.Unlock()

		if err := p.GitRepo.Fetch(p.RemoteName, git.FetchOptions{
			Prune: opts.Prune,
			Tags:  opts.Tags,
			Depth: opts.Depth,
		}); err != nil {
			logger.Error("项目 [%s] 更新failed: %v", p.Name, err)
			return fmt.Errorf("获取更新failed: %w", err)
		}

		// 设置需要垃圾回收标
		p.mu.Lock()
		p.NeedGC = true
		p.mu.Unlock()
	}

	// 如果不是只获取，更新工作区
	if !opts.NetworkOnly {
		// 减少日志输出
		if !opts.Quiet {
			logger.Debug("更新项目 [%s] 工作区", p.Name)
		}

		// 检查是否有本地修改
		clean, err := p.GitRepo.IsClean()
		if err != nil {
			logger.Error("项目 [%s] 工作区检查失 %v", p.Name, err)
			return fmt.Errorf("failed to check if working tree is clean: %w", err)
		}

		// 如果有本地修改且不强制同步，报错
		if !clean && !opts.Force {
			logger.Warn("项目 [%s] working tree is not clean，需要使--force-sync 覆盖", p.Name)
			return fmt.Errorf("working tree is not clean，使--force-sync 覆盖本地修改")
		}

		// 检出指定版
		if err := p.GitRepo.Checkout(p.Revision); err != nil {
			logger.Error("项目 [%s] 检%s failed: %v", p.Name, p.Revision, err)
			return fmt.Errorf("检出修订版本失 %w", err)
		}

		if !opts.Quiet {
			logger.Info("项目 [%s] 更新完成", p.Name)
		}
	}

	return nil
}

// GC 执行垃圾回收
func (p *Project) GC() error {
	// 检查是否需要垃圾回
	p.mu.RLock()
	needGC := p.NeedGC
	p.mu.RUnlock()

	if !needGC {
		return nil
	}

	logger.Debug("项目 [%s] 执行垃圾回收", p.Name)

	// 执行 git gc 命令
	_, err := p.GitRepo.RunCommand("gc", "--auto")
	if err != nil {
		logger.Error("项目 [%s] 垃圾回收failed: %v", p.Name, err)
		return fmt.Errorf("failed to run garbage collection: %w", err)
	}

	// 重置垃圾回收标志
	p.mu.Lock()
	p.NeedGC = false
	p.mu.Unlock()

	return nil
}

// SyncNetworkOptions 包含网络同步的选项
type SyncNetworkOptions struct {
	Quiet               bool
	CurrentBranch       bool
	ForceSync           bool
	NoCloneBundle       bool
	Tags                bool
	IsArchive           bool
	OptimizedFetch      bool
	RetryFetches        int
	Prune               bool
	SSHProxy            interface{}
	CloneFilter         string
	PartialCloneExclude string
}

// SyncNetworkHalf 执行网络同步
func (p *Project) SyncNetworkHalf(opts SyncNetworkOptions) bool {

	logger.Debug("开始执行项目%s 的网络同步", p.Name)

	// 检查项目目录是否存在
	exists, err := p.GitRepo.Exists()
	if err != nil {
		logger.Error("检查项目%s 是否存在failed: %v", p.Name, err)
		return false
	}

	// 如果不存在，克隆仓库
	if !exists {
		if !opts.Quiet {
			logger.Info("克隆项目 %s %s", p.Name, p.RemoteURL)
		}

		// 创建父目
		parentDir := filepath.Dir(p.Path)
		if err := os.MkdirAll(parentDir, 0755); err != nil {
			logger.Error("为项%s 创建目录 %s failed: %v", p.Name, parentDir, err)
			return false
		}

		// 克隆选项
		options := git.CloneOptions{
			Branch: p.Revision,
		}

		// 如果指定了深度，设置深度
		if opts.RetryFetches > 0 {
			options.Depth = opts.RetryFetches
		}

		// 克隆仓库
		if err := p.GitRepo.Clone(p.RemoteURL, options); err != nil {
			logger.Error("克隆项目 %s failed: %v", p.Name, err)
			return false
		}

		logger.Debug("项目 %s 克隆完成", p.Name)
		return true
	}

	// 如果存在，获取更新
	if !opts.Quiet {
		logger.Info("failed to get project %s 的更新", p.Name)
	}

	// 更新最后获取时间
	p.mu.Lock()
	p.LastFetch = time.Now()
	p.mu.Unlock()

	// 获取选项
	fetchOpts := git.FetchOptions{
		Prune: opts.Prune,
		Tags:  opts.Tags,
	}

	// 如果指定了深度，设置深度
	if opts.RetryFetches > 0 {
		fetchOpts.Depth = opts.RetryFetches
	}

	// 执行获取，支持重
	var fetchErr error
	for i := 0; i <= opts.RetryFetches; i++ {
		fetchErr = p.GitRepo.Fetch(p.RemoteName, fetchOpts)
		if fetchErr == nil {
			break
		}

		if i < opts.RetryFetches {
			logger.Warn("failed to get project %s 更新failed，将重试 (%d/%d): %v", p.Name, i+1, opts.RetryFetches, fetchErr)
			time.Sleep(time.Second * time.Duration(i+1)) // 指数退
		}
	}

	if fetchErr != nil {
		logger.Error("failed to get project %s 更新failed: %v", p.Name, fetchErr)
		return false
	}

	logger.Debug("项目 %s 网络同步完成", p.Name)
	return true
}

// SyncLocalHalf 执行本地同步
func (p *Project) SyncLocalHalf(_ bool, forceSync bool, forceOverwrite bool) bool {
	logger.Debug("开始执行项目%s 的本地同步", p.Name)

	// 检查是否有本地修改
	clean, err := p.GitRepo.IsClean()
	if err != nil {
		logger.Error("检查项目%s 工作区是否干净failed: %v", p.Name, err)
		return false
	}

	// 如果有本地修改且不强制同步，报错
	if !clean && !forceSync && !forceOverwrite {
		logger.Warn("项目 %s working tree is not clean，use --force-sync to overwrite local changes", p.Name)
		return false
	}

	// 获取当前分支
	currentBranch, err := p.GitRepo.CurrentBranch()
	if err != nil {
		logger.Warn("failed to get project %s 当前分支failed: %v", p.Name, err)
		// 继续执行，不影响检出操
	}

	// 如果当前分支与目标分支不同，或者强制检
	if currentBranch != p.Revision || forceSync || forceOverwrite {
		logger.Debug("检出项%s 的修订版%s", p.Name, p.Revision)

		// 检出指定版
		if err := p.GitRepo.Checkout(p.Revision); err != nil {
			logger.Error("检出项%s 的修订版%s failed: %v", p.Name, p.Revision, err)
			return false
		}
	} else {
		logger.Debug("项目 %s 已经在正确的修订版本 %s 上", p.Name, p.Revision)
	}

	logger.Debug("项目 %s 本地同步完成", p.Name)
	return true
}

// GetStatus failed to get project状态
func (p *Project) GetStatus() (string, error) {
	logger.Debug("failed to get project %s 的状态", p.Name)

	status, err := p.GitRepo.Status()
	if err != nil {
		logger.Error("failed to get project %s 状态failed: %v", p.Name, err)
		return "", fmt.Errorf("failed to get project status: %w", err)
	}

	return string(status), nil
}

// DeleteWorktree 删除工作
func (p *Project) DeleteWorktree(quiet bool, forceRemoveDirty bool) error {
	logger.Debug("准备删除项目 %s 的工作树", p.Name)

	// 检查工作树是否存在
	if _, err := os.Stat(p.Worktree); os.IsNotExist(err) {
		logger.Debug("项目 %s 的工作树不存在，无需删除", p.Name)
		return nil
	}

	// 检查是否有本地修改
	if !forceRemoveDirty {
		clean, err := p.GitRepo.IsClean()
		if err != nil {
			logger.Error("检查项目%s 工作区是否干净failed: %v", p.Name, err)
			return fmt.Errorf("failed to check if working tree is clean: %w", err)
		}

		if !clean {
			logger.Warn("项目 %s working tree is not clean，use --force-remove-dirty to force delete", p.Name)
			return fmt.Errorf("working tree is not clean，使用--force-remove-dirty 强制删除")
		}
	}

	// 删除工作
	if !quiet {
		logger.Info("删除项目 %s 的工作树 %s", p.Name, p.Worktree)
	}

	// 安全检查：确保要删除的目录在repo根目录下
	repoRoot, repoErr := config.GetRepoRoot()
	if repoErr != nil {
		logger.Error("failed to get repo root: %v", repoErr)
		return fmt.Errorf("failed to get repo root: %w", repoErr)
	}

	// 检查working directory是否在repo根目录下
	absWorktree, absErr := filepath.Abs(p.Worktree)
	if absErr != nil {
		logger.Error("无法获取working directory绝对路径: %v", absErr)
		return fmt.Errorf("无法获取working directory绝对路径: %w", absErr)
	}

	relPath, relErr := filepath.Rel(repoRoot, absWorktree)
	if relErr != nil || strings.HasPrefix(relPath, "..") {
		// 目录is not under repo root，deletion refused
		logger.Error("working directory %s is not under repo root，deletion refused", p.Worktree)
		return fmt.Errorf("working directory %s is not under repo root，deletion refused", p.Worktree)
	}

	if err := os.RemoveAll(p.Worktree); err != nil {
		logger.Error("删除项目 %s 的工作树failed: %v", p.Name, err)
		return fmt.Errorf("failed to delete worktree: %w", err)
	}

	logger.Debug("项目 %s 的工作树已删除", p.Name)
	return nil
}

// GetCurrentBranch 获取当前分支
func (p *Project) GetCurrentBranch() (string, error) {
	logger.Debug("failed to get project %s 的当前分支", p.Name)

	branch, err := p.GitRepo.CurrentBranch()
	if err != nil {
		logger.Error("failed to get project %s 当前分支failed: %v", p.Name, err)
		return "", fmt.Errorf("failed to get current branch: %w", err)
	}

	return branch, nil
}

// HasBranch 检查分支是否存在
func (p *Project) HasBranch(branch string) (bool, error) {
	logger.Debug("检查项目%s 是否有分支%s", p.Name, branch)

	output, err := p.GitRepo.RunCommand("branch", "--list", branch)
	if err != nil {
		logger.Error("列出项目 %s 的分支failed: %v", p.Name, err)
		return false, fmt.Errorf("列出分支failed: %w", err)
	}

	return strings.TrimSpace(string(output)) != "", nil
}

// ListLocalBranches 返回项目中所有本地分支名（不含远程分支与当前分支标记）。
func (p *Project) ListLocalBranches() ([]string, error) {
	logger.Debug("列出项目 %s 的所有本地分支", p.Name)

	// 使用 refname:short 格式输出干净的分支名，避免解析 "*" 标记
	output, err := p.GitRepo.RunCommand("for-each-ref", "--format=%(refname:short)", "refs/heads/")
	if err != nil {
		logger.Error("列出项目 %s 的本地分支failed: %v", p.Name, err)
		return nil, fmt.Errorf("列出本地分支failed: %w", err)
	}

	var branches []string
	for _, line := range strings.Split(string(output), "\n") {
		name := strings.TrimSpace(line)
		if name != "" {
			branches = append(branches, name)
		}
	}
	return branches, nil
}

// DeleteBranch 删除指定的分支。
// 对齐上游 project.py AbandonBranch：
//   - 分支不存在返回错误（上游返回 None，由调用方判定"no project has local branch"）；
//   - 分支为当前分支时先分离 HEAD 到 manifest 修订版本再删除，删除失败如实返回错误，
//     绝不把失败吞成成功。
func (p *Project) DeleteBranch(branch string) error {
	logger.Debug("准备删除项目 %s 的分支%s", p.Name, branch)

	if branch == "" {
		logger.Error("尝试删除项目 %s 的空分支名", p.Name)
		return fmt.Errorf("分支名为空")
	}

	// 检查分支是否存在
	exists, err := p.HasBranch(branch)
	if err != nil {
		return fmt.Errorf("检查分支 %s 是否存在: %w", branch, err)
	}
	if !exists {
		return fmt.Errorf("分支 %s 不存在: %w", branch, ErrBranchNotFound)
	}

	// 分支为当前分支：先分离 HEAD 到 manifest 修订版本（上游 AbandonBranch 语义），
	// 否则 git branch -D 会因"分支被工作区使用"而失败
	currentBranch, err := p.GetCurrentBranch()
	if err != nil {
		return fmt.Errorf("failed to get current branch: %w", err)
	}
	if currentBranch == branch {
		detachTarget := p.Revision
		if detachTarget == "" {
			// 无修订版本时退化为分离到当前 HEAD
			out, cerr := p.GitRepo.RunCommand("rev-parse", "HEAD")
			if cerr != nil {
				return fmt.Errorf("resolve head for detach: %w", cerr)
			}
			detachTarget = strings.TrimSpace(string(out))
		}
		if _, derr := p.GitRepo.RunCommand("checkout", "--detach", detachTarget); derr != nil {
			return fmt.Errorf("detach head to %s: %w", detachTarget, derr)
		}
	}

	// 删除分支（上游 abandon 等价于 git branch -D）
	if _, derr := p.GitRepo.RunCommand("branch", "-D", branch); derr != nil {
		logger.Error("删除项目 %s 的分支%s failed: %v", p.Name, branch, derr)
		return fmt.Errorf("failed to delete branch %s: %w", branch, derr)
	}

	logger.Debug("已删除项目%s 的分支%s", p.Name, branch)
	return nil
}

// ErrBranchNotFound 分支不存在（对齐上游 AbandonBranch 返回 None 的判定）
var ErrBranchNotFound = errors.New("branch not found")

// SetNeedGC 设置是否需要垃圾回收
func (p *Project) SetNeedGC(need bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.NeedGC = need
}

// GetRemoteURL 获取远程URL
func (p *Project) GetRemoteURL() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.RemoteURL
}

// SetRemoteURL 设置远程URL
func (p *Project) SetRemoteURL(url string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.RemoteURL = url
}

// GetRevision 获取修订版本
func (p *Project) GetRevision() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.Revision
}

// SetRevision 设置修订版本
func (p *Project) SetRevision(revision string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.Revision = revision
	p.RevisionID = revision
}
