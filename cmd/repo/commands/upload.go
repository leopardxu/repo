package commands

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"

	"github.com/leopardxu/repo-go/internal/config"
	"github.com/leopardxu/repo-go/internal/logger"
	"github.com/leopardxu/repo-go/internal/manifest"
	"github.com/leopardxu/repo-go/internal/project"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// UploadOptions 包含upload命令的选项
type UploadOptions struct {
	Branch          string
	CurrentBranch   bool
	NoCurrentBranch bool
	Draft           bool
	Force           bool
	DryRun          bool
	PushOption      []string
	Reviewers       []string // --re/--reviewers：可重复
	Ready           bool     // -r/--ready：标记为 ready for review（Gerrit %ready）
	Topic           string   // --topic：显式主题
	TopicBranch     bool     // -t/--topic-branch：用本地分支名作主题
	NoVerify        bool
	IgnoreHooks     bool // 忽略hooks
	Replace         bool // 替换现有change
	Private         bool
	Wip             bool
	Jobs            int
	Hashtags        string
	HashtagBranch   bool
	Labels          []string // -l/--label：可重复
	CC              string
	NoEmails        bool
	Destination     string
	Yes             bool
	NoCertChecks    bool
	Verbose         bool
	Quiet           bool
	CommonManifestOptions
	// 添加配置字段，避免重复加载
	Config *config.Config
	// Manifest 为本次解析出的清单，用于按分支 remote 解析 review/push 目标。
	Manifest *manifest.Manifest
}

// uploadStats 用于统计上传结果
type uploadStats struct {
	mu      sync.Mutex
	total   int
	success int
	failed  int
}

// increment 增加计数
func (s *uploadStats) increment(success bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.total++
	if success {
		s.success++
	} else {
		s.failed++
	}
}

// UploadCmd 返回upload命令
func UploadCmd() *cobra.Command {
	opts := &UploadOptions{}

	cmd := &cobra.Command{
		Use:   "upload [--re --cc] [<project>...]",
		Short: "Upload changes for code review",
		Long: `Upload changes to Gerrit code review system.

By default, changes are uploaded for review (ready for review).
The upload pushes to refs/for/<branch> for Gerrit review, not directly to the branch.

Use --wip to upload as Work In Progress, --draft for draft changes, or --private for private changes.
Specify reviewers with --re/--reviewers (repeatable) and CC with --cc. Use --ready (or -r) to mark a
change ready for review, clearing any WIP state.`,
		RunE: func(_ *cobra.Command, args []string) error {
			return runUpload(opts, args)
		},
	}

	// 添加命令行选项
	cmd.Flags().StringVarP(&opts.Branch, "branch", "b", "", "上传指定本地分支")
	cmd.Flags().StringVar(&opts.Branch, "br", "", "alias for --branch")
	cmd.Flags().BoolVarP(&opts.CurrentBranch, "current-branch", "c", false, "仅上传当前分支")
	cmd.Flags().BoolVar(&opts.NoCurrentBranch, "no-current-branch", false, "upload all git branches")
	cmd.Flags().BoolVarP(&opts.Draft, "draft", "d", false, "upload as WIP (deprecated alias of --wip)")
	cmd.Flags().BoolVarP(&opts.Force, "force", "f", false, "强制上传，即使没有变更")
	cmd.Flags().BoolVarP(&opts.DryRun, "dry-run", "n", false, "不实际上传，仅显示将要上传的内容")
	cmd.Flags().StringArrayVarP(&opts.PushOption, "push-option", "o", nil, "push option (may be given multiple times)")
	// --re/--reviewers：请求 reviewer，可重复（对齐上游 action="append"）。
	// --re 与 --reviewers 共享同一 pflag.Value，混用时也按追加语义累积。
	cmd.Flags().StringArrayVar(&opts.Reviewers, "reviewers", nil, "request reviews from these people (may be given multiple times)")
	if reviewersFlag := cmd.Flags().Lookup("reviewers"); reviewersFlag != nil {
		cmd.Flags().AddFlag(&pflag.Flag{
			Name:     "re",
			Usage:    "alias for --reviewers",
			Value:    reviewersFlag.Value,
			DefValue: reviewersFlag.DefValue,
		})
	}
	// -r/--ready：标记为 ready for review（Gerrit %ready）
	cmd.Flags().BoolVarP(&opts.Ready, "ready", "r", false, "mark change as ready for review (clears WIP)")
	// -t/--topic-branch：用本地分支名作 topic；--topic 显式指定（长选项）
	cmd.Flags().BoolVarP(&opts.TopicBranch, "topic-branch", "t", false, "set the topic to the local branch name")
	cmd.Flags().StringVar(&opts.Topic, "topic", "", "set topic for the change")
	cmd.Flags().BoolVar(&opts.NoVerify, "no-verify", false, "绕过上传前钩子")
	cmd.Flags().BoolVar(&opts.IgnoreHooks, "ignore-hooks", false, "忽略所有hooks")
	cmd.Flags().BoolVar(&opts.Replace, "replace", false, "替换现有的change")
	cmd.Flags().BoolVarP(&opts.Private, "private", "p", false, "上传为私有状态")
	cmd.Flags().BoolVarP(&opts.Wip, "wip", "w", false, "上传为进行中状态（WIP）")
	cmd.Flags().IntVarP(&opts.Jobs, "jobs", "j", runtime.NumCPU()*2, "并行运行的任务数量")
	cmd.Flags().StringVar(&opts.Hashtags, "hashtag", "", "添加标签（逗号分隔）到审查中")
	cmd.Flags().BoolVar(&opts.HashtagBranch, "hashtag-branch", false, "将本地分支名添加为标签")
	cmd.Flags().StringArrayVarP(&opts.Labels, "label", "l", nil, "add a label when uploading (may be given multiple times)")
	cmd.Flags().StringVar(&opts.CC, "cc", "", "同时发送邮件给这些邮箱地址")
	cmd.Flags().StringVarP(&opts.Destination, "destination", "D", "", "提交到此目标分支进行审查")
	cmd.Flags().BoolVar(&opts.NoEmails, "no-emails", false, "上传时不发送邮件")
	cmd.Flags().BoolVarP(&opts.Yes, "yes", "y", false, "对所有安全提示回答是")
	cmd.Flags().BoolVar(&opts.NoCertChecks, "no-cert-checks", false, "禁用SSL证书验证（不安全）")
	cmd.Flags().BoolVarP(&opts.Verbose, "verbose", "v", false, "显示详细输出，包括调试信息")
	cmd.Flags().BoolVarP(&opts.Quiet, "quiet", "q", false, "仅显示错误信息")
	cmd.Flags().StringVarP(&opts.Groups, "groups", "g", "", "restrict upload to projects in specified groups (comma-separated)")
	AddManifestFlags(cmd, &opts.CommonManifestOptions)

	return cmd
}

