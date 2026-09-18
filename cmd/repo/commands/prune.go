package commands

import (
	"fmt"
	"github.com/leopardxu/repo-go/internal/config"
	"github.com/leopardxu/repo-go/internal/logger"
	"github.com/leopardxu/repo-go/internal/manifest"
	"github.com/leopardxu/repo-go/internal/project"
	"github.com/spf13/cobra"
	"strconv"
	"strings"
	"sync"
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
	cmd.Flags().BoolVarP(&opts.Force, "force", "f", false, "force pruning even if there are local changes")
	cmd.Flags().BoolVarP(&opts.DryRun, "dry-run", "n", false, "don't actually prune, just show what would be pruned")
	cmd.Flags().BoolVarP(&opts.Verbose, "verbose", "v", false, "show all output")
	cmd.Flags().BoolVarP(&opts.Quiet, "quiet", "q", false, "only show errors")
	cmd.Flags().IntVarP(&opts.Jobs, "jobs", "j", 8, "number of jobs to run in parallel")
	AddManifestFlags(cmd, &opts.CommonManifestOptions)

	return cmd
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
	manifestObj, err := parser.ParseFromFile(cfg.ManifestName, manifest.SplitGroups(cfg.Groups))
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
	} else {
		log.Debug("获取指定的项目: %v", args)
		projects, err = manager.GetProjectsByNames(args)
		if err != nil {
			log.Error("获取指定项目failed: %v", err)
			return fmt.Errorf("failed to get projects by name: %w", err)
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

	// 输出统计信息（dry-run 不宣称已删除，仅报告计划）
	if opts.DryRun {
		log.Info("dry-run 完成: 将删除 %d 个已合并分支, %d 个未合并分支保留", stats.success, len(pending))
	} else {
		log.Info("清理完成: 共检查分支 %d 个, 成功删除 %d 个, failed %d 个",
			stats.total, stats.success, stats.failed)
	}

	// 输出存活（未合并）分支清单，对齐上游 "Pending Branches" 展示
	printPendingBranches(pending, opts, log)

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
// 对齐上游 project.py PruneHeads：
//   - 待删分支 = 除当前分支外的所有本地分支（当前分支在指向 manifest 修订版本且
//     工作树干净时也会被纳入删除：先 detach 到修订版本再删）；
//   - 实际删除用 git branch -d 批量执行，由 git 按"各自的上游跟踪分支（无上游时
//     为 HEAD）"判定是否已合并，而非统一按 manifest 修订版本判定；
//   - --force 用 -D 强删；--dry-run 只报告计划，不删除；
//   - 存活（未合并）分支返回为 pending 供上层按上游 "Pending Branches" 格式输出。
func pruneProjectBranches(proj *project.Project, opts *PruneOptions, log logger.Logger) (int, []pendingBranch, *pruneBranchResult) {
	result := &pruneBranchResult{}

	// 列出所有本地分支：git for-each-ref --format='%(refname:short)' refs/heads/
	output, err := proj.GitRepo.RunCommand("for-each-ref", "--format=%(refname:short)", "refs/heads/")
	if err != nil {
		result.err = fmt.Errorf("list branches: %w", err)
		return 0, nil, result
	}

	var localBranches []string
	for _, b := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		if b = strings.TrimSpace(b); b != "" {
			localBranches = append(localBranches, b)
		}
	}
	if len(localBranches) == 0 {
		return 0, nil, result
	}

	// 当前分支（分离 HEAD 时 GetCurrentBranch 返回 "HEAD"，视为无当前分支）
	currentBranch, _ := proj.GetCurrentBranch()
	if currentBranch == "HEAD" {
		currentBranch = ""
	}

	// manifest 修订版本 sha 与 HEAD sha（best-effort，解析失败时跳过当前分支删除判定）
	revSha := resolveRef(proj, proj.Revision)
	headSha := resolveRef(proj, "HEAD")

	// 待删分支 = 除当前分支外的所有本地分支
	var kill []string
	for _, b := range localBranches {
		if b == currentBranch {
			continue
		}
		kill = append(kill, b)
	}

	// 当前分支指向 manifest 修订版本且工作树干净（不含 untracked，对齐上游
	// IsDirty(consider_untracked=False)）时：detach 后一并删除
	killedCurrent := false
	if currentBranch != "" && revSha != "" && headSha == revSha && isWorktreeClean(proj) {
		kill = append(kill, currentBranch)
		killedCurrent = true
	}

	result.total = len(kill)
	if len(kill) == 0 {
		return 0, nil, result
	}

	delFlag := "-d"
	if opts.Force {
		delFlag = "-D"
	}

	if opts.DryRun {
		// dry-run：按 git -d 的判定标准（各自上游，无上游时 HEAD）报告计划删除的分支
		var pending []pendingBranch
		for _, b := range kill {
			if branchMerged(proj, b, headSha) {
				log.Info("[dry-run] 将删除项目 %s 的已合并分支 %s", proj.Name, b)
				result.success++
				continue
			}
			pending = append(pending, newPendingBranch(proj, b, b == currentBranch, headSha))
		}
		return result.success, pending, result
	}

	// 删除当前分支前先分离 HEAD 到 manifest 修订版本
	if killedCurrent {
		if _, err := proj.GitRepo.RunCommand("checkout", "--detach", revSha); err != nil {
			result.err = fmt.Errorf("detach head to %s: %w", revSha, err)
			return 0, nil, result
		}
	}

	// 批量安全删除（git 对每个分支独立按其上游判定是否已合并）
	args := append([]string{"branch", delFlag}, kill...)
	if _, err := proj.GitRepo.RunCommand(args...); err != nil {
		// 部分分支删除失败（如被其他 worktree 占用）：统计仍存活的分支后继续
		log.Debug("项目 %s 批量删除分支返回错误（部分分支未合并或被占用）: %v", proj.Name, err)
	}

	// 重新列出分支：仍存活的即未合并的 pending（对齐上游 PruneHeads 的 kept 计算）
	after, err := proj.ListLocalBranches()
	if err != nil {
		result.err = fmt.Errorf("re-list branches: %w", err)
		return 0, nil, result
	}
	survivors := make(map[string]bool, len(after))
	for _, b := range after {
		survivors[b] = true
	}

	var pending []pendingBranch
	for _, b := range kill {
		if survivors[b] {
			pending = append(pending, newPendingBranch(proj, b, b == currentBranch, headSha))
		} else {
			result.success++
		}
	}

	return result.success, pending, result
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

// branchMerged 判断分支是否已合并到其删除判定基准：
// 分支配置了上游跟踪分支时基准为该上游（git branch -d 语义），否则为 HEAD
func branchMerged(proj *project.Project, branch, headSha string) bool {
	base := resolveRef(proj, branch+"@{upstream}")
	if base == "" {
		base = headSha
	}
	if base == "" {
		return false
	}
	_, err := proj.GitRepo.RunCommand("merge-base", "--is-ancestor", branch, base)
	return err == nil
}

// newPendingBranch 构造 pending 分支信息：提交数 = base..branch 区间，日期 = 分支顶端提交日期
func newPendingBranch(proj *project.Project, branch string, current bool, headSha string) pendingBranch {
	pb := pendingBranch{Project: proj.Path, Branch: branch, Current: current}
	base := resolveRef(proj, branch+"@{upstream}")
	if base == "" {
		base = headSha
	}
	if base != "" {
		if out, err := proj.GitRepo.RunCommand("rev-list", "--count", base+".."+branch); err == nil {
			if n, err := strconv.Atoi(strings.TrimSpace(string(out))); err == nil {
				pb.Commits = n
			}
		}
	}
	if out, err := proj.GitRepo.RunCommand("log", "-1", "--format=%cs", branch); err == nil {
		pb.Date = strings.TrimSpace(string(out))
	}
	return pb
}

// printPendingBranches 按上游 "Pending Branches" 格式输出存活分支清单
func printPendingBranches(pending []pendingBranch, opts *PruneOptions, log logger.Logger) {
	if len(pending) == 0 || opts.Quiet {
		return
	}

	log.Info("Pending Branches")
	lastProject := ""
	for _, pb := range pending {
		if pb.Project != lastProject {
			lastProject = pb.Project
			log.Info("")
			log.Info("project %s/", pb.Project)
		}
		marker := " "
		if pb.Current {
			marker = "*"
		}
		log.Info("%s %-33s (%d commits, %s)", marker, pb.Branch, pb.Commits, pb.Date)
	}
}
