package commands

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/leopardxu/repo-go/internal/config"
	"github.com/leopardxu/repo-go/internal/logger"
	"github.com/leopardxu/repo-go/internal/manifest"
	"github.com/leopardxu/repo-go/internal/project"
	"github.com/spf13/cobra"
)

// DownloadOptions holds the options for the download command
type DownloadOptions struct {
	CherryPick bool
	Revert     bool
	FFOnly     bool
	Branch     string // -b/--branch：应用 change 前先创建/切换的本地分支名
	Truncate   bool   // -t/--truncate：忽略用户给定 patchset，始终取最新 patchset
	Verbose    bool
	Quiet      bool
	Jobs       int
	Config     *config.Config
	CommonManifestOptions
}

// DownloadCmd creates the download command
func DownloadCmd() *cobra.Command {
	opts := &DownloadOptions{}
	cmd := &cobra.Command{
		Use:   "download [<project>...] [<change>...]",
		Short: "Download project changes from the remote server",
		Long: `Downloads changes for the specified projects from their remote repositories.

By default the change is fetched and checked out in detached HEAD state.
Use --cherry-pick (-c) to cherry-pick the change, --revert (-r) to revert it, or
--ff-only (-f) to fast-forward merge it. Use --branch (-b) to create/switch to a
local branch before applying the change.`,
		RunE: func(_ *cobra.Command, args []string) error {
			return runDownload(opts, args)
		},
	}

	// Add flags
	cmd.Flags().BoolVarP(&opts.CherryPick, "cherry-pick", "c", false, "download and cherry-pick specific changes")
	cmd.Flags().BoolVarP(&opts.Revert, "revert", "r", false, "download and revert specific changes")
	cmd.Flags().BoolVarP(&opts.FFOnly, "ff-only", "f", false, "fast-forward merge instead of checkout")
	cmd.Flags().StringVarP(&opts.Branch, "branch", "b", "", "create a new branch first")
	cmd.Flags().BoolVarP(&opts.Truncate, "truncate", "t", false, "truncate to the latest patchset of the change")
	cmd.Flags().BoolVarP(&opts.Verbose, "verbose", "v", false, "show all output")
	cmd.Flags().BoolVarP(&opts.Quiet, "quiet", "q", false, "only show errors")
	cmd.Flags().IntVarP(&opts.Jobs, "jobs", "j", 8, "number of jobs to run in parallel")
	cmd.Flags().StringVarP(&opts.Groups, "groups", "g", "", "restrict download to projects in specified groups (comma-separated)")
	AddManifestFlags(cmd, &opts.CommonManifestOptions)

	return cmd
}

// loadDownloadConfig loads the configuration
func loadDownloadConfig() (*config.Config, error) {
	return config.Load()
}

// loadDownloadManifest loads the manifest file
func loadDownloadManifest(cfg *config.Config, groups []string) (*manifest.Manifest, error) {
	parser := manifest.NewParser()
	return parser.ParseFromFile(cfg.ManifestName, groups)
}

// changeInfo represents a Gerrit change to download
type ChangeInfo struct {
	Project         string   `json:"project"`
	Branch          string   `json:"branch"`
	ChangeID        string   `json:"id"`
	Subject         string   `json:"subject"`
	Status          string   `json:"status"`
	URL             string   `json:"url"`
	CreatedOn       int      `json:"created_on"`
	LastUpdated     int      `json:"last_updated"`
	Number          int      `json:"number"`
	CurrentPatchSet patchSet `json:"currentPatchSet"`
	PatchID         string
}

// patchSet 表示 Gerrit change 的补丁集信息（由 --current-patch-set 查询返回，用于取最新 patchset）
type patchSet struct {
	Number int `json:"number"`
}

// 变更标识格式：
//   - Change-Id：I + 40 位 hex
//   - 数字 change number：非零开头
//   - 可选 Gerrit 三元组前缀 project~branch~（project/branch 可含 /）
//   - 可选 /<patchset> 后缀
var (
	downloadChangeIDPattern = regexp.MustCompile(`^I[0-9a-fA-F]{40}$`)
	downloadNumberPattern   = regexp.MustCompile(`^[1-9][0-9]*$`)
)

// splitChangeTriple 拆分 Gerrit 的 project~branch~change 三元组。
// project/branch 允许包含 '/'（如 platform/build、feature/x）。
func splitChangeTriple(arg string) (projectName, branch, spec string, ok bool) {
	first := strings.Index(arg, "~")
	if first <= 0 {
		return "", "", arg, false
	}
	second := strings.Index(arg[first+1:], "~")
	if second <= 0 {
		return "", "", arg, false
	}
	second += first + 1
	projectName = arg[:first]
	branch = arg[first+1 : second]
	spec = arg[second+1:]
	if projectName == "" || branch == "" || spec == "" {
		return "", "", arg, false
	}
	return projectName, branch, spec, true
}