// runUpload 执行upload命令
func runUpload(opts *UploadOptions, args []string) error {
	// 创建日志记录
	log := logger.NewDefaultLogger()
	switch {
	case opts.Verbose:
		log.SetLevel(logger.LogLevelDebug)
	case opts.Quiet:
		log.SetLevel(logger.LogLevelError)
	default:
		log.SetLevel(logger.LogLevelInfo)
	}

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

	log.Info("开始上传代码变更进行审核")

	// 加载配置
	if opts.Config == nil {
		opts.Config, err = config.Load()
		if err != nil {
			log.Error("failed to load config: %v", err)
			return fmt.Errorf("failed to load config: %w", err)
		}
	}

	// 组过滤：显式 --groups 优先，否则使用配置的有效组（空 Groups 视为 default）。
	effectiveGroups := uploadEffectiveGroups(opts)

	// 加载清单
	parser := manifest.NewParser()
	manifestObj, err := parser.ParseFromFile(opts.Config.ManifestName, manifest.SplitGroups(effectiveGroups))
	if err != nil {
		log.Error("failed to parse manifest: %v", err)
		return fmt.Errorf("failed to parse manifest: %w", err)
	}
	opts.Manifest = manifestObj

	// 创建项目管理
	manager := project.NewManagerFromManifest(manifestObj, opts.Config)

	// 获取要处理的项目：无参=全部（上游 Command.GetProjects 语义），
	// 有参时支持名称、"."、相对/绝对路径。
	var projects []*project.Project
	if len(args) == 0 {
		log.Debug("未指定项目，将处理所有项目")
		projects = manager.GetProjects()
		// 对齐上游 GetProjects：无项目参数时跳过未 checkout 的项目
		existing := make([]*project.Project, 0, len(projects))
		for _, p := range projects {
			if _, statErr := os.Stat(p.Worktree); statErr == nil {
				existing = append(existing, p)
			}
		}
		projects = existing
	} else {
		log.Debug("将处理指定的项目: %v", args)
		projects, err = manager.GetProjectsByArgs(args, originalDir)
		if err != nil {
			log.Error("获取指定项目failed: %v", err)
			return fmt.Errorf("failed to get projects: %w", err)
		}
	}

	// -b/--branch：先按本地分支是否存在过滤项目。这里只做只读的 ref 检查，
	// 不 checkout 任何项目；不存在的项目直接跳过，全部都没有该分支时才报错。
	if opts.Branch != "" && !uploadCurrentOnly(opts) {
		branch := uploadShortBranchName(opts.Branch)
		if branch == "" {
			return fmt.Errorf("分支名不能为空")
		}
		present := uploadSelectProjectsWithBranch(projects, branch, log)
		if len(present) == 0 {
			return fmt.Errorf("没有任何项目包含本地分支 %s", branch)
		}
		projects = present
	}

	log.Info("共有 %d 个项目需要处理", len(projects))

	// 规范化并发数：-j <= 0 会导致 makechan panic，回退到默认 8（与其他命令一致）
	if opts.Jobs <= 0 {
		opts.Jobs = 8
	}

	// 预计算上传计划（只读枚举分支）：用于 -y 门禁与计划输出，并避免重复枚举。
	plans, planErrs := uploadBuildPlans(opts, projects, log)

	// 创建统计对象
	stats := &uploadStats{}
	preErrs := make([]error, 0, len(planErrs))
	for _, planErr := range planErrs {
		stats.increment(false)
		preErrs = append(preErrs, planErr)
	}

	if len(plans) == 0 {
		if len(preErrs) > 0 {
			return errors.Join(preErrs...)
		}
		log.Info("没有需要上传的分支")
		return nil
	}

	// 多项目/多分支且未 -y：输出计划并拒绝自动上传；单项目单分支保持自动。
	if err := uploadConfirmPlan(opts, plans, manifestObj.Default.DestBranch, os.Stdout); err != nil {
		return errors.Join(append(preErrs, err)...)
	}

	// 创建错误通道和工作通道
	errChan := make(chan error, len(plans))
	sem := make(chan struct{}, opts.Jobs)
	var wg sync.WaitGroup

	log.Info("开始并行处理项目，并发 %d", opts.Jobs)

	for _, plan := range plans {
		plan := plan
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()

			log.Debug("处理项目: %s", plan.project.Name)
			if err := uploadProjectBranches(opts, plan.project, plan.branches, manager.TopDir, manifestObj.Default.DestBranch, log, stats); err != nil {
				errChan <- err
			}
		}()
	}

	// 等待所有goroutine完成
	log.Debug("等待所有上传任务完成")
	wg.Wait()
	close(errChan)

	// 收集错误
	errs := make([]error, 0, len(preErrs)+len(errChan))
	errs = append(errs, preErrs...)
	for err := range errChan {
		errs = append(errs, err)
	}

	// 输出统计信息
	log.Info("上传完成，总计: %d, 成功: %d, failed: %d", stats.total, stats.success, stats.failed)

	if len(errs) > 0 {
		return errors.Join(errs...)
	}

	log.Info("所有项目上传成功完成")
	return nil
}

// uploadCurrentOnly 返回是否仅上传当前分支：
// -c/--current-branch 生效，--no-current-branch 显式覆盖（优先级更高）。
func uploadCurrentOnly(opts *UploadOptions) bool {
	return opts.CurrentBranch && !opts.NoCurrentBranch
}

// uploadEffectiveGroups 返回清单解析使用的组过滤值：显式 -g/--groups 优先，
// 未显式指定时使用 cfg.EffectiveGroups()（未配置回退 default）。
func uploadEffectiveGroups(opts *UploadOptions) string {
	if opts == nil {
		return "default"
	}
	if strings.TrimSpace(opts.Groups) != "" {
		return opts.Groups
	}
	if opts.Config == nil {
		return "default"
	}
	return opts.Config.EffectiveGroups()
}

// uploadProjectPlan 描述单个项目中本次要上传的本地分支。
type uploadProjectPlan struct {
	project  *project.Project
	branches []string
}

