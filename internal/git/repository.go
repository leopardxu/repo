package git

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/leopardxu/repo-go/internal/config"
	"github.com/leopardxu/repo-go/internal/logger"
)

// 使用包级别的日志记录器
var repoLog logger.Logger = logger.NewDefaultLogger()
var repoLogMu sync.RWMutex

// SetRepositoryLogger 设置仓库操作的日志记录器
func SetRepositoryLogger(l logger.Logger) {
	repoLogMu.Lock()
	defer repoLogMu.Unlock()
	repoLog = l
}

// URL cache with bounded size to prevent unbounded memory growth.
// When the cache exceeds maxURLCacheEntries, it is cleared entirely
// (simple eviction strategy sufficient for this use case where entries
// are small strings and the working set is typically stable).
const maxURLCacheEntries = 256

var (
	urlCache      = make(map[string]string)
	urlCacheMutex sync.RWMutex
)

// cacheURL stores a resolved URL in the cache. If the cache has grown
// beyond maxURLCacheEntries, it is reset to prevent unbounded growth.
func cacheURL(key, value string) {
	urlCacheMutex.Lock()
	defer urlCacheMutex.Unlock()
	if len(urlCache) >= maxURLCacheEntries {
		urlCache = make(map[string]string, maxURLCacheEntries/2)
	}
	urlCache[key] = value
}

// getCachedURL retrieves a cached URL, returning ok=false if not found.
func getCachedURL(key string) (string, bool) {
	urlCacheMutex.RLock()
	defer urlCacheMutex.RUnlock()
	val, ok := urlCache[key]
	return val, ok
}

// Repository 表示一个Git仓库
type Repository struct {
	Path   string
	Runner Runner

	// 缓存
	statusCache     string
	statusCacheTime time.Time
	branchCache     string
	branchCacheTime time.Time
	cacheMutex      sync.RWMutex
	cacheExpiration time.Duration
}

// RepositoryError 表示仓库操作错误
type RepositoryError struct {
	Op      string // 操作名称
	Path    string // 仓库路径
	Command string // Git命令
	Err     error  // 原始错误
}

func (e *RepositoryError) Error() string {
	if e.Path != "" && e.Command != "" {
		return fmt.Sprintf("git repository error: %s failed in '%s': %s: %v", e.Op, e.Path, e.Command, e.Err)
	}
	if e.Path != "" {
		return fmt.Sprintf("git repository error: %s failed in '%s': %v", e.Op, e.Path, e.Err)
	}
	if e.Command != "" {
		return fmt.Sprintf("git repository error: %s failed: %s: %v", e.Op, e.Command, e.Err)
	}
	return fmt.Sprintf("git repository error: %s failed: %v", e.Op, e.Err)
}

func (e *RepositoryError) Unwrap() error {
	return e.Err
}

// NewRepository 创建一个新的Git仓库实例
func NewRepository(path string, runner Runner) *Repository {
	return &Repository{
		Path:            path,
		Runner:          runner,
		cacheExpiration: time.Minute * 5, // 默认缓存过期时间分钟
	}
}

// SetCacheExpiration 设置缓存过期时间
func (r *Repository) SetCacheExpiration(duration time.Duration) {
	r.cacheMutex.Lock()
	defer r.cacheMutex.Unlock()
	r.cacheExpiration = duration
}

// ClearCache 清除缓存
func (r *Repository) ClearCache() {
	r.cacheMutex.Lock()
	defer r.cacheMutex.Unlock()
	r.statusCache = ""
	r.branchCache = ""
}

