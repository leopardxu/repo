package commands

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/leopardxu/repo-go/internal/config"
	"github.com/leopardxu/repo-go/internal/logger"
	"github.com/leopardxu/repo-go/internal/manifest"
	"github.com/leopardxu/repo-go/internal/project"
	"github.com/spf13/cobra"
)

// PruneOptions 包含prune命令的选项
type PruneOptions struct {
	CommonManifestOptions
	Force   bool
	DryRun  bool
	Verbose bool
	Quiet   bool
	Jobs    int
}

// pruneStats 用于统计prune命令的执行结果
type pruneStats struct {
	mu      sync.Mutex
	success int
	failed  int
	total   int
}

// PruneCmd 返回prune命令
func PruneCmd() *cobra.Command {
	opts := &PruneOptions{}

	cmd := &cobra.Command{
		Use:   "prune [<project>...]",
		Short: "Prune (delete) already merged topics",
		Long:  `Prune (delete) already merged topics.`,
		RunE: func(_ *cobra.Command, args []string) error {
			return runPrune(opts, args)
		},
	}

	// 添加命令行选项
	cmd.Flags().BoolVarP(&opts.Force, "force", "f", false, "force pruning of branches that are not merged into upstream (skips the merge check)")
	cmd.Flags().BoolVarP(&opts.DryRun, "dry-run", "n", false, "don't actually prune, just show what would be pruned")
	cmd.Flags().BoolVarP(&opts.Verbose, "verbose", "v", false, "show all output")
	cmd.Flags().BoolVarP(&opts.Quiet, "quiet", "q", false, "only show errors")
	cmd.Flags().IntVarP(&opts.Jobs, "jobs", "j", 8, "number of jobs to run in parallel")
	AddManifestFlags(cmd, &opts.CommonManifestOptions)

	return cmd
}

// pruneRejectMirrorProjects 校验显式指定的项目不是镜像项目。
// 镜像项目是裸仓库（无工作树），prune 的分支清理语义不适用，明确报错。
func pruneRejectMirrorProjects(projects []*project.Project) error {
	errs := make([]error, 0)
	for _, p := range projects {
		if p != nil && p.Mirror {
			errs = append(errs, fmt.Errorf("project %s is a mirror project; prune requires a worktree and does not support mirrors", p.Name))
		}
	}
	return errors.Join(errs...)
}