// uploadBuildPlans 只读枚举各项目的待上传分支，供 -y 门禁/计划输出和实际上传共用。
// 单个项目的枚举错误不阻断其他项目，通过 errs 返回。
func uploadBuildPlans(opts *UploadOptions, projects []*project.Project, log logger.Logger) ([]uploadProjectPlan, []error) {
	plans := make([]uploadProjectPlan, 0, len(projects))
	errs := make([]error, 0)
	for _, p := range projects {
		branches, err := uploadBranchesForProject(p, opts)
		if err != nil {
			errs = append(errs, fmt.Errorf("项目 %s: %w", p.Name, err))
			continue
		}
		if len(branches) == 0 {
			log.Debug("项目 %s 没有可上传的本地分支", p.Name)
			continue
		}
		plans = append(plans, uploadProjectPlan{project: p, branches: branches})
	}
	return plans, errs
}

// uploadPlanRequiresConfirmation 判断待上传的 (项目, 分支) 组合是否多于一个。
func uploadPlanRequiresConfirmation(plans []uploadProjectPlan) bool {
	total := 0
	for _, plan := range plans {
		total += len(plan.branches)
	}
	return total > 1
}

// uploadConfirmPlan 在没有 -y/--yes 且待上传组合多于一个时，输出计划并拒绝
// 自动上传；单项目单分支保持自动，--dry-run 不实际上传因而无需确认。
func uploadConfirmPlan(opts *UploadOptions, plans []uploadProjectPlan, defaultDestBranch string, w io.Writer) error {
	if opts.Yes || opts.DryRun || !uploadPlanRequiresConfirmation(plans) {
		return nil
	}
	uploadPrintPlan(w, plans, opts, defaultDestBranch)
	return fmt.Errorf("有多个项目或分支待上传；请使用 -y/--yes 确认，或用 --branch/--current-branch 限定单个分支")
}

// uploadPrintPlan 把多项目/多分支上传计划写到 w（结果 payload，-q 不吞）。
func uploadPrintPlan(w io.Writer, plans []uploadProjectPlan, opts *UploadOptions, defaultDestBranch string) {
	if len(plans) == 0 {
		return
	}
	fmt.Fprintln(w, "The following changes will be uploaded:")
	fmt.Fprintln(w, "Use -y/--yes to confirm.")
	for _, plan := range plans {
		path := plan.project.Relpath
		if path == "" {
			path = plan.project.Name
		}
		fmt.Fprintf(w, "project %s/:\n", path)
		remoteName := plan.project.RemoteName
		if remoteName == "" {
			remoteName = "origin"
		}
		for _, branch := range plan.branches {
			dest := uploadDestBranch(opts, plan.project, branch, remoteName, defaultDestBranch)
			fmt.Fprintf(w, "  branch %s to remote branch %s\n", branch, dest)
		}
	}
}

// uploadProject 处理单个项目的上传：确定要上传的分支，逐分支上传。
// -b/--branch 的 checkout 延后到 uploadOneBranch，仅在确实要上传时执行。
func uploadProject(opts *UploadOptions, p *project.Project, repoRoot, defaultDestBranch string, log logger.Logger, stats *uploadStats) error {
	branches, err := uploadBranchesForProject(p, opts)
	if err != nil {
		stats.increment(false)
		return err
	}
	return uploadProjectBranches(opts, p, branches, repoRoot, defaultDestBranch, log, stats)
}

// uploadProjectBranches 逐分支上传已枚举好的分支列表。
func uploadProjectBranches(opts *UploadOptions, p *project.Project, branches []string, repoRoot, defaultDestBranch string, log logger.Logger, stats *uploadStats) error {
	if len(branches) == 0 {
		log.Info("项目 %s 没有可上传的本地分支", p.Name)
		return nil
	}

	remoteName := p.RemoteName
	if remoteName == "" {
		remoteName = "origin"
	}

	errs := make([]error, 0, len(branches))
	for _, branch := range branches {
		if err := uploadOneBranch(opts, p, branch, remoteName, repoRoot, defaultDestBranch, log); err != nil {
			stats.increment(false)
			errs = append(errs, fmt.Errorf("项目 %s 分支 %s: %w", p.Name, branch, err))
			continue
		}
		stats.increment(true)
	}
	return errors.Join(errs...)
}

// uploadProbe 执行只读探测命令（预期可能失败，如分支无配置、detached HEAD），
// 直接走 Runner 避免 Repository.RunCommand 对预期失败输出 Warn 日志。
func uploadProbe(p *project.Project, args ...string) ([]byte, error) {
	if p.GitRepo == nil || p.GitRepo.Runner == nil {
		return nil, fmt.Errorf("项目 %s 没有可用的 git runner", p.Name)
	}
	return p.GitRepo.Runner.RunInDir(p.GitRepo.Path, args...)
}

// checkoutUploadBranch 切换到指定本地分支，返回恢复函数：
// 原 HEAD 在分支上时恢复原分支的真实短名；原为 detached HEAD 时恢复原 SHA。
func checkoutUploadBranch(p *project.Project, branch string, log logger.Logger) (func(), error) {
	// symbolic-ref HEAD 返回完整 ref（refs/heads/foo），剥掉 refs/heads/ 后
	// 得到真实短分支名用于恢复，避免 checkout 时被同名 tag 干扰。
	origBranch := ""
	if out, err := uploadProbe(p, "symbolic-ref", "HEAD"); err == nil {
		origBranch = uploadShortBranchName(string(out))
	}
	origSHA := ""
	if out, err := uploadProbe(p, "rev-parse", "--verify", "HEAD"); err == nil {
		origSHA = strings.TrimSpace(string(out))
	}

	if _, err := p.GitRepo.RunCommand("checkout", uploadShortBranchName(branch)); err != nil {
		return func() {}, fmt.Errorf("项目 %s 切换到分支 %s failed: %w", p.Name, branch, err)
	}

	restore := func() {
		target := origBranch
		if target == "" {
			target = origSHA
		}
		if target == "" {
			return
		}
		if _, err := p.GitRepo.RunCommand("checkout", target); err != nil {
			log.Warn("项目 %s 恢复原分支/提交 %s failed: %v", p.Name, target, err)
		}
	}
	return restore, nil
}

// uploadLocalBranchRef 返回本地分支的无歧义完整 ref（refs/heads/<name>）。
func uploadLocalBranchRef(branch string) string {
	return "refs/heads/" + uploadShortBranchName(branch)
}

