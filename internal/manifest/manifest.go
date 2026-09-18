package manifest

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"text/template"
	"time"
)

// 定义错误类型
type ManifestError struct { //nolint:revive // 命名遵循 CLAUDE.md 约定的 XxxError 错误类型形态，保持包内 API 稳定
	Op   string // 操作名称
	Path string // 文件路径
	Err  error  // 原始错误
}

func (e *ManifestError) Error() string {
	if e.Path == "" {
		return fmt.Sprintf("manifest %s: %v", e.Op, e.Err)
	}
	return fmt.Sprintf("manifest %s %s: %v", e.Op, e.Path, e.Err)
}

func (e *ManifestError) Unwrap() error {
	return e.Err
}

// 全局缓存
var (
	manifestCache    = make(map[string]*Manifest)
	manifestCacheMux sync.RWMutex
	fileModTimeCache = make(map[string]time.Time)
	fileModTimeMux   sync.RWMutex
)

// Manifest 表示repo的清单文件
// 支持自定义属性，可以通过CustomAttrs字段访问未在结构体中定义的XML属性
// 在现有的 manifest.go 文件中添加以下字段和方法

// Manifest 表示清单文件
type Manifest struct {
	XMLName        xml.Name          `xml:"manifest"`
	Remotes        []Remote          `xml:"remote"`
	Default        Default           `xml:"default"`
	Projects       []Project         `xml:"project"`
	ExtendProjects []ExtendProject   `xml:"extend-project"` // 扩展已有项目
	Includes       []Include         `xml:"include"`
	RemoveProjects []RemoveProject   `xml:"remove-project"`
	RepoHooks      *RepoHooks        `xml:"repo-hooks"`      // repo钩子配置
	Superproject   *Superproject     `xml:"superproject"`    // 超级项目配置
	ManifestServer *ManifestServer   `xml:"manifest-server"` // manifest服务器
	Notice         string            `xml:"notice"`          // 上游 <notice> 元素：repo sync 完成后展示给用户的提示文本（include 的 notice 会合并至此）
	CustomAttrs    map[string]string `xml:"-"`               // 存储自定义属性

	// 添加与engine.go 兼容的字段
	Subdir              string   // 清单子目录
	RepoDir             string   // 仓库目录
	Topdir              string   // 顶层目录
	WorkDir             string   // working directory
	Server              string   // 服务器
	ManifestProject     *Project // 清单项目
	RepoProject         *Project // 仓库项目
	IsArchive           bool     // 是否为归档
	CloneFilter         string   // 克隆过滤器
	PartialCloneExclude string   // 部分克隆排除

	// 静默模式控制
	SilentMode bool // 是否启用静默模式，不输出非关键日志
}

// ManifestServer 表示 manifest-server 节点
type ManifestServer struct { //nolint:revive // 与上游 repo 的 manifest-server 术语一致，重命名会破坏既有调用方
	URL string `xml:"url,attr"`
}

// GetCustomAttr 获取自定义属性值
func (m *Manifest) GetCustomAttr(name string) (string, bool) {
	val, ok := m.CustomAttrs[name]
	return val, ok
}

// Remote 表示远程Git服务器
// 支持自定义属性，可以通过CustomAttrs字段访问未在结构体中定义的XML属性
type Remote struct {
	Name        string            `xml:"name,attr"`
	Fetch       string            `xml:"fetch,attr"`
	Review      string            `xml:"review,attr,omitempty"`
	Revision    string            `xml:"revision,attr,omitempty"`
	Alias       string            `xml:"alias,attr,omitempty"`
	PushURL     string            `xml:"pushurl,attr,omitempty"` // 新增：推送URL
	CustomAttrs map[string]string `xml:"-"`                      // 存储自定义属性
}

// GetCustomAttr 获取自定义属性值
func (r *Remote) GetCustomAttr(name string) (string, bool) {
	val, ok := r.CustomAttrs[name]
	return val, ok
}

// OptBool 三态布尔：区分 XML 属性未声明与显式 false。取值对齐上游 XmlBool：
// true/1/yes（大小写不敏感）为真，false/0/no 为假；非法值按上游语义忽略
// （非致命警告，视为未声明，取默认值）。
type OptBool struct {
	Set   bool // 属性是否显式声明
	Value bool // 显式声明的值
}

// UnmarshalXMLAttr 实现 XML 属性反序列化（encoding/xml 标准接口）。
func (o *OptBool) UnmarshalXMLAttr(attr xml.Attr) error {
	switch strings.ToLower(attr.Value) {
	case "true", "1", "yes":
		o.Set, o.Value = true, true
	case "false", "0", "no":
		o.Set, o.Value = true, false
	default:
		// 对齐上游 XmlBool：非法值不中断解析，按未声明处理
		o.Set, o.Value = false, false
	}
	return nil
}

// Get 返回最终值：未显式声明时返回 def。
func (o OptBool) Get(def bool) bool {
	if o.Set {
		return o.Value
	}
	return def
}

// Default 表示默认设置
// 支持自定义属性，可以通过CustomAttrs字段访问未在结构体中定义的XML属
type Default struct {
	Remote      string            `xml:"remote,attr"`
	Revision    string            `xml:"revision,attr"`
	DestBranch  string            `xml:"dest-branch,attr,omitempty"` // 默认目标分支
	Upstream    string            `xml:"upstream,attr,omitempty"`    // 默认上游分支
	SyncJ       int               `xml:"sync-j,attr,omitempty"`      // 默认并发数
	SyncC       bool              `xml:"sync-c,attr,omitempty"`      // 默认仅同步当前分支
	SyncS       bool              `xml:"sync-s,attr,omitempty"`      // 默认同步子模块
	SyncTags    OptBool           `xml:"sync-tags,attr,omitempty"`   // 默认同步标签（上游缺省 true）
	Sync        string            `xml:"sync,attr,omitempty"`
	CustomAttrs map[string]string `xml:"-"` // 存储自定义属
}

// GetCustomAttr 获取自定义属性值
func (d *Default) GetCustomAttr(name string) (string, bool) {
	val, ok := d.CustomAttrs[name]
	return val, ok
}

// Project 表示一个Git项目
// 支持自定义属性，可以通过CustomAttrs字段访问未在结构体中定义的XML属
type Project struct {
	Name        string       `xml:"name,attr"`
	Path        string       `xml:"path,attr,omitempty"`
	Remote      string       `xml:"remote,attr,omitempty"`
	Revision    string       `xml:"revision,attr,omitempty"`
	Upstream    string       `xml:"upstream,attr,omitempty"`    // 上游分支，用于跟踪
	DestBranch  string       `xml:"dest-branch,attr,omitempty"` // 目标分支，用于上传审查
	Groups      string       `xml:"groups,attr,omitempty"`
	SyncC       bool         `xml:"sync-c,attr,omitempty"`
	SyncS       bool         `xml:"sync-s,attr,omitempty"`
	SyncTags    OptBool      `xml:"sync-tags,attr,omitempty"` // 项目级同步标签（上游缺省继承 default）
	CloneDepth  int          `xml:"clone-depth,attr,omitempty"`
	ForcePath   bool         `xml:"force-path,attr,omitempty"` // 强制通过 path 而非 name 使用本地镜像
	Copyfiles   []Copyfile   `xml:"copyfile"`
	Linkfiles   []Linkfile   `xml:"linkfile"`
	Annotations []Annotation `xml:"annotation"` // 项目注解
	References  string       `xml:"references,attr,omitempty"`
	// 嵌套子项目：上游用嵌套 <project> 表达 Git submodule（子项目 name 以
	// "父name/子name" 前缀命名，path 相对父项目展开）。Parse 阶段展平进
	// Projects 后此字段会被清空。
	Projects    []Project         `xml:"project"`
	CustomAttrs map[string]string `xml:"-"` // 存储自定义属

	// 添加engine.go 兼容的字
	LastFetch time.Time // 最后一次获取的时间
	NeedGC    bool      // 是否需要垃圾回
}

// GetCustomAttr 获取自定义属性值
func (p *Project) GetCustomAttr(name string) (string, bool) {
	val, ok := p.CustomAttrs[name]
	return val, ok
}

// GetBranch 获取当前分支
func (p *Project) GetBranch() (string, error) {
	if p == nil {
		return "", fmt.Errorf("project is nil")
	}
	return p.Revision, nil
}

// Include 表示包含的清单文
// 支持自定义属性，可以通过CustomAttrs字段访问未在结构体中定义的XML属
type Include struct {
	Name        string            `xml:"name,attr"`
	CustomAttrs map[string]string `xml:"-"` // 存储自定义属
	manifest    *Manifest
}

// GetOuterManifest returns the outermost manifest in the include chain
func (i *Include) GetOuterManifest() *Manifest {
	if i.manifest == nil {
		return nil
	}
	return i.manifest.GetOuterManifest()
}

// GetInnerManifest returns the innermost manifest in the include chain
func (i *Include) GetInnerManifest() *Manifest {
	if i.manifest == nil {
		return nil
	}
	return i.manifest.GetInnerManifest()
}

// GetCustomAttr 获取自定义属性值
func (i *Include) GetCustomAttr(name string) (string, bool) {
	val, ok := i.CustomAttrs[name]
	return val, ok
}

// RemoveProject 表示要移除的项目
// 支持自定义属性，可以通过CustomAttrs字段访问未在结构体中定义的XML属性
type RemoveProject struct {
	Name        string            `xml:"name,attr"`
	Path        string            `xml:"path,attr,omitempty"`     // 新增：路径
	Optional    bool              `xml:"optional,attr,omitempty"` // 新增：可选标志
	CustomAttrs map[string]string `xml:"-"`                       // 存储自定义属性
}

// GetCustomAttr 获取自定义属性值
func (r *RemoveProject) GetCustomAttr(name string) (string, bool) {
	val, ok := r.CustomAttrs[name]
	return val, ok
}

// Copyfile 表示要复制的文件
// 支持自定义属性，可以通过CustomAttrs字段访问未在结构体中定义的XML属
type Copyfile struct {
	Src         string            `xml:"src,attr"`
	Dest        string            `xml:"dest,attr"`
	CustomAttrs map[string]string `xml:"-"` // 存储自定义属
}

// GetCustomAttr 获取自定义属性值
func (c *Copyfile) GetCustomAttr(name string) (string, bool) {
	val, ok := c.CustomAttrs[name]
	return val, ok
}

