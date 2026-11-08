package repo_sync

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/leopardxu/repo-go/internal/git"
	"github.com/leopardxu/repo-go/internal/manifest"
	"github.com/leopardxu/repo-go/internal/project"
)

// Superproject 表示超级项目
type Superproject struct {
	manifest *manifest.Manifest
	quiet    bool
	repoDir  string // .repo 元数据目录（worktree/输出均位于其下）
	worktree string
	gitRepo  *git.Repository
}

// NewSuperproject 创建一个新的超级项目。
// 工作树固定位于 <repo>/.repo/superproject，避免污染工作区根目录。
func NewSuperproject(manifest *manifest.Manifest, quiet bool) (*Superproject, error) {
	repoDir := resolveRepoMetadataDir(manifest)
	worktree := filepath.Join(repoDir, "superproject")

	// 创建超级项目
	sp := &Superproject{
		manifest: manifest,
		quiet:    quiet,
		repoDir:  repoDir,
		worktree: worktree,
		gitRepo:  git.NewRepository(worktree, git.NewRunner()),
	}

	// 初始化超级项目
	if err := sp.init(); err != nil {
		return nil, err
	}

	return sp, nil
}

// init 初始化超级项目工作树。
// 对齐上游：超级项目是位于 .repo 下的普通工作树，
// 先在磁盘上创建目录，再在工作树内执行非裸 git init；
// 后续 fetch/checkout 也必须在该工作树内运行（gitRepo 的 Path 即 worktree）。
func (sp *Superproject) init() error {
	// 确保 .repo 目录存在（superproject-manifest.xml 会写入其中）
	if err := os.MkdirAll(sp.repoDir, 0755); err != nil {
		return fmt.Errorf("创建超级项目元数据目录failed: %w", err)
	}

	// 先创建工作树目录（git init 需要一个已存在的目录作为工作区）
	if err := os.MkdirAll(sp.worktree, 0755); err != nil {
		return fmt.Errorf("创建超级项目工作树failed: %w", err)
	}

	// 已初始化的超级项目直接复用
	if _, err := os.Stat(filepath.Join(sp.worktree, ".git")); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("检查超级项目工作树failed: %w", err)
	}

	// 在工作树内初始化普通（非裸）仓库
	if _, err := sp.gitRepo.RunCommand("init"); err != nil {
		return fmt.Errorf("初始化超级项目failed: %w", err)
	}

	return nil
}

// superprojectConfig 从清单的 <superproject> 元素解析出名称、远程 URL 与修订。
// remote 为空时直接使用清单 default remote（不探测同名 remote）；
// revision 为空时依次回退 remote revision、default revision。
//
// 返回值：
//   - projectURL：用于 remote add/fetch 的完整 URL（fetch 基址 + "/" + name）；
//   - fetchBase：解析后的 fetch 基址，写入生成的 superproject-manifest.xml；
//   - remote 元素里的相对 fetch 按清单仓库 URL（.repo/config.json 的
//     manifest_url）做 urljoin 解析，绝对 fetch 原样保留。
func (sp *Superproject) superprojectConfig() (name, projectURL, fetchBase, revision string, err error) {
	if sp.manifest == nil || sp.manifest.Superproject == nil {
		return "", "", "", "", fmt.Errorf("清单中未定义超级项目（<superproject>）")
	}
	sup := sp.manifest.Superproject
	name = sup.Name

	remoteName := strings.TrimSpace(sup.Remote)
	if remoteName == "" {
		// <superproject> 未指定 remote 时直接使用清单 default remote，
		// 不探测与 superproject 同名的 remote（对齐上游语义）。
		remoteName = strings.TrimSpace(sp.manifest.Default.Remote)
	}
	if remoteName == "" {
		return "", "", "", "", fmt.Errorf("清单中未定义超级项目 %q 的远程仓库", name)
	}

	rawFetch, err := sp.manifest.GetRemoteURL(remoteName)
	if err != nil {
		return "", "", "", "", fmt.Errorf("解析超级项目 %q 的远程仓库 %q failed: %w", name, remoteName, err)
	}
	if strings.TrimSpace(rawFetch) == "" {
		return "", "", "", "", fmt.Errorf("超级项目 %q 的远程仓库 %q URL 为空", name, remoteName)
	}

	fetchBase, err = sp.resolveFetchBase(name, rawFetch)
	if err != nil {
		return "", "", "", "", err
	}
	projectURL = strings.TrimSuffix(fetchBase, "/") + "/" + name

	revision = strings.TrimSpace(sup.Revision)
	if revision == "" {
		for _, remote := range sp.manifest.Remotes {
			if remote.Name == remoteName {
				revision = strings.TrimSpace(remote.Revision)
				break
			}
		}
	}
	if revision == "" {
		revision = strings.TrimSpace(sp.manifest.Default.Revision)
	}
	if revision == "" {
		return "", "", "", "", fmt.Errorf("清单中未定义超级项目 %q 的分支/修订", name)
	}

	return name, projectURL, fetchBase, revision, nil
}