// splitChangePatchset 拆分 "</patchset>" 后缀（对齐上游 CHANGE_RE 的 [/.-] 分隔符）；
// 仅当后缀为纯数字时视为 patchset。这样 Change-Id 中的任何字符都不会被误当成
// patchset 分隔符。
func splitChangePatchset(spec string) (changePart, patchset string) {
	for i := len(spec) - 1; i > 0; i-- {
		switch spec[i] {
		case '/', '.', '-':
			if suffix := spec[i+1:]; downloadIsDigits(suffix) {
				return spec[:i], suffix
			}
		}
	}
	return spec, ""
}

// downloadIsDigits 判断字符串是否全部为数字（非空）。
func downloadIsDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// parseChangeArg parses a change argument in the format
// "[<project>~<branch>~]<change-id>[/<patch-id>]".
func parseChangeArg(arg string) (*ChangeInfo, error) {
	projectName := ""
	branch := ""
	spec := arg
	if p, b, rest, ok := splitChangeTriple(arg); ok {
		projectName, branch, spec = p, b, rest
	}

	changeID, patchID := splitChangePatchset(spec)
	if !downloadChangeIDPattern.MatchString(changeID) && !downloadNumberPattern.MatchString(changeID) {
		return nil, fmt.Errorf("invalid change argument %q", arg)
	}
	if patchID != "" {
		if n, err := strconv.Atoi(patchID); err != nil || n <= 0 {
			return nil, fmt.Errorf("invalid patchset %q in change argument %q", patchID, arg)
		}
	}

	return &ChangeInfo{
		Project:  projectName,
		Branch:   branch,
		ChangeID: changeID,
		PatchID:  patchID,
	}, nil
}

// isChangeArg checks if an argument is a change ID
func isChangeArg(arg string) bool {
	spec := arg
	if _, _, rest, ok := splitChangeTriple(arg); ok {
		spec = rest
	}
	changeID, _ := splitChangePatchset(spec)
	return downloadChangeIDPattern.MatchString(changeID) || downloadNumberPattern.MatchString(changeID)
}

// downloadRequest 表示一次 <project> + <change> 下载请求。
// Project 为 nil 时表示未显式指定项目（由 cwd/查询结果决定）。
type downloadRequest struct {
	Project *project.Project
	Change  *ChangeInfo
}

// parseDownloadArgs 按上游 repo download 语义解析参数：
//   - <change> 参数归属其前面最近的 <project> 参数（未指定则使用 cwd 项目）；
//   - <project> 参数支持名称、"."、相对/绝对路径（GetProjectsByArgs）；
//   - 与多个项目匹配时，若 cwd 项目在候选内则选它，否则返回歧义错误。
func parseDownloadArgs(args []string, manager *project.Manager, baseDir string) ([]downloadRequest, error) {
	requests := make([]downloadRequest, 0, len(args))
	var currentProject *project.Project

	for _, arg := range args {
		if isChangeArg(arg) {
			change, err := parseChangeArg(arg)
			if err != nil {
				return nil, err
			}
			requests = append(requests, downloadRequest{Project: currentProject, Change: change})
			continue
		}

		projects, err := manager.GetProjectsByArgs([]string{arg}, baseDir)
		if err != nil {
			return nil, fmt.Errorf("解析项目参数 %q failed: %w", arg, err)
		}
		if len(projects) == 0 {
			return nil, fmt.Errorf("no project matched %q", arg)
		}
		if len(projects) > 1 {
			cwdProject := findCwdProject(manager, baseDir)
			chosen := (*project.Project)(nil)
			for _, p := range projects {
				if p == cwdProject {
					chosen = p
					break
				}
			}
			if chosen == nil {
				names := make([]string, 0, len(projects))
				for _, p := range projects {
					names = append(names, p.Name+" @ "+p.Revision)
				}
				return nil, fmt.Errorf("%s matches too many projects (%s); please re-run inside the project checkout",
					arg, strings.Join(names, ", "))
			}
			currentProject = chosen
			continue
		}
		currentProject = projects[0]
	}

	if len(requests) == 0 {
		return nil, fmt.Errorf("no change specified; usage: repo download [<project>...] <change>[/<patchset>] ... (see --help)")
	}
	return requests, nil
}