// Linkfile 表示要链接的文件
// 支持自定义属性，可以通过CustomAttrs字段访问未在结构体中定义的XML属
type Linkfile struct {
	Src         string            `xml:"src,attr"`
	Dest        string            `xml:"dest,attr"`
	CustomAttrs map[string]string `xml:"-"` // 存储自定义属
}

// GetCustomAttr 获取自定义属性值
func (l *Linkfile) GetCustomAttr(name string) (string, bool) {
	val, ok := l.CustomAttrs[name]
	return val, ok
}

// detectIncludeCycle 检测include循环引用
func (p *Parser) detectIncludeCycle(filename string) bool {
	if p.visitedFiles == nil {
		p.visitedFiles = make(map[string]bool)
	}
	if p.visitedFiles[filename] {
		return true // 发现循环引用
	}
	p.visitedFiles[filename] = true
	return false
}

// removeVisitedFile 从访问记录中移除文件（用于递归处理）
func (p *Parser) removeVisitedFile(filename string) {
	if p.visitedFiles != nil {
		delete(p.visitedFiles, filename)
	}
}

// Annotation 表示项目注解
type Annotation struct {
	Name  string `xml:"name,attr"`
	Value string `xml:"value,attr"`
	Keep  string `xml:"keep,attr,omitempty"` // 新增：保留标志，默认为"true"
}

// ExtendProject 表示扩展已有项目的配置
type ExtendProject struct {
	Name      string     `xml:"name,attr"`
	Path      string     `xml:"path,attr,omitempty"`
	Groups    string     `xml:"groups,attr,omitempty"`
	Revision  string     `xml:"revision,attr,omitempty"`
	Remote    string     `xml:"remote,attr,omitempty"`
	Copyfiles []Copyfile `xml:"copyfile"`
	Linkfiles []Linkfile `xml:"linkfile"`
}

// RepoHooks 表示repo钩子配置
type RepoHooks struct {
	InProject   string `xml:"in-project,attr"`
	EnabledList string `xml:"enabled-list,attr,omitempty"`
}

// Superproject 表示超级项目配置
type Superproject struct {
	Name     string `xml:"name,attr"`
	Remote   string `xml:"remote,attr,omitempty"`
	Revision string `xml:"revision,attr,omitempty"` // 新增：修订版本
}

// ToJSON 将清单转换为JSON格式。
// 使用专用结构以产出干净、稳定的 schema（排除运行时字段与 Go 内部字段），
// 字段命名对齐上游 repo manifest --json（experimental）的约定。
func (m *Manifest) ToJSON() (string, error) {
	type remoteJSON struct {
		Name     string `json:"name"`
		Fetch    string `json:"fetch,omitempty"`
		Review   string `json:"review,omitempty"`
		Revision string `json:"revision,omitempty"`
		Alias    string `json:"alias,omitempty"`
		PushURL  string `json:"pushurl,omitempty"`
	}
	type defaultJSON struct {
		Remote     string `json:"remote,omitempty"`
		Revision   string `json:"revision,omitempty"`
		DestBranch string `json:"dest-branch,omitempty"`
		Upstream   string `json:"upstream,omitempty"`
		SyncJ      int    `json:"sync-j,omitempty"`
		SyncC      bool   `json:"sync-c,omitempty"`
		SyncS      bool   `json:"sync-s,omitempty"`
		SyncTags   bool   `json:"sync-tags,omitempty"`
	}
	type copyfileJSON struct {
		Src  string `json:"src"`
		Dest string `json:"dest"`
	}
	type linkfileJSON struct {
		Src  string `json:"src"`
		Dest string `json:"dest"`
	}
	type projectJSON struct {
		Name       string         `json:"name"`
		Path       string         `json:"path,omitempty"`
		Remote     string         `json:"remote,omitempty"`
		Revision   string         `json:"revision,omitempty"`
		Upstream   string         `json:"upstream,omitempty"`
		DestBranch string         `json:"dest-branch,omitempty"`
		Groups     string         `json:"groups,omitempty"`
		SyncC      bool           `json:"sync-c,omitempty"`
		SyncS      bool           `json:"sync-s,omitempty"`
		CloneDepth int            `json:"clone-depth,omitempty"`
		Copyfiles  []copyfileJSON `json:"copyfiles,omitempty"`
		Linkfiles  []linkfileJSON `json:"linkfiles,omitempty"`
	}
	type manifestJSON struct {
		Remotes  []remoteJSON  `json:"remotes"`
		Default  defaultJSON   `json:"default"`
		Projects []projectJSON `json:"projects"`
	}

	out := manifestJSON{
		Default: defaultJSON{
			Remote:     m.Default.Remote,
			Revision:   m.Default.Revision,
			DestBranch: m.Default.DestBranch,
			Upstream:   m.Default.Upstream,
			SyncJ:      m.Default.SyncJ,
			SyncC:      m.Default.SyncC,
			SyncS:      m.Default.SyncS,
			SyncTags:   m.Default.SyncTags.Get(true),
		},
	}
	for _, r := range m.Remotes {
		out.Remotes = append(out.Remotes, remoteJSON{
			Name:     r.Name,
			Fetch:    r.Fetch,
			Review:   r.Review,
			Revision: r.Revision,
			Alias:    r.Alias,
			PushURL:  r.PushURL,
		})
	}
	for _, p := range m.Projects {
		pj := projectJSON{
			Name:       p.Name,
			Path:       p.Path,
			Remote:     p.Remote,
			Revision:   p.Revision,
			Upstream:   p.Upstream,
			DestBranch: p.DestBranch,
			Groups:     p.Groups,
			SyncC:      p.SyncC,
			SyncS:      p.SyncS,
			CloneDepth: p.CloneDepth,
		}
		for _, c := range p.Copyfiles {
			pj.Copyfiles = append(pj.Copyfiles, copyfileJSON{Src: c.Src, Dest: c.Dest})
		}
		for _, l := range p.Linkfiles {
			pj.Linkfiles = append(pj.Linkfiles, linkfileJSON{Src: l.Src, Dest: l.Dest})
		}
		out.Projects = append(out.Projects, pj)
	}

	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return "", fmt.Errorf("failed to marshal manifest to JSON: %w", err)
	}
	return string(data), nil
}

// GetRemoteURL 根据远程名称获取对应的URL
func (m *Manifest) GetRemoteURL(remoteName string) (string, error) {
	for _, remote := range m.Remotes {
		if remote.Name == remoteName {
			return remote.Fetch, nil
		}
	}
	return "", fmt.Errorf("remote %s not found", remoteName)
}

// GetRemoteReview 返回指定 remote 的 Gerrit review URL（<remote review="...">）。
// 匹配时同时考虑 Alias（远程别名）。未找到或未配置时返回 ("", nil)。
func (m *Manifest) GetRemoteReview(remoteName string) (string, error) {
	for _, remote := range m.Remotes {
		if remote.Name == remoteName || (remote.Alias != "" && remote.Alias == remoteName) {
			return remote.Review, nil
		}
	}
	return "", fmt.Errorf("remote %s not found", remoteName)
}

// GetRemotePushURL 返回指定 remote 的推送地址：优先 pushurl 属性，否则回退到 fetch 属性。
// 匹配时同时考虑 Alias（远程别名）。未找到 remote 时返回 ("", error)。
// 用于 repo download 通过 SSH 查询 Gerrit 时取 host（对齐上游：取 push 地址的 host + 29418 端口）。
func (m *Manifest) GetRemotePushURL(remoteName string) (string, error) {
	for _, remote := range m.Remotes {
		if remote.Name == remoteName || (remote.Alias != "" && remote.Alias == remoteName) {
			if remote.PushURL != "" {
				return remote.PushURL, nil
			}
			return remote.Fetch, nil
		}
	}
	return "", fmt.Errorf("remote %s not found", remoteName)
}

// GetOuterManifest 获取最外层的清
func (m *Manifest) GetOuterManifest() *Manifest {
	if len(m.Includes) == 0 {
		return m
	}
	return m.Includes[0].GetOuterManifest()
}

// GetInnerManifest 获取最内层的清单
func (m *Manifest) GetInnerManifest() *Manifest {
	if len(m.Includes) == 0 {
		return m
	}
	return m.Includes[len(m.Includes)-1].GetInnerManifest()
}

// Compiled at package level for performance (O(n) compilation done once, not per call)
var (
	envVarRegex       = regexp.MustCompile(`\$\{([^}]+)\}`)
	envVarSimpleRegex = regexp.MustCompile(`\$([a-zA-Z_][a-zA-Z0-9_]*)`)
)

// replaceVariables replaces variable references in content, supporting ${VAR} and $VAR formats.
func (p *Parser) replaceVariables(content []byte, envVars map[string]string) []byte {
	// First handle ${VAR} format
	result := envVarRegex.ReplaceAllFunc(content, func(match []byte) []byte {
		if val, exists := envVars[string(match[2:len(match)-1])]; exists {
			return []byte(val)
		}
		// If variable is undefined, keep as-is
		return match
	})

	// Then handle $VAR format (single letter or digit variable names)
	result = envVarSimpleRegex.ReplaceAllFunc(result, func(match []byte) []byte {
		if val, exists := envVars[string(match[1:])]; exists {
			return []byte(val)
		}
		// If variable is undefined, keep as-is
		return match
	})

	return result
}

// expandMacros 使用 text/template 引擎执行宏展开
func (p *Parser) expandMacros(content []byte, envVars map[string]string) ([]byte, error) {
	// 定义模板可用函数
	funcMap := template.FuncMap{
		"env": func(key string) string {
			return envVars[key]
		},
	}

	tmpl, err := template.New("manifest").Funcs(funcMap).Parse(string(content))
	if err != nil {
		// 模板解析failed时（可能由于文件包含不成对的 {{ 或 }}），降级为原始内容
		if !p.silentMode {
			fmt.Fprintf(os.Stderr, "警告: 清单文件未能通过模板解析 (%v)，将跳过高级宏展开\n", err)
		}
		return content, nil
	}

	var buf bytes.Buffer
	// 执行模板渲染，传入的 data 为 envVars 映射，使得支持 {{ .VAR_NAME }} 访问环境变量
	if err := tmpl.Execute(&buf, envVars); err != nil {
		return nil, fmt.Errorf("failed to render manifest template: %w", err)
	}

	return buf.Bytes(), nil
}