// uploadShortBranchName 从完整 ref（如 symbolic-ref HEAD 的 refs/heads/foo）
// 或用户提供的分支名中提取真实短分支名。
// 使用 %(refname)/symbolic-ref 的完整 ref 再剥 refs/heads/ 前缀，避免存在
// 同名 tag 时 %(refname:short) 产出 "heads/foo" 这类歧义名。
func uploadShortBranchName(ref string) string {
	return strings.TrimPrefix(strings.TrimSpace(ref), "refs/heads/")
}

// uploadLocalBranchExists 判断本地是否存在指定分支（用完整 ref 校验，
// 不受同名 tag 影响）。
func uploadLocalBranchExists(p *project.Project, branch string) bool {
	ref := uploadLocalBranchRef(branch)
	if ref == "refs/heads/" {
		return false
	}
	_, err := uploadProbe(p, "rev-parse", "--verify", "--quiet", ref)
	return err == nil
}

// uploadRemoteURLs 返回指定 remote 的 review URL 与 push URL：
//   - remote 命中项目自身 remote 时，优先复用 manager 已解析的 Project 字段；
//   - 其他 remote 严格按清单 remote 定义解析；解析不到时返回空串，
//     绝不回退项目级字段（项目级字段仅属于项目自身 remote）。
//   - review 是 Gerrit 端点：只做规范化并拼接项目名，永不 urljoin；
//   - push 仅对点前缀相对地址（./、../）按清单 URL 做 urljoin。
func uploadRemoteURLs(opts *UploadOptions, p *project.Project, remoteName string) (reviewURL, pushURL string) {
	projectRemote := p.RemoteName
	if projectRemote == "" {
		projectRemote = "origin"
	}
	remoteName = strings.TrimSpace(remoteName)
	if remoteName == "" {
		remoteName = projectRemote
	}

	var cfg *config.Config
	var mf *manifest.Manifest
	if opts != nil {
		cfg = opts.Config
		mf = opts.Manifest
	}

	// 非项目 remote 只能由 manifest remote 定义解析。
	if remote := uploadFindManifestRemote(mf, remoteName); remote != nil {
		reviewURL = project.ResolveReviewURL(remote.Review, p.Name)
		pushURL = project.ResolvePushURL(cfg, remote.PushURL, remote.Fetch, p.Name)
	}

	// 项目自身 remote：manager 已解析的字段更权威（清单可能未定义该 remote）。
	if remoteName == projectRemote {
		if p.ReviewURL != "" {
			reviewURL = p.ReviewURL
		}
		if p.PushURL != "" {
			pushURL = p.PushURL
		}
	}
	return reviewURL, pushURL
}

// uploadFindManifestRemote 按 name 或 alias 查找清单 remote；未找到返回 nil。
func uploadFindManifestRemote(mf *manifest.Manifest, remoteName string) *manifest.Remote {
	if mf == nil || remoteName == "" {
		return nil
	}
	for i := range mf.Remotes {
		remote := &mf.Remotes[i]
		if remote.Name == remoteName || (remote.Alias != "" && remote.Alias == remoteName) {
			return remote
		}
	}
	return nil
}

// uploadReceivePackOption 返回针对 ssh review 端点的额外 push 参数。
// 对齐上游 UploadForReview：ssh:// 形态需要显式 gerrit receive-pack。
func uploadReceivePackOption(pushTarget string) []string {
	if strings.HasPrefix(pushTarget, "ssh://") {
		return []string{"--receive-pack=gerrit receive-pack"}
	}
	return nil
}

// uploadSelectProjectsWithBranch 过滤出本地存在指定分支的项目。
// 仅执行只读 ref 检查，不切换任何项目的工作区。
func uploadSelectProjectsWithBranch(projects []*project.Project, branch string, log logger.Logger) []*project.Project {
	present := make([]*project.Project, 0, len(projects))
	for _, p := range projects {
		if !uploadLocalBranchExists(p, branch) {
			log.Debug("跳过项目 %s：本地不存在分支 %s", p.Name, branch)
			continue
		}
		present = append(present, p)
	}
	return present
}

// uploadBranchesForProject 返回本次要上传的本地分支：
//   - -c/--current-branch：只上传当前分支；
//   - -b/--branch：只上传指定分支；
//   - 默认（含 --no-current-branch）：上传所有带跟踪分支的本地分支（对齐上游 GetUploadableBranches）。
func uploadBranchesForProject(p *project.Project, opts *UploadOptions) ([]string, error) {
	currentOnly := uploadCurrentOnly(opts)

	if currentOnly {
		current, err := uploadCurrentBranchName(p)
		if err != nil {
			return nil, fmt.Errorf("failed to get project %s 的当前分支failed: %w", p.Name, err)
		}
		if uploadBranchMergeRef(p, current) == "" {
			return nil, fmt.Errorf("项目 %s 当前分支 %s 未跟踪远程分支，无法上传", p.Name, current)
		}
		if uploadBranchPublishedAtTip(p, current) && !opts.Force {
			return nil, nil
		}
		return []string{current}, nil
	}

	if opts.Branch != "" {
		branch := uploadShortBranchName(opts.Branch)
		if branch == "" {
			return nil, fmt.Errorf("分支名不能为空")
		}
		// 项目没有该本地分支时跳过（由调用方在全部项目都缺失时统一报错），
		// 绝不能在这里 checkout 或报错中断其他项目。
		if !uploadLocalBranchExists(p, branch) {
			return nil, nil
		}
		if uploadBranchMergeRef(p, branch) == "" {
			return nil, fmt.Errorf("项目 %s 分支 %s 未跟踪远程分支，无法上传", p.Name, branch)
		}
		if uploadBranchPublishedAtTip(p, branch) && !opts.Force {
			return nil, nil
		}
		return []string{branch}, nil
	}

	// 使用 %(refname) 取完整 ref 再剥 refs/heads/ 前缀，得到真实短分支名。
	// %(refname:short) 在存在同名 tag 时会输出 "heads/foo"，导致后续
	// branch.<name>.merge 查不到、refs/heads/heads/foo 拼错。
	out, err := p.GitRepo.RunCommand("for-each-ref", "--format=%(refname)", "refs/heads")
	if err != nil {
		return nil, fmt.Errorf("列出项目 %s 的本地分支failed: %w", p.Name, err)
	}

	branches := make([]string, 0)
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		ref := strings.TrimSpace(line)
		if !strings.HasPrefix(ref, "refs/heads/") {
			continue
		}
		branch := uploadShortBranchName(ref)
		if branch == "" {
			continue
		}
		// 对齐上游 GetUploadableBranches：仅上传跟踪了远程分支的本地分支
		if uploadBranchMergeRef(p, branch) == "" {
			continue
		}
		// 已发布且 tip 未变化的本地分支无需再次上传；--force 显式覆盖该跳过
		// （否则 --ready 无法为已发布的 WIP change 清除 WIP 状态）。
		if uploadBranchPublishedAtTip(p, branch) && !opts.Force {
			continue
		}
		branches = append(branches, branch)
	}
	return branches, nil
}

