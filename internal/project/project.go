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
	Name        string
	Path        string
	RemoteName  string
	RemoteURL   string
	FetchBase   string // 清单 remote 的原始 fetch 基址（可能为相对路径），用于解析完整项目 URL
	PushURL     string // 推送地址（完整 URL；manager 由 remote pushurl/回退 fetch 解析并拼接项目名）
	ReviewURL   string // 规范化 review 端点 + 项目名，可直接作为 git push 目标
	Revision    string
	Groups      []string
	GitRepo     *git.Repository
	ManifestURL string // 清单服务器/仓库 URL，用于解析相对 fetch 路径

	// 添加与engine.go 兼容的字段
	Relpath    string     // 项目相对路径
	Worktree   string     // 项目working directory
	Gitdir     string     // Git 目录
	RevisionID string     // 修订ID
	Linkfiles  []LinkFile // 链接文件列表
	Copyfiles  []CopyFile // 复制文件列表
	Objdir     string     // 对象目录
	Mirror     bool       // 镜像模式：裸仓库，Gitdir/Objdir 即仓库目录，无工作树

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

// repoDir 返回项目仓库目录：镜像项目是裸仓库 Gitdir，普通项目为工作树。
func (p *Project) repoDir() string {
	if p.Mirror {
		if p.Gitdir != "" {
			return p.Gitdir
		}
		return p.Objdir
	}
	return p.Worktree
}

// isBareRepository 判断目录是否为裸 git 仓库（HEAD/objects/refs 齐备）。
func isBareRepository(dir string) (bool, error) {
	if dir == "" {
		return false, nil
	}
	for _, sub := range []string{"HEAD", "objects", "refs"} {
		target := filepath.Join(dir, sub)
		if _, err := os.Stat(target); err != nil {
			if os.IsNotExist(err) {
				return false, nil
			}
			return false, fmt.Errorf("stat %s: %w", target, err)
		}
	}
	return true, nil
}

// exists 检查项目仓库是否存在。镜像项目是裸仓库，必须检查 HEAD/objects/refs，
// 不能沿用工作树下的 .git 子目录判定。
func (p *Project) exists() (bool, error) {
	if p.Mirror {
		return isBareRepository(p.repoDir())
	}
	return p.GitRepo.Exists()
}

// cloneRepository 克隆项目仓库，并完成镜像、remote 名与 SHA revision 后处理。
func (p *Project) cloneRepository(cloneURL string, depth int) error {
	dest := p.repoDir()
	if dest == "" {
		return fmt.Errorf("project %s has no clone destination", p.Name)
	}
	// 创建父目录
	if err := os.MkdirAll(filepath.Dir(dest), 0755); err != nil {
		return fmt.Errorf("create parent directory for %s: %w", dest, err)
	}

	if p.Mirror {
		// 镜像必须是裸仓库且 fetch refspec 为 +refs/*:refs/*，
		// 只能通过 git clone --mirror 表达（CloneOptions 无对应字段）。
		if _, err := p.GitRepo.Runner.Run("clone", "--mirror", "--", cloneURL, dest); err != nil {
			return fmt.Errorf("mirror clone %s: %w", cloneURL, err)
		}
	} else {
		// 克隆仓库：--branch 只接受短分支名，提交 ID 不传
		if err := p.GitRepo.Clone(cloneURL, git.CloneOptions{
			Depth:  depth,
			Branch: cloneBranch(p.Revision),
		}); err != nil {
			return fmt.Errorf("clone %s: %w", cloneURL, err)
		}
	}

	// clone 固定创建 origin；清单 remote 名不同时改名，后续 fetch 才能
	// 直接使用 RemoteName（对齐上游 --origin 语义）。
	if p.RemoteName != "" && p.RemoteName != "origin" {
		if _, err := p.GitRepo.Runner.RunInDir(dest, "remote", "rename", "origin", p.RemoteName); err != nil {
			return fmt.Errorf("rename remote origin to %s: %w", p.RemoteName, err)
		}
	}

	// 提交 ID 不能作为 --branch；克隆后显式检出，避免静默停在默认分支。
	// 镜像没有工作树，只更新裸仓库引用，不执行 checkout。
	if !p.Mirror && isRevisionID(strings.TrimSpace(p.Revision)) {
		rev := strings.TrimSpace(p.Revision)
		if _, err := p.GitRepo.Runner.RunInDir(dest, "checkout", rev, "--"); err != nil {
			return fmt.Errorf("checkout revision %s: %w", rev, err)
		}
	}

	return nil
}