// runDownload executes the download command
func runDownload(opts *DownloadOptions, args []string) error {
	// 初始化日志记录器
	log := logger.NewDefaultLogger()
	switch {
	case opts.Verbose:
		log.SetLevel(logger.LogLevelDebug)
	case opts.Quiet:
		log.SetLevel(logger.LogLevelError)
	default:
		log.SetLevel(logger.LogLevelInfo)
	}
	// 与命令内一致的本地 logger，同步到全局以供 downloadProjects 等使用
	logger.SetGlobalLogger(log)

	// 确保在repo根目录下执行
	originalDir, err := EnsureRepoRoot(log)
	if err != nil {
		log.Error("查找repo根目录failed: %v", err)
		return fmt.Errorf("failed to locate repo root: %w", err)
	}
	defer func() {
		if err := RestoreWorkDir(originalDir, log); err != nil {
			log.Warn("恢复工作目录failed: %v", err)
		}
	}()

	// 加载配置
	log.Debug("Loading configuration")
	cfg, err := loadDownloadConfig()
	if err != nil {
		log.Error("Failed to load config: %v", err)
		return fmt.Errorf("failed to load config: %w", err)
	}
	opts.Config = cfg

	// 组过滤：显式 --groups 优先，否则使用配置的有效组（空 Groups 视为 default）。
	effectiveGroups := opts.Groups
	if effectiveGroups == "" {
		effectiveGroups = cfg.EffectiveGroups()
	}

	// 加载清单
	log.Debug("Parsing manifest file")
	mf, err := loadDownloadManifest(cfg, manifest.SplitGroups(effectiveGroups))
	if err != nil {
		log.Error("Failed to parse manifest: %v", err)
		return fmt.Errorf("failed to parse manifest: %w", err)
	}

	// 创建项目管理器
	log.Debug("Creating project manager")
	manager := project.NewManagerFromManifest(mf, cfg)

	// 解析 <project> 参数与 change 参数（保留顺序配对）
	requests, err := parseDownloadArgs(args, manager, originalDir)
	if err != nil {
		log.Error("Failed to parse download arguments: %v", err)
		return err
	}

	return downloadAndCherryPickChanges(opts, mf, manager, requests, originalDir)
}

// downloadAndCherryPickChanges downloads the specified changes and applies them.
//
// 模式（对齐上游 repo download）：
//   - 默认（无 -c/-r）：fetch 后 git checkout FETCH_HEAD（detached HEAD）。
//   - --cherry-pick/-c：fetch 后 git cherry-pick FETCH_HEAD。
//   - --revert/-r：fetch 后 git revert FETCH_HEAD。
//   - --ff-only/-f：fetch 后 git merge --no-stat --ff-only FETCH_HEAD（不 detach）。
//   - -b/--branch <name>：先创建/切换本地分支再应用 change（含 -c/-r/-f 组合）。
//   - --cherry-pick 与 --ff-only 组合时 cherry-pick 附加 --ff。
//
// ref 分片使用 Gerrit 数字 change number 的末 2 位（refs/changes/XX/<number>/<patchset>），
// 而非 Change-Id 的末 2 字符。
func downloadAndCherryPickChanges(opts *DownloadOptions, mf *manifest.Manifest, manager *project.Manager, requests []downloadRequest, originalDir string) error {
	log := logger.Global()

	if len(requests) == 0 {
		return fmt.Errorf("no changes specified for download")
	}

	log.Debug("Downloading %d change(s)", len(requests))

	// 上游 repo download 支持多对 <project> <change>：依次获取并应用每个 change，
	// 任一failed即停止。
	for _, req := range requests {
		if err := downloadOneChange(opts, mf, manager, req, originalDir); err != nil {
			return err
		}
	}
	return nil
}