// RunCommand 执行Git命令并返回结
func (r *Repository) RunCommand(args ...string) ([]byte, error) {
	repoLog.Debug("在仓'%s' 执行命令: git %s", r.Path, strings.Join(args, " "))

	output, err := r.Runner.RunInDir(r.Path, args...)
	if err != nil {
		// 失败详情以 Warn 记录（含 stderr 细节），错误本身返回给调用方决定错误级别，
		// 避免 log+return 双重处理
		repoLog.Warn("命令执行failed: git %s: %v", strings.Join(args, " "), err)
		return nil, &RepositoryError{
			Op:      "run_command",
			Path:    r.Path,
			Command: fmt.Sprintf("git %s", strings.Join(args, " ")),
			Err:     err,
		}
	}

	repoLog.Debug("命令执行成功: git %s", strings.Join(args, " "))
	return output, nil
}

// CloneOptions contains options for git clone
// CloneOptions 包含克隆选项
type CloneOptions struct {
	Depth  int
	Branch string
	Config *config.Config // 添加Config字段
}

// FetchOptions contains options for git fetch
type FetchOptions struct {
	Prune  bool
	Tags   bool
	Depth  int
	Config *config.Config // 添加Config字段
}

// Exists 检查仓库是否存
func (r *Repository) Exists() (bool, error) {
	gitDir := filepath.Join(r.Path, ".git")
	_, err := os.Stat(gitDir)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// Clone 克隆一个Git仓库
func (r *Repository) Clone(repoURL string, opts CloneOptions) error {
	repoLog.Info("克隆仓库: %s %s", repoURL, r.Path)

	// 构建克隆参数
	args := []string{"clone"}
	if opts.Depth > 0 {
		args = append(args, "--depth", fmt.Sprintf("%d", opts.Depth))
		repoLog.Debug("使用深度: %d", opts.Depth)
	}
	if opts.Branch != "" {
		args = append(args, "--branch", opts.Branch)
		repoLog.Debug("使用分支: %s", opts.Branch)
	}

	// 处理URL
	resolvedURL, err := resolveRepositoryURL(repoURL, opts.Config)
	if err != nil {
		repoLog.Error("解析仓库URLfailed: %v", err)
		return &RepositoryError{
			Op:   "clone",
			Path: r.Path,
			Err:  fmt.Errorf("failed to resolve repository URL: %w", err),
		}
	}

	repoLog.Debug("解析后的URL: %s", resolvedURL)

	// 执行克隆命令
	args = append(args, resolvedURL, r.Path)
	_, err = r.Runner.Run(args...)
	if err != nil {
		return &RepositoryError{
			Op:      "clone",
			Path:    r.Path,
			Command: fmt.Sprintf("git clone %s", resolvedURL),
			Err:     err,
		}
	}

	repoLog.Info("仓库克隆成功: %s", r.Path)
	return nil
}

// Fetch 从远程获取更
func (r *Repository) Fetch(remote string, opts FetchOptions) error {
	repoLog.Info("从远'%s' 获取更新'%s'", remote, r.Path)

	// 解析远程URL
	resolvedRemote := remote
	if strings.HasPrefix(remote, "../") || !strings.Contains(remote, "://") {
		var err error
		resolvedRemote, err = resolveRepositoryURL(remote, opts.Config)
		if err != nil {
			return &RepositoryError{
				Op:   "fetch",
				Path: r.Path,
				Err:  fmt.Errorf("failed to resolve remote URL: %w", err),
			}
		}
		repoLog.Debug("解析后的远程URL: %s", resolvedRemote)
	}

	// 构建fetch参数
	args := []string{"fetch", resolvedRemote}
	if opts.Prune {
		args = append(args, "--prune")
		repoLog.Debug("使用修剪选项")
	}
	if opts.Tags {
		args = append(args, "--tags")
		repoLog.Debug("获取所有标签")
	}
	if opts.Depth > 0 {
		args = append(args, "--depth", fmt.Sprintf("%d", opts.Depth))
		repoLog.Debug("使用深度: %d", opts.Depth)
	}

	// 执行fetch命令
	_, err := r.Runner.RunInDir(r.Path, args...)
	if err != nil {
		return &RepositoryError{
			Op:      "fetch",
			Path:    r.Path,
			Command: fmt.Sprintf("git fetch %s", resolvedRemote),
			Err:     err,
		}
	}

	// 清除缓存，因为fetch可能改变仓库状
	r.ClearCache()

	repoLog.Info("成功从远'%s' 获取更新", resolvedRemote)
	return nil
}

// Checkout checks out a specific revision
func (r *Repository) Checkout(revision string) error {
	_, err := r.Runner.RunInDir(r.Path, "checkout", revision)
	return err
}

// Status 获取仓库状
func (r *Repository) Status() (string, error) {
	// 检查缓
	r.cacheMutex.RLock()
	if r.statusCache != "" && time.Since(r.statusCacheTime) < r.cacheExpiration {
		status := r.statusCache
		r.cacheMutex.RUnlock()
		repoLog.Debug("使用缓存的仓库状态")
		return status, nil
	}
	r.cacheMutex.RUnlock()

	// 获取状态
	repoLog.Debug("获取仓库 '%s' 的状态", r.Path)
	output, err := r.Runner.RunInDir(r.Path, "status", "--porcelain")
	if err != nil {
		repoLog.Error("获取仓库状态failed: %v", err)
		return "", &RepositoryError{
			Op:      "status",
			Path:    r.Path,
			Command: "git status --porcelain",
			Err:     err,
		}
	}

	// 更新缓存
	status := string(output)
	r.cacheMutex.Lock()
	r.statusCache = status
	r.statusCacheTime = time.Now()
	r.cacheMutex.Unlock()

	return status, nil
}

// IsClean 检查仓库是否干净（没有未提交的更改）
func (r *Repository) IsClean() (bool, error) {
	repoLog.Debug("检查仓库'%s' 是否干净", r.Path)
	status, err := r.Status()
	if err != nil {
		return false, err
	}

	isClean := status == ""
	repoLog.Debug("仓库 '%s' %s", r.Path, map[bool]string{true: "干净", false: "有未提交的更改"}[isClean])
	return isClean, nil
}

// BranchExists 检查分支是否存在
func (r *Repository) BranchExists(branch string) (bool, error) {
	// 执行git命令检查分支是否存在
	_, err := r.Runner.RunInDir(r.Path, "rev-parse", "--verify", branch)
	if err != nil {
		// 如果命令failed，分支不存在
		return false, nil
	}
	return true, nil
}

// CurrentBranch 获取当前分支名称
func (r *Repository) CurrentBranch() (string, error) {
	// 检查缓
	r.cacheMutex.RLock()
	if r.branchCache != "" && time.Since(r.branchCacheTime) < r.cacheExpiration {
		branch := r.branchCache
		r.cacheMutex.RUnlock()
		repoLog.Debug("使用缓存的分支名称")
		return branch, nil
	}
	r.cacheMutex.RUnlock()

	// 获取当前分支
	repoLog.Debug("获取仓库 '%s' 的当前分支", r.Path)
	output, err := r.Runner.RunInDir(r.Path, "symbolic-ref", "--short", "HEAD")
	if err != nil {
		// 可能处于分离头指针状态
		output, err = r.Runner.RunInDir(r.Path, "rev-parse", "--short", "HEAD")
		if err != nil {
			repoLog.Error("failed to get current branch: %v", err)
			return "", &RepositoryError{
				Op:      "current_branch",
				Path:    r.Path,
				Command: "git symbolic-ref --short HEAD",
				Err:     err,
			}
		}
		// 处于分离头指针状
		branch := "HEAD detached at " + strings.TrimSpace(string(output))

		// 更新缓存
		r.cacheMutex.Lock()
		r.branchCache = branch
		r.branchCacheTime = time.Now()
		r.cacheMutex.Unlock()

		repoLog.Debug("仓库 '%s' 处于分离头指针状 %s", r.Path, branch)
		return branch, nil
	}

	// 更新缓存
	branch := strings.TrimSpace(string(output))
	r.cacheMutex.Lock()
	r.branchCache = branch
	r.branchCacheTime = time.Now()
	r.cacheMutex.Unlock()

	repoLog.Debug("仓库 '%s' 当前分支: %s", r.Path, branch)
	return branch, nil
}

// HasRevision 检查是否有指定的修订版
func (r *Repository) HasRevision(revision string) (bool, error) {
	repoLog.Debug("检查仓'%s' 是否有修订版 %s", r.Path, revision)
	_, err := r.Runner.RunInDir(r.Path, "rev-parse", "--verify", revision)
	if err != nil {
		repoLog.Debug("仓库 '%s' 没有修订版本: %s", r.Path, revision)
		return false, nil
	}

	repoLog.Debug("仓库 '%s' 有修订版 %s", r.Path, revision)
	return true, nil
}

// resolveRepositoryURL resolves a repository URL, handling relative paths and special formats.
func resolveRepositoryURL(repoURL string, cfg *config.Config) (string, error) {
	// Check cache
	if cachedURL, ok := getCachedURL(repoURL); ok {
		return cachedURL, nil
	}

	// 处理相对路径
	if strings.Contains(repoURL, "..") || strings.HasPrefix(repoURL, "../") {
		// 尝试从配置中获取基础URL
		baseURL := ""
		if cfg != nil {
			baseURL = cfg.ExtractBaseURLFromManifestURL(cfg.ManifestURL)
		}

		if baseURL == "" {
			// 无法确定基础URL，返回原始URL让调用方处理
			repoLog.Debug("failed to get base URL from config，返回原始URL: %s", repoURL)
			return repoURL, nil
		}

		// 确保baseURL不以/结尾
		baseURL = strings.TrimSuffix(baseURL, "/")

		// 处理不同格式的相对路
		var resolvedURL string
		if strings.HasPrefix(repoURL, "../") {
			// 移除相对路径前缀
			relPath := strings.TrimPrefix(repoURL, "../")
			resolvedURL = baseURL + "/" + relPath
		} else {
			// 替换..为baseURL
			resolvedURL = strings.ReplaceAll(repoURL, "..", baseURL)
		}

		// Update cache
		cacheURL(repoURL, resolvedURL)

		return resolvedURL, nil
	}

	// Handle URL format
	if !strings.Contains(repoURL, "://") && !strings.Contains(repoURL, "@") {
		// May be a simple path, try to parse as a valid URL
		if strings.HasPrefix(repoURL, "/") {
			// Absolute path, use file protocol
			resolvedURL := "file://" + repoURL

			cacheURL(repoURL, resolvedURL)

			return resolvedURL, nil
		}

		// Try to parse as HTTP/HTTPS URL
		if _, err := url.Parse("https://" + repoURL); err == nil {
			// Looks like a valid hostname, use HTTPS
			resolvedURL := "https://" + repoURL

			cacheURL(repoURL, resolvedURL)

			return resolvedURL, nil
		}
	}

	// URL is already a complete format or cannot be resolved, return as-is
	return repoURL, nil
}

// DeleteBranch 删除分支
func (r *Repository) DeleteBranch(branch string, force bool) error {
	args := []string{"branch"}

	if force {
		args = append(args, "-D")
	} else {
		args = append(args, "-d")
	}

	args = append(args, branch)

	_, err := r.Runner.RunInDir(r.Path, args...)
	if err != nil {
		return fmt.Errorf("failed to delete branch: %w", err)
	}

	return nil
}

// CreateBranch 创建新分
func (r *Repository) CreateBranch(branch string, startPoint string) error {
	args := []string{"branch"}
	if startPoint != "" {
		args = append(args, branch, startPoint)
	} else {
		args = append(args, branch)
	}

	_, err := r.Runner.RunInDir(r.Path, args...)
	if err != nil {
		return fmt.Errorf("failed to create branch: %w", err)
	}

	return nil
}

// HasChangesToPush 检查是否有需要推送的更改
// remote 参数是远程名称（如 "origin"）
func (r *Repository) HasChangesToPush(remote string) (bool, error) {
	// 获取当前分支名称
	currentBranch, err := r.CurrentBranch()
	if err != nil {
		return false, fmt.Errorf("failed to get current branch: %w", err)
	}

	// 如果当前是 detached HEAD 状态，返回 false
	if currentBranch == "HEAD" {
		return false, nil
	}

	// 构造远程分支名称
	remoteBranch := remote + "/" + currentBranch

	// 检查远程分支是否存在（--quiet：不存在时静默，避免 stderr 噪音）
	_, err = r.Runner.RunInDir(r.Path, "rev-parse", "--verify", "--quiet", remoteBranch)
	if err != nil {
		// 远程分支不存在，说明是新分支，有需要推送的内容
		return true, nil
	}

	// 检查本地分支和远程分支之间的差异
	output, err := r.Runner.RunInDir(r.Path, "rev-list", "--count", currentBranch, "^"+remoteBranch)
	if err != nil {
		return false, fmt.Errorf("failed to check changes to push: %w", err)
	}

	// 如果输出不为0，则有更改需要推
	count := strings.TrimSpace(string(output))
	return count != "0" && count != "", nil
}

// GetBranchName 获取当前分支名称
func (r *Repository) GetBranchName() (string, error) {
	// 使用 Runner 而不runner
	// 使用 Path 而不path
	output, err := r.Runner.RunInDir(r.Path, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return "", fmt.Errorf("failed to get current branch: %w", err)
	}

	return strings.TrimSpace(string(output)), nil
}

// ListRemotes 列出所有远程仓库
func (r *Repository) ListRemotes() ([]string, error) {
	repoLog.Debug("列出仓库 '%s' 的所有远程仓库", r.Path)

	output, err := r.RunCommand("remote")
	if err != nil {
		return nil, &RepositoryError{
			Op:   "list_remotes",
			Path: r.Path,
			Err:  err,
		}
	}

	remotes := strings.Split(strings.TrimSpace(string(output)), "\n")
	// 过滤空字符串
	var result []string
	for _, remote := range remotes {
		if remote != "" {
			result = append(result, remote)
		}
	}

	return result, nil
}

// RemoveRemote 删除指定的远程仓
func (r *Repository) RemoveRemote(remoteName string) error {
	repoLog.Debug("从仓'%s' 中删除远程仓'%s'", r.Path, remoteName)

	_, err := r.RunCommand("remote", "remove", remoteName)
	if err != nil {
		return &RepositoryError{
			Op:   "remove_remote",
			Path: r.Path,
			Err:  fmt.Errorf("failed to remove remote '%s': %w", remoteName, err),
		}
	}

	return nil
}

// CheckRefFormat 验证引用名称格式是否有效
// 相当于原生 git-repo 的 git.check_ref_format
func CheckRefFormat(refName string) error {
	runner := NewRunner()
	_, err := runner.Run("check-ref-format", refName)
	if err != nil {
		return fmt.Errorf("'%s' is not a valid ref name", refName)
	}
	return nil
}

// IsImmutable 检查修订版本表达式是否是不可变的（SHA1、tag或change）
// 不可变的修订版本不能直接推送，需要使用dest_branch或默认修订版本
func IsImmutable(revisionExpr string) bool {
	if revisionExpr == "" {
		return false
	}

	// SHA1 格式检查：40位或7+位十六进制字符
	if len(revisionExpr) >= 7 {
		allHex := true
		for _, c := range revisionExpr {
			// 德摩根变换：非十六进制字符即失败（数字/小写/大写三段均不命中）
			if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
				allHex = false
				break
			}
		}
		if allHex {
			return true
		}
	}

	// refs/tags/ 开头的是tag，不可变
	if strings.HasPrefix(revisionExpr, "refs/tags/") {
		return true
	}

	// refs/changes/ 开头的是change，不可变
	if strings.HasPrefix(revisionExpr, "refs/changes/") {
		return true
	}

	// HEAD, master, main 等分支名是可变的
	return false
}