// resolveFetchBase 将 superproject remote 的 fetch 解析为基址：
// 绝对地址（含 scp 风式/绝对路径）去掉尾部 '/' 后原样使用；其余地址
// （含 "."、".." 与不带点号的 "platform"）按清单仓库 URL 做 urljoin。
// 无法解析时返回错误，避免生成 "/foo" 伪绝对地址或把相对值交给 git。
func (sp *Superproject) resolveFetchBase(name, fetch string) (string, error) {
	fetch = strings.TrimSpace(fetch)
	if fetch == "" {
		return "", fmt.Errorf("超级项目 %q 的远程仓库 URL 为空", name)
	}
	if isAbsoluteRemoteURL(fetch) {
		return strings.TrimSuffix(fetch, "/"), nil
	}

	manifestURL := manifestURLFromRepoDir(sp.repoDir)
	if manifestURL == "" {
		return "", fmt.Errorf("无法解析超级项目 %q 的相对 fetch %q：清单仓库 URL 为空", name, fetch)
	}
	resolved, err := resolveURLAgainstManifest(manifestURL, fetch)
	if err != nil {
		return "", fmt.Errorf("无法解析超级项目 %q 的相对 fetch %q（清单仓库 URL %s）: %w",
			name, fetch, manifestURL, err)
	}
	return strings.TrimSuffix(resolved, "/"), nil
}

