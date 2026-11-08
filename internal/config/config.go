package config

import (
	"encoding/json"
	"encoding/xml"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/leopardxu/repo-go/internal/logger"
)

// 包级别的日志记录器
var log logger.Logger = logger.NewDefaultLogger()
var logMu sync.RWMutex

// 配置缓存。缓存 key 由配置文件路径（已包含 RepoRoot）与 mtime 组成，
// 避免在不同 repo 根目录之间错误复用配置。
var (
	configCache       *Config
	configCachePath   string
	configMutex       sync.RWMutex
	configLastModTime time.Time
)

// SetLogger 设置日志记录（线程安全）
func SetLogger(l logger.Logger) {
	logMu.Lock()
	defer logMu.Unlock()
	log = l
}

// ConfigError 表示配置操作中的错误
type ConfigError struct { //nolint:revive // 命名遵循 CLAUDE.md 约定的 XxxError 错误类型形态，保持包内 API 稳定
	Op   string // 操作名称
	Path string // 文件路径
	Err  error  // 原始错误
}

// Error 实现error接口
func (e *ConfigError) Error() string {
	if e.Path != "" {
		return fmt.Sprintf("%s %s: %v", e.Op, e.Path, e.Err)
	}
	return fmt.Sprintf("%s: %v", e.Op, e.Err)
}

// Unwrap 返回原始错误
func (e *ConfigError) Unwrap() error {
	return e.Err
}

// Config 表示repo配置
type Config struct {
	Version                int    `json:"version"`                  // 配置版本
	ManifestURL            string `json:"manifest_url"`             // 清单仓库的URL
	ManifestBranch         string `json:"manifest_branch"`          // 清单仓库的分支
	ManifestUpstreamBranch string `json:"manifest_upstream_branch"` // 提交版本时定位 commit 的 git ref
	ManifestName           string `json:"manifest_name"`            // 清单文件的名称
	Groups                 string `json:"groups"`                   // 项目组
	Platform               string `json:"platform"`                 // 平台
	Mirror                 bool   `json:"mirror"`                   // 是否为镜像
	Archive                bool   `json:"archive"`                  // 是否为存档
	Worktree               bool   `json:"worktree"`                 // 是否使用工作树
	UseLocalGitdirs        bool   `json:"use_local_gitdirs"`        // 是否使用标准 Git 布局
	Reference              string `json:"reference"`                // 引用
	NoSmartCache           bool   `json:"no_smart_cache"`           // 是否禁用智能缓存
	Dissociate             bool   `json:"dissociate"`               // 是否解除关联
	Depth                  int    `json:"depth"`                    // 克隆深度
	ManifestDepth          int    `json:"manifest_depth"`           // 清单仓库浅克隆深度
	PartialClone           bool   `json:"partial_clone"`            // 是否部分克隆
	PartialCloneExclude    string `json:"partial_clone_exclude"`    // 部分克隆排除
	CloneFilter            string `json:"clone_filter"`             // 克隆过滤
	UseSuperproject        bool   `json:"use_superproject"`         // 是否使用超级项目
	CloneBundle            bool   `json:"clone_bundle"`             // 是否使用克隆包
	GitLFS                 bool   `json:"git_lfs"`                  // 是否使用Git LFS
	RepoURL                string `json:"repo_url"`                 // Repo URL
	RepoRev                string `json:"repo_rev"`                 // Repo版本
	NoRepoVerify           bool   `json:"no_repo_verify"`           // 是否禁用Repo验证
	StandaloneManifest     bool   `json:"standalone_manifest"`      // 是否为独立清单
	Submodules             bool   `json:"submodules"`               // 是否包含子模块
	CurrentBranch          bool   `json:"current_branch"`           // 是否使用当前分支
	Tags                   bool   `json:"tags"`                     // 是否包含标签
	ConfigName             string `json:"config_name"`              // 配置名称
	RepoRoot               string `json:"repo_root"`                // 仓库根目录
	DefaultRemoteURL       string `json:"default_remote_url"`       // 默认远程URL
	Verbose                bool   `json:"verbose"`                  // 是否详细输出
	Quiet                  bool   `json:"quiet"`                    // 是否安静模式
	Jobs                   int    `json:"jobs"`                     // 并发任务数
	Color                  string `json:"color"`                    // 输出颜色设置
	Trace                  bool   `json:"trace"`                    // 是否启用跟踪
	ThisManifestOnly       bool   `json:"this_manifest_only"`       // 仅使用当前清单
	AllManifests           bool   `json:"all_manifests"`            // 使用所有清单
	ManifestProject        bool   `json:"manifest_project"`         // 清单项目标记
	NoCurrentBranch        bool   `json:"no_current_branch"`        // 不使用当前分支
	NoTags                 bool   `json:"no_tags"`                  // 不包含标签
	NoPartialClone         bool   `json:"no_partial_clone"`         // 不使用部分克隆
	NoUseSuperproject      bool   `json:"no_use_superproject"`      // 不使用超级项目
	NoCloneBundle          bool   `json:"no_clone_bundle"`          // 不使用克隆包
	NoGitLFS               bool   `json:"no_git_lfs"`               // 不使用Git LFS
	OuterManifest          bool   `json:"outer_manifest"`           // 外部清单
	NoOuterManifest        bool   `json:"no_outer_manifest"`        // 不使用外部清单
}