// getEnvVars 获取环境变量映射
func (p *Parser) getEnvVars() map[string]string {
	vars := make(map[string]string)
	for _, env := range os.Environ() {
		parts := strings.SplitN(env, "=", 2)
		if len(parts) == 2 {
			vars[parts[0]] = parts[1]
		}
	}
	return vars
}

// GetThisManifest 获取当前清单
func (m *Manifest) GetThisManifest() *Manifest {
	return m
}

// Global silent mode setting (thread-safe)
var (
	globalSilentMode   bool
	globalSilentModeMu sync.RWMutex
)

// SetSilentMode sets the global silent mode
func SetSilentMode(silent bool) {
	globalSilentModeMu.Lock()
	defer globalSilentModeMu.Unlock()
	globalSilentMode = silent
}

// getGlobalSilentMode reads the global silent mode (thread-safe)
func getGlobalSilentMode() bool {
	globalSilentModeMu.RLock()
	defer globalSilentModeMu.RUnlock()
	return globalSilentMode
}

// Parser 负责解析清单文件
type Parser struct {
	silentMode   bool
	cacheEnabled bool
	visitedFiles map[string]bool // 用于检测循环引用
	includeDepth int             // 当前解析位于 include 链内的深度（>0 表示是被 include 的子清单）
}

// NewParser creates a manifest parser
func NewParser() *Parser {
	return &Parser{
		silentMode:   getGlobalSilentMode(),
		cacheEnabled: true,
		visitedFiles: make(map[string]bool),
	}
}

// SetParserSilentMode 设置解析器的静默模式
func (p *Parser) SetSilentMode(silent bool) {
	p.silentMode = silent
}

// SetCacheEnabled 设置是否启用缓存
func (p *Parser) SetCacheEnabled(enabled bool) {
	p.cacheEnabled = enabled
}

// ParseFromFile 从文件解析清单
func (p *Parser) ParseFromFile(filename string, groups []string) (*Manifest, error) {
	// 检查参数
	if filename == "" {
		return nil, &ManifestError{Op: "parse", Err: fmt.Errorf("文件名不能为空")}
	}

	// 查找文件
	successPath, err := p.findManifestFile(filename)
	if err != nil {
		return nil, err
	}
	// 缓存 key 统一为绝对路径：findManifestFile 可能返回相对路径，
	// 同名相对路径在不同 cwd 下指向不同文件，跨目录会互相污染缓存
	if absPath, absErr := filepath.Abs(successPath); absErr == nil {
		successPath = absPath
	}

	// 检查缓存
	if p.cacheEnabled {
		manifestCacheMux.RLock()
		fileModTimeMux.RLock()
		cachedManifest, hasCachedManifest := manifestCache[successPath]
		cachedModTime, hasCachedModTime := fileModTimeCache[successPath]
		fileModTimeMux.RUnlock()
		manifestCacheMux.RUnlock()

		if hasCachedManifest && hasCachedModTime {
			// 检查文件是否被修改
			fileInfo, err := os.Stat(successPath)
			if err == nil && !fileInfo.ModTime().After(cachedModTime) {
				// 文件未被修改，使用缓存
				// 深拷贝以避免调用方修改缓存中的切片（Projects/Remotes/Includes 共享底层数组）
				manifestCopy := deepCopyManifest(cachedManifest)

				// 应用组过
				if len(groups) > 0 && !MatchesAllGroups(groups) {
					return p.filterProjectsByGroups(manifestCopy, groups)
				}

				return manifestCopy, nil
			}
		}
	}

	// 读取文件
	data, err := os.ReadFile(successPath)
	if err != nil {
		return nil, &ManifestError{Op: "read", Path: successPath, Err: err}
	}

	// 解析数据：始终以 nil groups 解析出完整清单，缓存只保存未过滤版本；
	// 若直接缓存组过滤结果，同一进程内后续以不同 groups（或 nil）再次解析
	// 同一文件时会拿到被过滤过的项目列表，无法恢复已丢弃的项目
	manifest, err := p.Parse(data, nil)
	if err != nil {
		return nil, err
	}

	// 更新缓存（缓存未过滤的完整清单的深拷贝）
	if p.cacheEnabled {
		fileInfo, err := os.Stat(successPath)
		if err == nil {
			// 深拷贝以避免缓存与返回值共享切片底层数组
			manifestCopy := deepCopyManifest(manifest)

			manifestCacheMux.Lock()
			fileModTimeMux.Lock()
			manifestCache[successPath] = manifestCopy
			fileModTimeCache[successPath] = fileInfo.ModTime()
			fileModTimeMux.Unlock()
			manifestCacheMux.Unlock()
		}
	}

	// 应用组过滤（在返回给调用方的对象上原地过滤，不影响缓存副本）
	if len(groups) > 0 && !MatchesAllGroups(groups) {
		return p.filterProjectsByGroups(manifest, groups)
	}

	return manifest, nil
}

// findManifestFile 查找清单文件的实际路径
func (p *Parser) findManifestFile(filename string) (string, error) {
	// 获取当前working directory
	cwd, err := os.Getwd()
	if err != nil {
		return "", &ManifestError{Op: "find", Err: fmt.Errorf("failed to get current directory: %w", err)}
	}

	// 查找顶层仓库目录
	topDir := findTopLevelRepoDir(cwd)
	if topDir == "" {
		topDir = cwd // 如果找不到顶层目录，使用当前目录
	}

	// 构建可能的路径列表。优先级：显式路径 > .repo/manifests/<name> > 其他位置 >
	// .repo/manifest.xml（合并后的清单，sync 等命令读取）。
	// 注意：-m <name> 必须先于 .repo/manifest.xml 解析到 .repo/manifests/<name>，
	// 否则给定的相对名会被静默替换为默认清单（上游 Override 语义是在
	// .repo/manifests/ 下查找指定清单文件）。
	paths := []string{}

	// 1. 首先尝试调用方显式传入的原始路径：init 传入 .repo/manifests/<name>.xml，
	//    重复 init 时不应回退读取上一次残留的 .repo/manifest.xml，故显式路径优先级最高。
	paths = append(paths, filename)

	// 2. 相对路径：优先在 .repo/manifests/ 下按清单名查找（对齐上游 Override）
	if !filepath.IsAbs(filename) {
		if !strings.HasPrefix(filename, ".repo") {
			paths = append(paths, filepath.Join(".repo", "manifests", filename))
			paths = append(paths, filepath.Join(cwd, ".repo", "manifests", filename))
			paths = append(paths, filepath.Join(topDir, ".repo", "manifests", filename))

			// .repo/<name>（历史兼容位置）
			paths = append(paths, filepath.Join(".repo", filename))
			paths = append(paths, filepath.Join(cwd, ".repo", filename))
			paths = append(paths, filepath.Join(topDir, ".repo", filename))
		}

		// 只使用文件名，在 .repo/manifests/ 目录下查找
		paths = append(paths, filepath.Join(".repo", "manifests", filepath.Base(filename)))
		paths = append(paths, filepath.Join(cwd, ".repo", "manifests", filepath.Base(filename)))
		paths = append(paths, filepath.Join(topDir, ".repo", "manifests", filepath.Base(filename)))

		// 尝试当前目录
		paths = append(paths, filepath.Join(".", filename))
		paths = append(paths, filepath.Join(cwd, filename))
		paths = append(paths, filepath.Join(topDir, filename))
	}

	// 3. 回退到 .repo/manifest.xml（合并后的清单，sync 等命令读取）
	paths = append(paths, ".repo/manifest.xml")
	paths = append(paths, filepath.Join(cwd, ".repo", "manifest.xml"))
	paths = append(paths, filepath.Join(topDir, ".repo", "manifest.xml"))

	// 3. 如果是绝对路径，也尝试其他可能的位置
	if filepath.IsAbs(filename) {
		base := filepath.Base(filename)
		paths = append(paths, filepath.Join(".repo", base))
		paths = append(paths, filepath.Join(cwd, ".repo", base))
		paths = append(paths, filepath.Join(topDir, ".repo", base))
		paths = append(paths, filepath.Join(".repo", "manifests", base))
		paths = append(paths, filepath.Join(cwd, ".repo", "manifests", base))
		paths = append(paths, filepath.Join(topDir, ".repo", "manifests", base))
	}

	// 去除重复的路
	uniquePaths := make([]string, 0, len(paths))
	pathMap := make(map[string]bool)
	for _, path := range paths {
		// 规范化路
		normalizedPath := filepath.Clean(path)
		if !pathMap[normalizedPath] {
			pathMap[normalizedPath] = true
			uniquePaths = append(uniquePaths, normalizedPath)
		}
	}
	paths = uniquePaths

	// 尝试查找清单文件

	// 尝试读取文件
	for _, path := range paths {
		if fileExists(path) {
			return path, nil
		}
	}

	// 检repo目录是否存在
	repoPath := filepath.Join(cwd, ".repo")
	if !fileExists(repoPath) {
		return "", &ManifestError{Op: "find", Err: fmt.Errorf(".repo目录不存在，请先运行 'repo init' 命令")}
	}

	// 检repo/manifest.xml是否存在
	manifestPath := filepath.Join(repoPath, "manifest.xml")
	if !fileExists(manifestPath) {
		return "", &ManifestError{Op: "find", Err: fmt.Errorf(".repo目录中未找到manifest.xml文件，请先运'repo init' 命令")}
	}

	return "", &ManifestError{Op: "find", Err: fmt.Errorf("failed to find manifest file from any location (已尝%d 个路", len(paths))}
}