// uploadCurrentBranchName 返回当前分支的真实短名；detached HEAD 时返回错误。
func uploadCurrentBranchName(p *project.Project) (string, error) {
	out, err := uploadProbe(p, "symbolic-ref", "HEAD")
	if err != nil {
		return "", fmt.Errorf("not currently on a branch: %w", err)
	}
	ref := strings.TrimSpace(string(out))
	if !strings.HasPrefix(ref, "refs/heads/") {
		return "", fmt.Errorf("not currently on a local branch: %s", ref)
	}
	branch := uploadShortBranchName(ref)
	if branch == "" {
		return "", fmt.Errorf("not currently on a branch")
	}
	return branch, nil
}

// uploadOneBranch 上传单个本地分支到 Gerrit 审查。
func uploadOneBranch(opts *UploadOptions, p *project.Project, branch, remoteName, repoRoot, defaultDestBranch string, log logger.Logger) error {
	// 对齐上游：push/rev-list 均使用 branch.<name>.remote（缺失时回退项目 remote），
	// 而不是统一使用项目 remote。
	branchBase := uploadResolveBranchBase(p, branch, remoteName)
	branchRemote := branchBase.Remote
	if branchRemote == "" {
		branchRemote = remoteName
	}

	// Gerrit 审查必须使用分支 remote 的 review URL；没有 review 配置时
	// 绝不能把 refs/for 推到普通 fetch/push 目标（那只会上传真实分支）。
	reviewURL, _ := uploadRemoteURLs(opts, p, branchRemote)
	if reviewURL == "" {
		return fmt.Errorf("remote %s has no review url", branchRemote)
	}

	hasChanges, err := uploadBranchHasChanges(p, branchRemote, branch)
	if err != nil {
		return err
	}
	if !hasChanges && !opts.Force {
		log.Info("跳过项目 %s 分支 %s (没有变更需要上传)", p.Name, branch)
		return nil
	}

	// 项目声明了 dest-branch 且用户未显式 --destination 时，校验本地分支的
	// merge 目标是否与项目 revision/dest-branch 一致（对齐上游 _UploadBranch）：
	// 不一致往往是误操作，警告并跳过，而不是把 change 推到意外的目标分支。
	if opts.Destination == "" && uploadDestBranchMismatch(p, branch) {
		log.Warn("项目 %s 分支 %s 的 merge 目标 %s 与 dest-branch %s / revision %s 不一致，跳过上传；如确属有意请使用 --destination %s",
			p.Name, branch, uploadBranchMergeRef(p, branch), p.DestBranch, p.Revision, p.DestBranch)
		return nil
	}

	// 确定目标分支（用于 Gerrit 审查）。对齐上游 UploadForReview 的回退链：
	// --destination > manifest 项目 dest-branch > default dest-branch >
	// 本地分支跟踪的 merge 目标（branch.<name>.merge，经其 remote 映射）。
	destBranch := uploadDestBranch(opts, p, branch, branchRemote, defaultDestBranch)
	if destBranch == "" {
		return fmt.Errorf("无法确定项目 %s 分支 %s 的目标分支", p.Name, branch)
	}

	// -b/--branch 只在存在 pre-upload hook（需要 cwd 看到目标分支）时 checkout；
	// push 本身使用完整 refspec，不依赖工作区状态。--dry-run 绝不切换工作区。
	if uploadNeedsHookCheckout(opts, repoRoot) {
		restore, err := checkoutUploadBranch(p, branch, log)
		if err != nil {
			// checkout 失败不阻断上传：仍按 refs/heads/<branch> 推送。
			log.Warn("项目 %s 切换到分支 %s failed，继续按 ref 上传: %v", p.Name, branch, err)
		} else {
			defer restore()
		}
	}

	// 每个分支独立计算待上传 commit 列表
	commits := uploadBranchCommits(p, branchRemote, branch)

	// --no-verify：完全跳过 .repo/hooks；--ignore-hooks：运行 hook 但失败仅告警，
	// 不阻断上传。
	if !opts.NoVerify {
		if err := runUploadHook(p, repoRoot, commits, log); err != nil {
			if opts.IgnoreHooks {
				log.Warn("项目 %s 分支 %s 的 pre-upload hook failed，已按 --ignore-hooks 继续: %v", p.Name, branch, err)
			} else {
				return fmt.Errorf("upload hook failed: %w", err)
			}
		}
	}

	gerritOptions, err := buildUploadGerritOptions(opts, branch)
	if err != nil {
		return err
	}
	wip := (opts.Wip || opts.Draft) && !opts.Ready

	gerritRef := fmt.Sprintf("refs/for/%s", destBranch)
	// 每个分支独立 refspec：refs/heads/<branch>:refs/for/<dest>%opt1%opt2
	refspec := fmt.Sprintf("refs/heads/%s:%s", branch, gerritRef)
	if len(gerritOptions) > 0 {
		refspec = refspec + "%" + strings.Join(gerritOptions, ",")
	}

	pushArgs := uploadBuildPushArgs(opts, reviewURL, refspec)
	if opts.NoCertChecks {
		log.Debug("项目 %s：--no-cert-checks 已禁用 HTTPS 证书验证", p.Name)
	}

	log.Info("正在上传项目 %s 分支 %s 的变更到 Gerrit 审查系统 (%s -> %s)", p.Name, branch, reviewURL, gerritRef)
	if wip {
		log.Info("将创建 WIP (进行中) 状态的审查")
	}

	if opts.DryRun {
		// dry-run 计划属于结果 payload，直接写 stdout（-q 不得吞掉）。
		fmt.Fprintf(os.Stdout, "Would upload %s branch %s to %s\n", p.Name, branch, gerritRef)
		fmt.Fprintf(os.Stdout, "  git %s\n", strings.Join(pushArgs, " "))
		log.Debug("模拟运行: 项目 %s 分支 %s，命令: git %s", p.Name, branch, strings.Join(pushArgs, " "))
		return nil
	}

	outputBytes, stderrBytes, err := p.GitRepo.RunCommandWithStderr(pushArgs...)
	if err != nil {
		// GitCommandError.Error() 已包含 stderr（回退 stdout），无需重复附加。
		return fmt.Errorf("上传变更failed: %w", err)
	}

	// 记录本次已发布状态：tips 相同时后续上传直接跳过。
	if err := uploadWritePublishedRef(p, branch, branchRemote, destBranch); err != nil {
		return err
	}

	log.Info("成功上传项目 %s 分支 %s 的变更", p.Name, branch)
	if output := strings.TrimSpace(string(outputBytes)); output != "" {
		log.Debug("上传输出:\n%s", output)
	}
	// Gerrit 把 review URL 写在 push 的 stderr；作为结果 payload 输出到 stdout。
	uploadWriteReviewURLs(os.Stdout, stderrBytes)
	return nil
}