// UpdateProjectsRevisionID 从超级项目更新项目的修订ID
func (sp *Superproject) UpdateProjectsRevisionID(projects []*project.Project) (string, error) {
	// 从清单的 <superproject>（name/remote/revision）解析远程 URL 与修订，
	// 不再依赖 superproject-remote/superproject-branch 自定义属性。
	// projectURL 用于 git remote add/fetch；fetchBase 写入生成的清单，
	// 保持清单中的 fetch 仍是基址（与上游清单语义一致）。
	name, superprojectURL, superprojectFetch, superprojectRevision, err := sp.superprojectConfig()
	if err != nil {
		return "", err
	}

	// 添加远程仓库（使用完整项目 URL，而不是相对 fetch 基址）
	if _, err := sp.gitRepo.RunCommand("remote", "add", "origin", superprojectURL); err != nil {
		// 如果远程仓库已存在，则设置URL
		if _, err := sp.gitRepo.RunCommand("remote", "set-url", "origin", superprojectURL); err != nil {
			return "", fmt.Errorf("设置超级项目远程仓库failed: %w", err)
		}
	}

	// 获取超级项目
	if !sp.quiet {
		fmt.Printf("获取超级项目 %s (%s, revision=%s)\n", name, superprojectURL, superprojectRevision)
	}

	// 获取超级项目：分支/标签按名称 fetch；完整提交 SHA 直接按对象 fetch
	// （服务端禁止按 SHA fetch 时退化为获取全部引用）。
	if _, fetchErr := sp.gitRepo.RunCommand("fetch", "origin", superprojectRevision); fetchErr != nil {
		if !isCommitHash(superprojectRevision) {
			return "", fmt.Errorf("获取超级项目failed: %w", fetchErr)
		}
		if _, retryErr := sp.gitRepo.RunCommand("fetch", "origin"); retryErr != nil {
			return "", fmt.Errorf("获取超级项目failed: %w", retryErr)
		}
	}

	// 检出超级项目：提交 SHA 直接 checkout，其余使用 FETCH_HEAD。
	checkoutTarget := "FETCH_HEAD"
	if isCommitHash(superprojectRevision) {
		checkoutTarget = superprojectRevision
	}
	if _, err := sp.gitRepo.RunCommand("checkout", checkoutTarget); err != nil {
		return "", fmt.Errorf("检出超级项目failed: %w", err)
	}

	// 获取超级项目的提交ID
	superprojectCommitID, err := sp.gitRepo.RunCommand("rev-parse", "HEAD")
	if err != nil {
		return "", fmt.Errorf("获取超级项目提交ID failed: %w", err)
	}
	superprojectCommitIDStr := strings.TrimSpace(string(superprojectCommitID))

	// 对齐上游 _GetAllProjectsCommitIds：整树读取 gitlink（mode 160000）。
	// stdout 为空或命令失败均视为整体失败，不能退化为“跳过部分项目”。
	gitlinks, err := sp.lsTreeGitlinks("HEAD")
	if err != nil {
		return "", err
	}

	// 只更新非 local-manifest、非已固定（RevisionID 为完整 SHA）且有 gitlink
	// 的项目；任一项目缺失 gitlink 都按上游语义整体失败，不写半覆盖清单。
	localKeys := sp.localManifestProjectKeys()
	missing := make([]string, 0)
	overrides := make(map[int]string, len(projects))
	for i, p := range projects {
		if superprojectSkipProject(p, localKeys) {
			continue
		}
		relpath := superprojectProjectRelpath(p)
		commitID, ok := gitlinks[relpath]
		if !ok {
			missing = append(missing, relpath)
			continue
		}
		overrides[i] = commitID
	}
	if len(missing) > 0 {
		return "", fmt.Errorf("superproject does not have commit ids for projects: %s", strings.Join(missing, ", "))
	}
	for i, commitID := range overrides {
		projects[i].RevisionID = commitID
	}

	// 创建超级项目清单（写入 .repo 下，而非 cwd）
	manifestPath := filepath.Join(sp.repoDir, "superproject-manifest.xml")

	// 创建清单内容
	var content strings.Builder
	content.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<manifest>
  <remote name="superproject" fetch="`)
	content.WriteString(xmlAttrEscape(superprojectFetch))
	content.WriteString(`" />
  <default remote="superproject" revision="`)
	content.WriteString(xmlAttrEscape(superprojectCommitIDStr))
	content.WriteString(`" />
`)

	for _, p := range projects {
		if p == nil {
			continue
		}
		// 对齐上游 ToXml(omit_local=True)：local manifest 项目不写入
		// 超级项目生成的清单，避免重复定义。
		if _, ok := localKeys[p.Name]; ok {
			continue
		}
		if _, ok := localKeys[superprojectProjectRelpath(p)]; ok {
			continue
		}
		revision := strings.TrimSpace(p.RevisionID)
		if !isFullCommitID(revision) {
			// 跳过（local manifest/已固定）项目的清单条目仍保留其原始 revision。
			revision = strings.TrimSpace(p.Revision)
		}
		content.WriteString(`  <project name="`)
		content.WriteString(xmlAttrEscape(p.Name))
		content.WriteString(`" path="`)
		content.WriteString(xmlAttrEscape(p.Path))
		content.WriteString(`" revision="`)
		content.WriteString(xmlAttrEscape(revision))
		content.WriteString(`" />
`)
	}

	// 关闭清单
	content.WriteString(`</manifest>
`)

	// 写入清单文件
	if err := os.WriteFile(manifestPath, []byte(content.String()), 0644); err != nil {
		return "", fmt.Errorf("写入超级项目清单 failed: %w", err)
	}

	return manifestPath, nil
}

// lsTreeGitlinks 读取 revision 整树中的 gitlink（mode 160000），
// 返回 path→commit ID。stdout 为空按失败处理（对齐上游 fatal）。
func (sp *Superproject) lsTreeGitlinks(revision string) (map[string]string, error) {
	output, err := sp.gitRepo.RunCommand("ls-tree", "-z", "-r", revision)
	if err != nil {
		return nil, fmt.Errorf("git ls-tree failed for superproject %s: %w", revision, err)
	}
	if len(bytes.TrimSpace(output)) == 0 {
		return nil, fmt.Errorf("git ls-tree returned no data for superproject %s", revision)
	}
	return parseLsTreeGitlinks(output)
}