// cloneBranch 把清单 revision 转换为 git clone --branch 可接受的参数：
//   - 去掉 refs/heads/ 与 refs/tags/ 前缀（git clone 只接受短名）；
//   - 提交 ID（40/64 位十六进制）不传 --branch，由后续同步检出；
//   - 空 revision 返回空串。
func cloneBranch(revision string) string {
	rev := strings.TrimSpace(revision)
	rev = strings.TrimPrefix(rev, "refs/heads/")
	rev = strings.TrimPrefix(rev, "refs/tags/")
	if rev == "" || isRevisionID(rev) {
		return ""
	}
	return rev
}

// isRevisionID 判断字符串是否为完整提交 ID（对齐上游 git_config.IsId：
// 40 或 64 位小写十六进制）。
func isRevisionID(rev string) bool {
	if len(rev) != 40 && len(rev) != 64 {
		return false
	}
	for _, c := range rev {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// resolveRelativeFetchURL 基于清单 URL 解析相对 fetch 路径；
// 无 base 时原样返回，绝不生成伪绝对路径。
func resolveRelativeFetchURL(base, rel string) string {
	if base == "" {
		return rel
	}
	cfg := &config.Config{ManifestURL: base}
	return cfg.ResolveRelativeURL(rel)
}

// joinProjectName 对已解析的 fetch 基地址拼接项目名（上游 ToRemoteSpec 语义：
// rstrip('/') + '/' + name），去掉全部尾部斜杠避免双斜杠。
func joinProjectName(base, name string) string {
	if name == "" {
		return base
	}
	return strings.TrimRight(base, "/") + "/" + name
}

// NormalizeReviewURL 把 manifest remote 的 review 规范化为 Gerrit 服务端点
// （对齐上游 git_config.Remote.ReviewUrl 的规范化部分）：
//   - 去掉 "persistent-" 前缀；
//   - 无 scheme（http/https/sso/ssh）时补 "http://"；
//   - 去掉尾部 "/Gerrit"、"/ssh_info"；
//   - 合并为单个尾部 "/"。
//
// 只做端点归一化，不拼接项目名、不做 urljoin。
func NormalizeReviewURL(raw string) string {
	u := strings.TrimSpace(raw)
	if u == "" {
		return ""
	}
	u = strings.TrimPrefix(u, "persistent-")

	scheme := ""
	if i := strings.Index(u, ":"); i > 0 {
		scheme = u[:i]
	}
	switch scheme {
	case "http", "https", "sso", "ssh":
	default:
		u = "http://" + u
	}

	u = strings.TrimSuffix(u, "/Gerrit")
	u = strings.TrimSuffix(u, "/ssh_info")
	for strings.HasSuffix(u, "//") {
		u = strings.TrimSuffix(u, "/")
	}
	if !strings.HasSuffix(u, "/") {
		u += "/"
	}
	return u
}

// ResolveReviewURL 在规范化端点上拼接项目名，得到可直接作为 git push 目标
// 的完整 review URL（对齐上游 ReviewUrl 返回 _review_url + projectname）。
// 无条件拼接一次，不做"端点已含项目名"的后缀猜测，也不做 urljoin。
func ResolveReviewURL(raw, projectName string) string {
	endpoint := NormalizeReviewURL(raw)
	if endpoint == "" {
		return ""
	}
	return joinProjectName(endpoint, strings.TrimSpace(projectName))
}

// ResolvePushURL 计算项目的完整推送地址：pushurl 优先，缺省回退 fetch；
// 仅对点前缀相对基址（"."、".."、"./"、"../"）按清单 URL 做 urljoin，
// 其余（绝对 URL、scp 风式、绝对文件路径、不带点号的相对值）原样保留，
// 最后无条件拼接一次 "/"+项目名（对齐上游 ToRemoteSpec）。空地址返回空串。
func ResolvePushURL(cfg *config.Config, pushURL, fetchURL, name string) string {
	source := strings.TrimSpace(pushURL)
	if source == "" {
		source = strings.TrimSpace(fetchURL)
	}
	if source == "" {
		return ""
	}
	if cfg != nil && isRelativeFetchURL(source) {
		if resolved := cfg.ResolveRelativeURL(source); resolved != "" {
			source = resolved
		}
	}
	return joinProjectName(source, strings.TrimSpace(name))
}

// resolveBaseURL 以 baseURL 为基准把 fetch/pushurl 基址解析为绝对形式：
// 可 urljoin 的相对路径（含不带点号的 "platform"）交给
// config.ResolveRelativeURL；绝对 URL、scp 风式与绝对文件路径原样返回。
// 无基准或无法解析时保留原值，保证调用方不会拿到空地址。
func resolveBaseURL(baseURL, raw string) string {
	if raw == "" {
		return ""
	}
	if baseURL == "" {
		return raw
	}
	cfg := &config.Config{ManifestURL: baseURL}
	if resolved := cfg.ResolveRelativeURL(raw); resolved != "" {
		return resolved
	}
	return raw
}

// resolveFetchURL 返回项目的完整 fetch URL：有原始 fetch 基址时以 baseURL
// 为基准做 urljoin，再无条件拼接一次项目名（对齐上游 ToRemoteSpec）；
// 没有 FetchBase 的直接构造项目沿用已完整解析的 RemoteURL。
func (p *Project) resolveFetchURL(baseURL string) string {
	if p.FetchBase == "" {
		return p.RemoteURL
	}
	return joinProjectName(resolveBaseURL(baseURL, p.FetchBase), p.Name)
}

// cloneURL 返回克隆使用的 URL。通过清单创建的项目有 FetchBase，直接解析
// 基址并拼接项目名；直接构造的项目没有 FetchBase，仅旧的相对 RemoteURL
// 需要基于清单 URL 解析后拼名。
func (p *Project) cloneURL() string {
	if p.FetchBase != "" {
		return p.resolveFetchURL(p.ManifestURL)
	}
	remote := p.RemoteURL
	if remote == "" || !isRelativeFetchURL(remote) {
		return remote
	}
	resolved := resolveRelativeFetchURL(p.ManifestURL, remote)
	if resolved == "" || resolved == remote {
		return remote
	}
	return joinProjectName(resolved, p.Name)
}

// fetchTarget 返回传给 git fetch 的目标：remote name 原样传递（由 git 解析
// 配置中的 URL）；没有 remote name 时才使用 URL。通过清单创建的项目按
// FetchBase 解析完整 URL；直接构造的项目仅相对 RemoteURL 需要解析并拼名。
// 绝不能把 remote name 当成 URL 解析。
func (p *Project) fetchTarget() string {
	if p.RemoteName != "" {
		return p.RemoteName
	}
	if p.FetchBase != "" {
		return p.resolveFetchURL(p.ManifestURL)
	}
	remote := p.RemoteURL
	if remote == "" || !isRelativeFetchURL(remote) {
		return remote
	}
	resolved := resolveRelativeFetchURL(p.ManifestURL, remote)
	if resolved == "" || resolved == remote {
		return remote
	}
	return joinProjectName(resolved, p.Name)
}

// fetchRemote 执行 git fetch。target 为 remote name 或 URL，原样交给 git
// （Repository.Fetch 只对相对 URL 解析）；retries 为失败重试次数，不再复用作 depth。
func (p *Project) fetchRemote(target string, prune, tags bool, depth, retries int) error {
	if target == "" {
		return fmt.Errorf("fetch target is empty")
	}
	if retries < 0 {
		retries = 0
	}

	fetchOpts := git.FetchOptions{
		Prune: prune,
		Tags:  tags,
		Depth: depth,
	}

	var lastErr error
	for attempt := 0; attempt <= retries; attempt++ {
		if err := p.GitRepo.Fetch(target, fetchOpts); err == nil {
			return nil
		} else { //nolint:revive // 需要在循环内保留错误用于重试判断
			lastErr = err
		}
		if attempt < retries {
			logger.Warn("项目 %s 获取更新failed，将重试 (%d/%d): %v", p.Name, attempt+1, retries, lastErr)
			time.Sleep(time.Second * time.Duration(attempt+1))
		}
	}
	return fmt.Errorf("fetch from %s: %w", target, lastErr)
}

// resolveRevisionID 把 revision 解析为提交 ID。仅 40/64 位十六进制视为提交
// ID；分支 revision（含显式 refs/heads/X）优先解析远程跟踪引用
// （refs/remotes/<remote>/<branch>），再回退本地分支（refs/heads/<branch>）：
// manifest 锁定的是远端版本，本地同名分支可能过期。裸分支名绝不回退到同名
// tag；其他显式 ref（refs/remotes/...、refs/tags/...）直接解析。
func (p *Project) resolveRevisionID(revision string) (string, error) {
	revision = strings.TrimSpace(revision)
	if revision == "" {
		return "", nil
	}

	candidates := make([]string, 0, 2)
	switch {
	case isRevisionID(revision):
		// 只有 40/64 位十六进制提交 ID 才直接解析，短十六进制仍按分支名处理
		candidates = append(candidates, revision)
	case strings.HasPrefix(revision, "refs/heads/"):
		// 显式 refs/heads/X 也优先远端跟踪引用：manifest 锁定远端版本，
		// 本地同名分支可能过期。
		branch := strings.TrimPrefix(revision, "refs/heads/")
		if p.RemoteName != "" {
			candidates = append(candidates, "refs/remotes/"+p.RemoteName+"/"+branch)
		}
		candidates = append(candidates, "refs/heads/"+branch)
	case strings.HasPrefix(revision, "refs/"):
		// 其他显式 ref（refs/remotes/...、refs/tags/...）直接解析
		candidates = append(candidates, revision)
	default:
		// 裸分支名只按分支解析，绝不回退到同名 tag（对齐上游 GetRevisionId）
		if p.RemoteName != "" {
			candidates = append(candidates, "refs/remotes/"+p.RemoteName+"/"+revision)
		}
		candidates = append(candidates, "refs/heads/"+revision)
	}

	var lastErr error
	for _, candidate := range candidates {
		out, err := p.GitRepo.RunCommand("rev-parse", "--verify", candidate+"^{commit}")
		if err == nil {
			return strings.TrimSpace(string(out)), nil
		}
		lastErr = err
	}
	return "", fmt.Errorf("resolve revision %s: %w", revision, lastErr)
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

	// DryRun 只打印将要执行的动作，不 clone/fetch/checkout
	if opts.DryRun {
		logger.Info("dry-run: 将同步项目 %s (%s)", p.Name, p.cloneURL())
		return nil
	}

	// 检查项目仓库是否存在（镜像项目按裸仓库布局判定）
	exists, err := p.exists()
	if err != nil {
		logger.Error("项目 [%s] 检查失 %v", p.Name, err)
		return fmt.Errorf("failed to check if project exists失 %w", err)
	}

	// 如果不存在，克隆仓库
	if !exists {
		// 相对 fetch 需要先基于清单 URL 解析并拼接项目名
		cloneURL := p.cloneURL()
		if cloneURL == "" {
			logger.Error("项目 [%s] remote URL is empty", p.Name)
			return fmt.Errorf("failed to clone project %s: remote URL is empty", p.Name)
		}

		// 只在非静默模式下输出信息日志
		if !opts.Quiet {
			logger.Info("克隆 [%s] <- %s", p.Name, cloneURL)
		}

		if err := p.cloneRepository(cloneURL, opts.Depth); err != nil {
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

		// remote name 原样传给 git，仅相对 URL 才解析
		if err := p.fetchRemote(p.fetchTarget(), opts.Prune, opts.Tags, opts.Depth, 0); err != nil {
			logger.Error("项目 [%s] 更新failed: %v", p.Name, err)
			return fmt.Errorf("获取更新failed: %w", err)
		}

		// 设置需要垃圾回收标
		p.mu.Lock()
		p.NeedGC = true
		p.mu.Unlock()
	}

	// 如果不是只获取，更新工作区（镜像项目没有工作树，跳过本地同步）
	if !opts.NetworkOnly && !p.Mirror {
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

		// 解析 manifest revision 后再 fast-forward/detach；绝不能直接按分支名
		// 检出后就报成功（同名本地分支可能指向旧提交）。
		if err := p.syncToRevision(opts.Force); err != nil {
			logger.Error("项目 [%s] 同步到 %s failed: %v", p.Name, p.Revision, err)
			return fmt.Errorf("sync to revision: %w", err)
		}

		if !opts.Quiet {
			logger.Info("项目 [%s] 更新完成", p.Name)
		}
	}

	return nil
}

// syncToRevision 把工作树同步到 manifest revision：
//   - 先把 revision 解析为提交 ID（分支优先远程跟踪，再本地分支）；
//   - HEAD 已等于该提交时直接返回；
//   - force 时强制检出该提交（不移动分支指针）；
//   - 当前分支跟踪该 revision 时 fast-forward，否则 detach 到该提交。
//
// 直接 checkout 分支名无法保证停留在 manifest 锁定提交上，因此统一按
// 解析后的提交 ID 操作。
func (p *Project) syncToRevision(force bool) error {
	revID, err := p.resolveRevisionID(p.Revision)
	if err != nil {
		return err
	}
	if revID == "" {
		return nil
	}

	headOut, err := p.GitRepo.RunCommand("rev-parse", "--verify", "HEAD")
	if err != nil {
		return fmt.Errorf("resolve HEAD: %w", err)
	}
	if strings.TrimSpace(string(headOut)) == revID {
		return nil
	}

	if force {
		if _, err := p.GitRepo.RunCommand("checkout", "--force", revID, "--"); err != nil {
			return fmt.Errorf("force checkout %s: %w", revID, err)
		}
		return nil
	}

	branch, err := p.currentBranch()
	if err != nil {
		return err
	}
	if branch != "" && p.branchTracksRevision(branch) {
		if _, err := p.GitRepo.RunCommand("merge", "--ff-only", revID); err != nil {
			return fmt.Errorf("fast-forward to %s failed (local branch diverged, use --force-sync): %w", revID, err)
		}
		return nil
	}

	if _, err := p.GitRepo.RunCommand("checkout", revID, "--"); err != nil {
		return fmt.Errorf("checkout revision %s: %w", revID, err)
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
	Depth               int // 克隆/获取深度（RetryFetches 仅表示重试次数）
	Prune               bool
	SSHProxy            interface{}
	CloneFilter         string
	PartialCloneExclude string
}

// SyncNetworkHalf 执行网络同步
func (p *Project) SyncNetworkHalf(opts SyncNetworkOptions) bool {

	logger.Debug("开始执行项目%s 的网络同步", p.Name)

	// 检查项目仓库是否存在（镜像项目按裸仓库布局判定）
	exists, err := p.exists()
	if err != nil {
		logger.Error("检查项目%s 是否存在failed: %v", p.Name, err)
		return false
	}

	// 如果不存在，克隆仓库
	if !exists {
		cloneURL := p.cloneURL()
		if cloneURL == "" {
			logger.Error("项目 %s remote URL is empty", p.Name)
			return false
		}
		if !opts.Quiet {
			logger.Info("克隆项目 %s %s", p.Name, cloneURL)
		}

		if err := p.cloneRepository(cloneURL, opts.Depth); err != nil {
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

	// remote name 原样传给 git；仅相对 URL 解析；RetryFetches 只表示重试次数
	if err := p.fetchRemote(p.fetchTarget(), opts.Prune, opts.Tags, opts.Depth, opts.RetryFetches); err != nil {
		logger.Error("failed to get project %s 更新failed: %v", p.Name, err)
		return false
	}

	logger.Debug("项目 %s 网络同步完成", p.Name)
	return true
}

// SyncLocalHalf 执行本地同步
func (p *Project) SyncLocalHalf(_ bool, forceSync bool, forceOverwrite bool) bool {
	logger.Debug("开始执行项目%s 的本地同步", p.Name)

	// 镜像项目是裸仓库，没有工作树/检出状态，本地同步无事可做
	if p.Mirror {
		logger.Debug("项目 %s 为镜像仓库，跳过本地同步", p.Name)
		return true
	}

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

	// 解析 manifest revision 后 fast-forward/detach；分支名与 revision 字符串
	// 相等不代表已同步，同名分支可能指向旧提交。
	if err := p.syncToRevision(forceSync || forceOverwrite); err != nil {
		logger.Error("项目 %s 同步到修订版本 failed: %v", p.Name, err)
		return false
	}
	logger.Debug("项目 %s 本地同步完成", p.Name)
	return true
}

// currentBranch 返回当前附着的本地分支名；游离 HEAD 返回空串。
func (p *Project) currentBranch() (string, error) {
	out, err := p.GitRepo.RunCommand("rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return "", fmt.Errorf("resolve current branch: %w", err)
	}
	branch := strings.TrimSpace(string(out))
	if branch == "HEAD" {
		// rev-parse 在游离 HEAD 时输出 HEAD
		return "", nil
	}
	return branch, nil
}

// branchTracksRevision 判断当前分支是否跟踪 manifest revision 对应的分支。
// 对齐上游 LocalMerge 判定：分支名等于 revision（短名），或
// branch.<name>.merge 指向 refs/heads/<revision>。
func (p *Project) branchTracksRevision(branch string) bool {
	rev := strings.TrimSpace(p.Revision)
	rev = strings.TrimPrefix(rev, "refs/heads/")
	if branch == "" || rev == "" {
		return false
	}
	if branch == rev {
		return true
	}
	out, err := p.GitRepo.RunCommand("config", "--get", "branch."+branch+".merge")
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(out)) == "refs/heads/"+rev
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

// safeWorktreeForDeletion 把 repoRoot 与 worktree 统一为绝对路径并校验
// 删除目标：必须是 repo 根目录下、非根目录本身、非 .repo 自身、且不越界。
// 使用 filepath.Rel 做分隔符感知的组件级判断，避免 repoRoot+"-other"
// 这类前缀兄弟目录被 strings.HasPrefix 误判为合法。
func (p *Project) safeWorktreeForDeletion(repoRoot, worktree string) (string, error) {
	absRoot, err := filepath.Abs(repoRoot)
	if err != nil {
		return "", fmt.Errorf("failed to resolve repo root %s: %w", repoRoot, err)
	}
	absWorktree, err := filepath.Abs(worktree)
	if err != nil {
		return "", fmt.Errorf("failed to resolve worktree %s: %w", worktree, err)
	}

	relPath, err := filepath.Rel(absRoot, absWorktree)
	if err != nil {
		return "", fmt.Errorf("working directory %s is not under repo root，deletion refused", worktree)
	}

	// 拒绝根目录本身、.repo 自身与任何越界路径（含前缀兄弟目录）
	if relPath == "." || relPath == ".repo" ||
		relPath == ".." || strings.HasPrefix(relPath, ".."+string(filepath.Separator)) ||
		filepath.IsAbs(relPath) {
		return "", fmt.Errorf("working directory %s is not a deletable project worktree under repo root，deletion refused", worktree)
	}

	return absWorktree, nil
}

// DeleteWorktree 删除工作树
func (p *Project) DeleteWorktree(quiet bool, forceRemoveDirty bool) error {
	logger.Debug("准备删除项目 %s 的工作树", p.Name)

	// 检查工作树是否存在
	if _, err := os.Stat(p.Worktree); err != nil {
		if os.IsNotExist(err) {
			logger.Debug("项目 %s 的工作树不存在，无需删除", p.Name)
			return nil
		}
		logger.Error("检查项目 %s 工作树failed: %v", p.Name, err)
		return fmt.Errorf("failed to stat worktree %s: %w", p.Worktree, err)
	}

	// 安全检查：先统一 repoRoot 与工作树为绝对路径并校验
	repoRoot, repoErr := config.GetRepoRoot()
	if repoErr != nil {
		logger.Error("failed to get repo root: %v", repoErr)
		return fmt.Errorf("failed to get repo root: %w", repoErr)
	}
	absWorktree, err := p.safeWorktreeForDeletion(repoRoot, p.Worktree)
	if err != nil {
		logger.Error("拒绝删除工作树 %s: %v", p.Worktree, err)
		return err
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

	// 删除工作树
	if !quiet {
		logger.Info("删除项目 %s 的工作树 %s", p.Name, absWorktree)
	}

	if err := os.RemoveAll(absWorktree); err != nil {
		logger.Error("删除项目 %s 的工作树failed: %v", p.Name, err)
		return fmt.Errorf("failed to delete worktree: %w", err)
	}

	logger.Debug("项目 %s 的工作树已删除", p.Name)
	return nil
}

// GetCurrentBranch 获取当前分支（短名）。
// Repository.CurrentBranch 返回完整 symbolic-ref HEAD（refs/heads/X）：
// 这里统一剥离 refs/heads/ 前缀，避免与 ListLocalBranches 的短名比较失配；
// 分离 HEAD 时 Repository 返回 "HEAD detached at <sha>"，原样返回供调用方识别。
func (p *Project) GetCurrentBranch() (string, error) {
	logger.Debug("failed to get project %s 的当前分支", p.Name)

	branch, err := p.GitRepo.CurrentBranch()
	if err != nil {
		logger.Error("failed to get project %s 当前分支failed: %v", p.Name, err)
		return "", fmt.Errorf("failed to get current branch: %w", err)
	}

	return strings.TrimPrefix(strings.TrimSpace(branch), "refs/heads/"), nil
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

	// 使用完整 refname 输出后剥掉 refs/heads/ 前缀：%(refname:short) 在分支与
	// 同名 tag 共存时会输出 heads/foo 这类有歧义的名字。
	output, err := p.GitRepo.RunCommand("for-each-ref", "--format=%(refname)", "refs/heads/")
	if err != nil {
		logger.Error("列出项目 %s 的本地分支failed: %v", p.Name, err)
		return nil, fmt.Errorf("列出本地分支failed: %w", err)
	}

	branches := make([]string, 0)
	for _, line := range strings.Split(string(output), "\n") {
		name := strings.TrimSpace(line)
		if name == "" {
			continue
		}
		name = strings.TrimPrefix(name, "refs/heads/")
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

	branch = strings.TrimPrefix(strings.TrimSpace(branch), "refs/heads/")
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