// uploadBuildPushArgs 构造 git push 参数：review 目标 + refspec，恒加
// --no-follow-tags（避免 push.followTags 把 tag 推到 Gerrit）；
// --quiet 时追加 --no-progress。
func uploadBuildPushArgs(opts *UploadOptions, pushTarget, refspec string) []string {
	args := make([]string, 0, 8)
	if opts.NoCertChecks {
		args = append(args, "-c", "http.sslVerify=false")
	}
	args = append(args, "push")
	if opts.Quiet {
		args = append(args, "--no-progress")
	}
	// ssh review 端点（对齐上游）：Gerrit 需要 gerrit receive-pack。
	args = append(args, uploadReceivePackOption(pushTarget)...)
	args = append(args, "--no-follow-tags")
	args = append(args, pushTarget, refspec)
	return args
}

// uploadWritePublishedRef 上传成功后写入 refs/published/<branch> = refs/heads/<branch>，
// 对齐上游 UploadForReview 的 UpdateRef 行为。
func uploadWritePublishedRef(p *project.Project, branch, remoteName, destBranch string) error {
	msg := fmt.Sprintf("posted to %s for %s", remoteName, destBranch)
	ref := "refs/published/" + branch
	if _, err := p.GitRepo.RunCommand("update-ref", "-m", msg, ref, "refs/heads/"+branch); err != nil {
		return fmt.Errorf("上传成功但写入 %s failed: %w", ref, err)
	}
	return nil
}

// uploadGerritURLPattern 匹配 Gerrit push 输出中的 review URL
// （对齐上游 _FindGerritUrls 的正则）。
var uploadGerritURLPattern = regexp.MustCompile(`https?://[^/\s]+/c/\S+?/\+/\d+`)

// uploadFindGerritURLs 从 git push 的 stderr 中提取 Gerrit review URL。
func uploadFindGerritURLs(stderr string) []string {
	urls := make([]string, 0)
	for _, match := range uploadGerritURLPattern.FindAllString(stderr, -1) {
		urls = append(urls, match)
	}
	return urls
}

// uploadWriteReviewURLs 把 stderr 中的 review URL 写到 w（结果 payload，
// 调用方传 os.Stdout；-q 不得吞掉）。
func uploadWriteReviewURLs(w io.Writer, stderr []byte) {
	for _, url := range uploadFindGerritURLs(string(stderr)) {
		fmt.Fprintln(w, url)
	}
}

// uploadBranchPublishedAtTip 判断 refs/published/<branch> 是否已等于分支 tip。
// 读取失败（未发布）时返回 false。
func uploadBranchPublishedAtTip(p *project.Project, branch string) bool {
	published, err := uploadProbe(p, "rev-parse", "--verify", "--quiet", "refs/published/"+branch)
	if err != nil {
		return false
	}
	tip, err := uploadProbe(p, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch)
	if err != nil {
		return false
	}
	publishedSHA := strings.TrimSpace(string(published))
	tipSHA := strings.TrimSpace(string(tip))
	return publishedSHA != "" && publishedSHA == tipSHA
}

// uploadNeedsHookCheckout 判断 -b 是否需要在运行 hook 前切换工作区：
// 仅当存在 pre-upload hook、hook 未被 --no-verify 跳过、且不是 dry-run 时。
func uploadNeedsHookCheckout(opts *UploadOptions, repoRoot string) bool {
	if opts.DryRun || opts.NoVerify || opts.Branch == "" || uploadCurrentOnly(opts) {
		return false
	}
	return uploadPreUploadHookPath(repoRoot) != ""
}

// buildUploadGerritOptions 构造 Gerrit push options（附加到 refs/for/<branch> 后，
// 以 % 分隔、逗号连接）。--ready 显式发送 ready（清除 WIP），不能只去掉 wip。
func buildUploadGerritOptions(opts *UploadOptions, branch string) ([]string, error) {
	options := make([]string, 0)

	wip := opts.Wip || opts.Draft
	switch {
	case opts.Ready:
		wip = false
		options = append(options, "ready")
	case wip:
		options = append(options, "wip")
	}
	if wip && opts.Private {
		return nil, fmt.Errorf("--wip/--draft 与 --private 互斥")
	}
	if opts.Private {
		options = append(options, "private")
	}
	if opts.Replace {
		options = append(options, "replace")
	}
	// --topic 显式优先；-t/--topic-branch 在未指定 --topic 时用本地分支名。
	topic := opts.Topic
	if topic == "" && opts.TopicBranch {
		topic = branch
	}
	if topic != "" {
		options = append(options, "topic="+topic)
	}
	for _, tag := range uploadSplitCommaList(opts.Hashtags) {
		options = append(options, "hashtag="+tag)
	}
	if opts.HashtagBranch {
		options = append(options, "hashtag="+branch)
	}
	for _, label := range opts.Labels {
		for _, item := range uploadSplitCommaList(label) {
			options = append(options, "label="+item)
		}
	}
	for _, reviewer := range opts.Reviewers {
		for _, item := range uploadSplitCommaList(reviewer) {
			options = append(options, "r="+item)
		}
	}
	for _, cc := range uploadSplitCommaList(opts.CC) {
		options = append(options, "cc="+cc)
	}
	if opts.NoEmails {
		options = append(options, "notify=NONE")
	}
	// 自定义 push-option（可重复）
	options = append(options, opts.PushOption...)
	return options, nil
}