// fileExists 检查文件是否存
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// deepCopyManifest 创建 manifest 的深拷贝，确保 Projects/Remotes/Includes 等切片
// 不与原对象共享底层数组。用于缓存读写，防止调用方修改切片元素时污染缓存。
func deepCopyManifest(src *Manifest) *Manifest {
	if src == nil {
		return nil
	}
	dst := *src
	dst.CustomAttrs = copyAttrsMap(src.CustomAttrs)
	dst.Default.CustomAttrs = copyAttrsMap(src.Default.CustomAttrs)
	if len(src.Remotes) > 0 {
		dst.Remotes = make([]Remote, len(src.Remotes))
		copy(dst.Remotes, src.Remotes)
		for i := range dst.Remotes {
			dst.Remotes[i].CustomAttrs = copyAttrsMap(src.Remotes[i].CustomAttrs)
		}
	}
	if len(src.Projects) > 0 {
		dst.Projects = make([]Project, len(src.Projects))
		copy(dst.Projects, src.Projects)
		for i := range dst.Projects {
			dst.Projects[i].CustomAttrs = copyAttrsMap(src.Projects[i].CustomAttrs)
			dst.Projects[i].Copyfiles = copyCopyfiles(src.Projects[i].Copyfiles)
			dst.Projects[i].Linkfiles = copyLinkfiles(src.Projects[i].Linkfiles)
		}
	}
	if len(src.Includes) > 0 {
		dst.Includes = make([]Include, len(src.Includes))
		copy(dst.Includes, src.Includes)
		for i := range dst.Includes {
			dst.Includes[i].CustomAttrs = copyAttrsMap(src.Includes[i].CustomAttrs)
			// 递归深拷贝被 include 的清单，避免通过 GetOuter/GetInnerManifest
			// 链修改到缓存共享的内层对象
			dst.Includes[i].manifest = deepCopyManifest(src.Includes[i].manifest)
		}
	}
	if len(src.RemoveProjects) > 0 {
		dst.RemoveProjects = make([]RemoveProject, len(src.RemoveProjects))
		copy(dst.RemoveProjects, src.RemoveProjects)
		for i := range dst.RemoveProjects {
			dst.RemoveProjects[i].CustomAttrs = copyAttrsMap(src.RemoveProjects[i].CustomAttrs)
		}
	}
	if len(src.ExtendProjects) > 0 {
		dst.ExtendProjects = make([]ExtendProject, len(src.ExtendProjects))
		copy(dst.ExtendProjects, src.ExtendProjects)
		for i := range dst.ExtendProjects {
			dst.ExtendProjects[i].Copyfiles = copyCopyfiles(src.ExtendProjects[i].Copyfiles)
			dst.ExtendProjects[i].Linkfiles = copyLinkfiles(src.ExtendProjects[i].Linkfiles)
		}
	}
	return &dst
}

// copyAttrsMap 深拷贝自定义属性 map；nil 保持 nil 以维持零值语义
func copyAttrsMap(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// copyCopyfiles 深拷贝 copyfile 列表（含 CustomAttrs map）
func copyCopyfiles(src []Copyfile) []Copyfile {
	if len(src) == 0 {
		return src
	}
	out := make([]Copyfile, len(src))
	copy(out, src)
	for i := range out {
		out[i].CustomAttrs = copyAttrsMap(src[i].CustomAttrs)
	}
	return out
}

// copyLinkfiles 深拷贝 linkfile 列表（含 CustomAttrs map）
func copyLinkfiles(src []Linkfile) []Linkfile {
	if len(src) == 0 {
		return src
	}
	out := make([]Linkfile, len(src))
	copy(out, src)
	for i := range out {
		out[i].CustomAttrs = copyAttrsMap(src[i].CustomAttrs)
	}
	return out
}

// filterProjectsByGroups 根据组过滤项
func (p *Parser) filterProjectsByGroups(manifest *Manifest, groups []string) (*Manifest, error) {
	if len(groups) == 0 || MatchesAllGroups(groups) {
		return manifest, nil
	}

	filteredProjects := make([]Project, 0)
	for _, proj := range manifest.Projects {
		if shouldIncludeProject(proj, groups) {
			filteredProjects = append(filteredProjects, proj)
		}
	}

	// 过滤项目完成

	manifest.Projects = filteredProjects
	return manifest, nil
}

// ParseFromBytes 从字节数据解析清
func (p *Parser) ParseFromBytes(data []byte, groups []string) (*Manifest, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("manifest data is empty")
	}
	return p.Parse(data, groups)
}

// Parse 解析清单数据
func (p *Parser) Parse(data []byte, groups []string) (*Manifest, error) {
	// 首先进行变量替换 (保留原有的 $VAR 兼容)
	envVars := p.getEnvVars()
	processedData := p.replaceVariables(data, envVars)

	// 然后应用模板引擎进行高级宏展开
	processedData, err := p.expandMacros(processedData, envVars)
	if err != nil {
		return nil, &ManifestError{Op: "expand_macros", Err: err}
	}

	// 使用处理后的数据进行标准解析
	var manifest Manifest
	if err := xml.Unmarshal(processedData, &manifest); err != nil {
		return nil, &ManifestError{Op: "parse", Err: fmt.Errorf("failed to parse manifest XML: %w", err)}
	}

	// 初始化所有结构体的CustomAttrs字段
	manifest.CustomAttrs = make(map[string]string)
	manifest.Default.CustomAttrs = make(map[string]string)
	for i := range manifest.Remotes {
		manifest.Remotes[i].CustomAttrs = make(map[string]string)
	}
	for i := range manifest.Projects {
		manifest.Projects[i].CustomAttrs = make(map[string]string)
		for j := range manifest.Projects[i].Copyfiles {
			manifest.Projects[i].Copyfiles[j].CustomAttrs = make(map[string]string)
		}
		for j := range manifest.Projects[i].Linkfiles {
			manifest.Projects[i].Linkfiles[j].CustomAttrs = make(map[string]string)
		}
	}
	for i := range manifest.Includes {
		manifest.Includes[i].CustomAttrs = make(map[string]string)
	}
	for i := range manifest.RemoveProjects {
		manifest.RemoveProjects[i].CustomAttrs = make(map[string]string)
	}

	// notice 文本按上游 docstring 风格清洗缩进（对齐 _ParseNotice）
	manifest.Notice = cleanNotice(manifest.Notice)

	// 解析自定义属性：必须先于任何属性消费完成填充。旧实现把根级
	// is-archive 等提取放在本函数之前，读到的永远是空 map。
	if err := parseCustomAttributes(processedData, &manifest); err != nil {
		return nil, &ManifestError{Op: "parse_custom_attrs", Err: err}
	}

	// 初始化新添加的字
	manifest.IsArchive = false        // 默认不是归档
	manifest.CloneFilter = ""         // 默认无克隆过滤器
	manifest.PartialCloneExclude = "" // 默认无部分克隆排
	if isArchive, ok := manifest.GetCustomAttr("is-archive"); ok {
		manifest.IsArchive = isArchive == "true"
	}
	if cloneFilter, ok := manifest.GetCustomAttr("clone-filter"); ok {
		manifest.CloneFilter = cloneFilter
	}
	if partialCloneExclude, ok := manifest.GetCustomAttr("partial-clone-exclude"); ok {
		manifest.PartialCloneExclude = partialCloneExclude
	}

	// 当 default remote 为空且只有一个 remote 时，将该 remote 作为默认值
	if manifest.Default.Remote == "" && len(manifest.Remotes) == 1 {
		defaultRemote := manifest.Remotes[0]
		manifest.Default.Remote = defaultRemote.Name

		// 如果该 remote 有定义 revision，则将其作为默认的 revision
		if manifest.Default.Revision == "" && defaultRemote.Revision != "" {
			manifest.Default.Revision = defaultRemote.Revision
		}
	}

	// 扫描主清单顶层元素顺序（remove-project 依赖文档顺序执行）
	seq := scanTopLevelElements(processedData)

	// 处理包含的清单文件（include 项目追加，notice 合并，悬空 remove 传播）
	includeSegs, err := p.processIncludes(&manifest, groups)
	if err != nil {
		return nil, &ManifestError{Op: "process_includes", Err: err}
	}

	// 按文档顺序执行 remove-project，并展平嵌套子项目（上游用嵌套
	// <project> 表达 Git submodule）
	if err := p.applyRemoveProjects(&manifest, seq, includeSegs); err != nil {
		return nil, &ManifestError{Op: "apply_remove_projects", Err: err}
	}

	// 应用ExtendProject配置
	if err := p.applyExtendProjects(&manifest); err != nil {
		return nil, &ManifestError{Op: "apply_extend_projects", Err: err}
	}

	// 处理local_manifests（如果存在）
	if err := p.processLocalManifests(&manifest, groups); err != nil {
		// local_manifests是可选的，处理失败只记录警告并继续
		if !p.silentMode {
			fmt.Fprintf(os.Stderr, "警告: 处理local manifests失败 (%v)，已忽略\n", err)
		}
	}

	// 对全部项目（含 include/local_manifests 引入的）统一补全缺省值。
	// 对齐上游 _ParseProject 的继承链：path/remote/revision/dest-branch/
	// upstream 缺省时回退到 default 元素。旧实现只补全主清单自身项目，
	// include 引入且未自带 default 的项目 remote/revision 均为空。
	applyProjectDefaults(&manifest)

	// 对项目列表进行去重处理（使用 name + path 组合作为唯一标识）
	deduplicatedProjects := make([]Project, 0)
	projectKeyMap := make(map[string]bool) // 用于跟踪 name+path 组合

	for _, proj := range manifest.Projects {
		// 使用 name + path 组合作为唯一标识
		path := proj.Path
		if path == "" {
			path = proj.Name // 如果path为空，使用name作为path
		}
		key := proj.Name + "@@" + path // 使用特殊分隔符避免冲突

		// 如果 name+path 组合已存在，则跳过
		if projectKeyMap[key] {
			continue
		}

		// 标记 name+path 组合为已处理
		projectKeyMap[key] = true

		// 添加到去重后的列表
		deduplicatedProjects = append(deduplicatedProjects, proj)
	}

	// 更新项目列表
	manifest.Projects = deduplicatedProjects

	// 根据groups过滤项目
	if len(groups) > 0 && !MatchesAllGroups(groups) {
		return p.filterProjectsByGroups(&manifest, groups)
	}

	return &manifest, nil
}