// downloadOneChange 获取并应用单个 Gerrit change。
// req.Project 为显式 <project> 参数（可为 nil）；originalDir 为执行 EnsureRepoRoot
// 前的 cwd（绝对路径），用于未指定项目时回退到当前所在项目。
func downloadOneChange(opts *DownloadOptions, mf *manifest.Manifest, manager *project.Manager, req downloadRequest, originalDir string) error {
	log := logger.Global()
	change := req.Change

	log.Debug("Processing change %s (patch %s)", change.ChangeID, change.PatchID)

	// 上下文项目（cwd 项目）：用于 SSH 查询 Gerrit 的 push 地址 host 与歧义消解，
	// 也在 Gerrit 返回的 project 不在 manifest 时回退。
	contextProj := findCwdProject(manager, originalDir)

	// 显式项目（<project> 参数或 project~branch~change 三元组）优先。
	explicitProj := req.Project
	if explicitProj == nil && change.Project != "" {
		explicitProj = manager.GetProject(change.Project)
	}
	// 显式 <project> 也用于 Gerrit query 多结果消歧。
	if change.Project == "" && explicitProj != nil {
		change.Project = explicitProj.Name
	}
	queryProj := explicitProj
	if queryProj == nil {
		queryProj = contextProj
	}

	// 获取变更详细信息（通过 SSH 查询 Gerrit），得到数字 change number 与最新 patchset。
	changeDetail, err := change.GetChangeDetail(mf, queryProj)
	if err != nil {
		log.Error("Failed to get change details: %v", err)
		return fmt.Errorf("failed to get change details: %w", err)
	}

	// 解析 change 所属项目（对齐上游 repo download）：
	// 1) 显式 <project> 参数优先；
	// 2) Gerrit 返回的 project 在 manifest 中；
	// 3) 匹配failed时回退到 cwd 项目，并打印与上游一致的警告。
	proj := explicitProj
	if proj == nil {
		proj = manager.GetProject(changeDetail.Project)
	}
	if proj == nil {
		log.Warn("Failed to auto detect change project: Change not belong to any project: change: %s, repository: %s, branch: %s",
			change.ChangeID, changeDetail.Project, changeDetail.Branch)
		if contextProj == nil {
			return fmt.Errorf("cannot determine project for change %s: not in manifest and no cwd project", change.ChangeID)
		}
		log.Debug("Defaulting to cwd project %s", contextProj.Name)
		proj = contextProj
	}
	change.Project = proj.Name
	log.Info("Change belongs to project: %s", change.Project)

	// 构建 Gerrit change ref：refs/changes/XX/<number>/<patchset>
	// XX 为数字 change number 的末 2 位（Gerrit 约定）
	changeNumber := changeDetail.Number
	if changeNumber <= 0 {
		changeNumber = parseChangeNumber(changeDetail.ChangeID)
	}
	if changeNumber <= 0 {
		changeNumber = parseChangeNumber(change.ChangeID)
	}
	if changeNumber <= 0 {
		return fmt.Errorf("cannot determine change number for change %s", change.ChangeID)
	}

	// patchset 优先级：--truncate > 用户指定 patchset > Gerrit 最新 patchset > 1
	// 未指定 patchset 时取 Gerrit 返回的当前（最新）patchset，而非硬编码 1（避免取到旧补丁集）。
	latest := changeDetail.CurrentPatchSet.Number
	patchset := ""
	switch {
	case opts.Truncate:
		if latest > 0 {
			patchset = strconv.Itoa(latest)
		}
	case change.PatchID != "":
		patchset = change.PatchID
	case latest > 0:
		patchset = strconv.Itoa(latest)
	}
	if patchset == "" {
		patchset = "1"
	}
	if n, err := strconv.Atoi(patchset); err != nil || n <= 0 {
		return fmt.Errorf("invalid patchset %q for change %s", patchset, change.ChangeID)
	}

	ref := fmt.Sprintf("refs/changes/%02d/%d/%s", changeNumber%100, changeNumber, patchset)
	log.Debug("构建 change ref: %s (number=%d)", ref, changeNumber)

	// 获取远程名称
	remoteName := proj.RemoteName
	if remoteName == "" {
		remoteName = "origin"
	}

	// fetch change ref
	log.Info("Fetching %s from %s", ref, remoteName)
	if _, err := proj.GitRepo.RunCommand("fetch", remoteName, ref); err != nil {
		// 对齐上游：fetch failed时以 [<project>] change <n>/<patch> not found 形式提示
		log.Error("[%s] change %d/%s not found", proj.Name, changeNumber, patchset)
		return fmt.Errorf("[%s] change %d/%s not found: %w", proj.Name, changeNumber, patchset, err)
	}

	// 按模式应用变更
	if err := downloadApplyChange(proj, opts, change.ChangeID); err != nil {
		return err
	}
	return nil
}