// Load 加载配置
func Load() (*Config, error) {
	// 先定位顶层 repo 根目录，支持在任意子目录执行
	repoRoot, err := GetRepoRoot()
	if err != nil {
		// 保持原有错误语义：未初始化或不在repo客户端
		return nil, &ConfigError{Op: "load", Err: err}
	}

	configPath := filepath.Join(repoRoot, ".repo", "config.json")
	log.Debug("加载配置文件: %s", configPath)

	// 检查配置文件是否存在
	fileInfo, err := os.Stat(configPath)
	if os.IsNotExist(err) {
		log.Error("配置文件不存在 %s", configPath)
		return nil, &ConfigError{Op: "load", Path: configPath, Err: fmt.Errorf("repo not initialized, run 'repo init' first")}
	}
	if err != nil {
		log.Error("访问配置文件failed: %s, %v", configPath, err)
		return nil, &ConfigError{Op: "load", Path: configPath, Err: fmt.Errorf("failed to access config file: %w", err)}
	}

	// 缓存有效性：路径（含 RepoRoot）与 mtime 都必须一致。mtime 用 != 语义
	// 判定失效（仅相等才命中），避免旧实现 !After 把时间回退误判为有效。
	cacheValid := func() bool {
		return configCache != nil &&
			configCachePath == configPath &&
			fileInfo.ModTime().Equal(configLastModTime)
	}

	// 检查缓存是否有效（命中路径也返回副本，避免调用方修改缓存）
	configMutex.RLock()
	if cacheValid() {
		cached := *configCache
		cached.RepoRoot = repoRoot
		configMutex.RUnlock()
		log.Debug("使用缓存的配置")
		return &cached, nil
	}
	configMutex.RUnlock()

	// 缓存无效，重新加载
	configMutex.Lock()
	defer configMutex.Unlock()

	// 再次检查，避免在获取写锁期间其他goroutine已经更新了缓存
	if cacheValid() {
		cached := *configCache
		cached.RepoRoot = repoRoot
		return &cached, nil
	}

	// 读取配置文件
	data, err := os.ReadFile(configPath)
	if err != nil {
		log.Error("读取配置文件failed: %v", err)
		return nil, &ConfigError{Op: "read", Path: configPath, Err: err}
	}

	log.Debug("成功读取配置文件，大小 %d 字节", len(data))

	// 解析配置
	var config Config
	if err := json.Unmarshal(data, &config); err != nil {
		log.Error("解析配置文件failed: %v", err)
		return nil, &ConfigError{Op: "parse", Path: configPath, Err: err}
	}

	// 设置默认版本
	if config.Version == 0 {
		config.Version = 1
	}

	// 迁移配置
	if err := migrateConfig(&config); err != nil {
		log.Error("迁移配置failed: %v", err)
		return nil, &ConfigError{Op: "migrate", Path: configPath, Err: err}
	}

	// 应用环境变量
	config.ApplyEnvironment()

	// 验证配置
	if err := config.Validate(); err != nil {
		log.Warn("配置验证警告: %v", err)
	}

	// 写入正确的 RepoRoot，供上层使用
	config.RepoRoot = repoRoot

	// 更新缓存（miss 路径也返回副本，调用方修改不会污染包级缓存）
	configCache = &config
	configCachePath = configPath
	configLastModTime = fileInfo.ModTime()

	log.Debug("成功加载配置")
	result := config
	return &result, nil
}