// applyProjectDefaults 对全部项目统一补全 default 元素规定的缺省值，并构建
// 远程 URL 缓存属性。对齐上游 _ParseProject：
//   - path 缺省取 name；
//   - remote/revision/dest-branch/upstream 缺省继承 default 元素。
func applyProjectDefaults(m *Manifest) {
	for i := range m.Projects {
		proj := &m.Projects[i]

		// 如果项目没有指定路径，则使用项目名称作为默认路径
		if proj.Path == "" {
			proj.Path = proj.Name
		}
		// 如果项目没有指定远程仓库，则使用默认远程仓库
		if proj.Remote == "" {
			proj.Remote = m.Default.Remote
		}
		// 如果项目没有指定修订版本，则使用默认修订版本
		if proj.Revision == "" {
			proj.Revision = m.Default.Revision
		}
		// sync-tags 缺省继承 default（上游 _ParseProject：XmlBool(node,
		// "sync-tags", self._default.sync_tags)）；default 亦未声明时由消费方
		// 按上游缺省 true 处理
		if !proj.SyncTags.Set {
			proj.SyncTags = m.Default.SyncTags
		}
		// dest-branch/upstream 缺省继承 default 元素（上游 _ParseProject）
		if proj.DestBranch == "" {
			proj.DestBranch = m.Default.DestBranch
		}
		if proj.Upstream == "" {
			proj.Upstream = m.Default.Upstream
		}
		if proj.CustomAttrs == nil {
			proj.CustomAttrs = make(map[string]string)
		}

		// 验证远程仓库是否存在
		var remoteObj *Remote
		for j := range m.Remotes {
			if m.Remotes[j].Name == proj.Remote {
				remoteObj = &m.Remotes[j]
				break
			}
		}
		if remoteObj == nil {
			// 如果找不到远程仓库，不中断处理
			continue
		}
		// 记录远程仓库的Fetch属性，用于后续构建完整URL
		proj.CustomAttrs["__remote_fetch"] = remoteObj.Fetch

		// 构建完整的远程URL并存储在自定义属性中
		remoteURL := remoteObj.Fetch
		if !strings.HasSuffix(remoteURL, "/") {
			remoteURL += "/"
		}
		remoteURL += proj.Name

		// 存储完整的远程URL
		proj.CustomAttrs["__remote_url"] = remoteURL
	}
}

// parseCustomAttributes 解析XML中的自定义属
func parseCustomAttributes(data []byte, manifest *Manifest) error {
	// 创建一个临时结构来解析XML
	type xmlNode struct {
		XMLName xml.Name   `xml:""`
		Attrs   []xml.Attr `xml:",any,attr"`
		Nodes   []xmlNode  `xml:",any"`
	}

	// 解析XML到临时结
	var root xmlNode
	if err := xml.Unmarshal(data, &root); err != nil {
		return fmt.Errorf("解析XMLfailed: %w", err)
	}

	// 处理根节点的属
	for _, attr := range root.Attrs {
		// 跳过已知属
		if isStandardManifestAttr(attr.Name.Local) {
			continue
		}
		// 存储自定义属
		manifest.CustomAttrs[attr.Name.Local] = attr.Value
	}

	// 处理子节
	for _, node := range root.Nodes {
		switch node.XMLName.Local {
		case "remote":
			// 查找匹配的远程仓
			var name string
			for _, attr := range node.Attrs {
				if attr.Name.Local == "name" {
					name = attr.Value
					break
				}
			}
			// 找到匹配的远程仓库并添加自定义属
			for i, remote := range manifest.Remotes {
				if remote.Name == name {
					for _, attr := range node.Attrs {
						if !isKnownRemoteAttr(attr.Name.Local) {
							manifest.Remotes[i].CustomAttrs[attr.Name.Local] = attr.Value
						}
					}
					break
				}
			}
		case "default":
			// 处理默认设置的自定义属
			for _, attr := range node.Attrs {
				if !isKnownDefaultAttr(attr.Name.Local) {
					manifest.Default.CustomAttrs[attr.Name.Local] = attr.Value
				}
			}
		case "project":
			// 查找匹配的项
			var name string
			for _, attr := range node.Attrs {
				if attr.Name.Local == "name" {
					name = attr.Value
					break
				}
			}
			// 找到匹配的项目并添加自定义属
			for i, project := range manifest.Projects {
				if project.Name == name {
					for _, attr := range node.Attrs {
						if !isKnownProjectAttr(attr.Name.Local) {
							manifest.Projects[i].CustomAttrs[attr.Name.Local] = attr.Value
						}
					}
					// 处理项目的子节点（copyfile和linkfile
					for _, subNode := range node.Nodes {
						switch subNode.XMLName.Local {
						case "copyfile":
							// 查找匹配的copyfile
							var src, dest string
							for _, attr := range subNode.Attrs {
								switch attr.Name.Local {
								case "src":
									src = attr.Value
								case "dest":
									dest = attr.Value
								}
							}
							// 找到匹配的copyfile并添加自定义属
							for j, copyfile := range manifest.Projects[i].Copyfiles {
								if copyfile.Src == src && copyfile.Dest == dest {
									for _, attr := range subNode.Attrs {
										if !isKnownCopyfileAttr(attr.Name.Local) {
											manifest.Projects[i].Copyfiles[j].CustomAttrs[attr.Name.Local] = attr.Value
										}
									}
									break
								}
							}
						case "linkfile":
							// 查找匹配的linkfile
							var src, dest string
							for _, attr := range subNode.Attrs {
								switch attr.Name.Local {
								case "src":
									src = attr.Value
								case "dest":
									dest = attr.Value
								}
							}
							// 找到匹配的linkfile并添加自定义属
							for j, linkfile := range manifest.Projects[i].Linkfiles {
								if linkfile.Src == src && linkfile.Dest == dest {
									for _, attr := range subNode.Attrs {
										if !isKnownLinkfileAttr(attr.Name.Local) {
											manifest.Projects[i].Linkfiles[j].CustomAttrs[attr.Name.Local] = attr.Value
										}
									}
									break
								}
							}
						}
					}
					break
				}
			}
		case "include":
			// 查找匹配的include
			var name string
			for _, attr := range node.Attrs {
				if attr.Name.Local == "name" {
					name = attr.Value
					break
				}
			}
			// 找到匹配的include并添加自定义属
			for i, include := range manifest.Includes {
				if include.Name == name {
					for _, attr := range node.Attrs {
						if !isKnownIncludeAttr(attr.Name.Local) {
							manifest.Includes[i].CustomAttrs[attr.Name.Local] = attr.Value
						}
					}
					break
				}
			}
		case "remove-project":
			// 查找匹配的remove-project
			var name string
			for _, attr := range node.Attrs {
				if attr.Name.Local == "name" {
					name = attr.Value
					break
				}
			}
			// 找到匹配的remove-project并添加自定义属
			for i, removeProject := range manifest.RemoveProjects {
				if removeProject.Name == name {
					for _, attr := range node.Attrs {
						if !isKnownRemoveProjectAttr(attr.Name.Local) {
							manifest.RemoveProjects[i].CustomAttrs[attr.Name.Local] = attr.Value
						}
					}
					break
				}
			}
		}
	}

	return nil
}

// findTopLevelRepoDir 查找包含.repo目录的顶层目
func findTopLevelRepoDir(startDir string) string {
	currentDir := startDir

	// 最多向上查0层目
	for i := 0; i < 10; i++ {
		// 检查当前目录是否包repo目录
		repoDir := filepath.Join(currentDir, ".repo")
		if fileExists(repoDir) {
			return currentDir
		}

		// 获取父目
		parentDir := filepath.Dir(currentDir)

		// 如果已经到达根目录，则停止查
		if parentDir == currentDir {
			break
		}

		currentDir = parentDir
	}

	return ""
}

// processIncludes 处理包含的清单文件。上游将 include 的内容按文档顺序并入
// 同一节点流（子清单的 default/remove 对全局生效）；本实现的递归聚合模型
// 通过以下机制对齐：
//   - 子清单项目在递归 Parse 中应用其自身 default，缺省值再由外层
//     applyProjectDefaults 补全；
//   - 子清单中无法在子清单内匹配的 remove-project（悬空 remove，
//     目标在外层清单）通过 includeSeg 向外传播，在外层重放时执行；
//   - 子清单 <notice> 合并进外层（上游 repo sync 会打印所有清单的 notice）。
func (p *Parser) processIncludes(manifest *Manifest, groups []string) ([]includeSeg, error) {
	// 获取当前working directory
	cwd, err := os.Getwd()
	if err != nil {
		return nil, &ManifestError{Op: "process_includes", Err: fmt.Errorf("failed to get current directory: %w", err)}
	}

	// 查找顶层仓库目录
	topDir := findTopLevelRepoDir(cwd)
	if topDir == "" {
		topDir = cwd // 如果找不到顶层目录，使用当前目录
	}

	segs := make([]includeSeg, 0, len(manifest.Includes))

	// 处理所有包含的清单文件
	for i, include := range manifest.Includes {
		includeName := include.Name

		// 检测循环引用
		if p.detectIncludeCycle(includeName) {
			return nil, fmt.Errorf("include cycle detected: %s", includeName)
		}

		// 构建可能的路径
		paths := []string{}

		// 尝试repo/manifests/目录下查找
		paths = append(paths, filepath.Join(".repo", "manifests", includeName))
		paths = append(paths, filepath.Join(cwd, ".repo", "manifests", includeName))
		paths = append(paths, filepath.Join(topDir, ".repo", "manifests", includeName))

		// 尝试直接使用路径
		paths = append(paths, includeName)
		paths = append(paths, filepath.Join(cwd, includeName))
		paths = append(paths, filepath.Join(topDir, includeName))

		// 去除重复的路径
		uniquePaths := make([]string, 0, len(paths))
		pathMap := make(map[string]bool)
		for _, path := range paths {
			normalizedPath := filepath.Clean(path)
			if !pathMap[normalizedPath] {
				pathMap[normalizedPath] = true
				uniquePaths = append(uniquePaths, normalizedPath)
			}
		}
		paths = uniquePaths

		// 尝试读取文件
		var data []byte
		var readErr error
		var foundFile bool

		for _, path := range paths {
			data, readErr = os.ReadFile(path)
			if readErr == nil {
				foundFile = true
				break
			}
		}

		if !foundFile {
			// 移除访问记录，因为文件不存在
			p.removeVisitedFile(includeName)
			return nil, fmt.Errorf("failed to read included manifest file %s: %w", includeName, readErr)
		}

		// failed to parse included manifest
		// 被解析的文件位于 include 链内：其中悬空的 remove-project 不视为错误
		p.includeDepth++
		includedManifest, err := p.Parse(data, groups)
		p.includeDepth--
		// 解析完成后移除访问记录，允许在其他路径中再次包含
		p.removeVisitedFile(includeName)
		if err != nil {
			return nil, fmt.Errorf("failed to parse included manifest %s: %w", includeName, err)
		}

		// 设置包含关系
		manifest.Includes[i].manifest = includedManifest

		// 合并远程仓库列表
		for _, remote := range includedManifest.Remotes {
			// 检查是否已存在相同名称的远程仓库
			var exists bool
			for _, existingRemote := range manifest.Remotes {
				if existingRemote.Name == remote.Name {
					exists = true
					break
				}
			}
			if !exists {
				manifest.Remotes = append(manifest.Remotes, remote)
			}
		}

		// 记录本 include 引入的项目区间；子清单中无法匹配的悬空
		// remove-project 交由外层重放时执行
		seg := includeSeg{
			start:   len(manifest.Projects),
			removes: includedManifest.RemoveProjects,
		}
		manifest.Projects = append(manifest.Projects, includedManifest.Projects...)
		seg.end = len(manifest.Projects)
		segs = append(segs, seg)

		// 合并 notice（上游 repo sync 会展示所有清单的 notice，此处统一
		// 收敛到最外层 Manifest.Notice）
		if includedManifest.Notice != "" {
			if manifest.Notice == "" {
				manifest.Notice = includedManifest.Notice
			} else if !strings.Contains(manifest.Notice, includedManifest.Notice) {
				manifest.Notice = manifest.Notice + "\n" + includedManifest.Notice
			}
		}
	}

	return segs, nil
}