// downloadApplyChange 按 download 模式把已 fetch 的 FETCH_HEAD 应用到工作树：
//   - --revert：必要时先创建/切换 -b 指定分支，再 git revert FETCH_HEAD。
//   - --cherry-pick：必要时先创建/切换分支，再 git cherry-pick [--ff] FETCH_HEAD。
//   - --ff-only：必要时先创建/切换分支，再 git merge --no-stat --ff-only FETCH_HEAD，
//     HEAD 保持附着在分支上（不 detach）。
//   - 默认：-b 时在 FETCH_HEAD 上创建/切换分支，否则 detached checkout。
func downloadApplyChange(proj *project.Project, opts *DownloadOptions, changeLabel string) error {
	log := logger.Global()

	// cherry-pick/revert/ff-only 共用“在当前位置建分支再应用”的上游语义；
	// 默认 checkout 模式在 FETCH_HEAD 上建分支，故不在此处理。
	if opts.Branch != "" && (opts.CherryPick || opts.Revert || opts.FFOnly) {
		if err := downloadStartBranch(proj, opts.Branch, ""); err != nil {
			return err
		}
	}

	switch {
	case opts.Revert:
		// --revert：git revert FETCH_HEAD。始终追加 --no-edit，--quiet 只控制日志，
		// 不能因为静默就打开交互式编辑器。
		revertArgs := []string{"revert", "--no-edit"}
		if opts.FFOnly {
			log.Warn("--ff-only 对 revert 不适用，已忽略")
		}
		revertArgs = append(revertArgs, "FETCH_HEAD")
		log.Info("Reverting FETCH_HEAD")
		if _, err := proj.GitRepo.RunCommand(revertArgs...); err != nil {
			return fmt.Errorf("failed to revert change: %w", err)
		}
		log.Info("Successfully reverted change %s in project %s", changeLabel, proj.Name)

	case opts.CherryPick:
		// --cherry-pick：git cherry-pick FETCH_HEAD
		cpArgs := []string{"cherry-pick"}
		if opts.FFOnly {
			cpArgs = append(cpArgs, "--ff")
		}
		cpArgs = append(cpArgs, "FETCH_HEAD")
		log.Info("Cherry-picking FETCH_HEAD")
		if _, err := proj.GitRepo.RunCommand(cpArgs...); err != nil {
			// 检查是否是空提交的情况
			statusOutput, statusErr := proj.GitRepo.RunCommand("status", "--porcelain")
			if statusErr == nil && len(statusOutput) == 0 {
				log.Warn("Cherry-pick resulted in empty commit, skipping")
				if _, skipErr := proj.GitRepo.RunCommand("cherry-pick", "--skip"); skipErr != nil {
					return fmt.Errorf("failed to cherry-pick change (empty commit): %w", err)
				}
				log.Info("Skipped empty cherry-pick for change %s", changeLabel)
			} else {
				return fmt.Errorf("failed to cherry-pick change: %w", err)
			}
		}
		log.Info("Successfully cherry-picked change %s to project %s", changeLabel, proj.Name)

	case opts.FFOnly:
		// --ff-only：对齐上游 _FastForward，git merge --no-stat --ff-only FETCH_HEAD
		log.Info("Fast-forward merging FETCH_HEAD")
		if _, err := proj.GitRepo.RunCommand("merge", "--no-stat", "--ff-only", "FETCH_HEAD"); err != nil {
			return fmt.Errorf("failed to fast-forward merge change: %w", err)
		}
		log.Info("Successfully fast-forward merged change %s in project %s", changeLabel, proj.Name)

	default:
		// 默认模式：detached-HEAD checkout（对齐上游 repo download）
		if opts.Branch != "" {
			if err := downloadStartBranch(proj, opts.Branch, "FETCH_HEAD"); err != nil {
				return err
			}
			log.Info("Created branch %s at %s in project %s", opts.Branch, changeLabel, proj.Name)
		} else {
			log.Info("Checking out FETCH_HEAD (detached)")
			if _, err := proj.GitRepo.RunCommand("checkout", "FETCH_HEAD"); err != nil {
				return fmt.Errorf("failed to checkout change: %w", err)
			}
			log.Info("Successfully checked out change %s (detached HEAD) in project %s", changeLabel, proj.Name)
		}
	}

	return nil
}

// downloadStartBranch 创建或切换到名为 name 的本地分支（对齐上游 StartBranch）：
// 分支已存在则直接 checkout；否则用 checkout -b <name> [<revision>] 创建。
// revision 为空时以清单 revision 映射的远端跟踪 ref 为基线（对齐上游
// StartBranch 的 GetRevisionId），而不是当前 HEAD。
func downloadStartBranch(proj *project.Project, name, revision string) error {
	if name == "" {
		return fmt.Errorf("branch name must not be empty")
	}
	if proj.GitRepo == nil || proj.GitRepo.Runner == nil {
		return fmt.Errorf("project %s has no usable git runner", proj.Name)
	}

	if out, err := proj.GitRepo.Runner.RunInDir(proj.GitRepo.Path, "symbolic-ref", "--short", "HEAD"); err == nil {
		if strings.TrimSpace(string(out)) == name {
			return nil
		}
	}

	if _, err := proj.GitRepo.Runner.RunInDir(proj.GitRepo.Path, "rev-parse", "--verify", "--quiet", "refs/heads/"+name); err == nil {
		if _, err := proj.GitRepo.RunCommand("checkout", name); err != nil {
			return fmt.Errorf("failed to checkout branch %s: %w", name, err)
		}
		return nil
	}

	if revision == "" {
		revision = downloadRevisionBase(proj)
	}
	args := []string{"checkout", "-b", name}
	if revision != "" {
		args = append(args, revision)
	}
	if _, err := proj.GitRepo.RunCommand(args...); err != nil {
		return fmt.Errorf("failed to create branch %s: %w", name, err)
	}
	// 新建分支后写入 branch.<name>.remote/merge（对齐上游 StartBranch），
	// 让后续 upload 能识别该分支的跟踪基线并上传。
	if err := downloadConfigureBranchTracking(proj, name); err != nil {
		return err
	}
	return nil
}