// Save 保存配置
func (c *Config) Save() error {
	log.Debug("保存配置")

	// 先定位顶层 repo 根目录，支持在任意子目录执行
	repoRoot, err := GetRepoRoot()
	if err != nil {
		return &ConfigError{Op: "save", Err: err}
	}

	// 确保.repo目录存在
	repoDir := filepath.Join(repoRoot, ".repo")
	if err := os.MkdirAll(repoDir, 0755); err != nil {
		log.Error("failed to create .repo directory: %v", err)
		return &ConfigError{Op: "save", Path: repoDir, Err: fmt.Errorf("failed to create .repo directory: %w", err)}
	}

	// 验证配置
	if err := c.Validate(); err != nil {
		log.Warn("配置验证警告: %v", err)
	}

	// 确保版本号存在
	if c.Version == 0 {
		c.Version = 1
	}

	// 序列化配置
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		log.Error("序列化配置失 %v", err)
		return &ConfigError{Op: "serialize", Err: err}
	}

	// 写入配置文件
	configPath := filepath.Join(repoRoot, ".repo", "config.json")
	if err := os.WriteFile(configPath, data, 0644); err != nil {
		log.Error("failed to write config file: %v", err)
		return &ConfigError{Op: "write", Path: configPath, Err: err}
	}

	// 更新缓存：写入副本，避免包级缓存与调用方持有的 *Config 别名共享
	configMutex.Lock()
	fileInfo, statErr := os.Stat(configPath)
	if statErr == nil {
		cached := *c
		configCache = &cached
		configCachePath = configPath
		configLastModTime = fileInfo.ModTime()
	} else {
		// 无法确认 mtime 时让缓存失效，下次 Load 重新读取
		configCache = nil
		configCachePath = ""
		configLastModTime = time.Time{}
	}
	configMutex.Unlock()

	log.Debug("配置保存成功")
	return nil
}