// cleanNotice 按 docstring 风格清洗 <notice> 文本，对齐上游 _ParseNotice：
// 第二行起计算最小缩进并剥离，去除行尾空白与首尾空行；首行（与 <notice>
// 标签同行）整体去空白。
func cleanNotice(s string) string {
	if s == "" {
		return ""
	}
	// 统一换行符（上游 splitlines 兼容 \r\n）
	s = strings.ReplaceAll(s, "\r\n", "\n")
	lines := strings.Split(s, "\n")

	minIndent := int(^uint(0) >> 1) // sys.maxsize 等价
	for _, line := range lines[1:] {
		lstripped := strings.TrimLeft(line, " \t")
		if lstripped != "" {
			if indent := len(line) - len(lstripped); indent < minIndent {
				minIndent = indent
			}
		}
	}

	clean := make([]string, 0, len(lines))
	clean = append(clean, strings.TrimSpace(lines[0]))
	for _, line := range lines[1:] {
		if minIndent <= len(line) {
			clean = append(clean, strings.TrimRight(line[minIndent:], " \t"))
		} else {
			clean = append(clean, strings.TrimRight(line, " \t"))
		}
	}
	for len(clean) > 0 && clean[0] == "" {
		clean = clean[1:]
	}
	for len(clean) > 0 && clean[len(clean)-1] == "" {
		clean = clean[:len(clean)-1]
	}
	return strings.Join(clean, "\n")
}

// topElem 记录主清单顶层子元素的出现顺序与类别
type topElem struct {
	kind string // "project" / "include" / "remove-project"
	idx  int    // 在对应 Unmarshal 结果切片中的序号
}

// includeSeg 记录单个 <include> 引入的项目在 Projects 切片中的区间，
// 以及子清单中无法在其内部匹配的悬空 remove-project（目标在外层清单）
type includeSeg struct {
	start, end int
	removes    []RemoveProject
}

// scanTopLevelElements 扫描 manifest 根元素的直接子元素，按文档顺序记录
// project/include/remove-project 的出现次序。XML Unmarshal 后各类元素分别进入
// Projects/Includes/RemoveProjects 切片，相互之间的文档顺序丢失；而
// remove-project 的上游语义（只移除此前已定义的项目）依赖该顺序，故需重扫。
func scanTopLevelElements(data []byte) []topElem {
	seq := make([]topElem, 0)
	projIdx, incIdx, rmIdx := 0, 0, 0
	dec := xml.NewDecoder(bytes.NewReader(data))
	depth := 0
	for {
		tok, err := dec.Token()
		if err != nil {
			// io.EOF 或畸形输入：畸形 XML 已由 Unmarshal 统一报错
			break
		}
		switch t := tok.(type) {
		case xml.StartElement:
			depth++
			if depth == 2 { // manifest 根的直接子元素
				switch t.Name.Local {
				case "project":
					seq = append(seq, topElem{kind: "project", idx: projIdx})
					projIdx++
				case "include":
					seq = append(seq, topElem{kind: "include", idx: incIdx})
					incIdx++
				case "remove-project":
					seq = append(seq, topElem{kind: "remove-project", idx: rmIdx})
					rmIdx++
				}
			}
		case xml.EndElement:
			depth--
		}
	}
	return seq
}

// applyRemoveProjects 按文档顺序重放顶层元素：project/include 依序激活项目，
// remove-project 即时移除此前已定义的匹配项目，对齐上游语义：
//   - name 指定而 path 未指定：移除所有同名项目；
//   - path 指定：移除 path 匹配且（name 匹配或 name 未指定）的项目；
//   - name 与 path 均未指定：报错；
//   - 无匹配且 optional 不为 true：顶层解析报错；位于 include 链内时
//     视为悬空 remove（目标在外层清单），保留在 RemoveProjects 字段中
//     由 processIncludes 向外传播；
//   - 已生效的 remove 从 RemoveProjects 字段移除（项目已被删除，快照无需再输出）。
func (p *Parser) applyRemoveProjects(m *Manifest, seq []topElem, segs []includeSeg) error {
	hasRemoves := len(m.RemoveProjects) > 0
	for _, seg := range segs {
		if len(seg.removes) > 0 {
			hasRemoves = true
			break
		}
	}
	if !hasRemoves {
		// 无任何 remove 声明：仅展平嵌套子项目
		flattenNestedProjects(m)
		return nil
	}

	flat := make([]Project, 0, len(m.Projects))
	projCursor := 0 // 主清单顶层项目游标
	incIdx := 0     // include 段游标

	// 悬空 remove：include 段内未匹配且非 optional 的条目，回填到本层
	// RemoveProjects 字段供更外层传播
	dangling := make([]RemoveProject, 0)

	execRemove := func(rp RemoveProject) {
		if removeMatches(&flat, rp) {
			return
		}
		// 无匹配：保留为悬空条目（optional 或递归层交由外层/合并器处理）
		dangling = append(dangling, rp)
	}

	for _, e := range seq {
		switch e.kind {
		case "project":
			if projCursor < len(m.Projects) {
				flat = appendProjectWithChildren(flat, m.Projects[projCursor])
				projCursor++
			}
		case "include":
			if incIdx < len(segs) {
				seg := segs[incIdx]
				incIdx++
				flat = append(flat, m.Projects[seg.start:seg.end]...)
				// 子清单悬空的 remove 在本层继续执行
				for _, rp := range seg.removes {
					execRemove(rp)
				}
			}
		case "remove-project":
			rp := m.RemoveProjects[e.idx]
			if !removeMatches(&flat, rp) {
				dangling = append(dangling, rp)
			}
		}
	}
	// 兜底：seq 未覆盖到的剩余项目全部激活（正常流程应已被消费）
	for ; projCursor < len(m.Projects); projCursor++ {
		flat = appendProjectWithChildren(flat, m.Projects[projCursor])
	}
	for ; incIdx < len(segs); incIdx++ {
		seg := segs[incIdx]
		flat = append(flat, m.Projects[seg.start:seg.end]...)
		for _, rp := range seg.removes {
			execRemove(rp)
		}
	}

	m.Projects = flat

	// 顶层解析时，悬空的非 optional remove 视为错误（上游 fail-fast：
	// remove-project 引用了不存在的项目）。递归层（被 include/local
	// manifest 引入的清单）保留悬空条目供外层执行。
	if p.includeDepth == 0 {
		for _, rp := range dangling {
			if !rp.Optional {
				return fmt.Errorf("remove-project element specifies non-existent project: name=%q path=%q", rp.Name, rp.Path)
			}
		}
	}

	// 重建 RemoveProjects：仅保留悬空条目（供外层传播与合并器执行）
	m.RemoveProjects = dangling
	return nil
}

// removeMatches 从已定义项目列表中移除匹配 remove-project 的项目，返回是否
// 发生了移除。匹配规则对齐上游：
//   - name 与 path 均未指定视为非法（上游直接报错），此处返回 false；
//   - name 指定而 path 未指定：移除所有同名项目；
//   - path 指定：移除 path 匹配且（name 匹配或 name 未指定）的项目。
func removeMatches(projects *[]Project, rp RemoveProject) bool {
	if rp.Name == "" && rp.Path == "" {
		return false
	}
	kept := make([]Project, 0, len(*projects))
	removed := false
	for _, proj := range *projects {
		match := false
		if rp.Path == "" {
			// 仅 name：同名全部移除
			match = proj.Name == rp.Name
		} else {
			// path 匹配且（name 匹配或 name 未指定）
			match = proj.Path == rp.Path && (rp.Name == "" || proj.Name == rp.Name)
		}
		if match {
			removed = true
			continue
		}
		kept = append(kept, proj)
	}
	*projects = kept
	return removed
}

// flattenNestedProjects 将嵌套 <project> 子项目（上游用其表达 Git submodule）
// 展平进主项目列表：子项目紧跟其父之后。
func flattenNestedProjects(m *Manifest) {
	var hasNested bool
	for i := range m.Projects {
		if len(m.Projects[i].Projects) > 0 {
			hasNested = true
			break
		}
	}
	if !hasNested {
		return
	}
	flat := make([]Project, 0, len(m.Projects))
	for _, proj := range m.Projects {
		flat = appendProjectWithChildren(flat, proj)
	}
	m.Projects = flat
}

// appendProjectWithChildren 将项目连同其嵌套子项目追加到列表，子项目按上游规则
// 改写 name/path：
//   - 子项目 name 为 "父name/子name"（上游 _JoinName）；
//   - 子项目显式 path 相对父项目路径展开（上游 _JoinRelpath）；缺省时
//     path 即带父名前缀的 name（上游 _ParseProject 的 path 缺省取 JoinName
//     结果，再由 GetSubprojectPaths 与父路径拼接）；
//   - 其余属性（remote/revision 等）留待 applyProjectDefaults 统一补全。
func appendProjectWithChildren(dst []Project, proj Project) []Project {
	children := proj.Projects
	proj.Projects = nil // 展平后不再保留子树
	dst = append(dst, proj)

	// 父项目 path 缺省即 name（与 applyProjectDefaults 一致）
	parentPath := proj.Path
	if parentPath == "" {
		parentPath = proj.Name
	}
	for _, child := range children {
		child.Name = path.Join(proj.Name, child.Name)
		if child.Path == "" {
			child.Path = path.Join(parentPath, child.Name)
		} else {
			child.Path = path.Join(parentPath, child.Path)
		}
		if child.CustomAttrs == nil {
			child.CustomAttrs = make(map[string]string)
		}
		dst = appendProjectWithChildren(dst, child)
	}
	return dst
}