// downloadRevisionBase 返回清单 revision 对应的本地基线 ref：优先映射为
// 远端跟踪分支（refs/remotes/<remote>/X），而不是当前 HEAD。
//   - commit id 原样返回；
//   - refs/tags/、refs/remotes/ 及其他非 heads 的完整 ref 原样返回；
//   - 分支名（可带 refs/heads/）按 remote fetch refspec 映射。
//
// 映射语义复用 uploadMapMergeToLocal（与上游 Remote.ToLocal 一致）。
func downloadRevisionBase(proj *project.Project) string {
	if proj == nil {
		return ""
	}
	rev := strings.TrimSpace(proj.Revision)
	if rev == "" || downloadIsCommitID(rev) {
		return rev
	}
	if strings.HasPrefix(rev, "refs/tags/") || strings.HasPrefix(rev, "refs/remotes/") {
		return rev
	}
	if strings.HasPrefix(rev, "refs/") && !strings.HasPrefix(rev, "refs/heads/") {
		return rev
	}
	remote := downloadRemoteName(proj)
	// 形如 origin/main 的短远端分支名直接对应 refs/remotes/<remote>/<branch>。
	if strings.HasPrefix(rev, remote+"/") {
		return "refs/remotes/" + rev
	}
	if mapped := uploadMapMergeToLocal(proj, remote, rev); mapped != "" {
		return mapped
	}
	return rev
}

// downloadRemoteName 返回项目 remote 名（缺失时回退 origin）。
func downloadRemoteName(proj *project.Project) string {
	if proj == nil {
		return "origin"
	}
	if proj.RemoteName != "" {
		return proj.RemoteName
	}
	if proj.Remote != "" {
		return proj.Remote
	}
	return "origin"
}

// downloadConfigureBranchTracking 为新建的本地分支写入跟踪配置：
//   - branch.<name>.remote = 项目 remote（缺失时回退 origin）；
//   - branch.<name>.merge  = 清单 revision 表达式；非 refs/ 且非 commit id
//     时按上游语义补 refs/heads/ 前缀。revision 为空时不写 merge。
func downloadConfigureBranchTracking(proj *project.Project, name string) error {
	remoteName := downloadRemoteName(proj)
	if _, err := proj.GitRepo.RunCommand("config", "branch."+name+".remote", remoteName); err != nil {
		return fmt.Errorf("failed to set branch.%s.remote: %w", name, err)
	}

	merge := strings.TrimSpace(proj.Revision)
	if merge == "" {
		return nil
	}
	if !strings.HasPrefix(merge, "refs/") && !downloadIsCommitID(merge) {
		merge = "refs/heads/" + merge
	}
	if _, err := proj.GitRepo.RunCommand("config", "branch."+name+".merge", merge); err != nil {
		return fmt.Errorf("failed to set branch.%s.merge: %w", name, err)
	}
	return nil
}

// downloadIsCommitID 判断字符串是否为完整 commit id（40/64 位 hex）。
func downloadIsCommitID(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	for _, c := range value {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
			return false
		}
	}
	return true
}

// parseChangeNumber 尝试从 change 标识中解析数字 change number。
// 支持 "project~branch~<number>" 三元组前缀；Change-Id（I+40hex）无数字语义，返回 0。
func parseChangeNumber(changeID string) int {
	if idx := strings.LastIndex(changeID, "~"); idx >= 0 {
		changeID, _ = splitChangePatchset(changeID[idx+1:])
	} else {
		changeID, _ = splitChangePatchset(changeID)
	}
	if changeID == "" {
		return 0
	}
	n := 0
	for _, c := range changeID {
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int(c-'0')
	}
	return n
}

// findCwdProject 返回working directory落在其 worktree 内的项目。
// originalDir 须为绝对路径（EnsureRepoRoot 在 chdir 前返回的 cwd）。
// worktree 为相对路径时基于 repo 顶层目录解析，绝对路径直接比较。
// 匹配规则：originalDir 等于项目 worktree，或位于其下（祖先目录）。
// 无匹配返回 nil。
func findCwdProject(manager *project.Manager, originalDir string) *project.Project {
	if originalDir == "" {
		return nil
	}
	cwd := filepath.Clean(originalDir)
	sep := string(filepath.Separator)

	base := manager.TopDir
	if base == "" {
		if root, err := config.GetRepoRoot(); err == nil {
			base = root
		}
	}

	for _, p := range manager.GetProjects() {
		wt := p.Worktree
		if wt == "" {
			wt = p.Path
		}
		if wt == "" {
			continue
		}
		if !filepath.IsAbs(wt) {
			if base == "" {
				continue
			}
			wt = filepath.Join(base, wt)
		}
		wt = filepath.Clean(wt)
		if wt == cwd || strings.HasPrefix(cwd, wt+sep) {
			return p
		}
	}
	return nil
}