// parseLsTreeGitlinks 解析 `git ls-tree -z -r` 输出：
//   - 每条记录为 "<mode> <type> <sha>\t<path>"（NUL 分隔，path 不转义）；
//   - 只有 mode 160000（gitlink）才记录，普通 blob/tree/symlink 一律忽略；
//   - 格式异常返回错误（整体失败），避免静默漏掉项目。
func parseLsTreeGitlinks(data []byte) (map[string]string, error) {
	gitlinks := make(map[string]string)
	for _, record := range bytes.Split(data, []byte{0}) {
		if len(bytes.TrimSpace(record)) == 0 {
			continue
		}
		meta, pathBytes, ok := bytes.Cut(record, []byte{'\t'})
		if !ok {
			return nil, fmt.Errorf("malformed git ls-tree entry %q: missing tab separator", record)
		}
		fields := strings.Fields(string(meta))
		if len(fields) != 3 {
			return nil, fmt.Errorf("malformed git ls-tree entry %q: expected mode/type/sha", record)
		}
		mode, objectType, commitID := fields[0], fields[1], fields[2]
		if mode != "160000" {
			continue
		}
		if objectType != "commit" {
			return nil, fmt.Errorf("gitlink %q has unexpected object type %q", string(pathBytes), objectType)
		}
		gitlinks[string(pathBytes)] = commitID
	}
	return gitlinks, nil
}

// superprojectSkipProject 对齐上游 _SkipUpdatingProjectRevisionId：
//   - 空路径项目跳过；
//   - RevisionID 已是完整提交 SHA（已固定/已覆盖）的项目跳过；
//   - local manifest 中声明的项目跳过（按 name 或 path 匹配）。
func superprojectSkipProject(p *project.Project, localKeys map[string]struct{}) bool {
	if p == nil || superprojectProjectRelpath(p) == "" {
		return true
	}
	if isFullCommitID(p.RevisionID) {
		return true
	}
	if _, ok := localKeys[p.Name]; ok {
		return true
	}
	if _, ok := localKeys[superprojectProjectRelpath(p)]; ok {
		return true
	}
	return false
}

// superprojectProjectRelpath 返回项目在超级项目树中的相对路径。
func superprojectProjectRelpath(p *project.Project) string {
	if p == nil {
		return ""
	}
	if relpath := strings.TrimSpace(p.Relpath); relpath != "" {
		return strings.TrimRight(relpath, "/")
	}
	return strings.TrimRight(strings.TrimSpace(p.Path), "/")
}

// localManifestProjectKeys 收集 .repo/local_manifests 中声明的项目 name/path。
// local manifest 合并后已无法从 project.Project 区分来源，因此按上游
// IsFromLocalManifest 语义直接扫描本地清单文件（不可读/不存在时视为无）。
func (sp *Superproject) localManifestProjectKeys() map[string]struct{} {
	keys := make(map[string]struct{})
	repoDir := sp.repoDir
	if repoDir == "" {
		repoDir = resolveRepoMetadataDir(sp.manifest)
	}
	localDir := filepath.Join(repoDir, "local_manifests")
	entries, err := os.ReadDir(localDir)
	if err != nil {
		return keys
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".xml") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(localDir, entry.Name()))
		if err != nil {
			continue
		}
		var doc struct {
			Projects []struct {
				Name string `xml:"name,attr"`
				Path string `xml:"path,attr"`
			} `xml:"project"`
			ExtendProjects []struct {
				Name string `xml:"name,attr"`
				Path string `xml:"path,attr"`
			} `xml:"extend-project"`
		}
		if err := xml.Unmarshal(data, &doc); err != nil {
			continue
		}
		addKeys := func(name, path string) {
			if name != "" {
				keys[name] = struct{}{}
			}
			if path != "" {
				keys[path] = struct{}{}
			}
		}
		for _, p := range doc.Projects {
			addKeys(p.Name, p.Path)
		}
		for _, p := range doc.ExtendProjects {
			addKeys(p.Name, p.Path)
		}
	}
	return keys
}

// xmlAttrEscape 转义写入 XML 属性值的文本。
func xmlAttrEscape(value string) string {
	var buf strings.Builder
	_ = xml.EscapeText(&buf, []byte(value))
	return buf.String()
}