// applyExtendProjects 应用ExtendProject配置到现有项目
func (p *Parser) applyExtendProjects(manifest *Manifest) error {
	if len(manifest.ExtendProjects) == 0 {
		return nil
	}

	// 创建项目名称到索引的映射
	projectMap := make(map[string]int)
	for i, proj := range manifest.Projects {
		projectMap[proj.Name] = i
		if proj.Path != "" && proj.Path != proj.Name {
			projectMap[proj.Path] = i
		}
	}

	// 应用每个ExtendProject
	for _, extProj := range manifest.ExtendProjects {
		// 查找对应的项目
		idx, found := projectMap[extProj.Name]
		if !found {
			// 如果通过name找不到，尝试通过path查找
			if extProj.Path != "" {
				idx, found = projectMap[extProj.Path]
			}
		}

		if !found {
			// ExtendProject引用的项目不存在，记录警告并跳过
			if !p.silentMode {
				fmt.Fprintf(os.Stderr, "警告: extend-project引用的项目 %s 不存在，已跳过\n", extProj.Name)
			}
			continue
		}

		// 应用扩展配置（增量更新，而非覆盖）
		proj := &manifest.Projects[idx]

		// 更新path（如果指定）
		if extProj.Path != "" {
			proj.Path = extProj.Path
		}

		// 更新groups（如果指定）
		if extProj.Groups != "" {
			proj.Groups = extProj.Groups
		}

		// 更新revision（如果指定）
		if extProj.Revision != "" {
			proj.Revision = extProj.Revision
		}

		// 更新remote（如果指定）
		if extProj.Remote != "" {
			proj.Remote = extProj.Remote
		}

		// 追加copyfiles
		if len(extProj.Copyfiles) > 0 {
			proj.Copyfiles = append(proj.Copyfiles, extProj.Copyfiles...)
		}

		// 追加linkfiles
		if len(extProj.Linkfiles) > 0 {
			proj.Linkfiles = append(proj.Linkfiles, extProj.Linkfiles...)
		}
	}

	return nil
}

// processLocalManifests 处理.repo/local_manifests目录下的所有manifest文件
func (p *Parser) processLocalManifests(manifest *Manifest, groups []string) error {
	// 获取当前working directory
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("failed to get current directory: %w", err)
	}

	// 查找顶层仓库目录
	topDir := findTopLevelRepoDir(cwd)
	if topDir == "" {
		topDir = cwd
	}

	// 构建local_manifests目录路径
	localManifestsDir := filepath.Join(topDir, ".repo", "local_manifests")

	// 检查目录是否存在
	if _, err := os.Stat(localManifestsDir); os.IsNotExist(err) {
		// local_manifests目录不存在，这是正常的
		return nil
	}

	// 读取目录中的所有.xml文件
	files, err := os.ReadDir(localManifestsDir)
	if err != nil {
		return fmt.Errorf("failed to read local_manifests directory: %w", err)
	}

	// 按文件名排序以确保确定性顺序
	var xmlFiles []string
	for _, file := range files {
		if !file.IsDir() && strings.HasSuffix(file.Name(), ".xml") {
			xmlFiles = append(xmlFiles, filepath.Join(localManifestsDir, file.Name()))
		}
	}

	if len(xmlFiles) == 0 {
		return nil
	}

	// 处理每个local manifest文件
	for _, xmlFile := range xmlFiles {
		data, err := os.ReadFile(xmlFile)
		if err != nil {
			// 读取failed，记录警告并继续
			if !p.silentMode {
				fmt.Fprintf(os.Stderr, "警告: 读取local manifest文件 %s 失败 (%v)，已跳过\n", xmlFile, err)
			}
			continue
		}

		// 解析local manifest：其 remove-project 的目标通常在主清单中，
		// 独立解析时无法匹配，须以子清单上下文（悬空保留）解析，
		// 由 mergeLocalManifest 在主清单上执行
		p.includeDepth++
		localManifest, err := p.Parse(data, groups)
		p.includeDepth--
		if err != nil {
			// 解析failed，记录警告并继续
			if !p.silentMode {
				fmt.Fprintf(os.Stderr, "警告: 解析local manifest文件 %s 失败 (%v)，已跳过\n", xmlFile, err)
			}
			continue
		}

		// 合并到主manifest
		if err := p.mergeLocalManifest(manifest, localManifest); err != nil {
			// 合并failed，记录警告并继续
			if !p.silentMode {
				fmt.Fprintf(os.Stderr, "警告: 合并local manifest %s 失败 (%v)，已跳过\n", xmlFile, err)
			}
			continue
		}
	}

	return nil
}

// mergeLocalManifest 将local manifest合并到主manifest
func (p *Parser) mergeLocalManifest(main *Manifest, local *Manifest) error {
	// 合并remotes
	for _, remote := range local.Remotes {
		exists := false
		for _, r := range main.Remotes {
			if r.Name == remote.Name {
				exists = true
				break
			}
		}
		if !exists {
			main.Remotes = append(main.Remotes, remote)
		}
	}

	// 合并projects（覆盖同名项目）
	projectMap := make(map[string]int)
	for i, proj := range main.Projects {
		projectMap[proj.Name] = i
	}

	for _, proj := range local.Projects {
		if idx, exists := projectMap[proj.Name]; exists {
			// 覆盖现有项目
			main.Projects[idx] = proj
		} else {
			// 添加新项目
			main.Projects = append(main.Projects, proj)
			projectMap[proj.Name] = len(main.Projects) - 1
		}
	}

	// 处理remove-project
	for _, rp := range local.RemoveProjects {
		// 从主manifest中移除项目
		for i := 0; i < len(main.Projects); i++ {
			if main.Projects[i].Name == rp.Name {
				main.Projects = append(main.Projects[:i], main.Projects[i+1:]...)
				i--
			}
		}
	}
	// remove-project 的删除会使 projectMap 中记录的下标失效（切片前移），
	// 若沿用旧 map，extend-project 会修改错误的项目甚至越界 panic；
	// 此处基于删除后的项目列表重建索引
	projectMap = make(map[string]int)
	for i, proj := range main.Projects {
		projectMap[proj.Name] = i
	}

	// 应用extend-project
	for _, extProj := range local.ExtendProjects {
		if idx, exists := projectMap[extProj.Name]; exists {
			proj := &main.Projects[idx]
			// 增量更新
			if extProj.Path != "" {
				proj.Path = extProj.Path
			}
			if extProj.Groups != "" {
				proj.Groups = extProj.Groups
			}
			if extProj.Revision != "" {
				proj.Revision = extProj.Revision
			}
			if extProj.Remote != "" {
				proj.Remote = extProj.Remote
			}
			if len(extProj.Copyfiles) > 0 {
				proj.Copyfiles = append(proj.Copyfiles, extProj.Copyfiles...)
			}
			if len(extProj.Linkfiles) > 0 {
				proj.Linkfiles = append(proj.Linkfiles, extProj.Linkfiles...)
			}
		}
	}

	return nil
}

// CreateRepoStructure 创建.repo目录结构
func (m *Manifest) CreateRepoStructure() error {
	// 创建.repo目录
	if err := os.MkdirAll(".repo", 0755); err != nil {
		return fmt.Errorf("failed to create .repo directory: %w", err)
	}

	// 创建.repo/manifests目录
	if err := os.MkdirAll(".repo/manifests", 0755); err != nil {
		return fmt.Errorf("failed to create .repo/manifests directory: %w", err)
	}

	// 创建.repo/project-objects目录
	if err := os.MkdirAll(".repo/project-objects", 0755); err != nil {
		return fmt.Errorf("failed to create .repo/project-objects directory: %w", err)
	}

	// 创建.repo/projects目录
	if err := os.MkdirAll(".repo/projects", 0755); err != nil {
		return fmt.Errorf("failed to create .repo/projects directory: %w", err)
	}

	// 创建.repo/hooks目录
	if err := os.MkdirAll(".repo/hooks", 0755); err != nil {
		return fmt.Errorf("failed to create .repo/hooks directory: %w", err)
	}

	return nil
}

// GitRunner Config 结构体在这里定义，但实际的克隆逻辑在clone.go中实

// GitRunner 接口定义
type GitRunner interface {
	Run(args ...string) ([]byte, error)
}

// Config 配置结构
type Config struct {
	ManifestURL    string
	ManifestBranch string
	ManifestName   string
	Mirror         bool
	Reference      string
	Depth          int
}

// 以下是用于检查属性是否为标准属性的辅助函数
func isStandardManifestAttr(_ string) bool {
	// Manifest没有标准属
	return false
}

func isStandardDefaultAttr(name string) bool {
	switch name {
	case "remote", "revision", "sync", "dest-branch", "upstream", "sync-j", "sync-c", "sync-s", "sync-tags":
		return true
	}
	return false
}

func isKnownDefaultAttr(name string) bool {
	return isStandardDefaultAttr(name)
}

func isKnownRemoteAttr(name string) bool {
	return isStandardRemoteAttr(name)
}

func isStandardRemoteAttr(name string) bool {
	switch name {
	case "name", "fetch", "review", "revision", "alias", "pushurl":
		return true
	}
	return false
}

func isStandardProjectAttr(name string) bool {
	switch name {
	case "name", "path", "remote", "revision", "upstream", "dest-branch", "groups", "sync-c", "sync-s", "sync-tags", "clone-depth", "references":
		return true
	}
	return false
}

func isStandardCopyfileAttr(name string) bool {
	switch name {
	case "src", "dest":
		return true
	}
	return false
}

func isStandardLinkfileAttr(name string) bool {
	switch name {
	case "src", "dest":
		return true
	}
	return false
}

func isStandardRemoveProjectAttr(name string) bool {
	switch name {
	case "name", "path", "optional":
		return true
	}
	return false
}

// 以下是用于检查属性是否为已知属性的辅助函数
func isKnownProjectAttr(name string) bool {
	return isStandardProjectAttr(name)
}

func isKnownCopyfileAttr(name string) bool {
	return isStandardCopyfileAttr(name)
}