// GetChangeDetail 获取变更详细信息，包括修订版本信息。
//
// SSH 查询 Gerrit 的目标 host 优先取项目的 PushURL/ReviewURL（真正的推送/审查
// 端点），fetch 地址（RemoteURL）仅作最后兜底。contextProj 为执行 EnsureRepoRoot
// 前的 cwd 所在项目，可为 nil。
func (c *ChangeInfo) GetChangeDetail(mf *manifest.Manifest, contextProj *project.Project) (*ChangeInfo, error) {
	url := downloadGerritQueryURL(mf, contextProj)

	logger.Debug("获取变更详细信息: %s, 补丁ID: %s (push=%s)", c.ChangeID, c.PatchID, url)

	change, err := c.getChangeViaSSH(c.ChangeID, url)
	if err != nil {
		return nil, err
	}

	return change, nil
}

// downloadGerritQueryURL 选择 Gerrit SSH 查询地址，优先级：
//  1. 上下文项目的 PushURL；
//  2. 上下文项目的 ReviewURL；
//  3. default remote 的 pushurl（未配置则 fetch）；
//  4. default remote 的 review URL；
//  5. 上下文项目的 RemoteURL（fetch 地址，仅兜底）。
//
// 前三者可能为 https；getChangeViaSSH 无法解析时会回退默认 SSH 别名。
func downloadGerritQueryURL(mf *manifest.Manifest, contextProj *project.Project) string {
	if contextProj != nil {
		if contextProj.PushURL != "" {
			return contextProj.PushURL
		}
		if contextProj.ReviewURL != "" {
			return contextProj.ReviewURL
		}
	}

	if mf != nil {
		if pushURL, err := mf.GetRemotePushURL(mf.Default.Remote); err == nil && pushURL != "" {
			return pushURL
		}
		if review, err := mf.GetRemoteReview(mf.Default.Remote); err == nil && review != "" {
			return review
		}
	}

	if contextProj != nil {
		return contextProj.RemoteURL
	}
	return ""
}

// sshTarget 表示解析后的 SSH 连接目标
type sshTarget struct {
	host string
	port string
	user string
}

// parseSSHReviewURL 从 Gerrit review URL 解析出 SSH 连接目标。
// 支持 ssh://[user@]host[:port]/path 与 scp 形式 user@host:path。
// scp 形式中冒号后的内容只有在纯数字时才视为端口，否则是远端路径
// （不能把 user@host:path 的 path 当端口）。无法解析时返回 nil。
func parseSSHReviewURL(reviewURL string) *sshTarget {
	reviewURL = strings.TrimSpace(reviewURL)
	if reviewURL == "" {
		return nil
	}

	rest := reviewURL
	switch {
	case strings.HasPrefix(rest, "ssh://"):
		rest = strings.TrimPrefix(rest, "ssh://")
	case strings.Contains(rest, "://"):
		// 非 ssh scheme（如 https）无法用于 ssh gerrit
		return nil
	}
	// 去掉路径部分（ssh://host:port/path 或 user@host:port/path）
	if idx := strings.Index(rest, "/"); idx >= 0 {
		rest = rest[:idx]
	}
	if rest == "" {
		return nil
	}

	target := &sshTarget{port: "29418"} // Gerrit 默认 SSH 端口

	// user@host
	if at := strings.Index(rest, "@"); at >= 0 {
		target.user = rest[:at]
		rest = rest[at+1:]
	}

	if strings.HasPrefix(rest, "[") {
		// IPv6：[addr]:port
		if end := strings.Index(rest, "]"); end >= 0 {
			target.host = rest[:end+1]
			after := rest[end+1:]
			if strings.HasPrefix(after, ":") && downloadIsDigits(after[1:]) {
				target.port = after[1:]
			}
		}
	} else if colon := strings.Index(rest, ":"); colon >= 0 {
		target.host = rest[:colon]
		suffix := rest[colon+1:]
		// scp 形式 user@host:path：仅纯数字后缀是端口，path 不是端口。
		if downloadIsDigits(suffix) {
			target.port = suffix
		}
	} else {
		target.host = rest
	}

	if target.host == "" {
		return nil
	}
	return target
}

// buildSSHQueryArgs 构造通过 SSH 查询 Gerrit 的 ssh 参数（不含 "ssh" 本身）。
// host 仅用 user@host，端口通过 -p 单独传递；禁止把 :port 拼进 host，
// 否则 ssh 会把 "host:port" 当作非法主机名导致 DNS 解析failed（exit status 255）。
func buildSSHQueryArgs(target *sshTarget, changeID string) []string {
	dest := target.host
	if target.user != "" {
		dest = fmt.Sprintf("%s@%s", target.user, target.host)
	}
	subcmd := []string{"gerrit", "query", "--format=JSON", "--current-patch-set", changeID}
	if target.port != "" {
		return append([]string{"-p", target.port, dest}, subcmd...)
	}
	return append([]string{dest}, subcmd...)
}