// runPrune 执行prune命令
//
// 上游 repo prune 的语义：删除每个项目里已合并到上游的本地 topic 分支
// （等价于对已合并分支执行 git branch -d）。本实现不再删除磁盘上的项目目录。
func runPrune(opts *PruneOptions, args []string) error {
	// 初始化日志记录器
	log := logger.NewDefaultLogger()
	if opts.Verbose {
		log.SetLevel(logger.LogLevelDebug)
	} else if opts.Quiet {
		log.SetLevel(logger.LogLevelError)
	} else {
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

	log.Debug("开始清理已合并的本地分支")

	// 加载配置
	log.Debug("正在加载配置...")
	cfg, err := config.Load()
	if err != nil {
		log.Error("failed to load config: %v", err)
		return fmt.Errorf("failed to load config: %w", err)
	}

	// 加载清单
	log.Debug("正在解析清单文件...")
	parser := manifest.NewParser()
	manifestObj, err := parser.ParseFromFile(cfg.ManifestName, manifest.SplitGroups(cfg.EffectiveGroups()))
	if err != nil {
		log.Error("failed to parse manifest file: %v", err)
		return fmt.Errorf("failed to parse manifest: %w", err)
	}

	// 创建项目管理
	log.Debug("正在创建项目管理器...")
	manager := project.NewManagerFromManifest(manifestObj, cfg)

	var projects []*project.Project

	// failed to get project列表
	log.Debug("正在failed to get project列表...")
	if len(args) == 0 {
		log.Debug("获取所有项目")
		projects, err = manager.GetProjectsInGroups(nil)
		if err != nil {
			log.Error("获取所有项目failed: %v", err)
			return fmt.Errorf("failed to get projects: %w", err)
		}
		// 对齐上游 GetProjects：未指定项目参数时跳过未 checkout 的项目，
		// 不能让缺失工作树拖垮整个 prune。
		projects = filterSyncedProjects(projects)
	} else {
		log.Debug("获取指定的项目: %v", args)
		projects, err = manager.GetProjectsByArgs(args, originalDir)
		if err != nil {
			log.Error("获取指定项目failed: %v", err)
			return fmt.Errorf("failed to get projects by name: %w", err)
		}
		// 重复的项目参数只处理一次，避免同一分支被并发重复删除。
		projects = dedupeProjects(projects)
		// 镜像项目没有工作树，prune 的删除语义不适用：明确报错而不是静默跳过。
		if err := pruneRejectMirrorProjects(projects); err != nil {
			log.Error("镜像项目不支持 prune: %v", err)
			return err
		}
		// 显式指定的项目必须已同步（对齐上游 NoSuchProjectError）。
		if err := requireSyncedProjects(projects); err != nil {
			log.Error("指定的项目未同步: %v", err)
			return err
		}
	}

	// 创建统计对象（按分支计数）
	stats := &pruneStats{}
	// 待处理分支（删除后仍存活的未合并分支，对齐上游 "Pending Branches" 输出）
	var pendingMu sync.Mutex
	var pending []pendingBranch

	// 并发删除已合并分支
	log.Debug("开始并发清理已合并分支...")
	errChan := make(chan error, len(projects))
	var wg sync.WaitGroup

	// 设置并发控制
	maxWorkers := opts.Jobs
	if maxWorkers <= 0 {
		maxWorkers = 8
	}
	log.Debug("设置并发数为: %d", maxWorkers)
	sem := make(chan struct{}, maxWorkers)

	for _, p := range projects {
		wg.Add(1)
		sem <- struct{}{}
		go func(proj *project.Project) {
			defer wg.Done()
			defer func() { <-sem }()

			deleted, projPending, perr := pruneProjectBranches(proj, opts, log)
			stats.mu.Lock()
			stats.total += perr.total
			stats.success += perr.success
			stats.failed += perr.failed
			stats.mu.Unlock()

			if len(projPending) > 0 {
				pendingMu.Lock()
				pending = append(pending, projPending...)
				pendingMu.Unlock()
			}

			if perr.err != nil {
				log.Error("清理项目 %s 的分支failed: %v", proj.Name, perr.err)
				errChan <- fmt.Errorf("project %s: %w", proj.Name, perr.err)
				return
			}
			if deleted > 0 {
				log.Debug("已清理项目 %s 的 %d 个已合并分支", proj.Name, deleted)
			}
		}(p)
	}

	// 等待所有goroutine完成
	log.Debug("等待所有清理任务完成...")
	wg.Wait()
	close(errChan)

	// 收集所有错误
	var errs []error
	for err := range errChan {
		if err != nil {
			errs = append(errs, err)
		}
	}

	// 输出统计信息（dry-run 不宣称已删除，仅报告计划）。
	// 结果 payload 统一写 stdout，诊断日志仍走 logger（默认 stderr）。
	prunePrintSummary(os.Stdout, stats, len(pending), opts)

	// 输出存活（未合并）分支清单，对齐上游 "Pending Branches" 展示
	// 并发收集导致顺序不稳定，按项目/分支排序保证输出可复现。
	sort.SliceStable(pending, func(i, j int) bool {
		if pending[i].Project != pending[j].Project {
			return pending[i].Project < pending[j].Project
		}
		return pending[i].Branch < pending[j].Branch
	})
	printPendingBranches(os.Stdout, pending, opts)

	if len(errs) > 0 {
		log.Error("清理过程中遇到 %d 个错误", len(errs))
		return fmt.Errorf("encountered %d errors during pruning", len(errs))
	}

	return nil
}

// pruneBranchResult 记录单个项目分支清理的统计
type pruneBranchResult struct {
	total   int
	success int
	failed  int
	err     error
}

// pendingBranch 删除后仍存活的未合并分支（对齐上游 ReviewableBranch 的展示信息）
type pendingBranch struct {
	Project string
	Branch  string
	Current bool // 是否当前分支（上游用 * 标记）
	Commits int
	Date    string
}

// pruneProjectBranches 删除单个项目中已合并到上游的本地分支。
// 对齐上游 project.py PruneHeads，但不在删除非当前分支前无条件 detach：
//   - 非当前分支：先用 merge-base 验证已合并（有上游时基准为上游，否则为
//     manifest 修订提交；--force 跳过验证），通过后直接 git branch -D；
//     工作树是否干净不影响删除，也不会 checkout；
//   - 当前分支：仅当 HEAD 等于 manifest 修订版本且工作树干净时才 detach 到该
//     修订版本后删除；detach 失败只跳过当前分支，不影响其他可删分支；
//   - --dry-run 只报告计划，不删除；
//   - manifest revision 无法解析时返回错误（对齐上游 ManifestInvalidRevisionError），
//     绝不回退到 HEAD 作为合并基准；
//   - 未合并/被其他 worktree 占用的分支按 pending 报告而非错误；
//   - 已合并却删除失败（伪分支/权限等）或 HEAD 恢复失败都计入 failed 并返回错误。
func pruneProjectBranches(proj *project.Project, opts *PruneOptions, log logger.Logger) (int, []pendingBranch, *pruneBranchResult) {
	result := &pruneBranchResult{}

	// 列出所有本地分支。必须使用完整 refname 再剥离 refs/heads/：
	// %(refname:short) 在存在同名 tag 时会退化为 "heads/foo"（歧义消解格式），
	// 传给 git branch -D 会因找不到分支而删除失败。
	localBranches, err := pruneListLocalBranches(proj)
	if err != nil {
		result.err = err
		return 0, nil, result
	}
	if len(localBranches) == 0 {
		return 0, nil, result
	}

	// 当前分支：symbolic-ref HEAD 返回完整 ref（refs/heads/main），自行剥前缀。
	// --short 在同名 tag 存在时同样会返回 "heads/main"，不能直接使用；
	// detached HEAD 时命令失败，返回空串（对齐上游 CurrentBranch=None）。
	currentBranch := pruneCurrentBranch(proj)

	// manifest 修订版本 sha 与 HEAD sha。
	// manifest 修订版本必须按远端跟踪 ref 解析，绝不能 rev-parse 本地同名分支：
	// 否则本地 topic 恰好与 manifest 分支同名时会被误判为"已处于 manifest 修订版本"。
	// 解析失败按上游 ManifestInvalidRevisionError 报错，绝不静默回退到 HEAD —— 那会
	// 把"已合并"判定基准换成工作区 HEAD，误删未合并分支。
	revSha := pruneManifestRevisionSha(proj)
	if revSha == "" {
		rev := strings.TrimSpace(proj.Revision)
		if rev == "" {
			rev = "<unknown>"
		}
		result.err = fmt.Errorf("manifest revision %s not found in project %s", rev, proj.Name)
		return 0, nil, result
	}
	headSha := resolveRef(proj, "HEAD")

	// 被其他 worktree 占用的分支 git branch -D 会拒绝，按 pending 处理。
	worktreeBranches := pruneWorktreeBranches(proj)

	base := revSha
	merged := func(branch string) bool { return branchMerged(proj, branch, base) }

	// 非当前分支不参与 detach：直接按 merge-base/base 的预验证结果决定是否删除。
	deletable := make([]string, 0, len(localBranches))
	occupiedPending := make([]string, 0)
	for _, b := range localBranches {
		if b == currentBranch {
			continue
		}
		if worktreeBranches[b] {
			occupiedPending = append(occupiedPending, b)
			continue
		}
		deletable = append(deletable, b)
	}
	sort.Strings(deletable)
	sort.Strings(occupiedPending)
	if len(occupiedPending) > 0 {
		log.Debug("项目 %s 有 %d 个分支被其他 worktree 占用，按 pending 处理", proj.Name, len(occupiedPending))
	}

	// deleted：已验证可删（--force 时全部非当前分支）；refused：未合并保留。
	deleted, refused := pruneDryRunPlan(deletable, opts.Force, merged)
	result.total = len(deletable) + len(occupiedPending)

	// 当前分支只在指向 manifest 修订版本且工作树干净（不含 untracked，对齐上游
	// IsDirty(consider_untracked=False)）时才删除。
	currentDeletable := currentBranch != "" && revSha != "" && headSha == revSha && isWorktreeClean(proj)
	includeCurrent := currentBranch != "" && !currentDeletable
	if currentDeletable {
		result.total++
	}
	if result.total == 0 && !includeCurrent {
		return 0, nil, result
	}

	if opts.DryRun {
		// dry-run：结果 payload 写 stdout，不执行任何 checkout/删除。
		plan := append([]string(nil), deleted...)
		if currentDeletable {
			plan = append(plan, currentBranch)
			sort.Strings(plan)
		}
		prunePrintDryRunPlan(os.Stdout, proj.Name, plan)
		pendingNames := make([]string, 0, len(refused)+len(occupiedPending)+1)
		pendingNames = append(pendingNames, refused...)
		pendingNames = append(pendingNames, occupiedPending...)
		if includeCurrent {
			pendingNames = append(pendingNames, currentBranch)
		}
		sort.Strings(pendingNames)
		pending := make([]pendingBranch, 0, len(pendingNames))
		for _, b := range pendingNames {
			pending = append(pending, newPendingBranch(proj, b, b == currentBranch, base))
		}
		result.success = len(plan)
		return result.success, pending, result
	}

	// 目标分支：并入当前分支（仅在成功 detach 后追加）。
	targets := deleted
	var currentErr error
	if currentDeletable {
		if _, derr := proj.GitRepo.RunCommand("checkout", "--detach", revSha); derr != nil {
			// detach 失败只影响当前分支，其他可删分支继续删除。
			currentErr = fmt.Errorf("detach HEAD to %s: %w", revSha, derr)
			log.Debug("项目 %s detach HEAD 失败，仅跳过当前分支: %v", proj.Name, derr)
		} else {
			// finally：无论删除/重列成功与否都恢复原 HEAD/分支。
			// 恢复失败属于 prune 失败，计入 failed 统计（不能只记 err 却少报失败数）。
			defer func() {
				if rerr := pruneRestoreHead(proj, currentBranch, headSha); rerr != nil {
					result.err = errors.Join(result.err, fmt.Errorf("restore HEAD: %w", rerr))
					result.failed++
				}
			}()
			targets = append(targets, currentBranch)
			sort.Strings(targets)
		}
	}

	// 直接强删：删除前已用 merge-base 验证（或 --force），无需再依赖 HEAD 基准。
	var delErr error
	if len(targets) > 0 {
		args := append([]string{"branch", "-D"}, targets...)
		if _, derr := proj.GitRepo.RunCommand(args...); derr != nil {
			delErr = derr
			log.Debug("项目 %s 批量删除分支返回非零: %v", proj.Name, derr)
		}
	}

	// 重新列出分支：仍存活的需区分"未合并（预期保留）"与"删除失败（伪分支/权限）"
	after, err := pruneListLocalBranches(proj)
	if err != nil {
		result.err = errors.Join(currentErr, fmt.Errorf("re-list branches: %w", err))
		return 0, nil, result
	}
	survivors := make(map[string]bool, len(after))
	for _, b := range after {
		survivors[b] = true
	}

	pendingNames, failedNames := pruneReportBranches(targets, refused, survivors, currentBranch, includeCurrent)
	if currentErr != nil && survivors[currentBranch] {
		failedNames = append(failedNames, currentBranch)
		sort.Strings(failedNames)
	}
	pendingNames = append(pendingNames, occupiedPending...)
	sort.Strings(pendingNames)
	pending := make([]pendingBranch, 0, len(pendingNames))
	for _, b := range pendingNames {
		pending = append(pending, newPendingBranch(proj, b, b == currentBranch, base))
	}

	for _, b := range targets {
		if !survivors[b] {
			result.success++
		}
	}
	result.failed = len(failedNames)
	errs := make([]error, 0, len(failedNames)+2)
	if currentErr != nil {
		errs = append(errs, currentErr)
	}
	for _, b := range failedNames {
		errs = append(errs, fmt.Errorf("failed to delete branch %s", b))
	}
	if delErr != nil && len(failedNames) > 0 {
		errs = append(errs, delErr)
	}
	if len(errs) > 0 {
		result.err = errors.Join(errs...)
	}

	return result.success, pending, result
}

// pruneCurrentBranch 返回项目当前分支短名。
// 使用完整 symbolic-ref HEAD 再剥离 refs/heads/：--short 在同名 tag 存在时会
// 返回 "heads/main" 这类歧义形式；detached HEAD 时返回空串（对齐上游 CurrentBranch=None）。
func pruneCurrentBranch(proj *project.Project) string {
	out, err := proj.GitRepo.RunCommand("symbolic-ref", "HEAD")
	if err != nil {
		return ""
	}
	ref := strings.TrimSpace(string(out))
	if !strings.HasPrefix(ref, "refs/heads/") {
		return ""
	}
	return strings.TrimPrefix(ref, "refs/heads/")
}

// pruneListLocalBranches 列出项目本地分支名。
//
// 使用完整 refname（refs/heads/foo）再剥离前缀，避免 %(refname:short) 在同名
// tag 存在时返回 "heads/foo" 这种歧义消解格式（git branch -d 不接受）。
func pruneListLocalBranches(proj *project.Project) ([]string, error) {
	output, err := proj.GitRepo.RunCommand("for-each-ref", "--format=%(refname)", "refs/heads/")
	if err != nil {
		return nil, fmt.Errorf("list branches: %w", err)
	}

	branches := make([]string, 0)
	for _, line := range strings.Split(string(output), "\n") {
		ref := strings.TrimSpace(line)
		if !strings.HasPrefix(ref, "refs/heads/") {
			continue
		}
		if name := strings.TrimPrefix(ref, "refs/heads/"); name != "" {
			branches = append(branches, name)
		}
	}
	return branches, nil
}

// prunePrintDryRunPlan 把 dry-run 计划写到 w（结果 payload 不走 logger）。
func prunePrintDryRunPlan(w io.Writer, projectName string, deleted []string) {
	for _, b := range deleted {
		fmt.Fprintf(w, "[dry-run] 将删除项目 %s 的分支 %s\n", projectName, b)
	}
}

// prunePrintSummary 把 prune 统计写到 w（结果 payload 不走 logger）。
func prunePrintSummary(w io.Writer, stats *pruneStats, pendingCount int, opts *PruneOptions) {
	if opts.DryRun {
		fmt.Fprintf(w, "dry-run 完成: 计划删除 %d 个分支, Pending Branches 报告 %d 个分支\n",
			stats.success, pendingCount)
		return
	}
	fmt.Fprintf(w, "清理完成: 共检查分支 %d 个, 成功删除 %d 个, failed %d 个\n",
		stats.total, stats.success, stats.failed)
}

// pruneDryRunPlan 计算 dry-run 计划：返回将删除的分支与保留（未合并）的分支名。
func pruneDryRunPlan(kill []string, force bool, merged func(string) bool) (deleted, kept []string) {
	deleted = make([]string, 0, len(kill))
	kept = make([]string, 0)
	for _, b := range kill {
		if force || merged(b) {
			deleted = append(deleted, b)
			continue
		}
		kept = append(kept, b)
	}
	return deleted, kept
}

// pruneReportBranches 对删除后仍存活的分支分类（纯逻辑，便于测试）：
//   - targets 中仍存活：删除失败（伪分支/权限等），必须作为错误上报；
//   - refused 中仍存活：未合并被预期保留，列入 Pending Branches；
//   - includeCurrent 时当前分支始终加入报告（上游 * 标记），但不参与删除判定。
func pruneReportBranches(targets, refused []string, survivors map[string]bool, currentBranch string, includeCurrent bool) (pending []string, failed []string) {
	pending = make([]string, 0)
	failed = make([]string, 0)
	for _, b := range targets {
		if survivors[b] {
			failed = append(failed, b)
		}
	}
	for _, b := range refused {
		if survivors[b] {
			pending = append(pending, b)
		}
	}
	if includeCurrent && currentBranch != "" && survivors[currentBranch] {
		pending = append(pending, currentBranch)
	}
	sort.Strings(pending)
	sort.Strings(failed)
	return pending, failed
}

// resolveRef 解析 ref 的 sha，失败返回空串（best-effort）
func resolveRef(proj *project.Project, ref string) string {
	if ref == "" {
		return ""
	}
	out, err := proj.GitRepo.RunCommand("rev-parse", "--verify", ref)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// isWorktreeClean 判断工作树是否有已跟踪文件的改动（忽略 untracked，对齐上游
// IsDirty(consider_untracked=False)）
func isWorktreeClean(proj *project.Project) bool {
	_, err := proj.GitRepo.RunCommand("diff-index", "--quiet", "HEAD", "--")
	return err == nil
}

// pruneManifestRevisionSha 解析 manifest revision 对应的提交 SHA。
//
// 对齐 sync 的 resolveRevisionID：分支名一律走远端跟踪 ref
// refs/remotes/<remote>/<revision>，绝不回退到本地同名分支；revision 本身就是
// 提交 ID 时直接使用（超级项目写入的 RevisionID 优先）。解析失败返回空串。
func pruneManifestRevisionSha(proj *project.Project) string {
	if proj == nil {
		return ""
	}
	if revID := strings.TrimSpace(proj.RevisionID); pruneLooksLikeCommitID(revID) && len(revID) == 40 {
		return revID
	}

	rev := strings.TrimSpace(proj.Revision)
	if rev == "" {
		return ""
	}

	refs := make([]string, 0, 2)
	switch {
	case pruneLooksLikeCommitID(rev):
		refs = append(refs, rev)
	case strings.HasPrefix(rev, "refs/heads/"):
		if ref := pruneRemoteTrackingRef(proj.RemoteName, strings.TrimPrefix(rev, "refs/heads/")); ref != "" {
			refs = append(refs, ref)
		}
	case strings.HasPrefix(rev, "refs/"):
		refs = append(refs, rev)
	default:
		if ref := pruneRemoteTrackingRef(proj.RemoteName, rev); ref != "" {
			refs = append(refs, ref)
		}
	}

	for _, ref := range refs {
		if sha := pruneResolveCommit(proj, ref); sha != "" {
			return sha
		}
	}
	return ""
}

// pruneRemoteTrackingRef 返回 revision 分支对应的远端跟踪 ref；无远端时不构造，
// 以保证"不回退到本地同名分支"的约束。
func pruneRemoteTrackingRef(remote, branch string) string {
	if branch == "" || remote == "" || remote == "." {
		return ""
	}
	if strings.HasPrefix(branch, remote+"/") {
		return "refs/remotes/" + branch
	}
	return "refs/remotes/" + remote + "/" + branch
}

// pruneResolveCommit 解析 ref 指向的提交 SHA（^0 剥离 annotated tag），失败返回空串。
func pruneResolveCommit(proj *project.Project, ref string) string {
	if ref == "" {
		return ""
	}
	out, err := proj.GitRepo.RunCommand("rev-parse", "--verify", "--quiet", ref+"^0")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// pruneLooksLikeCommitID 判断字符串是否为 7-40 位十六进制提交 ID。
func pruneLooksLikeCommitID(rev string) bool {
	if len(rev) < 7 || len(rev) > 40 {
		return false
	}
	for _, c := range rev {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
			return false
		}
	}
	return true
}

// pruneWorktreeBranches 返回被"其他 worktree"检出的本地分支集合。
// 当前项目自身 worktree 检出的分支不计入，否则当前分支会被误判为不可删除。
func pruneWorktreeBranches(proj *project.Project) map[string]bool {
	occupied := make(map[string]bool)
	if proj == nil {
		return occupied
	}
	out, err := proj.GitRepo.RunCommand("worktree", "list", "--porcelain")
	if err != nil {
		return occupied
	}

	self := strings.TrimSpace(proj.Worktree)
	if self == "" {
		self = proj.GitRepo.Path
	}
	self = filepath.Clean(self)
	if abs, absErr := filepath.Abs(self); absErr == nil {
		self = abs
	}

	blockPath := ""
	blockBranch := ""
	flush := func() {
		if blockPath != "" && blockBranch != "" {
			path := filepath.Clean(blockPath)
			if abs, absErr := filepath.Abs(path); absErr == nil {
				path = abs
			}
			if path != self {
				occupied[blockBranch] = true
			}
		}
		blockPath = ""
		blockBranch = ""
	}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		switch {
		case line == "":
			flush()
		case strings.HasPrefix(line, "worktree "):
			blockPath = strings.TrimSpace(strings.TrimPrefix(line, "worktree "))
		case strings.HasPrefix(line, "branch refs/heads/"):
			blockBranch = strings.TrimPrefix(line, "branch refs/heads/")
		}
	}
	flush()
	return occupied
}

// pruneRestoreHead 把 HEAD 恢复到原分支；分支已不存在时退回原提交的 detached HEAD。
func pruneRestoreHead(proj *project.Project, branch, headSha string) error {
	var checkoutErr error
	if branch != "" {
		if _, err := proj.GitRepo.RunCommand("checkout", branch); err == nil {
			return nil
		} else {
			checkoutErr = err
		}
	}
	if headSha != "" {
		if _, err := proj.GitRepo.RunCommand("checkout", "--detach", headSha); err == nil {
			return nil
		}
	}
	if checkoutErr != nil {
		return fmt.Errorf("checkout %s: %w", branch, checkoutErr)
	}
	return fmt.Errorf("restore HEAD to branch %s", branch)
}

// branchMerged 判断分支是否已合并到其删除判定基准：
// 分支配置了上游跟踪分支时基准为该上游（git branch -d 语义），否则为 manifest
// 修订提交（baseSha，对齐上游 PruneHeads 的 rev 兜底，不能用 HEAD）。
// merge-base 的实参使用完整 refs/heads/<branch>，避免存在同名 tag 时被歧义解析。
func branchMerged(proj *project.Project, branch, baseSha string) bool {
	base := resolveRef(proj, branch+"@{upstream}")
	if base == "" {
		base = baseSha
	}
	if base == "" {
		return false
	}
	_, err := proj.GitRepo.RunCommand("merge-base", "--is-ancestor", "refs/heads/"+branch, base)
	return err == nil
}

// newPendingBranch 构造 pending 分支信息：提交数 = base..branch 区间，日期 = 分支顶端提交日期。
// rev-list/log 的实参使用完整 refs/heads/<branch>，避免同名 tag 歧义；
// @{upstream} 仍使用分支短名（git 的 upstream 语法只接受短分支名）。
func newPendingBranch(proj *project.Project, branch string, current bool, baseSha string) pendingBranch {
	pb := pendingBranch{Project: proj.Path, Branch: branch, Current: current}
	base := resolveRef(proj, branch+"@{upstream}")
	if base == "" {
		base = baseSha
	}
	fullRef := "refs/heads/" + branch
	if base != "" {
		if out, err := proj.GitRepo.RunCommand("rev-list", "--count", base+".."+fullRef); err == nil {
			if n, err := strconv.Atoi(strings.TrimSpace(string(out))); err == nil {
				pb.Commits = n
			}
		}
	}
	if out, err := proj.GitRepo.RunCommand("log", "-1", "--format=%cs", fullRef); err == nil {
		pb.Date = strings.TrimSpace(string(out))
	}
	return pb
}

// printPendingBranches 按上游 "Pending Branches" 格式输出存活分支清单到 w。
// 这是结果 payload，即使 -q 也必须输出；只有诊断日志受 -q 影响。
func printPendingBranches(w io.Writer, pending []pendingBranch, opts *PruneOptions) {
	if len(pending) == 0 {
		return
	}

	fmt.Fprintln(w, "Pending Branches")
	lastProject := ""
	for _, pb := range pending {
		if pb.Project != lastProject {
			lastProject = pb.Project
			fmt.Fprintln(w)
			fmt.Fprintf(w, "project %s/\n", pb.Project)
		}
		marker := " "
		if pb.Current {
			marker = "*"
		}
		fmt.Fprintf(w, "%s %-33s (%d commits, %s)\n", marker, pb.Branch, pb.Commits, pb.Date)
	}
}
