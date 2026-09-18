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
	Branch     bool // -b/--branch：fetch 前先创建分支
	Truncate   bool // -t/--truncate：忽略用户给定 patchset，始终取最新 patchset
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
Use --cherry-pick (-c) to cherry-pick the change, or --revert (-r) to revert it.`,
		RunE: func(_ *cobra.Command, args []string) error {
			return runDownload(opts, args)
		},
	}

	// Add flags
	cmd.Flags().BoolVarP(&opts.CherryPick, "cherry-pick", "c", false, "download and cherry-pick specific changes")
	cmd.Flags().BoolVarP(&opts.Revert, "revert", "r", false, "download and revert specific changes")
	cmd.Flags().BoolVarP(&opts.FFOnly, "ff-only", "f", false, "only allow fast-forward when merging")
	cmd.Flags().BoolVarP(&opts.Branch, "branch", "b", false, "create a new branch first")
	cmd.Flags().BoolVarP(&opts.Truncate, "truncate", "t", false, "truncate to the latest patchset of the change")
	cmd.Flags().BoolVarP(&opts.Verbose, "verbose", "v", false, "show all output")
	cmd.Flags().BoolVarP(&opts.Quiet, "quiet", "q", false, "only show errors")
	cmd.Flags().IntVarP(&opts.Jobs, "jobs", "j", 8, "number of jobs to run in parallel")
	AddManifestFlags(cmd, &opts.CommonManifestOptions)

	return cmd
}

// loadDownloadConfig loads the configuration
func loadDownloadConfig() (*config.Config, error) {
	return config.Load()
}

// loadDownloadManifest loads the manifest file
func loadDownloadManifest(cfg *config.Config) (*manifest.Manifest, error) {
	parser := manifest.NewParser()
	return parser.ParseFromFile(cfg.ManifestName, manifest.SplitGroups(cfg.Groups))
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

// parseChangeArg parses a change argument in the format "<change-id>[/<patch-id>]"
func parseChangeArg(arg string) (*ChangeInfo, error) {

	parts := strings.Split(arg, "/")
	changeID := parts[0]
	patchID := ""
	if len(parts) > 1 {
		patchID = parts[1]
	}

	return &ChangeInfo{
		ChangeID: changeID,
		PatchID:  patchID,
	}, nil
}

// isChangeArg checks if an argument is a change ID
func isChangeArg(arg string) bool {
	// 检查是否是change-id格式
	changeIDPattern := regexp.MustCompile(`^([a-zA-Z0-9_-]+~[a-zA-Z0-9_-]+~)?I[0-9a-f]{40}(/\d+)?$`)
	if changeIDPattern.MatchString(arg) {
		return true
	}

	// 检查是否是数字格式的change number
	changeNumberPattern := regexp.MustCompile(`^\d+(/\d+)?$`)
	return changeNumberPattern.MatchString(arg)
}

// runDownload executes the download command
func runDownload(opts *DownloadOptions, args []string) error {
	// 初始化日志记录器
	log := logger.NewDefaultLogger()
	if opts.Verbose {
		log.SetLevel(logger.LogLevelDebug)
	} else if opts.Quiet {
		log.SetLevel(logger.LogLevelError)
	} else {
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

	// 加载清单
	log.Debug("Parsing manifest file")
	mf, err := loadDownloadManifest(cfg)
	if err != nil {
		log.Error("Failed to parse manifest: %v", err)
		return fmt.Errorf("failed to parse manifest: %w", err)
	}

	// 创建项目管理器
	log.Debug("Creating project manager")
	manager := project.NewManagerFromManifest(mf, cfg)

	// 分离项目名称和变更ID
	projectNames := []string{}
	changes := []*ChangeInfo{}

	for _, arg := range args {
		if isChangeArg(arg) {
			change, err := parseChangeArg(arg)
			if err != nil {
				log.Error("Failed to parse change argument: %v", err)
				return fmt.Errorf("failed to parse change argument: %w", err)
			}
			changes = append(changes, change)
		} else {
			projectNames = append(projectNames, arg)
		}
	}

	// repo download 必须指定 change；无 change 时不应退化为 fetch+merge（那是 sync 的语义）。
	if len(changes) == 0 {
		return fmt.Errorf("no change specified; usage: repo download [<project>...] <change>[/<patchset>] ... (see --help)")
	}
	return downloadAndCherryPickChanges(opts, mf, manager, changes, projectNames, originalDir)
}

// downloadAndCherryPickChanges downloads the specified changes and applies them.
//
// 模式（对齐上游 repo download）：
//   - 默认（无 -c/-r）：fetch 后 git checkout FETCH_HEAD（detached HEAD）。
//   - --cherry-pick/-c：fetch 后 git cherry-pick FETCH_HEAD。
//   - --revert/-r：fetch 后 git revert FETCH_HEAD。
//   - --ff-only/-f：cherry-pick/revert 时附加 --ff-only（merge 时 --ff-only）。
//
// ref 分片使用 Gerrit 数字 change number 的末 2 位（refs/changes/XX/<number>/<patchset>），
// 而非 Change-Id 的末 2 字符。
func downloadAndCherryPickChanges(opts *DownloadOptions, mf *manifest.Manifest, manager *project.Manager, changes []*ChangeInfo, _ []string, originalDir string) error {
	log := logger.Global()

	if len(changes) == 0 {
		return fmt.Errorf("no changes specified for download")
	}

	log.Debug("Downloading %d change(s)", len(changes))

	// 上游 repo download 支持多对 <project> <change>：依次获取并应用每个 change，
	// 任一failed即停止。每个 change 的所属项目由 Gerrit 查询结果决定。
	for _, change := range changes {
		if err := downloadOneChange(opts, mf, manager, change, originalDir); err != nil {
			return err
		}
	}
	return nil
}

// downloadOneChange 获取并应用单个 Gerrit change。
// originalDir 为执行 EnsureRepoRoot 前的 cwd（绝对路径），用于 Gerrit 返回的 project
// 不在 manifest 时回退到当前所在项目（对齐上游 repo download 的 cwd 回退行为）。
func downloadOneChange(opts *DownloadOptions, mf *manifest.Manifest, manager *project.Manager, change *ChangeInfo, originalDir string) error {
	log := logger.Global()

	log.Debug("Processing change %s (patch %s)", change.ChangeID, change.PatchID)

	// 上下文项目（cwd 项目）：既用于 SSH 查询 Gerrit 的 push 地址 host，
	// 也用于 Gerrit 返回的 project 不在 manifest 时的 cwd 回退（对齐上游 "Defaulting to cwd project"）。
	contextProj := findCwdProject(manager, originalDir)

	// 获取变更详细信息（通过 SSH 查询 Gerrit），得到数字 change number 与最新 patchset。
	// SSH 目标 host 取自 push 地址（优先 cwd 项目的 RemoteURL），端口 29418（见 GetChangeDetail）。
	changeDetail, err := change.GetChangeDetail(mf, contextProj)
	if err != nil {
		log.Error("Failed to get change details: %v", err)
		return fmt.Errorf("failed to get change details: %w", err)
	}

	// 解析 change 所属项目（对齐上游 repo download）：
	// 1) 优先用 Gerrit 返回的 project 在 manifest 中匹配；
	// 2) 匹配failed（change 所属仓库不在 manifest）时回退到 cwd 项目，
	//    并打印与上游一致的警告。
	proj := manager.GetProject(changeDetail.Project)
	if proj == nil {
		log.Warn("Failed to auto detect change project: Change not belong to any project: change: %s, repository: %s, branch: %s",
			change.ChangeID, changeDetail.Project, changeDetail.Branch)
		if contextProj == nil {
			return fmt.Errorf("cannot determine project for change %s: not in manifest and no cwd project", change.ChangeID)
		}
		log.Debug("Defaulting to cwd project %s", contextProj.Name)
		proj = contextProj
		change.Project = contextProj.Name
	} else {
		change.Project = changeDetail.Project
		log.Info("Change belongs to project: %s", change.Project)
	}

	// 构建 Gerrit change ref：refs/changes/XX/<number>/<patchset>
	// XX 为数字 change number 的末 2 位（Gerrit 约定）
	changeNumber := changeDetail.Number
	if changeNumber <= 0 {
		// 若服务器未返回数字 number，回退：尝试从 ChangeID 解析数字（若 ChangeID 本身是数字）
		log.Warn("服务器未返回 change number，回退使用 ChangeID: %s", change.ChangeID)
		changeNumber = parseChangeNumber(change.ChangeID)
	}
	// patchset 优先级：--truncate > 用户指定 patchset > Gerrit 最新 patchset > 1
	// 未指定 patchset 时取 Gerrit 返回的当前（最新）patchset，而非硬编码 1（避免取到旧补丁集）。
	patchset := change.PatchID
	if opts.Truncate || patchset == "" {
		if changeDetail.CurrentPatchSet.Number > 0 {
			patchset = strconv.Itoa(changeDetail.CurrentPatchSet.Number)
		} else if patchset == "" {
			patchset = "1"
		}
	}
	ref := fmt.Sprintf("refs/changes/%02d/%d/%s", changeNumber%100, changeNumber, patchset)
	log.Debug("构建 change ref: %s (number=%d)", ref, changeNumber)

	// 获取远程名称
	remoteName := proj.RemoteName

	// fetch change ref
	log.Info("Fetching %s from %s", ref, remoteName)
	if _, err := proj.GitRepo.RunCommand("fetch", remoteName, ref); err != nil {
		// 对齐上游：fetch failed时以 [<project>] change <n>/<patch> not found 形式提示
		log.Error("[%s] change %d/%s not found", proj.Name, changeNumber, patchset)
		return fmt.Errorf("[%s] change %d/%s not found: %w", proj.Name, changeNumber, patchset, err)
	}

	// 按模式应用变更
	switch {
	case opts.Revert:
		// --revert：git revert FETCH_HEAD
		revertArgs := []string{"revert"}
		if opts.FFOnly {
			log.Warn("--ff-only 对 revert 不适用，已忽略")
		}
		if !opts.Quiet {
			revertArgs = append(revertArgs, "--no-edit")
		}
		revertArgs = append(revertArgs, "FETCH_HEAD")
		log.Info("Reverting FETCH_HEAD")
		if _, err := proj.GitRepo.RunCommand(revertArgs...); err != nil {
			return fmt.Errorf("failed to revert change: %w", err)
		}
		log.Info("Successfully reverted change %s in project %s", change.ChangeID, change.Project)

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
				log.Info("Skipped empty cherry-pick for change %s", change.ChangeID)
			} else {
				return fmt.Errorf("failed to cherry-pick change: %w", err)
			}
		}
		log.Info("Successfully cherry-picked change %s to project %s", change.ChangeID, change.Project)

	default:
		// 默认模式：detached-HEAD checkout（对齐上游 repo download）
		// -b/--branch：先创建分支再 checkout（对齐上游 repo download -b）
		if opts.Branch {
			branchName := fmt.Sprintf("download_%d_%s", changeNumber, patchset)
			log.Info("Creating branch %s before checkout (-b)", branchName)
			if _, err := proj.GitRepo.RunCommand("checkout", "-b", branchName, "FETCH_HEAD"); err != nil {
				return fmt.Errorf("failed to create branch and checkout: %w", err)
			}
			log.Debug("Successfully created branch %s and checked out change %s in project %s", branchName, change.ChangeID, change.Project)
		} else {
			checkoutArgs := []string{"checkout"}
			if opts.FFOnly {
				log.Warn("--ff-only 对默认 checkout 模式不适用，已忽略")
			}
			checkoutArgs = append(checkoutArgs, "FETCH_HEAD")
			log.Info("Checking out FETCH_HEAD (detached)")
			if _, err := proj.GitRepo.RunCommand(checkoutArgs...); err != nil {
				return fmt.Errorf("failed to checkout change: %w", err)
			}
			log.Info("Successfully checked out change %s (detached HEAD) in project %s", change.ChangeID, change.Project)
		}
	}

	return nil
}

// parseChangeNumber 尝试从 change 标识中解析数字 change number。
// Change-Id（I+40hex）无数字语义，返回 0；纯数字标识直接返回其值。
func parseChangeNumber(changeID string) int {
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
// 匹配规则：originalDir 等于项目 worktree，或位于其下（祖先目录）。
// 用于 repo download 在 Gerrit 返回的 project 不在 manifest 时回退到当前所在项目。
// 无匹配返回 nil。
func findCwdProject(manager *project.Manager, originalDir string) *project.Project {
	if originalDir == "" {
		return nil
	}
	cwd := filepath.Clean(originalDir)
	sep := string(filepath.Separator)
	for _, p := range manager.GetProjects() {
		wt := filepath.Clean(p.Worktree)
		if wt == "" {
			continue
		}
		if wt == cwd || strings.HasPrefix(cwd, wt+sep) {
			return p
		}
	}
	return nil
}

// GetChangeDetail 获取变更详细信息，包括修订版本信息。
//
// SSH 查询 Gerrit 的目标 host 取自 push 地址（对齐上游 repo download：
// 取 push 地址的 host + 29418 端口）：
//  1. 优先用上下文项目（cwd 项目）的 push 地址（RemoteURL，已解析的 fetch/push URL）；
//  2. 回退到 default remote 的 pushurl（未配置则 fetch）；
//  3. 最后回退到 default remote 的 review URL（兼容仅配置 review 的旧 manifest）。
//
// 端口使用 Gerrit SSH 标准端口 29418（URL 未显式指定端口时由 parseSSHReviewURL 默认）。
// contextProj 为执行 EnsureRepoRoot 前的 cwd 所在项目，可为 nil。
func (c *ChangeInfo) GetChangeDetail(mf *manifest.Manifest, contextProj *project.Project) (*ChangeInfo, error) {
	url := ""

	// 优先用上下文项目（cwd）的 push 地址
	if contextProj != nil && contextProj.RemoteURL != "" {
		url = contextProj.RemoteURL
	}
	// 回退：default remote 的 push 地址（pushurl 优先，否则 fetch）
	if url == "" {
		if pushURL, err := mf.GetRemotePushURL(mf.Default.Remote); err == nil && pushURL != "" {
			url = pushURL
		}
	}
	// 最后回退：default remote 的 review URL（兼容旧 manifest）
	if url == "" {
		if review, err := mf.GetRemoteReview(mf.Default.Remote); err == nil {
			url = review
		}
	}

	logger.Debug("获取变更详细信息: %s, 补丁ID: %s (push=%s)", c.ChangeID, c.PatchID, url)

	change, err := c.getChangeViaSSH(c.ChangeID, url)
	if err != nil {
		return nil, err
	}

	return change, nil
}

// sshTarget 表示解析后的 SSH 连接目标
type sshTarget struct {
	host string
	port string
	user string
}

// parseSSHReviewURL 从 Gerrit review URL 解析出 SSH 连接目标。
// 支持 ssh://[user@]host[:port]/path 与 user@host:port 形式。
// 无法解析时返回 nil，调用方回退到默认别名。
func parseSSHReviewURL(reviewURL string) *sshTarget {
	reviewURL = strings.TrimSpace(reviewURL)
	if reviewURL == "" {
		return nil
	}

	// 提取 host[:port][user] 前的 scheme
	rest := reviewURL
	if strings.HasPrefix(rest, "ssh://") {
		rest = strings.TrimPrefix(rest, "ssh://")
	} else if strings.Contains(rest, "://") {
		// 非 ssh scheme（如 https）无法用于 ssh gerrit，返回 nil
		return nil
	}
	// 去掉路径部分
	if idx := strings.Index(rest, "/"); idx >= 0 {
		rest = rest[:idx]
	}

	target := &sshTarget{port: "29418"} // Gerrit 默认 SSH 端口

	// user@host:port
	if at := strings.Index(rest, "@"); at >= 0 {
		target.user = rest[:at]
		rest = rest[at+1:]
	}
	// host:port
	if colon := strings.LastIndex(rest, ":"); colon >= 0 {
		target.host = rest[:colon]
		target.port = rest[colon+1:]
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

	// 解析JSON输出
	lines := strings.Split(string(output), "\n")
	if len(lines) == 0 {
		return nil, fmt.Errorf("SSH命令没有输出")
	}

	// 第一行包含变更信息
	var change ChangeInfo
	if err := json.Unmarshal([]byte(lines[0]), &change); err != nil {
		return nil, fmt.Errorf("解析SSH输出failed: %w", err)
	}

	// 检查是否找到变更
	if change.ChangeID == "" {
		return nil, fmt.Errorf("未找到变更: %s", changeID)
	}

	return &change, nil
}