// selectGerritChange 从 Gerrit query 的多行结果中选择唯一变更。
// 单结果直接返回；多结果时用 project/branch 过滤；仍无法唯一确定时返回歧义错误，
// 而不是静默取第一行。
func selectGerritChange(changes []ChangeInfo, projectName, branch string) (*ChangeInfo, error) {
	if len(changes) == 0 {
		return nil, fmt.Errorf("no change found")
	}
	if len(changes) == 1 {
		selected := changes[0]
		return &selected, nil
	}

	filtered := make([]ChangeInfo, 0, len(changes))
	for _, change := range changes {
		if projectName != "" && change.Project != projectName {
			continue
		}
		if branch != "" && change.Branch != branch {
			continue
		}
		filtered = append(filtered, change)
	}
	if len(filtered) == 1 {
		selected := filtered[0]
		return &selected, nil
	}
	if len(filtered) == 0 {
		return nil, fmt.Errorf("found %d changes but none match project=%q branch=%q", len(changes), projectName, branch)
	}

	descriptions := make([]string, 0, len(filtered))
	for _, change := range filtered {
		descriptions = append(descriptions, fmt.Sprintf("%s/%s (change %d)", change.Project, change.Branch, change.Number))
	}
	return nil, fmt.Errorf("change is ambiguous: %d matches (%s); specify project~branch~Change-Id",
		len(filtered), strings.Join(descriptions, ", "))
}

// parseGerritQueryOutput 解析 `ssh gerrit query --format=JSON` 的多行输出。
// 每个非空行是一个 JSON 对象，stats 行被忽略；随后由 selectGerritChange
// 用 project/branch 消解多结果歧义。
func parseGerritQueryOutput(output []byte, projectName, branch string) (*ChangeInfo, error) {
	lines := strings.Split(string(output), "\n")
	changes := make([]ChangeInfo, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// stats 行没有 change id，直接跳过（不当作变更）
		var envelope struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal([]byte(line), &envelope); err != nil {
			return nil, fmt.Errorf("解析SSH输出failed: %w", err)
		}
		if envelope.Type == "stats" {
			continue
		}
		var change ChangeInfo
		if err := json.Unmarshal([]byte(line), &change); err != nil {
			return nil, fmt.Errorf("解析SSH输出failed: %w", err)
		}
		if change.ChangeID == "" {
			continue
		}
		changes = append(changes, change)
	}
	return selectGerritChange(changes, projectName, branch)
}

// getChangeViaSSH 通过SSH协议获取变更信息
//
// sshURL 为 Gerrit SSH 目标地址（优先 push 地址，回退 review URL）；
// 为空或无法解析（如 https）时回退到默认 SSH 别名 "gerrit"。
func (c *ChangeInfo) getChangeViaSSH(changeID, sshURL string) (*ChangeInfo, error) {
	log := logger.Global()

	var args []string
	target := parseSSHReviewURL(sshURL)
	if target != nil {
		args = buildSSHQueryArgs(target, changeID)
		log.Debug("通过SSH查询Gerrit (url=%s): ssh %s", sshURL, strings.Join(args, " "))
	} else {
		// 回退到默认别名 gerrit（兼容旧配置/未配置可用 SSH 地址的环境）
		if sshURL == "" {
			log.Warn("未在清单中配置可用的 push/review SSH 地址，回退到默认 SSH 别名 'gerrit'；建议在 manifest remote 上设置 review 或 pushurl 属性")
		} else {
			log.Warn("无法从地址解析 SSH 目标 (%s)，回退到默认别名 'gerrit'", sshURL)
		}
		// ssh <alias> gerrit query ... ：第一个 gerrit 为 SSH host 别名，第二个为 gerrit 子命令
		args = []string{"gerrit", "gerrit", "query", "--format=JSON", "--current-patch-set", changeID}
		log.Debug("通过SSH查询Gerrit (fallback): ssh %s", strings.Join(args, " "))
	}

	// 使用带超时的 context 执行 SSH 查询（与 init 克隆的 5 分钟超时对齐）：
	// 无超时的情况下，网络不可达或 SSH 主机密钥确认提示会永久挂起 repo download
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ssh", args...)
	output, err := cmd.Output()
	if err != nil {
		// 提供更明确的错误信息，指导用户如何手动执行SSH命令
		return nil, fmt.Errorf("执行SSH命令failed: %w；请确认 manifest remote 已配置 pushurl/fetch 或 review，或手动执行: ssh %s", err, strings.Join(args, " "))
	}

	// Gerrit query 每行一个 JSON 对象（最后一行通常是 {"type":"stats",...}）。
	selected, err := parseGerritQueryOutput(output, c.Project, c.Branch)
	if err != nil {
		return nil, fmt.Errorf("查询变更 %s failed: %w", changeID, err)
	}
	return selected, nil
}