func isKnownLinkfileAttr(name string) bool {
	return isStandardLinkfileAttr(name)
}

func isKnownIncludeAttr(name string) bool {
	return isStandardIncludeAttr(name)
}

func isKnownRemoveProjectAttr(name string) bool {
	return isStandardRemoveProjectAttr(name)
}

func isStandardIncludeAttr(name string) bool {
	switch name {
	case "name", "groups", "revision":
		return true
	}
	return false
}

// WriteToFile 将清单写入文件
func (m *Manifest) WriteToFile(filename string) error {
	xml, err := m.ToXML()
	if err != nil {
		return err
	}

	return os.WriteFile(filename, []byte(xml), 0644)
}

// ToXML 将清单转换为XML字符
func (m *Manifest) ToXML() (string, error) {
	// 序列化为 manifest XML。所有属性值经 escapeXMLAttr 转义，确保产出合法 XML，
	// 含 & < > " 的 fetch URL / revision 不会破坏解析（对齐上游 repo manifest）。
	// 自定义属性按键名排序，保证输出稳定可复现。
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<manifest`)
	// 根级自定义属性（例如 --platform 写入的 platform="true"）
	for _, k := range sortedKeys(m.CustomAttrs) {
		b.WriteString(attr(k, m.CustomAttrs[k]))
	}
	b.WriteString(">\n")

	// notice（上游 <notice> 元素：sync 完成后展示给用户的提示文本）
	if m.Notice != "" {
		b.WriteString("  <notice>\n")
		for _, line := range strings.Split(m.Notice, "\n") {
			b.WriteString("    ")
			b.WriteString(escapeXMLAttr(line))
			b.WriteString("\n")
		}
		b.WriteString("  </notice>\n")
	}

	// default
	b.WriteString("  <default")
	b.WriteString(attr("remote", m.Default.Remote))
	b.WriteString(attr("revision", m.Default.Revision))
	if m.Default.DestBranch != "" {
		b.WriteString(attr("dest-branch", m.Default.DestBranch))
	}
	if m.Default.Upstream != "" {
		b.WriteString(attr("upstream", m.Default.Upstream))
	}
	if m.Default.SyncJ > 0 {
		fmt.Fprintf(&b, ` sync-j="%d"`, m.Default.SyncJ)
	}
	if m.Default.SyncC {
		b.WriteString(` sync-c="true"`)
	}
	if m.Default.SyncS {
		b.WriteString(` sync-s="true"`)
	}
	// 上游 defaultToXml：仅最终值为 false 时输出（缺省 true 不输出）
	if !m.Default.SyncTags.Get(true) {
		b.WriteString(` sync-tags="false"`)
	}
	if m.Default.Sync != "" {
		b.WriteString(attr("sync", m.Default.Sync))
	}
	for _, k := range sortedKeys(m.Default.CustomAttrs) {
		b.WriteString(attr(k, m.Default.CustomAttrs[k]))
	}
	b.WriteString(" />\n")

	// 添加远程仓库
	for _, r := range m.Remotes {
		b.WriteString("  <remote")
		b.WriteString(attr("name", r.Name))
		b.WriteString(attr("fetch", r.Fetch))
		if r.Review != "" {
			b.WriteString(attr("review", r.Review))
		}
		if r.Revision != "" {
			b.WriteString(attr("revision", r.Revision))
		}
		if r.Alias != "" {
			b.WriteString(attr("alias", r.Alias))
		}
		if r.PushURL != "" {
			b.WriteString(attr("pushurl", r.PushURL))
		}
		for _, k := range sortedKeys(r.CustomAttrs) {
			b.WriteString(attr(k, r.CustomAttrs[k]))
		}
		b.WriteString(" />\n")
	}

	// 不输出 include 标签（init 合并后不需要）

	// 添加项目
	for _, p := range m.Projects {
		b.WriteString("  <project")
		b.WriteString(attr("name", p.Name))
		// 始终包含 path 属性，如果为空则使用项目名称
		if p.Path != "" {
			b.WriteString(attr("path", p.Path))
		} else {
			b.WriteString(attr("path", p.Name))
		}
		if p.Remote != "" {
			b.WriteString(attr("remote", p.Remote))
		}
		if p.Revision != "" {
			b.WriteString(attr("revision", p.Revision))
		}
		if p.Upstream != "" {
			b.WriteString(attr("upstream", p.Upstream))
		}
		if p.DestBranch != "" {
			b.WriteString(attr("dest-branch", p.DestBranch))
		}
		if p.Groups != "" {
			b.WriteString(attr("groups", p.Groups))
		}
		if p.SyncC {
			b.WriteString(` sync-c="true"`)
		}
		if p.SyncS {
			b.WriteString(` sync-s="true"`)
		}
		if p.CloneDepth > 0 {
			fmt.Fprintf(&b, ` clone-depth="%d"`, p.CloneDepth)
		}
		// 添加项目的自定义属性，但排除以 "__" 开头的内部属性
		for _, k := range sortedKeys(p.CustomAttrs) {
			if strings.HasPrefix(k, "__") {
				continue
			}
			b.WriteString(attr(k, p.CustomAttrs[k]))
		}
		// 上游 ProjectToXml：仅最终值为 false 时输出（缺省 true 不输出）
		if !p.SyncTags.Get(true) {
			b.WriteString(` sync-tags="false"`)
		}
		// 检查是否有 copyfile 或 linkfile 子元素
		if len(p.Copyfiles) > 0 || len(p.Linkfiles) > 0 {
			// 开标签闭合后才能写子元素（否则输出非法 XML）
			b.WriteString(">\n")
			for _, c := range p.Copyfiles {
				b.WriteString("    <copyfile")
				b.WriteString(attr("src", c.Src))
				b.WriteString(attr("dest", c.Dest))
				for _, k := range sortedKeys(c.CustomAttrs) {
					b.WriteString(attr(k, c.CustomAttrs[k]))
				}
				b.WriteString(" />\n")
			}

			for _, l := range p.Linkfiles {
				b.WriteString("    <linkfile")
				b.WriteString(attr("src", l.Src))
				b.WriteString(attr("dest", l.Dest))
				for _, k := range sortedKeys(l.CustomAttrs) {
					b.WriteString(attr(k, l.CustomAttrs[k]))
				}
				b.WriteString(" />\n")
			}

			b.WriteString("  </project>\n")
		} else {
			b.WriteString(" />\n")
		}
	}

	// 添加移除项目
	for _, r := range m.RemoveProjects {
		b.WriteString("  <remove-project")
		b.WriteString(attr("name", r.Name))
		if r.Path != "" {
			b.WriteString(attr("path", r.Path))
		}
		if r.Optional {
			b.WriteString(` optional="true"`)
		}
		for _, k := range sortedKeys(r.CustomAttrs) {
			b.WriteString(attr(k, r.CustomAttrs[k]))
		}
		b.WriteString(" />\n")
	}

	// 添加 superproject（如果存在）
	if m.Superproject != nil {
		b.WriteString("  <superproject")
		b.WriteString(attr("name", m.Superproject.Name))
		if m.Superproject.Remote != "" {
			b.WriteString(attr("remote", m.Superproject.Remote))
		}
		if m.Superproject.Revision != "" {
			b.WriteString(attr("revision", m.Superproject.Revision))
		}
		b.WriteString(" />\n")
	}

	// 关闭 XML
	b.WriteString("</manifest>\n")

	return b.String(), nil
}

// escapeXMLAttr 对 XML 属性值进行转义，保证产出的 manifest 为合法 XML。
// 转义 & < > " 以及换行/回车/制表符（属性值中不应出现裸换行）。
func escapeXMLAttr(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '&':
			b.WriteString("&amp;")
		case '<':
			b.WriteString("&lt;")
		case '>':
			b.WriteString("&gt;")
		case '"':
			b.WriteString("&quot;")
		case '\n':
			b.WriteString("&#xA;")
		case '\r':
			b.WriteString("&#xD;")
		case '\t':
			b.WriteString("&#x9;")
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// attr 返回形如 ` k="v"` 的已转义 XML 属性片段。
func attr(k, v string) string {
	return fmt.Sprintf(` %s="%s"`, k, escapeXMLAttr(v))
}

// sortedKeys 返回 map 键的有序切片，保证 XML 属性输出顺序稳定可复现。
func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func (m *Manifest) ParseFromBytes(data []byte, groups []string) error {
	if len(data) == 0 {
		return fmt.Errorf("manifest data is empty")
	}

	// 创建临时解析
	parser := NewParser()

	// 使用解析器解析数
	parsedManifest, err := parser.Parse(data, groups)
	if err != nil {
		return fmt.Errorf("failed to parse manifest data: %w", err)
	}

	// 更新当前manifest对象
	*m = *parsedManifest

	// 设置清单文件路径相关字段
	if m.RepoDir == "" {
		m.RepoDir = ".repo"
	}
	if m.Topdir == "" {
		if cwd, err := os.Getwd(); err == nil {
			m.Topdir = cwd
		}
	}

	return nil
}

func (m *Manifest) GetCurrentBranch() string {
	if m == nil || m.Default.Revision == "" {
		return ""
	}
	return m.Default.Revision
}

// ContainsAll reports whether the groups slice contains the "all" wildcard.
// This is the single source of truth for the "all" group check, used by both
// the manifest parser and the project manager.
func ContainsAll(groups []string) bool {
	for _, group := range groups {
		if group == "all" {
			return true
		}
	}
	return false
}

// containsAll is kept for backward compatibility within the manifest package.
func containsAll(groups []string) bool {
	return ContainsAll(groups)
}

// MatchesAllGroups 判断组过滤是否恒匹配全部项目：含 "all" 且无排除项。
// 含排除项（"-<group>"）的 filter 须按上游顺序求值语义过滤，不能短路。
func MatchesAllGroups(groups []string) bool {
	for _, g := range groups {
		if strings.HasPrefix(g, "-") {
			return false
		}
	}
	return containsAll(groups)
}

// shouldIncludeProject 检查项目是否应该包含在指定的组中。
// 判定逻辑委托至 Project.MatchesGroupFilter（单一真相源），支持否定表达式
// （以"-"开头的组表示排除）、default 归属、all 通配与 name:/path: 隐式组。
func shouldIncludeProject(project Project, groups []string) bool {
	return project.MatchesGroupFilter(groups)
}