// GetRepoRoot 获取repo根目录
func GetRepoRoot() (string, error) {
	log.Debug("查找repo根目录")

	// 从当前目录开始向上查找repo目录
	dir, err := os.Getwd()
	if err != nil {
		log.Error("获取当前目录failed: %v", err)
		return "", &ConfigError{Op: "get_repo_root", Err: fmt.Errorf("failed to get current directory: %w", err)}
	}

	for {
		repoDir := filepath.Join(dir, ".repo")
		if _, err := os.Stat(repoDir); err == nil {
			log.Debug("找到repo根目录: %s", dir)
			return dir, nil
		}

		// 到达根目录，停止查找
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}

	log.Error("未找到repo根目录")
	return "", &ConfigError{Op: "get_repo_root", Err: fmt.Errorf("not in a repo client")}
}
func (c *Config) GetRemoteURL() string {
	if c == nil {
		return ""
	}

	// 首先检查DefaultRemoteURL是否已设置
	if c.DefaultRemoteURL != "" {
		// 确保URL以斜杠结尾
		if !strings.HasSuffix(c.DefaultRemoteURL, "/") {
			return c.DefaultRemoteURL + "/"
		}
		return c.DefaultRemoteURL
	}

	// 先定位repo根目录
	repoRoot, err := GetRepoRoot()
	if err != nil {
		return ""
	}

	// 尝试repo/manifest.xml解析远程URL
	manifestPath := filepath.Join(repoRoot, ".repo", "manifest.xml")
	if _, err := os.Stat(manifestPath); err == nil {
		// 读取manifest.xml文件
		data, err := os.ReadFile(manifestPath)
		if err == nil {
			// 解析XML
			var manifest struct {
				XMLName xml.Name `xml:"manifest"`
				Remotes []struct {
					Name  string `xml:"name,attr"`
					Fetch string `xml:"fetch,attr"`
				} `xml:"remote"`
				Default struct {
					Remote string `xml:"remote,attr"`
				} `xml:"default"`
			}

			if err := xml.Unmarshal(data, &manifest); err == nil {
				// 获取默认远程名称
				defaultRemote := manifest.Default.Remote

				// 查找对应的远程URL
				for _, remote := range manifest.Remotes {
					if remote.Name == defaultRemote {
						fetch := remote.Fetch
						// 确保URL以斜杠结尾
						if !strings.HasSuffix(fetch, "/") {
							fetch += "/"
						}
						return fetch
					}
				}

				// 如果没有找到默认远程，但有其他远程，使用第一个
				if len(manifest.Remotes) > 0 {
					fetch := manifest.Remotes[0].Fetch
					// 确保URL以斜杠结尾
					if !strings.HasSuffix(fetch, "/") {
						fetch += "/"
					}
					return fetch
				}
			}
		}
	}

	// 如果无法从manifest.xml获取，尝试从已发现的 repoRoot 下读取 config.json
	// （不能依赖当前工作目录，命令可能从任意子目录执行）
	configPath := filepath.Join(repoRoot, ".repo", "config.json")
	if _, err := os.Stat(configPath); err == nil {
		// 读取config.json文件
		data, err := os.ReadFile(configPath)
		if err == nil {
			// 解析JSON
			var config struct {
				ManifestURL string `json:"manifest_url"`
			}

			if err := json.Unmarshal(data, &config); err == nil && config.ManifestURL != "" {
				// 使用config.json中的manifest_url
				return c.ExtractBaseURLFromManifestURL(config.ManifestURL)
			}
		}
	}

	// 如果无法从config.json获取，尝试从当前配置的ManifestURL提取
	if c.ManifestURL == "" {
		return ""
	}

	// 使用提取方法从ManifestURL获取基础URL
	return c.ExtractBaseURLFromManifestURL(c.ManifestURL)
}

// ExtractBaseURLFromManifestURL 从清单URL中提取基础URL。
// 带 scheme 的 URL 统一用 net/url 解析，保留 scheme 与 host（含 userinfo/端口）；
// 必须先判断 http(s)，否则 "https://user@host/..." 会被误判为 scp 风式。
func (c *Config) ExtractBaseURLFromManifestURL(manifestURL string) string {
	lower := strings.ToLower(manifestURL)
	if strings.HasPrefix(lower, "http://") ||
		strings.HasPrefix(lower, "https://") ||
		strings.HasPrefix(lower, "ssh://") {
		if u, err := url.Parse(manifestURL); err == nil && u.Host != "" {
			host := u.Host
			if u.User != nil {
				host = u.User.String() + "@" + host
			}
			return u.Scheme + "://" + host
		}
	}

	// 处理SCP格式: git@example.com:path/to/repo
	if strings.Contains(manifestURL, "@") && strings.Contains(manifestURL, ":") &&
		!strings.Contains(manifestURL, "://") {
		// 查找冒号的位置
		parts := strings.SplitN(manifestURL, ":", 2)
		if len(parts) == 2 {
			// 返回 user@hostname 部分
			return parts[0]
		}
	}

	// 无法解析的情况下返回原始URL
	log.Warn("ManifestURL中提取基础URL: %s", manifestURL)
	return manifestURL
}