// uploadBranchBase 描述本地分支的跟踪基线。
//   - Remote: branch.<name>.remote（缺失时回退项目 remote）
//   - Merge:  原始 branch.<name>.merge（如 refs/heads/main）
//   - Ref:    Merge 按该 remote 的 fetch refspec 映射后的本地 ref
//     （如 refs/remotes/origin/main）
type uploadBranchBase struct {
	Remote string
	Merge  string
	Ref    string
}

// uploadBranchHasChanges 判断指定本地分支是否有需要推送的变更（对齐 HasChangesToPush，
// 但按分支独立计算，而非依赖当前 HEAD）。基线为 branch.<name>.merge 经
// branch.<name>.remote 映射后的本地 ref（对齐上游 Branch.LocalMerge）。
// 配置了基线但本地不存在时返回错误，绝不静默视为无变更。
func uploadBranchHasChanges(p *project.Project, remote, branch string) (bool, error) {
	// 已发布且 tip 相同时没有待上传内容，避免重复 upload。
	if uploadBranchPublishedAtTip(p, branch) {
		return false, nil
	}

	base := uploadResolveBranchBase(p, branch, remote)
	if base.Ref != "" {
		if _, err := uploadProbe(p, "rev-parse", "--verify", "--quiet", base.Ref); err != nil {
			return false, fmt.Errorf("项目 %s 分支 %s 的基线 %s 在本地不存在（请先 fetch remote %s）: %w",
				p.Name, branch, base.Ref, base.Remote, err)
		}
		out, err := p.GitRepo.RunCommand("rev-list", "--count", base.Ref+"..refs/heads/"+branch)
		if err != nil {
			return false, fmt.Errorf("检查项目 %s 分支 %s 是否有变更failed: %w", p.Name, branch, err)
		}
		count := strings.TrimSpace(string(out))
		return count != "0" && count != "", nil
	}

	// 未配置跟踪基线时回退到远端同名分支。
	remoteRef := "refs/remotes/" + remote + "/" + branch
	if _, err := uploadProbe(p, "rev-parse", "--verify", "--quiet", remoteRef); err != nil {
		// 远程分支不存在，说明是新分支，有需要推送的内容
		return true, nil
	}
	out, err := p.GitRepo.RunCommand("rev-list", "--count", "refs/heads/"+branch, "^"+remoteRef)
	if err != nil {
		return false, fmt.Errorf("检查项目 %s 分支 %s 是否有变更failed: %w", p.Name, branch, err)
	}
	count := strings.TrimSpace(string(out))
	return count != "0" && count != "", nil
}

