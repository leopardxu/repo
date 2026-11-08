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

// manifestCacheEntry 缓存未过滤的完整清单及其依赖指纹。
// 指纹覆盖主清单、include 文件、.repo/local_manifests/*.xml 的 mtime，
// 任一依赖变化都会使缓存失效。
type manifestCacheEntry struct {
	manifest    *Manifest
	fingerprint string
}

// 全局缓存
var (
	manifestCache    = make(map[string]manifestCacheEntry)
	manifestCacheMux sync.RWMutex
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

	// elementOrder 记录顶层 project/include/remove-project/extend-project 的
	// 文档顺序（含各切片下标）；includeSegs 记录 include 引入的项目区间与
	// 子清单的文档顺序操作日志。Local manifest 以 fragment 模式解析时保留
	// 原始顺序，mergeLocalManifest 据此按文档顺序将元素应用到主清单。
	elementOrder []topElem
	includeSegs  []includeSeg
	// parsedAsFragment 标记该清单以 local fragment 模式解析（保留原始顺序，
	// 未应用 remove/extend），mergeLocalManifest 可安全递归重放。
	parsedAsFragment bool
	// replayOps 记录本清单按文档顺序执行后的顶层操作（project 已展平，
	// remove/extend 保留原条目）。父清单在 include 位置逐元素重放该日志，
	// 从而保留“remove a → 重新定义 a”等文档顺序语义。
	replayOps []replayOp
}