// GetProjectRemoteURL failed to get project的远程URL
func (c *Config) GetProjectRemoteURL(projectName string) string {
	if c == nil || projectName == "" {
		return ""
	}

	// 如果配置中有DefaultRemoteURL，使用它
	if c.DefaultRemoteURL != "" {
		baseURL := c.DefaultRemoteURL
		if !strings.HasSuffix(baseURL, "/") {
			baseURL += "/"
		}
		return baseURL + projectName
	}

	// 如果没有DefaultRemoteURL，尝试从ManifestURL提取
	if c.ManifestURL != "" {
		baseURL := c.ExtractBaseURLFromManifestURL(c.ManifestURL)
		if baseURL != "" {
			if !strings.HasSuffix(baseURL, "/") {
				baseURL += "/"
			}
			return baseURL + projectName
		}
	}

	return ""
}

// isScpLikeURL 判断是否为 git 的 scp 风式地址（[user@]host:path，无 scheme）。
// 判定规则与 git 一致：冒号必须出现在第一个路径分隔符之前（且 host 非空），
// 因此 "git@host:path" 与 "host:path" 都算 scp 风式，而 "dir/file:tag"、
// "https://user@host/..." 不算。userinfo 可省略，不能强制要求 "@"。
func isScpLikeURL(raw string) bool {
	if raw == "" || strings.Contains(raw, "://") {
		return false
	}
	colon := strings.Index(raw, ":")
	if colon <= 0 {
		return false
	}
	host := raw[:colon]
	// host 段不得包含路径分隔符或空白；带 userinfo 时形如 user@host。
	if strings.ContainsAny(host, `/\`) || strings.ContainsAny(host, " \t") {
		return false
	}
	return true
}

// normalizeScpURL 把 scp 风式地址规范化为 ssh:// URL，便于统一走 urljoin。
func normalizeScpURL(raw string) string {
	return "ssh://" + strings.Replace(raw, ":", "/", 1)
}

// ResolveRelativeURL 将 fetch 地址解析为完整 URL（urljoin 语义）：
//   - 带 scheme、scp 风式（[user@]host:path）与绝对文件路径已是完整地址，
//     原样返回（幂等），本地路径分支不得把它 filepath.Join 到 base 目录；
//   - 其余地址（含 "."、".."、"./"、"../" 以及不带点号的相对 fetch，
//     如 "platform"）一律按 urljoin 解析：带 scheme 或 scp 风式的 base
//     走 net/url，无 scheme 的绝对文件路径按 path/filepath 语义解析。
//
// 没有可用的 base URL 或无法解析时返回空串，绝不放回原样相对值，
// 避免调用方把相对路径误当成完整地址。
func (c *Config) ResolveRelativeURL(relativeURL string) string {
	log.Debug("解析相对URL: %s", relativeURL)

	if c == nil || relativeURL == "" {
		return ""
	}

	// 已完整的地址不需要（也不应）再解析。
	if strings.Contains(relativeURL, "://") || isScpLikeURL(relativeURL) || filepath.IsAbs(relativeURL) {
		return relativeURL
	}

	base := c.DefaultRemoteURL
	if base == "" {
		base = c.ManifestURL
	}
	base = strings.TrimRight(base, "/")
	if base == "" {
		return ""
	}

	// scp 风式先规范化为 ssh://，使 urljoin 能正确处理 host 与路径分隔。
	if isScpLikeURL(base) {
		base = normalizeScpURL(base)
	}

	// 无 scheme 的绝对文件路径：使用 path/filepath 语义（url.Parse 会得到空 scheme）。
	if !strings.Contains(base, "://") {
		if !filepath.IsAbs(base) {
			return ""
		}
		resolved := filepath.Join(filepath.Dir(base), filepath.FromSlash(relativeURL))
		log.Debug("解析结果: %s", resolved)
		return resolved
	}

	u, err := url.Parse(base)
	if err != nil || u.Scheme == "" {
		return ""
	}
	ref, err := url.Parse(relativeURL)
	if err != nil {
		return ""
	}
	resolved := u.ResolveReference(ref)
	if resolved.Scheme == "" {
		return ""
	}
	log.Debug("解析结果: %s", resolved.String())
	return resolved.String()
}

// Validate 验证配置的完整性和正确性
func (c *Config) Validate() error {
	var errs []string

	// 验证必填字段
	if c.ManifestURL == "" {
		errs = append(errs, "manifest_url is required")
	}

	if c.ManifestName == "" {
		errs = append(errs, "manifest_name is required")
	}

	// 验证深度
	if c.Depth < 0 {
		errs = append(errs, "depth must be non-negative")
	}

	// 验证Jobs参数
	if c.Jobs < 0 {
		errs = append(errs, "jobs must be non-negative")
	}

	// 验证互斥选项
	if c.Mirror && c.Archive {
		errs = append(errs, "mirror and archive options are mutually exclusive")
	}

	if c.CurrentBranch && c.NoCurrentBranch {
		errs = append(errs, "current_branch and no_current_branch options are mutually exclusive")
	}

	if c.Tags && c.NoTags {
		errs = append(errs, "tags and no_tags options are mutually exclusive")
	}

	if c.PartialClone && c.NoPartialClone {
		errs = append(errs, "partial_clone and no_partial_clone options are mutually exclusive")
	}

	if c.UseSuperproject && c.NoUseSuperproject {
		errs = append(errs, "use_superproject and no_use_superproject options are mutually exclusive")
	}

	if c.CloneBundle && c.NoCloneBundle {
		errs = append(errs, "clone_bundle and no_clone_bundle options are mutually exclusive")
	}

	if c.GitLFS && c.NoGitLFS {
		errs = append(errs, "git_lfs and no_git_lfs options are mutually exclusive")
	}

	if c.OuterManifest && c.NoOuterManifest {
		errs = append(errs, "outer_manifest and no_outer_manifest options are mutually exclusive")
	}

	if len(errs) > 0 {
		return fmt.Errorf("配置验证failed: %v", strings.Join(errs, "; "))
	}

	return nil
}

// GetJobs 获取并发任务数，如果没有设置则返回默认值
func (c *Config) GetJobs() int {
	if c.Jobs > 0 {
		return c.Jobs
	}
	// 默认使用CPU核心数
	return runtime.NumCPU()
}

// IsColorEnabled 检查是否启用颜色输出。
// auto（及空值）仅在 stdout 是终端时启用；重定向到文件/管道或 CI 环境无色。
func (c *Config) IsColorEnabled() bool {
	switch c.Color {
	case "never":
		return false
	case "always":
		return true
	default: // auto 或其他值
		return isTerminalFile(os.Stdout)
	}
}

// isTerminalFile 判断文件是否为字符设备（TTY）。测试可重定向 os.Stdout
// 到普通文件，此时返回 false。
func isTerminalFile(f *os.File) bool {
	if f == nil {
		return false
	}
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

// EffectiveGroups 返回解析项目清单时使用的组过滤值：
// 显式配置非空时返回原值，未配置（空）时回退 "default"，
// 使各命令在未显式指定 -g 时与上游 repo 的默认组语义一致。
func (c *Config) EffectiveGroups() string {
	if c == nil {
		return "default"
	}
	if strings.TrimSpace(c.Groups) != "" {
		return c.Groups
	}
	return "default"
}

// GetDefaultGroups 获取默认项目组
func (c *Config) GetDefaultGroups() []string {
	if c.Groups == "" {
		return []string{"all"}
	}
	return strings.Split(c.Groups, ",")
}

// GetDefaultPlatform 获取默认平台
func (c *Config) GetDefaultPlatform() []string {
	if c.Platform == "" {
		return []string{}
	}
	return strings.Split(c.Platform, ",")
}

// IsCurrentBranchMode 检查是否使用当前分支模式
func (c *Config) IsCurrentBranchMode() bool {
	if c.NoCurrentBranch {
		return false
	}
	return c.CurrentBranch
}

// IsTagsEnabled 检查是否启用标签
func (c *Config) IsTagsEnabled() bool {
	if c.NoTags {
		return false
	}
	return c.Tags
}

// IsPartialCloneEnabled 检查是否启用部分克隆
func (c *Config) IsPartialCloneEnabled() bool {
	if c.NoPartialClone {
		return false
	}
	return c.PartialClone
}

// IsSuperprojectEnabled 检查是否启用超级项目
func (c *Config) IsSuperprojectEnabled() bool {
	if c.NoUseSuperproject {
		return false
	}
	return c.UseSuperproject
}

// IsCloneBundleEnabled 检查是否启用克隆包
func (c *Config) IsCloneBundleEnabled() bool {
	if c.NoCloneBundle {
		return false
	}
	return c.CloneBundle
}

// IsGitLFSEnabled 检查是否启用Git LFS
func (c *Config) IsGitLFSEnabled() bool {
	if c.NoGitLFS {
		return false
	}
	return c.GitLFS
}

// IsOuterManifestEnabled 检查是否启用外部清单
func (c *Config) IsOuterManifestEnabled() bool {
	if c.NoOuterManifest {
		return false
	}
	return c.OuterManifest
}

// ApplyEnvironment 应用环境变量覆盖配置
func (c *Config) ApplyEnvironment() {
	log.Debug("应用环境变量覆盖配置")

	// 辅助函数：统一使用 REPO_ 前缀读取环境变量
	getEnv := func(key string) string {
		return os.Getenv("REPO_" + key)
	}

	// 检查环境变量并覆盖配置
	if manifestURL := getEnv("MANIFEST_URL"); manifestURL != "" {
		log.Debug("从环境变量设MANIFEST_URL: %s", manifestURL)
		c.ManifestURL = manifestURL
	}

	if manifestBranch := getEnv("MANIFEST_BRANCH"); manifestBranch != "" {
		log.Debug("从环境变量设MANIFEST_BRANCH: %s", manifestBranch)
		c.ManifestBranch = manifestBranch
	}

	if manifestName := getEnv("MANIFEST_NAME"); manifestName != "" {
		log.Debug("从环境变量设MANIFEST_NAME: %s", manifestName)
		c.ManifestName = manifestName
	}

	if groups := getEnv("GROUPS"); groups != "" {
		log.Debug("从环境变量设GROUPS: %s", groups)
		c.Groups = groups
	}

	if platform := getEnv("PLATFORM"); platform != "" {
		log.Debug("从环境变量设PLATFORM: %s", platform)
		c.Platform = platform
	}

	// 布尔值环境变量
	if mirror := getEnv("MIRROR"); mirror == "true" {
		log.Debug("从环境变量设MIRROR: true")
		c.Mirror = true
	} else if mirror == "false" {
		log.Debug("从环境变量设MIRROR: false")
		c.Mirror = false
	}

	if archive := getEnv("ARCHIVE"); archive == "true" {
		log.Debug("从环境变量设ARCHIVE: true")
		c.Archive = true
	} else if archive == "false" {
		log.Debug("从环境变量设ARCHIVE: false")
		c.Archive = false
	}

	// 整数值环境变量
	if depthStr := getEnv("DEPTH"); depthStr != "" {
		if depth, err := strconv.Atoi(depthStr); err == nil {
			log.Debug("从环境变量设DEPTH: %d", depth)
			c.Depth = depth
		} else {
			log.Warn("无效的DEPTH环境变量 %s", depthStr)
		}
	}

	// 日志级别环境变量
	if verbose := getEnv("VERBOSE"); verbose == "true" {
		log.Debug("从环境变量设VERBOSE: true")
		c.Verbose = true
	} else if verbose == "false" {
		log.Debug("从环境变量设VERBOSE: false")
		c.Verbose = false
	}

	if quiet := getEnv("QUIET"); quiet == "true" {
		log.Debug("从环境变量设QUIET: true")
		c.Quiet = true
	} else if quiet == "false" {
		log.Debug("从环境变量设QUIET: false")
		c.Quiet = false
	}
}

// migrateConfig 根据版本号迁移配置
func migrateConfig(config *Config) error {
	// 如果没有版本号，假设为版本1
	if config.Version == 0 {
		config.Version = 1
	}

	// 根据版本号进行迁移
	switch config.Version {
	case 1:
		// 当前版本，无需迁移
		return nil
	default:
		return fmt.Errorf("unsupported config version: %d", config.Version)
	}
}