// uploadBranchRemote 返回本地分支配置的跟踪 remote（branch.<name>.remote），
// 未配置时返回空串。
func uploadBranchRemote(p *project.Project, branch string) string {
	out, err := uploadProbe(p, "config", "--get", "branch."+branch+".remote")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// uploadBranchMergeRef 返回本地分支跟踪的远端 merge ref（branch.<name>.merge），
// 未配置时返回空串。返回值保留 refs/heads/ 前缀。
func uploadBranchMergeRef(p *project.Project, branch string) string {
	out, err := uploadProbe(p, "config", "--get", "branch."+branch+".merge")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// uploadResolveBranchBase 解析分支的跟踪基线，并把 branch.<name>.merge 按
// branch.<name>.remote 的 fetch refspec 映射为本地 ref。
func uploadResolveBranchBase(p *project.Project, branch, fallbackRemote string) uploadBranchBase {
	base := uploadBranchBase{
		Remote: uploadBranchRemote(p, branch),
		Merge:  uploadBranchMergeRef(p, branch),
	}
	if base.Remote == "" {
		base.Remote = fallbackRemote
	}
	if base.Merge == "" {
		return base
	}
	base.Ref = uploadMapMergeToLocal(p, base.Remote, base.Merge)
	return base
}

// uploadMapMergeToLocal 把远端分支 ref 映射为本地 ref（对齐上游 Remote.ToLocal）：
// 优先按 remote.<name>.fetch 的 refspec 映射，缺失时回退标准布局
// refs/remotes/<remote>/<branch>。remote 为 "." 时保持 ref 不变。
func uploadMapMergeToLocal(p *project.Project, remote, merge string) string {
	if merge == "" {
		return ""
	}
	if remote == "" || remote == "." {
		return merge
	}
	if uploadIsCommitID(merge) {
		return merge
	}

	ref := merge
	if !strings.HasPrefix(ref, "refs/") {
		ref = "refs/heads/" + ref
	}

	if out, err := uploadProbe(p, "config", "--get-all", "remote."+remote+".fetch"); err == nil {
		for _, spec := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			if dest, ok := uploadMapFetchSpec(strings.TrimSpace(spec), ref); ok {
				return dest
			}
		}
	}

	// 标准布局回退：refs/heads/X -> refs/remotes/<remote>/X
	return "refs/remotes/" + remote + "/" + strings.TrimPrefix(ref, "refs/heads/")
}

// uploadMapFetchSpec 把 refspec（[+]<src>:<dst>）应用到 ref，返回映射后的 ref。
func uploadMapFetchSpec(spec, ref string) (string, bool) {
	spec = strings.TrimPrefix(spec, "+")
	src, dst, ok := strings.Cut(spec, ":")
	if !ok || src == "" || dst == "" {
		return "", false
	}
	if src == ref {
		return dst, true
	}
	wildcard := strings.Index(src, "*")
	if wildcard < 0 {
		return "", false
	}
	prefix := src[:wildcard]
	suffix := src[wildcard+1:]
	if len(ref) < len(prefix)+len(suffix) || !strings.HasPrefix(ref, prefix) || !strings.HasSuffix(ref, suffix) {
		return "", false
	}
	middle := ref[len(prefix) : len(ref)-len(suffix)]
	if destWildcard := strings.Index(dst, "*"); destWildcard >= 0 {
		return dst[:destWildcard] + middle + dst[destWildcard+1:], true
	}
	return "", false
}

// uploadIsCommitID 判断字符串是否为完整 commit id（40/64 位 hex）。
func uploadIsCommitID(value string) bool {
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

// uploadBranchBaseDest 从映射后的基线推导 Gerrit 目标分支名：
// refs/remotes/<remote>/X -> X，refs/heads/X -> X；自定义 refspec 无法反推时
// 回退到 branch.<name>.merge 的分支名。
func uploadBranchBaseDest(base uploadBranchBase) string {
	if base.Ref != "" {
		if base.Remote != "" && base.Remote != "." {
			prefix := "refs/remotes/" + base.Remote + "/"
			if strings.HasPrefix(base.Ref, prefix) {
				return strings.TrimPrefix(base.Ref, prefix)
			}
		}
		if strings.HasPrefix(base.Ref, "refs/heads/") {
			return strings.TrimPrefix(base.Ref, "refs/heads/")
		}
	}
	return strings.TrimPrefix(base.Merge, "refs/heads/")
}

// uploadDestBranch 计算分支的 Gerrit 目标分支（回退链对齐上游 UploadForReview）：
// --destination > 项目 dest-branch > default dest-branch > 本地分支 merge 目标。
func uploadDestBranch(opts *UploadOptions, p *project.Project, branch, fallbackRemote, defaultDestBranch string) string {
	dest := opts.Destination
	if dest == "" {
		dest = p.DestBranch
	}
	if dest == "" {
		dest = defaultDestBranch
	}
	if dest == "" {
		dest = uploadBranchBaseDest(uploadResolveBranchBase(p, branch, fallbackRemote))
	}
	return strings.TrimPrefix(dest, "refs/heads/")
}

// uploadDestBranchMismatch 判断本地分支的 merge 目标是否与项目 revision 和
// dest-branch 都不一致（对齐上游 _UploadBranch 的安全检查）。
// 项目未声明 dest-branch 或分支未配置 merge 时不判定为不一致。
func uploadDestBranchMismatch(p *project.Project, branch string) bool {
	if p.DestBranch == "" {
		return false
	}
	merge := uploadBranchMergeRef(p, branch)
	if merge == "" {
		return false
	}
	fullDest := uploadFullHeadsRef(p.DestBranch)
	fullRevision := uploadFullHeadsRef(p.Revision)
	return merge != fullRevision && merge != fullDest
}

// uploadFullHeadsRef 把分支名补齐为 refs/heads/ 完整 ref（已完整时原样返回）。
func uploadFullHeadsRef(ref string) string {
	if strings.HasPrefix(ref, "refs/heads/") {
		return ref
	}
	return "refs/heads/" + ref
}

// uploadBranchCommits 返回本地分支相对其映射后跟踪基线的 commit SHA 列表。
func uploadBranchCommits(p *project.Project, remote, branch string) []string {
	base := uploadResolveBranchBase(p, branch, remote)
	revRange := "refs/remotes/" + remote + "/" + branch + "..refs/heads/" + branch
	if base.Ref != "" {
		revRange = base.Ref + "..refs/heads/" + branch
	}
	out, err := p.GitRepo.RunCommand("rev-list", revRange)
	if err != nil {
		return []string{}
	}
	commits := make([]string, 0)
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if sha := strings.TrimSpace(line); sha != "" {
			commits = append(commits, sha)
		}
	}
	return commits
}

// uploadSplitCommaList 拆分逗号分隔列表并去除空白项。
func uploadSplitCommaList(value string) []string {
	items := make([]string, 0)
	if value == "" {
		return items
	}
	for _, item := range strings.Split(value, ",") {
		if trimmed := strings.TrimSpace(item); trimmed != "" {
			items = append(items, trimmed)
		}
	}
	return items
}

// runUploadHook 在上传前运行 .repo/hooks/pre-upload（若存在）。
// hookPath 基于 repo 根目录拼接为绝对路径，避免 cmd.Dir 指向项目目录时相对
// 路径被解析到项目内导致 hook 找不到。commit SHA 列表作为位置参数传入。
func runUploadHook(p *project.Project, repoRoot string, commits []string, log logger.Logger) error {
	hookPath := uploadPreUploadHookPath(repoRoot)
	if hookPath == "" {
		// hook 不存在，跳过
		return nil
	}
	log.Debug("运行 upload hook: %s (项目 %s)", hookPath, p.Name)

	cmd := exec.Command(hookPath, commits...)
	cmd.Dir = uploadWorktreeAbs(repoRoot, p)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = uploadHookEnv(p)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("pre-upload hook failed: %w", err)
	}
	return nil
}

// uploadPreUploadHookPath 返回 .repo/hooks/pre-upload 的绝对路径；
// 文件不存在时返回空串。
func uploadPreUploadHookPath(repoRoot string) string {
	if repoRoot == "" {
		if root, err := config.GetRepoRoot(); err == nil {
			repoRoot = root
		}
	}
	if repoRoot == "" {
		return ""
	}
	hookPath := filepath.Join(repoRoot, ".repo", "hooks", "pre-upload")
	if _, err := os.Stat(hookPath); err != nil {
		return ""
	}
	if abs, err := filepath.Abs(hookPath); err == nil {
		hookPath = abs
	}
	return hookPath
}

// uploadWorktreeAbs 返回项目工作树的绝对路径（相对路径基于 repo 根目录解析）。
func uploadWorktreeAbs(repoRoot string, p *project.Project) string {
	wt := p.Worktree
	if wt == "" {
		wt = p.Gitdir
	}
	if wt == "" {
		wt = p.Path
	}
	if wt == "" {
		return repoRoot
	}
	if filepath.IsAbs(wt) {
		return filepath.Clean(wt)
	}
	if repoRoot == "" {
		return wt
	}
	return filepath.Clean(filepath.Join(repoRoot, wt))
}

// uploadHookEnv 构建 pre-upload hook 的环境变量（与 forall 的 REPO_* 约定一致）。
func uploadHookEnv(p *project.Project) []string {
	env := os.Environ()

	remoteName := p.RemoteName
	if remoteName == "" {
		remoteName = "origin"
	}
	outPwd := p.Worktree
	if outPwd == "" {
		outPwd = p.Gitdir
	}
	if abs, err := filepath.Abs(outPwd); err == nil {
		outPwd = abs
	}

	return append(env,
		"REPO_PATH="+p.Relpath,
		"REPO_PROJECT="+p.Name,
		"REPO_REMOTE="+remoteName,
		"REPO_LREV="+resolveForallLREV(p),
		"REPO_RREV="+p.Revision,
		"REPO_UPSTREAM="+p.Upstream,
		"REPO_DEST_BRANCH="+p.DestBranch,
		"REPO_OUT_PWD="+outPwd,
	)
}