// ManifestServer 表示 manifest-server 节点
type ManifestServer struct { //nolint:revive // 与上游 repo 的 manifest-server 术语一致，重命名会破坏既有调用方
	URL    string `xml:"url,attr"`
	Helper string `xml:"helper,attr,omitempty"` // 上游 <manifest-server helper="...">：辅助程序路径
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
	Annotations []Annotation      `xml:"annotation"`             // remote 注解（上游 _XmlRemote.annotations）
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
	SyncC       OptBool           `xml:"sync-c,attr,omitempty"`      // 默认仅同步当前分支
	SyncS       OptBool           `xml:"sync-s,attr,omitempty"`      // 默认同步子模块
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
	SyncC       OptBool      `xml:"sync-c,attr,omitempty"`
	SyncS       OptBool      `xml:"sync-s,attr,omitempty"`
	SyncTags    OptBool      `xml:"sync-tags,attr,omitempty"` // 项目级同步标签（上游缺省继承 default）
	CloneDepth  int          `xml:"clone-depth,attr,omitempty"`
	ForcePath   OptBool      `xml:"force-path,attr,omitempty"` // 强制通过 path 而非 name 使用本地镜像
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
	Groups      string            `xml:"groups,attr,omitempty"`   // include 级组：并入被包含清单的所有 project/extend-project
	Revision    string            `xml:"revision,attr,omitempty"` // include 级修订：补到被包含清单中未声明 revision 的 project
	CustomAttrs map[string]string `xml:"-"`                       // 存储自定义属
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
	Optional    OptBool           `xml:"optional,attr,omitempty"` // 新增：可选标志
	BaseRev     string            `xml:"base-rev,attr,omitempty"` // 命中时要求项目有效 revision 与此一致
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

// ExtendProject 表示扩展已有项目的配置。字段语义对齐上游 extend-project：
//   - path 仅作为匹配条件（不改变项目路径），dest-path 才是新路径；
//   - groups 与项目现有组取并集；
//   - base-rev 用于校验 revision 变更基线。
type ExtendProject struct {
	Name       string     `xml:"name,attr"`
	Path       string     `xml:"path,attr,omitempty"`
	DestPath   string     `xml:"dest-path,attr,omitempty"`
	Groups     string     `xml:"groups,attr,omitempty"`
	Revision   string     `xml:"revision,attr,omitempty"`
	BaseRev    string     `xml:"base-rev,attr,omitempty"`
	Remote     string     `xml:"remote,attr,omitempty"`
	DestBranch string     `xml:"dest-branch,attr,omitempty"`
	Upstream   string     `xml:"upstream,attr,omitempty"`
	Copyfiles  []Copyfile `xml:"copyfile"`
	Linkfiles  []Linkfile `xml:"linkfile"`

	// fromInclude 标记该条目由 include 段向外传播。此类条目的匹配目标
	// 可能在外层更后出现，未命中时不按顶层声明 fail-fast，继续向外传播。
	fromInclude bool
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
			SyncC:      m.Default.SyncC.Get(false),
			SyncS:      m.Default.SyncS.Get(false),
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
			SyncC:      p.SyncC.Get(false),
			SyncS:      p.SyncS.Get(false),
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

// findRemote 返回指定名称或别名的 remote（先精确匹配 Name，再匹配 Alias）。
// 与上游一致：项目/默认值中的 remote 属性指 Name；Alias 是本地配置名，
// 但 GetRemote* 系列接口需同时接受两者，避免同一 remote 在不同入口解析结果不一致。
func (m *Manifest) findRemote(name string) *Remote {
	if name == "" {
		return nil
	}
	for i := range m.Remotes {
		if m.Remotes[i].Name == name {
			return &m.Remotes[i]
		}
	}
	for i := range m.Remotes {
		if m.Remotes[i].Alias != "" && m.Remotes[i].Alias == name {
			return &m.Remotes[i]
		}
	}
	return nil
}

// GetRemoteURL 根据远程名称获取对应的URL。匹配时同时考虑 Alias（远程别名），
// 与 GetRemoteReview/GetRemotePushURL 保持一致。
func (m *Manifest) GetRemoteURL(remoteName string) (string, error) {
	if remote := m.findRemote(remoteName); remote != nil {
		return remote.Fetch, nil
	}
	return "", fmt.Errorf("remote %s not found", remoteName)
}

// GetRemoteReview 返回指定 remote 的 Gerrit review URL（<remote review="...">）。
// 匹配时同时考虑 Alias（远程别名）。未找到或未配置时返回 ("", nil)。
func (m *Manifest) GetRemoteReview(remoteName string) (string, error) {
	if remote := m.findRemote(remoteName); remote != nil {
		return remote.Review, nil
	}
	return "", fmt.Errorf("remote %s not found", remoteName)
}

// GetRemotePushURL 返回指定 remote 的推送地址：优先 pushurl 属性，否则回退到 fetch 属性。
// 匹配时同时考虑 Alias（远程别名）。未找到 remote 时返回 ("", error)。
// 用于 repo download 通过 SSH 查询 Gerrit 时取 host（对齐上游：取 push 地址的 host + 29418 端口）。
func (m *Manifest) GetRemotePushURL(remoteName string) (string, error) {
	if remote := m.findRemote(remoteName); remote != nil {
		if remote.PushURL != "" {
			return remote.PushURL, nil
		}
		return remote.Fetch, nil
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
	// useLocalManifests 控制是否在最外层解析时合并 .repo/local_manifests。
	// 默认 true；-m/--manifest-name、diff-manifests 等需要“纯净上游清单”的
	// 场景应显式关闭，避免把当前检出的本地覆盖混入结果。
	useLocalManifests bool
	// localFragment 表示当前 Parse 正在解析 local manifest 片段：跳过
	// remove/extend 的应用与去重，保留原始切片下标供 mergeLocalManifest
	// 按文档顺序重放。
	localFragment bool
	// includeGroups/includeRevision 是 include 链向下继承的组与修订：
	// include 声明的 groups 并集作用于被包含清单的 project/extend-project，
	// revision 补到未声明 revision 的 project（对齐上游 _ParseManifestXml）。
	includeGroups   []string
	includeRevision string
	visitedFiles    map[string]bool // 用于检测循环引用
	includeDepth    int             // 当前解析位于 include 链内的深度（>0 表示是被 include 的子清单）
	parseDepth      int             // Parse 递归深度：1 表示最外层（只有最外层处理 local_manifests）
}

// NewParser creates a manifest parser
func NewParser() *Parser {
	return &Parser{
		silentMode:        getGlobalSilentMode(),
		cacheEnabled:      true,
		useLocalManifests: true,
		visitedFiles:      make(map[string]bool),
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

// SetUseLocalManifests 设置是否在最外层解析时合并 .repo/local_manifests。
// 默认启用；显式传入 false 可解析“纯净”的上游清单（例如 -m 指定清单
// 或 diff-manifests 场景）。该模式参与缓存 key，两种模式互不污染。
func (p *Parser) SetUseLocalManifests(enabled bool) {
	p.useLocalManifests = enabled
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

	// 读取文件（指纹计算与变量检测都需要原始内容）
	data, err := os.ReadFile(successPath)
	if err != nil {
		return nil, &ManifestError{Op: "read", Path: successPath, Err: err}
	}

	// 变量/模板参与替换时清单内容依赖进程环境，直接禁用缓存，避免
	// 同一路径在环境变化后返回陈旧结果。
	cacheable := p.cacheEnabled && !manifestTreeHasVariables(successPath, data, p.useLocalManifests)
	fingerprint := ""
	cacheKey := ""
	if cacheable {
		fingerprint = manifestDependencyFingerprint(successPath, p.useLocalManifests)
		cacheKey = manifestCacheKey(successPath, p.useLocalManifests, p.includeDepth, p.localFragment)
	}

	// 检查缓存
	if cacheable {
		manifestCacheMux.RLock()
		cached, ok := manifestCache[cacheKey]
		manifestCacheMux.RUnlock()
		if ok && cached.fingerprint == fingerprint && cached.manifest != nil {
			// 深拷贝以避免调用方修改缓存中的切片（Projects/Remotes/Includes 共享底层数组）
			manifestCopy := deepCopyManifest(cached.manifest)
			if len(groups) > 0 && !MatchesAllGroups(groups) {
				return p.filterProjectsByGroups(manifestCopy, groups)
			}
			return manifestCopy, nil
		}
	}

	// 解析数据：始终以 nil groups 解析出完整清单，缓存只保存未过滤版本；
	// 若直接缓存组过滤结果，同一进程内后续以不同 groups（或 nil）再次解析
	// 同一文件时会拿到被过滤过的项目列表，无法恢复已丢弃的项目
	manifest, err := p.Parse(data, nil)
	if err != nil {
		return nil, err
	}

	// 更新缓存（缓存未过滤的完整清单的深拷贝）
	if cacheable && manifestFingerprintUnchanged(successPath, fingerprint, p.useLocalManifests) {
		manifestCacheMux.Lock()
		manifestCache[cacheKey] = manifestCacheEntry{
			manifest:    deepCopyManifest(manifest),
			fingerprint: fingerprint,
		}
		manifestCacheMux.Unlock()
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

// manifestVarRefRegex 匹配会被 replaceVariables/expandMacros 替换的变量与模板语法。
var manifestVarRefRegex = regexp.MustCompile(`\$\{|\$[A-Za-z_]|\{\{`)

// hasManifestVariables 判断清单内容是否包含变量/模板引用。此类清单的解析
// 结果依赖进程环境，缓存不可靠，直接禁用。
func hasManifestVariables(data []byte) bool {
	return manifestVarRefRegex.Match(data)
}

// manifestIncludeNames 扫描 XML 中所有 <include name="..."> 的 name（不关心
// 嵌套层级：include 可能出现在任意 depth，processIncludes 会递归展开）。
func manifestIncludeNames(data []byte) []string {
	names := make([]string, 0)
	dec := xml.NewDecoder(bytes.NewReader(data))
	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		start, ok := tok.(xml.StartElement)
		if !ok || start.Name.Local != "include" {
			continue
		}
		for _, a := range start.Attr {
			if a.Name.Local == "name" && a.Value != "" {
				names = append(names, a.Value)
				break
			}
		}
	}
	return names
}

// manifestIncludeRoots 返回 include 名称的解析根目录（与 processIncludes 的
// 查找顺序保持一致）：.repo/manifests、cwd、topDir。
func manifestIncludeRoots(mainPath string) []string {
	cwd, err := os.Getwd()
	if err != nil {
		cwd = "."
	}
	topDir := findTopLevelRepoDir(cwd)
	if topDir == "" {
		topDir = cwd
	}
	roots := []string{}
	for _, dir := range []string{
		filepath.Join(topDir, ".repo", "manifests"),
		cwd,
		topDir,
		filepath.Dir(mainPath),
	} {
		roots = append(roots, dir)
	}
	return roots
}

// manifestDependencyFiles 递归收集清单依赖：主清单自身、include 链（可位于
// 子目录）解析到的文件，以及启用 local manifests 时的 .repo/local_manifests
// 下所有 *.xml 及其 include 依赖。返回收集到的文件（去重）与 include 名称
// 的所有候选路径（含尚未存在的文件，用于感知“文件后来被创建”）。
func manifestDependencyFiles(mainPath string, useLocal bool) (files []string, candidates []string) {
	roots := manifestIncludeRoots(mainPath)
	seen := make(map[string]bool)
	candSeen := make(map[string]bool)

	var walk func(p string)
	walk = func(p string) {
		abs, err := filepath.Abs(p)
		if err != nil {
			abs = p
		}
		if seen[abs] {
			return
		}
		seen[abs] = true
		files = append(files, abs)

		data, readErr := os.ReadFile(abs)
		if readErr != nil {
			return
		}
		for _, name := range manifestIncludeNames(data) {
			resolved := ""
			for _, root := range roots {
				candidate := filepath.Clean(filepath.Join(root, name))
				if !candSeen[candidate] {
					candSeen[candidate] = true
					candidates = append(candidates, candidate)
				}
				if resolved == "" && fileExists(candidate) {
					resolved = candidate
				}
			}
			if resolved != "" {
				walk(resolved)
			}
		}
	}

	walk(mainPath)

	if useLocal {
		topDir := findTopLevelRepoDirOrCwd()
		localDir := filepath.Join(topDir, ".repo", "local_manifests")
		entries, err := os.ReadDir(localDir)
		if err == nil {
			for _, entry := range entries {
				if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".xml") {
					continue
				}
				walk(filepath.Join(localDir, entry.Name()))
			}
		}
	}

	return files, candidates
}

// findTopLevelRepoDirOrCwd 返回包含 .repo 的顶层目录；找不到时退回 cwd。
func findTopLevelRepoDirOrCwd() string {
	cwd, err := os.Getwd()
	if err != nil {
		return "."
	}
	if topDir := findTopLevelRepoDir(cwd); topDir != "" {
		return topDir
	}
	return cwd
}

// manifestTreeHasVariables 检查主清单及其递归 include/local manifest 依赖是否
// 包含变量引用：任一依赖依赖环境时禁用整个文件的缓存。
func manifestTreeHasVariables(mainPath string, data []byte, useLocal bool) bool {
	if hasManifestVariables(data) {
		return true
	}
	files, _ := manifestDependencyFiles(mainPath, useLocal)
	mainAbs, err := filepath.Abs(mainPath)
	if err != nil {
		mainAbs = mainPath
	}
	for _, file := range files {
		if file == mainAbs {
			continue
		}
		content, readErr := os.ReadFile(file)
		if readErr != nil {
			continue
		}
		if hasManifestVariables(content) {
			return true
		}
	}
	return false
}

// manifestCacheKey 生成缓存 key：同一路径在“合并 local manifests”与“纯净
// 上游清单”两种模式下结果不同，必须分开缓存。解析上下文同样影响结果：
// include 链内解析不强制 revision（requireRevision=false），local fragment
// 模式保留原始文档顺序而不应用 remove/extend，因此一并纳入 key，避免不同
// 模式互相命中陈旧结果。
func manifestCacheKey(mainPath string, useLocal bool, includeDepth int, localFragment bool) string {
	return fmt.Sprintf("%s|useLocalManifests=%t|includeDepth=%d|requireRevision=%t|localFragment=%t",
		mainPath, useLocal, includeDepth, includeDepth == 0, localFragment)
}

// manifestDependencyFingerprint 计算清单及其依赖文件的指纹：主清单、递归
// include 文件（可位于子目录）与 local manifests 的 mtime/size，标准目录的
// 有序文件列表，以及 include 候选路径（新增文件同样触发失效）。
func manifestDependencyFingerprint(mainPath string, useLocal bool) string {
	seen := make(map[string]bool)
	entries := make([]string, 0)
	addFile := func(p string) {
		abs, err := filepath.Abs(p)
		if err != nil {
			abs = p
		}
		if seen[abs] {
			return
		}
		seen[abs] = true
		st, statErr := os.Stat(abs)
		if statErr != nil {
			entries = append(entries, abs+":missing")
			return
		}
		entries = append(entries, fmt.Sprintf("%s:%d:%d", abs, st.ModTime().UnixNano(), st.Size()))
	}

	files, candidates := manifestDependencyFiles(mainPath, useLocal)
	for _, f := range files {
		addFile(f)
	}
	// 未解析到的 include 候选路径也纳入指纹：文件从无到有时 stat 结果变化
	for _, candidate := range candidates {
		addFile(candidate)
	}

	topDir := findTopLevelRepoDirOrCwd()
	dirs := []string{
		filepath.Dir(mainPath),
		filepath.Join(topDir, ".repo", "manifests"),
	}
	if useLocal {
		dirs = append(dirs, filepath.Join(topDir, ".repo", "local_manifests"))
	}
	// include 子目录的文件集合也需纳入：递归收集每个候选路径的父目录
	dirSeen := make(map[string]bool)
	for _, candidate := range candidates {
		dir := filepath.Dir(candidate)
		if candidate == mainPath || dirSeen[dir] {
			continue
		}
		dirSeen[dir] = true
		dirs = append(dirs, dir)
	}
	for _, dir := range dirs {
		dirEntries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		names := make([]string, 0, len(dirEntries))
		for _, entry := range dirEntries {
			if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".xml") {
				names = append(names, entry.Name())
			}
		}
		sort.Strings(names)
		for _, name := range names {
			addFile(filepath.Join(dir, name))
		}
		// 目录文件列表本身也纳入指纹：新增/删除 *.xml 必须使缓存失效
		entries = append(entries, "dir:"+dir+":"+strings.Join(names, ","))
	}

	sort.Strings(entries)
	return strings.Join(entries, "\n")
}

// manifestFingerprintUnchanged 在解析完成后重新计算指纹，确认解析期间依赖未变化，
// 避免把基于旧依赖解析的结果写入缓存。
func manifestFingerprintUnchanged(mainPath, fingerprint string, useLocal bool) bool {
	return manifestDependencyFingerprint(mainPath, useLocal) == fingerprint
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
			dst.Remotes[i].Annotations = copyAnnotations(src.Remotes[i].Annotations)
		}
	}
	if len(src.Projects) > 0 {
		dst.Projects = make([]Project, len(src.Projects))
		copy(dst.Projects, src.Projects)
		for i := range dst.Projects {
			dst.Projects[i] = copyProject(src.Projects[i])
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
	if len(src.replayOps) > 0 {
		dst.replayOps = make([]replayOp, len(src.replayOps))
		for i := range src.replayOps {
			dst.replayOps[i] = src.replayOps[i]
			if len(src.replayOps[i].projects) > 0 {
				projects := make([]Project, len(src.replayOps[i].projects))
				for j := range src.replayOps[i].projects {
					projects[j] = copyProject(src.replayOps[i].projects[j])
				}
				dst.replayOps[i].projects = projects
			}
			dst.replayOps[i].remove.CustomAttrs = copyAttrsMap(src.replayOps[i].remove.CustomAttrs)
			dst.replayOps[i].extend.Copyfiles = copyCopyfiles(src.replayOps[i].extend.Copyfiles)
			dst.replayOps[i].extend.Linkfiles = copyLinkfiles(src.replayOps[i].extend.Linkfiles)
		}
	}
	// 指针型节点按值复制，避免缓存与调用方共享同一对象
	if src.RepoHooks != nil {
		hooks := *src.RepoHooks
		dst.RepoHooks = &hooks
	}
	if src.ManifestServer != nil {
		server := *src.ManifestServer
		dst.ManifestServer = &server
	}
	if src.Superproject != nil {
		superproject := *src.Superproject
		dst.Superproject = &superproject
	}
	return &dst
}

// copyProject 深拷贝项目（含嵌套子项目/注解/copyfile/linkfile），用于缓存隔离。
func copyProject(src Project) Project {
	dst := src
	dst.CustomAttrs = copyAttrsMap(src.CustomAttrs)
	dst.Copyfiles = copyCopyfiles(src.Copyfiles)
	dst.Linkfiles = copyLinkfiles(src.Linkfiles)
	dst.Annotations = copyAnnotations(src.Annotations)
	if len(src.Projects) > 0 {
		dst.Projects = make([]Project, len(src.Projects))
		for i := range src.Projects {
			dst.Projects[i] = copyProject(src.Projects[i])
		}
	}
	return dst
}

// copyAnnotations 深拷贝注解切片。
func copyAnnotations(src []Annotation) []Annotation {
	if len(src) == 0 {
		return src
	}
	out := make([]Annotation, len(src))
	copy(out, src)
	return out
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

// validateUniqueProjectPaths 校验项目有效 path（Path 缺省取 Name）唯一性。
// 展平后的嵌套子项目同样参与校验；重复 path（包括 name+path 完全相同）
// 都是清单错误，不能静默去重，否则后续按 path 定位项目会指向错误目标。
func validateUniqueProjectPaths(m *Manifest) error {
	seen := make(map[string]string, len(m.Projects))
	for _, proj := range m.Projects {
		projectPath := normalizeManifestPath(proj.Path)
		if projectPath == "" {
			projectPath = normalizeManifestPath(proj.Name)
		}
		if prev, ok := seen[projectPath]; ok {
			return &ManifestError{
				Op: "parse",
				Err: fmt.Errorf("duplicate path %q: projects %q and %q",
					projectPath, prev, proj.Name),
			}
		}
		seen[projectPath] = proj.Name
	}
	return nil
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
	// parseDepth 用于保证 local_manifests 只在最外层 Parse 处理一次：
	// include/local manifest 的递归 Parse 不得再次扫描 local_manifests，
	// 否则会无限递归。
	p.parseDepth++
	defer func() { p.parseDepth-- }()

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
		initProjectMaps(&manifest.Projects[i])
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

	// default remote 缺省：仅一个 remote 时取之（上游 _get_remote）。
	if manifest.Default.Remote == "" && len(manifest.Remotes) == 1 {
		manifest.Default.Remote = manifest.Remotes[0].Name
	}
	// 注意：上游不会把“第一个声明 revision 的 remote”灌入 Default.Revision。
	// revision 解析按项目进行：project.revision → 该项目 remote.revision →
	// default.revision（见 applyProjectDefaults）。

	// 项目 name/path 合法性校验（上游 _CheckLocalPath 等价）：必须在任何
	// remove/extend/展平之前执行，避免 ../ 路径逃逸或非法组件进入后续逻辑。
	if err := validateProjectTree(manifest.Projects); err != nil {
		return nil, err
	}

	// include 链继承：include 的 groups 并集作用于被包含清单的
	// project/extend-project；include 的 revision 补到未声明 revision 的项目
	// 与嵌套 include（对齐上游 _ParseManifestXml 的 node.setAttribute 语义）。
	p.applyIncludeInheritance(&manifest)

	// 扫描主清单顶层元素顺序（remove-project 依赖文档顺序执行）
	seq := scanTopLevelElements(processedData)
	manifest.elementOrder = seq
	manifest.parsedAsFragment = p.localFragment

	// 处理包含的清单文件（include 项目追加，notice 合并，悬空 remove/extend 传播）。
	// 子清单以 nil groups 解析：先按 -g 过滤会让 include 内的 remove/extend
	// 看不到被排除的项目而误判，最终统一在最外层过滤。
	includeSegs, err := p.processIncludes(&manifest, nil)
	if err != nil {
		return nil, &ManifestError{Op: "process_includes", Err: err}
	}
	manifest.includeSegs = includeSegs

	if p.localFragment {
		// local manifest 片段：保留原始顺序与切片下标。remove/extend 不在
		// 此应用，由 mergeLocalManifest 按文档顺序重放到主清单；也不做去重，
		// 否则下标映射会与 elementOrder 失配。
		if err := applyProjectDefaults(&manifest, false); err != nil {
			return nil, err
		}
		return &manifest, nil
	}

	// include 链内的清单在重放前先补全自身 default：操作日志中的项目条目
	// 会携带这些缺省值，父层重放后仍保留子清单自己的 remote/revision
	// （dest-path 等 extend 改变有效 path 时，重放日志无法事后按 path 回填）。
	if p.includeDepth > 0 {
		if err := applyProjectDefaults(&manifest, false); err != nil {
			return nil, err
		}
	}

	// 按文档顺序重放 project/include/remove-project/extend-project，并展平
	// 嵌套子项目（上游用嵌套 <project> 表达 Git submodule）。操作日志供
	// 父层 include 位置逐元素重放，保留文档顺序语义。
	replayOps, err := p.replayDocumentOrder(&manifest, seq, includeSegs)
	if err != nil {
		return nil, &ManifestError{Op: "replay_manifest", Err: err}
	}
	manifest.replayOps = replayOps

	// 处理local_manifests（如果存在）。只允许最外层、非 include 链内的 Parse
	// 处理一次：include / local manifest 的递归 Parse 不得重复扫描。开关关闭
	// 时（-m/diff-manifests 场景）完全跳过，避免混入当前检出的本地覆盖。
	if p.useLocalManifests && p.parseDepth == 1 && p.includeDepth == 0 {
		if err := p.processLocalManifests(&manifest, nil); err != nil {
			// 上游对 local manifest 解析错误是致命的（不会静默跳过），
			// 即使 silent 模式也必须返回错误。
			return nil, &ManifestError{Op: "process_local_manifests", Err: err}
		}
	}

	// 对全部项目（含 include/local_manifests 引入的）统一补全缺省值。
	// 对齐上游 _ParseProject 的继承链：path/remote/revision/dest-branch/
	// upstream 缺省时回退到 default 元素；revision 链全部为空时报错。
	// 只有最外层 Parse 强制 revision（include/local 片段可能依赖外层 default）。
	if err := applyProjectDefaults(&manifest, p.includeDepth == 0); err != nil {
		return nil, err
	}

	// 按有效 path（缺省取 name，含展平后的子项目）校验唯一性：重复 path
	// 视为清单错误（上游同样以 duplicate path 失败），不得静默去重。
	if err := validateUniqueProjectPaths(&manifest); err != nil {
		return nil, err
	}

	// 根据groups过滤项目
	if len(groups) > 0 && !MatchesAllGroups(groups) {
		return p.filterProjectsByGroups(&manifest, groups)
	}

	return &manifest, nil
}

// initProjectMaps 递归初始化项目及其嵌套子项目的 CustomAttrs/copyfile/linkfile
// CustomAttrs，保证后续属性填充不会写 nil map。
func initProjectMaps(p *Project) {
	if p.CustomAttrs == nil {
		p.CustomAttrs = make(map[string]string)
	}
	for j := range p.Copyfiles {
		if p.Copyfiles[j].CustomAttrs == nil {
			p.Copyfiles[j].CustomAttrs = make(map[string]string)
		}
	}
	for j := range p.Linkfiles {
		if p.Linkfiles[j].CustomAttrs == nil {
			p.Linkfiles[j].CustomAttrs = make(map[string]string)
		}
	}
	for j := range p.Projects {
		initProjectMaps(&p.Projects[j])
	}
}

// applyProjectDefaults 对全部项目统一补全 default 元素规定的缺省值，并构建
// 远程 URL 缓存属性。对齐上游 _ParseProject：
//   - path 缺省取 name；
//   - remote/dest-branch/upstream 缺省继承 default 元素；
//   - revision 按 priority 解析：project.revision → 该项目 remote.revision →
//     default.revision；requireRevision 为 true 时三者皆空报错
//     “no revision for project”（include/local 片段可延后由外层补全）。
func applyProjectDefaults(m *Manifest, requireRevision bool) error {
	for i := range m.Projects {
		proj := &m.Projects[i]

		// path/name 归一化：尾部 "/" 不参与语义（foo 与 foo/ 视为同一 path）。
		// 必须在 path 缺省、去重与 URL 构建之前完成。
		proj.Name = normalizeManifestPath(proj.Name)
		proj.Path = normalizeManifestPath(proj.Path)

		// 如果项目没有指定路径，则使用项目名称作为默认路径
		if proj.Path == "" {
			proj.Path = proj.Name
		}
		// 路径合法性校验（Parse 已对显式 name/path 校验；这里覆盖缺省 path
		// 以及 local 合并/extend-project 产生的有效路径，阻断 ../ 逃逸）。
		if msg := checkLocalPath(proj.Path, true, true); msg != "" {
			return &ManifestError{
				Op:  "parse",
				Err: fmt.Errorf("project %q invalid path %q: %s", proj.Name, proj.Path, msg),
			}
		}
		// 如果项目没有指定远程仓库，则使用默认远程仓库
		if proj.Remote == "" {
			proj.Remote = m.Default.Remote
		}

		// 验证远程仓库是否存在（Name 优先，其次 Alias，与 GetRemote* 一致）。
		remoteObj := m.findRemote(proj.Remote)
		if remoteObj == nil && requireRevision && proj.Remote != "" {
			// 显式指定（或经 default 继承）了非空 remote 却找不到定义：
			// 顶层解析必须报错，避免静默落成空 FetchBase 或错误相对 URL。
			// include/local 片段此时可能引用外层 remote，延后到外层统一校验。
			return &ManifestError{
				Op:  "parse",
				Err: fmt.Errorf("remote %s not defined", proj.Remote),
			}
		}

		// revision 继承链（上游 _ParseProject）：
		// project.revision → remote.revision → default.revision。
		if proj.Revision == "" && remoteObj != nil {
			proj.Revision = remoteObj.Revision
		}
		if proj.Revision == "" {
			proj.Revision = m.Default.Revision
		}
		if proj.Revision == "" && requireRevision {
			return &ManifestError{
				Op:  "parse",
				Err: fmt.Errorf("no revision for project %s", proj.Name),
			}
		}

		// sync-tags 缺省继承 default（上游 _ParseProject：XmlBool(node,
		// "sync-tags", self._default.sync_tags)）；default 亦未声明时由消费方
		// 按上游缺省 true 处理
		if !proj.SyncTags.Set {
			proj.SyncTags = m.Default.SyncTags
		}
		// sync-c/sync-s 缺省继承 default（上游 _ParseProject 的 XmlBool 默认值）
		if !proj.SyncC.Set {
			proj.SyncC = m.Default.SyncC
		}
		if !proj.SyncS.Set {
			proj.SyncS = m.Default.SyncS
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

		if remoteObj == nil {
			// 空 remote 且无 default：保留旧行为（不中断），由上层使用方决定。
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
	return nil
}

// validateProjectTree 递归校验项目 name 与显式 path（上游 _CheckLocalPath 等价）。
// name 始终必填；path 仅在显式声明时校验（缺省取 name 已在别处补全且 name 已校验）。
func validateProjectTree(projects []Project) error {
	for i := range projects {
		proj := &projects[i]
		if msg := checkLocalPath(proj.Name, true, false); msg != "" {
			return &ManifestError{
				Op:  "parse",
				Err: fmt.Errorf("project %q invalid name %q: %s", proj.Name, proj.Name, msg),
			}
		}
		if proj.Path != "" {
			if msg := checkLocalPath(proj.Path, true, true); msg != "" {
				return &ManifestError{
					Op:  "parse",
					Err: fmt.Errorf("project %q invalid path %q: %s", proj.Name, proj.Path, msg),
				}
			}
		}
		if err := validateProjectTree(proj.Projects); err != nil {
			return err
		}
	}
	return nil
}

// badLocalPathCodepoints 是文件系统规范化时可能被忽略的 Unicode 码点集合
// （对齐上游 _CheckLocalPath，来源 jgit ObjectChecker）。
var badLocalPathCodepoints = map[rune]bool{
	'\u200c': true, '\u200d': true, '\u200e': true, '\u200f': true,
	'\u202a': true, '\u202b': true, '\u202c': true, '\u202d': true,
	'\u202e': true, '\u206a': true, '\u206b': true, '\u206c': true,
	'\u206d': true, '\u206e': true, '\u206f': true, '\ufeff': true,
}

// checkLocalPath 校验路径组件是否可安全用于文件系统（上游 _CheckLocalPath 等价）：
//   - 不得为空、不得包含 "~"、不得包含可能被文件系统规范化的 Unicode 码点或换行；
//   - 组件（大小写不敏感）不得为 "."、"..",".git"，不得以 ".repo" 开头；
//   - dirOK 为 false 时不得以路径分隔符结尾；
//   - 归一化后不得为绝对路径或逃逸到上层目录（".." 开头）。
//
// cwdDotOK 为 true 时允许路径恰为 "."（上游 project path 的约定）。
// 返回空字符串表示合法，否则返回与上游一致的错误描述。
func checkLocalPath(p string, dirOK, cwdDotOK bool) string {
	if p == "" {
		return "empty paths not allowed"
	}
	if strings.ContainsRune(p, '~') {
		return "~ not allowed (due to 8.3 filenames on Windows filesystems)"
	}
	for _, r := range p {
		if badLocalPathCodepoints[r] {
			return "Unicode combining characters not allowed"
		}
	}
	if strings.ContainsAny(p, "\r\n") {
		return "Newlines not allowed"
	}

	lower := strings.ToLower(p)
	trailingSep := strings.HasSuffix(lower, "/") || strings.HasSuffix(lower, string(filepath.Separator))
	trimmed := strings.TrimRight(lower, "/"+string(filepath.Separator))

	parts := strings.FieldsFunc(trimmed, func(r rune) bool {
		return r == '/' || r == rune(filepath.Separator)
	})
	// 注意：FieldsFunc 会丢掉空组件；绝对路径通过 IsAbs/前缀判断另行拦截。
	dotAllowed := cwdDotOK && len(parts) == 1 && parts[0] == "."
	if !dotAllowed {
		for _, part := range parts {
			if part == "." || part == ".." || part == ".git" || strings.HasPrefix(part, ".repo") {
				return fmt.Sprintf("bad component: %s", part)
			}
		}
	}
	if !dirOK && trailingSep {
		return "dirs not allowed"
	}

	norm := filepath.Clean(lower)
	// Windows 根路径 "\foo"（以及 UNC "\\server\share"）在类 Unix 主机上
	// filepath.IsAbs 不成立，需显式拒绝，避免跨平台校验结果不一致。
	if strings.HasPrefix(lower, `\`) || strings.HasPrefix(norm, `\`) {
		return "path cannot be outside"
	}
	if norm == ".." || strings.HasPrefix(norm, ".."+string(filepath.Separator)) ||
		filepath.IsAbs(norm) || strings.HasPrefix(norm, "/") {
		return "path cannot be outside"
	}
	return ""
}

// normalizeManifestPath 去掉 path/name 末尾的 "/"（可重复）。清单语义中
// "foo" 与 "foo/" 是同一路径；归一化保证去重、duplicate path 校验、ToXML
// 输出与 diff 键（以 path 为键）一致。
func normalizeManifestPath(s string) string {
	if s == "" {
		return ""
	}
	trimmed := strings.TrimRight(s, "/")
	if trimmed == "" {
		// 全部由 "/" 构成（非法路径）：保持原值，交由路径校验报错。
		return s
	}
	return trimmed
}

type manifestXMLNode struct {
	XMLName xml.Name          `xml:""`
	Attrs   []xml.Attr        `xml:",any,attr"`
	Nodes   []manifestXMLNode `xml:",any"`
}

// xmlAttrValue 返回节点指定属性的值（不存在时为空）。
func xmlAttrValue(node manifestXMLNode, name string) string {
	for _, attr := range node.Attrs {
		if attr.Name.Local == name {
			return attr.Value
		}
	}
	return ""
}

// copyUnknownAttrs 将节点中非已知属性写入 dst。
func copyUnknownAttrs(node manifestXMLNode, dst map[string]string, known func(string) bool) {
	for _, attr := range node.Attrs {
		if !known(attr.Name.Local) {
			dst[attr.Name.Local] = attr.Value
		}
	}
}

// projectMatchesXMLNode 按 name（必选）+ path（可选）匹配项目：
//   - 节点显式指定 path：name 与有效 path 都必须匹配；
//   - 节点未指定 path（上游自定义属性常见写法）：仅匹配“有效 path == name”
//     的项目，避免对同名不同 path 的项目串写属性。
func projectMatchesXMLNode(p *Project, name, projectPath string) bool {
	if p.Name != name {
		return false
	}
	eff := p.Path
	if eff == "" {
		eff = p.Name
	}
	if projectPath == "" {
		return eff == name
	}
	return eff == projectPath
}

// applyProjectNodeAttrs 把 XML project 节点的自定义属性/copyfile/linkfile 属性
// 写入对应项目，并递归处理嵌套子项目（name+path 匹配）。
func applyProjectNodeAttrs(p *Project, node manifestXMLNode) {
	if p.CustomAttrs == nil {
		p.CustomAttrs = make(map[string]string)
	}
	copyUnknownAttrs(node, p.CustomAttrs, isKnownProjectAttr)
	for _, subNode := range node.Nodes {
		switch subNode.XMLName.Local {
		case "copyfile":
			src := xmlAttrValue(subNode, "src")
			dest := xmlAttrValue(subNode, "dest")
			for j := range p.Copyfiles {
				if p.Copyfiles[j].Src == src && p.Copyfiles[j].Dest == dest {
					if p.Copyfiles[j].CustomAttrs == nil {
						p.Copyfiles[j].CustomAttrs = make(map[string]string)
					}
					copyUnknownAttrs(subNode, p.Copyfiles[j].CustomAttrs, isKnownCopyfileAttr)
					break
				}
			}
		case "linkfile":
			src := xmlAttrValue(subNode, "src")
			dest := xmlAttrValue(subNode, "dest")
			for j := range p.Linkfiles {
				if p.Linkfiles[j].Src == src && p.Linkfiles[j].Dest == dest {
					if p.Linkfiles[j].CustomAttrs == nil {
						p.Linkfiles[j].CustomAttrs = make(map[string]string)
					}
					copyUnknownAttrs(subNode, p.Linkfiles[j].CustomAttrs, isKnownLinkfileAttr)
					break
				}
			}
		case "project":
			name := xmlAttrValue(subNode, "name")
			childPath := xmlAttrValue(subNode, "path")
			for j := range p.Projects {
				if projectMatchesXMLNode(&p.Projects[j], name, childPath) {
					applyProjectNodeAttrs(&p.Projects[j], subNode)
				}
			}
		}
	}
}

// parseCustomAttributes 解析XML中的自定义属性。project/remove-project 按
// name+path 匹配（同名不同 path 的项目不再互相串写属性）。
func parseCustomAttributes(data []byte, manifest *Manifest) error {
	var root manifestXMLNode
	if err := xml.Unmarshal(data, &root); err != nil {
		return fmt.Errorf("解析XML失败: %w", err)
	}

	for _, attr := range root.Attrs {
		if isStandardManifestAttr(attr.Name.Local) {
			continue
		}
		manifest.CustomAttrs[attr.Name.Local] = attr.Value
	}

	for _, node := range root.Nodes {
		switch node.XMLName.Local {
		case "remote":
			name := xmlAttrValue(node, "name")
			for i := range manifest.Remotes {
				if manifest.Remotes[i].Name == name {
					copyUnknownAttrs(node, manifest.Remotes[i].CustomAttrs, isKnownRemoteAttr)
					break
				}
			}
		case "default":
			copyUnknownAttrs(node, manifest.Default.CustomAttrs, isKnownDefaultAttr)
		case "project":
			name := xmlAttrValue(node, "name")
			projectPath := xmlAttrValue(node, "path")
			for i := range manifest.Projects {
				if projectMatchesXMLNode(&manifest.Projects[i], name, projectPath) {
					applyProjectNodeAttrs(&manifest.Projects[i], node)
				}
			}
		case "include":
			name := xmlAttrValue(node, "name")
			for i := range manifest.Includes {
				if manifest.Includes[i].Name == name {
					copyUnknownAttrs(node, manifest.Includes[i].CustomAttrs, isKnownIncludeAttr)
					break
				}
			}
		case "remove-project":
			name := xmlAttrValue(node, "name")
			removePath := xmlAttrValue(node, "path")
			for i := range manifest.RemoveProjects {
				if manifest.RemoveProjects[i].Name == name &&
					(removePath == "" || manifest.RemoveProjects[i].Path == removePath) {
					copyUnknownAttrs(node, manifest.RemoveProjects[i].CustomAttrs, isKnownRemoveProjectAttr)
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

// applyIncludeInheritance 把当前 include 链继承的 groups/revision 应用到本清单：
//   - revision 补到未声明 revision 的顶层 project 与嵌套 include（上游
//     _ParseManifestXml 对 include/project 节点 setAttribute 的行为）；
//   - groups 与顶层 project/extend-project 的已有组取并集（上游 parent_groups
//     对 project/extend-project 节点的处理）。嵌套子项目不继承，与上游一致。
func (p *Parser) applyIncludeInheritance(m *Manifest) {
	if p.includeRevision != "" {
		for i := range m.Projects {
			if m.Projects[i].Revision == "" {
				m.Projects[i].Revision = p.includeRevision
			}
		}
		for i := range m.Includes {
			if m.Includes[i].Revision == "" {
				m.Includes[i].Revision = p.includeRevision
			}
		}
	}
	if len(p.includeGroups) == 0 {
		return
	}
	groups := strings.Join(p.includeGroups, ",")
	for i := range m.Projects {
		m.Projects[i].Groups = unionGroupStrings(m.Projects[i].Groups, groups)
	}
	for i := range m.ExtendProjects {
		m.ExtendProjects[i].Groups = unionGroupStrings(m.ExtendProjects[i].Groups, groups)
	}
}

// processIncludes 处理包含的清单文件。上游将 include 的内容按文档顺序并入
// 同一节点流（子清单的 default/remove/extend 对全局生效）；本实现的递归
// 聚合模型通过以下机制对齐：
//   - 子清单项目在递归 Parse 中应用其自身 default，缺省值再由外层
//     applyProjectDefaults 补全；
//   - 子清单按文档顺序解析后记录操作日志（project/remove/extend），由
//     includeSeg 携带，父层在 include 位置逐元素重放，保留 remove 后重新
//     定义等顺序语义；
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
		// 被解析的文件位于 include 链内：其中悬空的 remove-project 不视为错误。
		// include 的 groups/revision 通过 Parser 状态下传：groups 与祖先 include
		// 的组取并集后作用于被包含清单的 project/extend-project；revision 补到
		// 未声明 revision 的项目与嵌套 include（对齐上游 _ParseManifestXml）。
		prevRevision := p.includeRevision
		prevGroups := p.includeGroups
		p.includeRevision = include.Revision
		p.includeGroups = unionGroupLists(prevGroups, splitTrimGroups(include.Groups))
		p.includeDepth++
		includedManifest, err := p.Parse(data, nil)
		p.includeDepth--
		p.includeRevision = prevRevision
		p.includeGroups = prevGroups
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

		// 记录本 include 引入的项目区间；同时携带子清单的文档顺序操作
		// 日志，父层据此在 include 位置逐元素重放（保留 remove/重新定义
		// 等顺序语义）。removes/extends 仅作为无操作日志时的兜底。
		seg := includeSeg{
			start:   len(manifest.Projects),
			removes: includedManifest.RemoveProjects,
			extends: includedManifest.ExtendProjects,
			ops:     includedManifest.replayOps,
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
	kind string // "project" / "include" / "remove-project" / "extend-project"
	idx  int    // 在对应 Unmarshal 结果切片中的序号
}

// includeSeg 记录单个 <include> 引入的项目在 Projects 切片中的区间，
// 以及子清单按文档顺序解析后的操作日志（project/remove/extend），供父层
// 在 include 位置逐元素重放。removes/extends 为无操作日志时的兜底。
type includeSeg struct {
	start, end int
	removes    []RemoveProject
	extends    []ExtendProject
	ops        []replayOp
}

// scanTopLevelElements 扫描 manifest 根元素的直接子元素，按文档顺序记录
// project/include/remove-project 的出现次序。XML Unmarshal 后各类元素分别进入
// Projects/Includes/RemoveProjects 切片，相互之间的文档顺序丢失；而
// remove-project 的上游语义（只移除此前已定义的项目）依赖该顺序，故需重扫。
func scanTopLevelElements(data []byte) []topElem {
	seq := make([]topElem, 0)
	projIdx, incIdx, rmIdx, extIdx := 0, 0, 0, 0
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
				case "extend-project":
					seq = append(seq, topElem{kind: "extend-project", idx: extIdx})
					extIdx++
				}
			}
		case xml.EndElement:
			depth--
		}
	}
	return seq
}

// replayOp 描述清单文档顺序中的一个顶层操作：project 携带展平后的项目，
// remove/extend 保留原条目。父清单据此在 include 位置逐元素重放。
type replayOp struct {
	kind     string // "project" / "remove" / "extend"
	projects []Project
	remove   RemoveProject
	extend   ExtendProject
}

// replayDocumentOrder 按文档顺序重放顶层元素：project/include/
// remove-project/extend-project 依序执行。include 段通过子清单记录的操作
// 日志逐元素重放，因此“remove a → 重新定义 a”不会被整段追加后的 remove
// 误删。返回本层文档顺序的操作日志，供父清单在 include 位置再次重放。
func (p *Parser) replayDocumentOrder(m *Manifest, seq []topElem, segs []includeSeg) ([]replayOp, error) {
	flat := make([]Project, 0, len(m.Projects))
	ops := make([]replayOp, 0, len(seq))
	projCursor := 0 // 主清单顶层项目游标
	incIdx := 0     // include 段游标

	// inInclude: 当前清单由 include 链引入。此时 remove/extend 仍按位置在
	// 子层项目集合上求值（兼容 Merger 与悬空条目），但 base-rev 校验推迟：
	// 只有父层在 include 位置重放时，父 remote/default 才是正确上下文。
	inInclude := p.includeDepth > 0

	// seenNames 记录本层曾经定义过的项目名：include 传播的未命中 extend
	// 若目标曾被定义（但随后被 remove 删除），应丢弃而非误报不存在。
	seenNames := make(map[string]bool)
	dangling := make([]RemoveProject, 0)
	pendingExtends := make([]ExtendProject, 0)
	unmatchedExtends := make([]ExtendProject, 0)
	// positionalUnresolved 记录 include 传播、在当前位置未命中的 extend 的
	// op 下标。它们不得延后应用到之后才定义的项目，文档末尾再决定丢弃/报错。
	positionalUnresolved := make([]int, 0)

	nameInFlat := func(name string) bool {
		name = normalizeManifestPath(name)
		for i := range flat {
			if normalizeManifestPath(flat[i].Name) == name {
				return true
			}
		}
		return false
	}

	addProject := func(proj Project) {
		withChildren := appendProjectWithChildren(nil, proj)
		for i := range withChildren {
			seenNames[normalizeManifestPath(withChildren[i].Name)] = true
		}
		flat = append(flat, withChildren...)
		ops = append(ops, replayOp{kind: "project", projects: withChildren})
	}

	execRemove := func(rp RemoveProject) error {
		// include 子层仍在自身项目集合上执行 remove（供 Merger/悬空条目
		// 兼容），但 base-rev 校验延后到父层重放位置。
		matched, err := removeMatches(m, &flat, rp, !inInclude)
		if err != nil {
			return err
		}
		if !matched {
			// 无匹配：悬空条目（optional 或交由外层/合并器处理）
			dangling = append(dangling, rp)
		}
		// 始终记录 remove：父层按 include 位置重放时，才能保留对更早定义
		// 项目的删除效果（include 等价于文档内联展开）。
		ops = append(ops, replayOp{kind: "remove", remove: rp})
		return nil
	}

	execExtend := func(ep ExtendProject, fromInclude bool) error {
		// include 子层在自身项目集合上按位置求值（不延后到文档末尾），且
		// base-rev 校验延后到父层。ops 中的 project 快照在 extend 之前捕获，
		// 因此 extend 不会被“烧进”快照；父层按 include 位置重放即可复现。
		matched, nameExists, err := applyExtendProjectToSlice(m, &flat, ep, !inInclude)
		if err != nil {
			return err
		}
		if matched {
			ops = append(ops, replayOp{kind: "extend", extend: ep})
			return nil
		}
		if inInclude || fromInclude {
			// 位置语义：未命中不得延后应用到之后定义的项目。记录 op 供父层
			// 在 include 位置重放；path 无命中但同名项目可见时也一样，因为
			// 父层可能存在 path 匹配的项目。
			ep.fromInclude = true
			ops = append(ops, replayOp{kind: "extend", extend: ep})
			if inInclude {
				unmatchedExtends = append(unmatchedExtends, ep)
			} else {
				positionalUnresolved = append(positionalUnresolved, len(ops)-1)
			}
			return nil
		}
		if nameExists {
			// name 存在但 path 无命中：no-op，不报告“项目不存在”。
			return nil
		}
		// 未命中：目标可能在更后的文档位置定义、已被后续 remove 删除，
		// 或位于外层清单。延后到文档末尾统一判定。
		pendingExtends = append(pendingExtends, ep)
		return nil
	}

	replayInclude := func(seg includeSeg) error {
		if seg.ops != nil {
			for _, op := range seg.ops {
				switch op.kind {
				case "project":
					for _, proj := range op.projects {
						addProject(proj)
					}
				case "remove":
					if err := execRemove(op.remove); err != nil {
						return err
					}
				case "extend":
					ep := op.extend
					ep.fromInclude = true
					if err := execExtend(ep, true); err != nil {
						return err
					}
				}
			}
			return nil
		}
		// 兜底：旧格式 includeSeg（无操作日志）按区间 + 悬空条目应用。
		if seg.start >= 0 && seg.start <= seg.end && seg.end <= len(m.Projects) {
			for _, proj := range m.Projects[seg.start:seg.end] {
				addProject(proj)
			}
		}
		for _, rp := range seg.removes {
			if err := execRemove(rp); err != nil {
				return err
			}
		}
		for _, ep := range seg.extends {
			ep.fromInclude = true
			if err := execExtend(ep, true); err != nil {
				return err
			}
		}
		return nil
	}

	for _, e := range seq {
		switch e.kind {
		case "project":
			if projCursor < len(m.Projects) {
				addProject(m.Projects[projCursor])
				projCursor++
			}
		case "include":
			if incIdx < len(segs) {
				if err := replayInclude(segs[incIdx]); err != nil {
					return nil, err
				}
				incIdx++
			}
		case "remove-project":
			if e.idx < len(m.RemoveProjects) {
				if err := execRemove(m.RemoveProjects[e.idx]); err != nil {
					return nil, err
				}
			}
		case "extend-project":
			if e.idx < len(m.ExtendProjects) {
				if err := execExtend(m.ExtendProjects[e.idx], false); err != nil {
					return nil, err
				}
			}
		}
	}
	// 兜底：seq 未覆盖到的剩余“直接项目”全部激活（正常流程应已被消费）。
	// 上界必须排除 include 段引入的项目：m.Projects 由“主清单直接项目”加
	// “include 段项目”拼接而成，若用 len(m.Projects) 作上界会把 include
	// 项目再次追加，使已被 remove-project 删除的项目复活。
	directProjectCount := len(m.Projects)
	for _, seg := range segs {
		directProjectCount -= seg.end - seg.start
	}
	if directProjectCount < 0 {
		directProjectCount = 0
	}
	for ; projCursor < directProjectCount; projCursor++ {
		addProject(m.Projects[projCursor])
	}
	for ; incIdx < len(segs); incIdx++ {
		if err := replayInclude(segs[incIdx]); err != nil {
			return nil, err
		}
	}

	// 文档末尾统一处理直接定义（非 include 传播）的未命中 extend：
	//   - 目标出现在本层最终项目集合：应用；
	//   - name 存在但 path 不匹配：no-op；
	//   - 顶层清单中从未出现过的 extend：报错（上游 fail-fast）。
	for _, ep := range pendingExtends {
		matched, nameExists, err := applyExtendProjectToSlice(m, &flat, ep, true)
		if err != nil {
			return nil, err
		}
		if matched {
			ops = append(ops, replayOp{kind: "extend", extend: ep})
			continue
		}
		if nameExists {
			continue
		}
		return nil, &ManifestError{
			Op:  "parse",
			Err: fmt.Errorf("extend-project element specifies non-existent project: %s", ep.Name),
		}
	}

	// include 传播的未命中 extend 只按位置生效：文档末尾不再尝试应用，
	// 否则会错误地作用到 include 位置之后才定义的项目。
	//   - 目标最终存在或曾经出现（含已被 remove）：丢弃该 op；
	//   - 目标从未出现：顶层报 “不存在”（上游 fail-fast）。
	if len(positionalUnresolved) > 0 {
		drop := make(map[int]bool, len(positionalUnresolved))
		for _, opIdx := range positionalUnresolved {
			ep := ops[opIdx].extend
			if nameInFlat(ep.Name) || seenNames[normalizeManifestPath(ep.Name)] {
				drop[opIdx] = true
				continue
			}
			return nil, &ManifestError{
				Op:  "parse",
				Err: fmt.Errorf("extend-project element specifies non-existent project: %s", ep.Name),
			}
		}
		if len(drop) > 0 {
			kept := make([]replayOp, 0, len(ops)-len(drop))
			for i := range ops {
				if drop[i] {
					continue
				}
				kept = append(kept, ops[i])
			}
			ops = kept
		}
	}

	// 顶层解析时，悬空的非 optional remove 视为错误（上游 fail-fast：
	// remove-project 引用了不存在的项目）。校验后清空残留条目：写盘顺序
	// 变成“项目在前、remove 在后”，保留它们会让二次解析重新生效或报错。
	// include 子层保留悬空条目，由父层在 include 位置执行。
	if p.includeDepth == 0 {
		for _, rp := range dangling {
			if !rp.Optional.Get(false) {
				return nil, fmt.Errorf("remove-project element specifies non-existent project: name=%q path=%q", rp.Name, rp.Path)
			}
		}
		m.RemoveProjects = nil
	} else {
		m.RemoveProjects = dangling
	}
	m.Projects = flat
	m.ExtendProjects = unmatchedExtends
	return ops, nil
}

// removeMatches 从已定义项目列表中移除匹配 remove-project 的项目，返回是否
// 发生了移除。匹配规则对齐上游：
//   - name 与 path 均未指定视为非法（上游直接报错），此处返回 false；
//   - name 指定而 path 未指定：移除所有同名项目；
//   - path 指定：移除 path 匹配且（name 匹配或 name 未指定）的项目；
//   - base-rev 指定时，命中项目的有效 revision（project → remote → default）
//     必须与 base-rev 一致，否则返回 base-revision mismatch 错误且不修改列表；
//     validateBaseRev=false 用于 include 子层（外层 remote/default 尚不可知），
//     校验推迟到父层在 include 位置重放时执行。
func removeMatches(manifest *Manifest, projects *[]Project, rp RemoveProject, validateBaseRev bool) (bool, error) {
	if rp.Name == "" && rp.Path == "" {
		return false, nil
	}
	rpName := normalizeManifestPath(rp.Name)
	rpPath := normalizeManifestPath(rp.Path)
	kept := make([]Project, 0, len(*projects))
	removed := false
	for _, proj := range *projects {
		projName := normalizeManifestPath(proj.Name)
		projectPath := normalizeManifestPath(proj.Path)
		if projectPath == "" {
			projectPath = projName
		}
		match := false
		if rpPath == "" {
			// 仅 name：同名全部移除
			match = projName == rpName
		} else {
			// path 匹配且（name 匹配或 name 未指定）。
			// 项目 Path 为空时按 Name 视为隐式 path（对齐上游 path 缺省取 name）
			match = projectPath == rpPath && (rpName == "" || projName == rpName)
		}
		if !match {
			kept = append(kept, proj)
			continue
		}
		if rp.BaseRev != "" && validateBaseRev {
			current := effectiveProjectRevision(manifest, &proj)
			if current != rp.BaseRev {
				return false, &ManifestError{
					Op: "parse",
					Err: fmt.Errorf("detected base-revision mismatch, updates needed:\nremove-project name %s:\n  base %s vs revision %s",
						rp.Name, rp.BaseRev, current),
				}
			}
		}
		removed = true
	}
	if !removed {
		return false, nil
	}
	*projects = kept
	return true, nil
}

// removeProjectMatchesProjects 判断 remove-project 是否会命中给定的任一项目。
// 用于 ToXML 写盘前过滤“会重新生效”的残留 remove 条目。
func removeProjectMatchesProjects(projects []Project, rp RemoveProject) bool {
	for _, proj := range projects {
		if removeProjectMatchesProject(rp, proj) {
			return true
		}
	}
	return false
}

// appendProjectWithChildren 将项目连同其嵌套子项目追加到列表，子项目按上游规则
// 改写 name/path：
//   - 子项目 name 为 "父name/子name"（上游 _JoinName）；
//   - 子项目路径 = 父项目路径 + "/" + path，其中 path 缺省时取“已拼接的
//     name”（上游 _ParseProject: path = name；GetSubprojectPaths:
//     relpath = _JoinRelpath(parent.relpath, path)）。因此深层缺省路径形如
//     parent/parent/child/…，这是上游的实际行为，不是重复拼接 bug；
//     显式 path 则相对父路径展开（上游 _JoinRelpath）。
//   - 其余属性（remote/revision 等）留待 applyProjectDefaults 统一补全。
func appendProjectWithChildren(dst []Project, proj Project) []Project {
	children := proj.Projects
	proj.Projects = nil // 展平后不再保留子树
	// name/path 尾部 "/" 归一化：让 foo 与 foo/ 在展平、去重与重放中一致。
	proj.Name = normalizeManifestPath(proj.Name)
	proj.Path = normalizeManifestPath(proj.Path)
	// 父项目 path 缺省即 name（与 applyProjectDefaults 一致）
	if proj.Path == "" {
		proj.Path = proj.Name
	}
	parentName := proj.Name
	parentPath := proj.Path
	dst = append(dst, proj)

	for _, child := range children {
		originalName := child.Name
		// 子项目 name = 父name/子name（上游 _JoinName）
		child.Name = path.Join(parentName, originalName)
		// 缺省 path 取拼接后的 name（上游 _ParseProject 的 path 缺省逻辑），
		// 再由父路径拼接；显式 path 直接相对父路径展开。
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

// applyExtendProjectToSlice 将单个 extend-project 应用到给定项目切片（就地
// 修改），返回是否命中（matched）以及是否存在同名项目（nameExists）。
// 匹配规则对齐上游：
//   - name 必选；path 仅作为匹配条件（不改变项目路径），dest-path 才改路径；
//   - 未指定 path 却指定 dest-path 且同名项目多于一个时报错；
//   - groups 与项目现有组取并集（不替换）；
//   - revision 变更时 base-rev 必须与当前有效 revision 一致，否则报错。
//
// nameExists 用于区分“项目完全不存在”（需要报错/向外传播）与“name 存在但
// path 无命中”（no-op），后者不得误报“项目不存在”。
//
// validateBaseRev=false 用于 include 子层：此时外层 remote/default 尚未合并，
// base-rev 校验推迟到父层在 include 位置重放时执行，避免合法清单误报。
func applyExtendProjectToSlice(manifest *Manifest, projects *[]Project, extProj ExtendProject, validateBaseRev bool) (matched, nameExists bool, err error) {
	extName := normalizeManifestPath(extProj.Name)
	extPath := normalizeManifestPath(extProj.Path)
	for i := range *projects {
		if normalizeManifestPath((*projects)[i].Name) == extName {
			nameExists = true
			break
		}
	}
	if extProj.DestPath != "" && extProj.Path == "" {
		sameName := 0
		for i := range *projects {
			if normalizeManifestPath((*projects)[i].Name) == extName {
				sameName++
			}
		}
		if sameName > 1 {
			return false, nameExists, &ManifestError{
				Op:  "parse",
				Err: fmt.Errorf("extend-project cannot use dest-path when matching multiple projects: %s", extProj.Name),
			}
		}
	}

	for i := range *projects {
		proj := &(*projects)[i]
		if normalizeManifestPath(proj.Name) != extName {
			continue
		}
		if extPath != "" {
			projectPath := normalizeManifestPath(proj.Path)
			if projectPath == "" {
				projectPath = normalizeManifestPath(proj.Name)
			}
			if projectPath != extPath {
				continue
			}
		}

		if extProj.Revision != "" {
			if extProj.BaseRev != "" && validateBaseRev {
				current := effectiveProjectRevision(manifest, proj)
				if current != extProj.BaseRev {
					return false, nameExists, &ManifestError{
						Op: "parse",
						Err: fmt.Errorf("extend-project name %s: base %s vs revision %s",
							extProj.Name, extProj.BaseRev, current),
					}
				}
			}
			proj.Revision = extProj.Revision
		}
		if extProj.Remote != "" {
			proj.Remote = extProj.Remote
		}
		if extProj.DestBranch != "" {
			proj.DestBranch = extProj.DestBranch
		}
		if extProj.Upstream != "" {
			proj.Upstream = extProj.Upstream
		}
		// dest-path 修改项目路径（path 属性仅用于匹配）
		if extProj.DestPath != "" {
			proj.Path = normalizeManifestPath(extProj.DestPath)
		}
		// groups 取并集
		if extProj.Groups != "" {
			proj.Groups = unionGroupStrings(proj.Groups, extProj.Groups)
		}
		// 追加copyfiles
		if len(extProj.Copyfiles) > 0 {
			proj.Copyfiles = append(proj.Copyfiles, extProj.Copyfiles...)
		}
		// 追加linkfiles
		if len(extProj.Linkfiles) > 0 {
			proj.Linkfiles = append(proj.Linkfiles, extProj.Linkfiles...)
		}
		matched = true
	}
	return matched, nameExists, nil
}

// effectiveProjectRevision 返回项目当前有效 revision（project → remote →
// default），用于 extend-project base-rev 校验。applyProjectDefaults 之前也可用。
func effectiveProjectRevision(manifest *Manifest, proj *Project) string {
	if proj.Revision != "" {
		return proj.Revision
	}
	remoteName := proj.Remote
	if remoteName == "" {
		remoteName = manifest.Default.Remote
	}
	if remote := manifest.findRemote(remoteName); remote != nil && remote.Revision != "" {
		return remote.Revision
	}
	return manifest.Default.Revision
}

// processLocalManifests 处理.repo/local_manifests目录下的所有manifest文件。
// 仅允许在最外层 Parse（parseDepth==1）执行；递归解析 local manifest 时
// 不得再次扫描该目录，否则会无限递归。
func (p *Parser) processLocalManifests(manifest *Manifest, groups []string) error {
	if p.parseDepth > 1 {
		return nil
	}
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
			return fmt.Errorf("failed to read local manifest %s: %w", xmlFile, err)
		}

		// 解析local manifest：以 fragment 模式保留顶层元素文档顺序与原始
		// 切片下标（remove/extend 不在此应用），再由 mergeLocalManifest
		// 按 local XML 顺序重放到主清单。includeDepth++ 表示其悬空
		// remove/extend 目标可能在外层清单，不应在此报错。
		prevFragment := p.localFragment
		p.localFragment = true
		p.includeDepth++
		localManifest, err := p.Parse(data, nil)
		p.includeDepth--
		p.localFragment = prevFragment
		if err != nil {
			return fmt.Errorf("failed to parse local manifest %s: %w", xmlFile, err)
		}

		// 合并到主manifest（事务性：失败时主清单保持不变）
		if err := p.mergeLocalManifest(manifest, localManifest); err != nil {
			return fmt.Errorf("failed to merge local manifest %s: %w", xmlFile, err)
		}
	}

	return nil
}

// mergeLocalManifest 将local manifest合并到主manifest。
// 合并是事务性的：在 main 的深拷贝上完整重放，仅当全部成功才替换 main；
// 失败时返回错误且不改动 main，避免半失败留下部分覆盖。
// remove-project 使用与解析期一致的 path/optional 匹配语义：
// path 为空按 Name 视为隐式 path；非 optional 且未命中视为错误。
func (p *Parser) mergeLocalManifest(main *Manifest, local *Manifest) error {
	if main == nil || local == nil {
		return fmt.Errorf("local manifest merge requires non-nil manifests")
	}
	working := deepCopyManifest(main)
	if err := p.mergeLocalManifestInto(working, local); err != nil {
		return err
	}
	*main = *working
	return nil
}

// mergeLocalManifestInto 在给定清单上就地重放 local manifest 的全部元素。
func (p *Parser) mergeLocalManifestInto(main *Manifest, local *Manifest) error {
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

	// local manifest 项目同样需要路径校验（防御手工构造的 Manifest 绕过 Parse）
	if err := validateProjectTree(local.Projects); err != nil {
		return err
	}

	// 按 local XML 顶层元素文档顺序重放 project/remove-project/extend-project。
	// 这是关键语义：<remove-project name="foo"/><project name="foo" .../>
	// 必须先移除旧定义再重新定义，而不是“先项目、再 remove、再 extend”。
	if len(local.elementOrder) > 0 {
		return p.replayLocalElements(main, local)
	}

	// 兜底（手工构造、无文档顺序信息）：保持旧的分阶段合并语义。
	for _, proj := range local.Projects {
		applyLocalProject(main, proj)
	}
	for _, rp := range local.RemoveProjects {
		matched, err := removeMatches(main, &main.Projects, rp, true)
		if err != nil {
			return err
		}
		if !matched && !rp.Optional.Get(false) {
			return fmt.Errorf("local manifest remove-project specifies non-existent project: name=%q path=%q", rp.Name, rp.Path)
		}
	}
	for _, extProj := range local.ExtendProjects {
		matched, nameExists, err := applyExtendProjectToSlice(main, &main.Projects, extProj, true)
		if err != nil {
			return err
		}
		if !matched && !nameExists {
			return fmt.Errorf("extend-project element specifies non-existent project: %s", extProj.Name)
		}
	}
	return nil
}

// replayLocalElements 按 local manifest 的顶层元素顺序把 project/include/
// remove-project/extend-project 应用到主清单。include 段内的项目与悬空
// remove/extend 在其 include 位置执行。
func (p *Parser) replayLocalElements(main, local *Manifest) error {
	applyProject := func(proj Project) {
		applyLocalProject(main, proj)
	}
	applyRemove := func(rp RemoveProject) error {
		matched, err := removeMatches(main, &main.Projects, rp, true)
		if err != nil {
			return err
		}
		if matched {
			return nil
		}
		if !rp.Optional.Get(false) {
			return fmt.Errorf("local manifest remove-project specifies non-existent project: name=%q path=%q", rp.Name, rp.Path)
		}
		return nil
	}
	applyExtend := func(extProj ExtendProject) error {
		matched, nameExists, err := applyExtendProjectToSlice(main, &main.Projects, extProj, true)
		if err != nil {
			return err
		}
		// name 存在但 path 无命中属于 no-op，不报告“项目不存在”。
		if !matched && !nameExists {
			return fmt.Errorf("extend-project element specifies non-existent project: %s", extProj.Name)
		}
		return nil
	}

	for _, elem := range local.elementOrder {
		switch elem.kind {
		case "project":
			if elem.idx < len(local.Projects) {
				applyProject(local.Projects[elem.idx])
			}
		case "include":
			// fragment 模式下被 include 的清单保留了原始文档顺序，递归重放
			// 可正确处理 include 内部的项目/remove/extend 交错顺序。
			if elem.idx < len(local.Includes) && local.Includes[elem.idx].manifest != nil &&
				local.Includes[elem.idx].manifest.parsedAsFragment &&
				len(local.Includes[elem.idx].manifest.elementOrder) > 0 {
				if err := p.replayLocalElements(main, local.Includes[elem.idx].manifest); err != nil {
					return err
				}
				continue
			}
			// 兜底：按 include 段整体应用（非 fragment 模式解析的 local 清单）。
			if elem.idx >= len(local.includeSegs) {
				continue
			}
			seg := local.includeSegs[elem.idx]
			if seg.start >= 0 && seg.start <= seg.end && seg.end <= len(local.Projects) {
				for _, proj := range local.Projects[seg.start:seg.end] {
					applyProject(proj)
				}
			}
			for _, rp := range seg.removes {
				if err := applyRemove(rp); err != nil {
					return err
				}
			}
			for _, extProj := range seg.extends {
				if err := applyExtend(extProj); err != nil {
					return err
				}
			}
		case "remove-project":
			if elem.idx < len(local.RemoveProjects) {
				if err := applyRemove(local.RemoveProjects[elem.idx]); err != nil {
					return err
				}
			}
		case "extend-project":
			if elem.idx < len(local.ExtendProjects) {
				if err := applyExtend(local.ExtendProjects[elem.idx]); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// applyLocalProject 将 local manifest 中的项目（含嵌套子项目展平）应用到主清单：
// 同名同有效 path 的项目用 local 的非空字段覆盖，否则追加。
func applyLocalProject(main *Manifest, proj Project) {
	for _, flatProj := range appendProjectWithChildren(nil, proj) {
		idx := findProjectIndex(main.Projects, flatProj.Name, flatProj.Path)
		if idx >= 0 {
			overlayProjectValues(&main.Projects[idx], flatProj)
			continue
		}
		main.Projects = append(main.Projects, flatProj)
	}
}

// findProjectIndex 按 name + 有效 path 查找项目下标（未找到返回 -1）。
func findProjectIndex(projects []Project, name, projectPath string) int {
	key := localProjectKey(name, projectPath)
	for i := range projects {
		if localProjectKey(projects[i].Name, projects[i].Path) == key {
			return i
		}
	}
	return -1
}

// localProjectKey 生成 name + 有效 path 的唯一键（path 为空时按 name）。
// 尾部 "/" 归一化，保证 foo 与 foo/ 命中同一项目。
func localProjectKey(name, projectPath string) string {
	name = normalizeManifestPath(name)
	projectPath = normalizeManifestPath(projectPath)
	if projectPath == "" {
		projectPath = name
	}
	return name + "\x00" + projectPath
}

// overlayProjectValues 用 src 的非空/已声明字段覆盖 dst，保留 dst 中 src
// 未声明的字段。用于 local manifest 对主清单项目的定制。
func overlayProjectValues(dst *Project, src Project) {
	if src.Path != "" {
		dst.Path = src.Path
	}
	if src.Remote != "" {
		dst.Remote = src.Remote
	}
	if src.Revision != "" {
		dst.Revision = src.Revision
	}
	if src.Upstream != "" {
		dst.Upstream = src.Upstream
	}
	if src.DestBranch != "" {
		dst.DestBranch = src.DestBranch
	}
	if src.Groups != "" {
		dst.Groups = src.Groups
	}
	if src.SyncC.Set {
		dst.SyncC = src.SyncC
	}
	if src.SyncS.Set {
		dst.SyncS = src.SyncS
	}
	if src.SyncTags.Set {
		dst.SyncTags = src.SyncTags
	}
	if src.CloneDepth != 0 {
		dst.CloneDepth = src.CloneDepth
	}
	if src.ForcePath.Set {
		dst.ForcePath = src.ForcePath
	}
	if src.References != "" {
		dst.References = src.References
	}
	if dst.CustomAttrs == nil {
		dst.CustomAttrs = make(map[string]string)
	}
	for k, v := range src.CustomAttrs {
		dst.CustomAttrs[k] = v
	}
	dst.Copyfiles = appendProjectCopyfiles(dst.Copyfiles, src.Copyfiles)
	dst.Linkfiles = appendProjectLinkfiles(dst.Linkfiles, src.Linkfiles)
	if len(src.Annotations) > 0 {
		dst.Annotations = append(dst.Annotations, src.Annotations...)
	}
}

// appendProjectCopyfiles 追加 copyfile（按 src+dest 去重）。
func appendProjectCopyfiles(dst, src []Copyfile) []Copyfile {
	for _, c := range src {
		exists := false
		for _, existing := range dst {
			if existing.Src == c.Src && existing.Dest == c.Dest {
				exists = true
				break
			}
		}
		if !exists {
			dst = append(dst, c)
		}
	}
	return dst
}

// appendProjectLinkfiles 追加 linkfile（按 src+dest 去重）。
func appendProjectLinkfiles(dst, src []Linkfile) []Linkfile {
	for _, l := range src {
		exists := false
		for _, existing := range dst {
			if existing.Src == l.Src && existing.Dest == l.Dest {
				exists = true
				break
			}
		}
		if !exists {
			dst = append(dst, l)
		}
	}
	return dst
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
	case "name", "path", "remote", "revision", "upstream", "dest-branch", "groups", "sync-c", "sync-s", "sync-tags", "clone-depth", "force-path", "references":
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
	case "name", "path", "optional", "base-rev":
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
	if m.Default.SyncC.Set {
		fmt.Fprintf(&b, ` sync-c="%t"`, m.Default.SyncC.Value)
	}
	if m.Default.SyncS.Set {
		fmt.Fprintf(&b, ` sync-s="%t"`, m.Default.SyncS.Value)
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
		if len(r.Annotations) > 0 {
			b.WriteString(">\n")
			writeAnnotations(&b, r.Annotations, "    ")
			b.WriteString("  </remote>\n")
		} else {
			b.WriteString(" />\n")
		}
	}

	// include 元素：保留 groups/revision 与自定义属性。仅输出尚未展开的
	// include（手工构造的 Manifest）；解析器产生并展开 include 的清单中，
	// 子清单项目已并入 Projects，再输出 include 会在二次解析时重复展开，
	// 破坏 parse→ToXML→parse 的项目集合不变量。init/manifest 命令写扁平
	// 清单时也会显式清空 Includes。
	for _, inc := range m.Includes {
		if inc.manifest != nil {
			continue
		}
		b.WriteString("  <include")
		b.WriteString(attr("name", inc.Name))
		if inc.Groups != "" {
			b.WriteString(attr("groups", inc.Groups))
		}
		if inc.Revision != "" {
			b.WriteString(attr("revision", inc.Revision))
		}
		for _, k := range sortedKeys(inc.CustomAttrs) {
			b.WriteString(attr(k, inc.CustomAttrs[k]))
		}
		b.WriteString(" />\n")
	}

	// 添加项目
	for _, p := range m.Projects {
		projectName := normalizeManifestPath(p.Name)
		projectPath := normalizeManifestPath(p.Path)
		if projectPath == "" {
			projectPath = projectName
		}
		b.WriteString("  <project")
		b.WriteString(attr("name", projectName))
		// 始终包含 path 属性，如果为空则使用项目名称
		b.WriteString(attr("path", projectPath))
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
		if p.SyncC.Set {
			fmt.Fprintf(&b, ` sync-c="%t"`, p.SyncC.Value)
		}
		if p.SyncS.Set {
			fmt.Fprintf(&b, ` sync-s="%t"`, p.SyncS.Value)
		}
		if p.CloneDepth > 0 {
			fmt.Fprintf(&b, ` clone-depth="%d"`, p.CloneDepth)
		}
		if p.ForcePath.Set {
			fmt.Fprintf(&b, ` force-path="%t"`, p.ForcePath.Value)
		}
		if p.References != "" {
			b.WriteString(attr("references", p.References))
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
		// 检查是否有 copyfile/linkfile/annotation 子元素
		if len(p.Copyfiles) > 0 || len(p.Linkfiles) > 0 || len(p.Annotations) > 0 {
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

			writeAnnotations(&b, p.Annotations, "    ")

			b.WriteString("  </project>\n")
		} else {
			b.WriteString(" />\n")
		}
	}

	// 添加 extend-project（保留 groups/revision/dest-* 等扩展字段）。
	// 写盘前过滤悬空条目：目标项目不存在时写回会生成下次解析直接失败的
	// 清单（上游 fail-fast），因此这类 extend 不落盘。
	projectNames := make(map[string]bool, len(m.Projects))
	for i := range m.Projects {
		projectNames[normalizeManifestPath(m.Projects[i].Name)] = true
	}
	for _, ext := range m.ExtendProjects {
		if !projectNames[normalizeManifestPath(ext.Name)] {
			continue
		}
		b.WriteString("  <extend-project")
		b.WriteString(attr("name", ext.Name))
		if ext.Path != "" {
			b.WriteString(attr("path", normalizeManifestPath(ext.Path)))
		}
		if ext.DestPath != "" {
			b.WriteString(attr("dest-path", normalizeManifestPath(ext.DestPath)))
		}
		if ext.Groups != "" {
			b.WriteString(attr("groups", ext.Groups))
		}
		if ext.Revision != "" {
			b.WriteString(attr("revision", ext.Revision))
		}
		if ext.BaseRev != "" {
			b.WriteString(attr("base-rev", ext.BaseRev))
		}
		if ext.Remote != "" {
			b.WriteString(attr("remote", ext.Remote))
		}
		if ext.DestBranch != "" {
			b.WriteString(attr("dest-branch", ext.DestBranch))
		}
		if ext.Upstream != "" {
			b.WriteString(attr("upstream", ext.Upstream))
		}
		if len(ext.Copyfiles) > 0 || len(ext.Linkfiles) > 0 {
			b.WriteString(">\n")
			for _, c := range ext.Copyfiles {
				b.WriteString("    <copyfile")
				b.WriteString(attr("src", c.Src))
				b.WriteString(attr("dest", c.Dest))
				for _, k := range sortedKeys(c.CustomAttrs) {
					b.WriteString(attr(k, c.CustomAttrs[k]))
				}
				b.WriteString(" />\n")
			}
			for _, l := range ext.Linkfiles {
				b.WriteString("    <linkfile")
				b.WriteString(attr("src", l.Src))
				b.WriteString(attr("dest", l.Dest))
				for _, k := range sortedKeys(l.CustomAttrs) {
					b.WriteString(attr(k, l.CustomAttrs[k]))
				}
				b.WriteString(" />\n")
			}
			b.WriteString("  </extend-project>\n")
		} else {
			b.WriteString(" />\n")
		}
	}

	// 添加移除项目。写盘顺序是“项目在前、remove 在后”，因此任何仍能命中
	// 当前项目的 remove 都会在二次解析时生效、改变项目集合；这类残留条目
	// 不得输出（已消费的 remove 在解析期已从列表移除）。不命中的 optional
	// 悬空条目保留，输出后二次解析仍是无操作。
	for _, r := range m.RemoveProjects {
		if removeProjectMatchesProjects(m.Projects, r) {
			continue
		}
		b.WriteString("  <remove-project")
		b.WriteString(attr("name", r.Name))
		if r.Path != "" {
			b.WriteString(attr("path", normalizeManifestPath(r.Path)))
		}
		if r.Optional.Get(false) {
			b.WriteString(` optional="true"`)
		}
		if r.BaseRev != "" {
			b.WriteString(attr("base-rev", r.BaseRev))
		}
		for _, k := range sortedKeys(r.CustomAttrs) {
			b.WriteString(attr(k, r.CustomAttrs[k]))
		}
		b.WriteString(" />\n")
	}

	// 添加 repo-hooks（如果存在）
	if m.RepoHooks != nil {
		b.WriteString("  <repo-hooks")
		b.WriteString(attr("in-project", m.RepoHooks.InProject))
		if m.RepoHooks.EnabledList != "" {
			b.WriteString(attr("enabled-list", m.RepoHooks.EnabledList))
		}
		b.WriteString(" />\n")
	}

	// 添加 manifest-server（如果存在）
	if m.ManifestServer != nil {
		b.WriteString("  <manifest-server")
		b.WriteString(attr("url", m.ManifestServer.URL))
		if m.ManifestServer.Helper != "" {
			b.WriteString(attr("helper", m.ManifestServer.Helper))
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

// writeAnnotations 输出 annotation 子元素。Keep 显式为 "false" 的注解不落盘
// （上游 ProjectToXml 仅输出 keep=="true" 的注解；缺省视为保留）。
func writeAnnotations(b *strings.Builder, annotations []Annotation, indent string) {
	for _, a := range annotations {
		if a.Keep == "false" {
			continue
		}
		b.WriteString(indent)
		b.WriteString("<annotation")
		b.WriteString(attr("name", a.Name))
		b.WriteString(attr("value", a.Value))
		if a.Keep != "" && a.Keep != "true" {
			b.WriteString(attr("keep", a.Keep))
		}
		b.WriteString(" />\n")
	}
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
