package commands

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"

	"github.com/leopardxu/repo-go/internal/color"
	"github.com/leopardxu/repo-go/internal/config"
	"github.com/leopardxu/repo-go/internal/logger"
	"github.com/leopardxu/repo-go/internal/manifest"
	"github.com/leopardxu/repo-go/internal/progress"
	"github.com/leopardxu/repo-go/internal/project"
	"github.com/spf13/cobra"
)

// BranchOptions holds the options for the branch command
type BranchOptions struct {
	All         bool
	Current     bool
	Remote      bool
	Color       string
	List        bool
	Verbose     bool
	SetUpstream string
	Jobs        int
	Quiet       bool
	Config      *config.Config // <-- Add this field
	CommonManifestOptions
}

// BranchCmd creates the branch command
func BranchCmd() *cobra.Command {
	opts := &BranchOptions{}

	cmd := &cobra.Command{
		Use:   "branches [<project>...]",
		Short: "View current topic branches",
		Long:  `Summarizes the currently available topic branches.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			// --color 复用全局持久 flag（不在命令级重复注册，避免遮蔽全局）
			opts.Color, _ = cmd.Flags().GetString("color")
			return runBranch(opts, args)
		},
	}

	cmd.Flags().BoolVarP(&opts.All, "all", "a", false, "show all branches")
	cmd.Flags().BoolVarP(&opts.Remote, "remote", "r", false, "show remote branches")
	cmd.Flags().BoolVar(&opts.Current, "current", false, "consider only the current branch")
	cmd.Flags().BoolVarP(&opts.List, "list", "l", false, "list branches")
	cmd.Flags().BoolVarP(&opts.Verbose, "verbose", "v", false, "show hash and subject, give twice for upstream branch")
	cmd.Flags().StringVar(&opts.SetUpstream, "set-upstream", "", "set upstream for git pull/fetch")
	cmd.Flags().IntVarP(&opts.Jobs, "jobs", "j", 8, "number of jobs to run in parallel")
	cmd.Flags().BoolVarP(&opts.Quiet, "quiet", "q", false, "only show errors")
	AddManifestFlags(cmd, &opts.CommonManifestOptions)

	return cmd
}

// runBranch executes the branch command logic
func runBranch(opts *BranchOptions, args []string) error {
	// 初始化日志系
	log := logger.NewDefaultLogger()
	if opts.Quiet {
		log.SetLevel(logger.LogLevelError)
	} else if opts.Verbose {
		log.SetLevel(logger.LogLevelDebug)
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

	log.Debug("正在加载配置文件...")
	cfg, err := config.Load()
	if err != nil {
		log.Error("加载配置文件failed: %v", err)
		return fmt.Errorf("failed to load config: %w", err)
	}

	log.Debug("正在解析清单文件 %s...", cfg.ManifestName)
	parser := manifest.NewParser()
	manifestObj, err := parser.ParseFromFile(cfg.ManifestName, manifest.SplitGroups(cfg.EffectiveGroups()))
	if err != nil {
		log.Error("failed to parse manifest file: %v", err)
		return fmt.Errorf("failed to parse manifest: %w", err)
	}

	log.Debug("正在初始化项目管理器...")
	manager := project.NewManagerFromManifest(manifestObj, cfg)

	var projects []*project.Project
	if len(args) == 0 {
		log.Debug("获取所有项..")
		projects, err = manager.GetProjectsInGroups(nil)
		if err != nil {
			log.Error("failed to get projectfailed: %v", err)
			return fmt.Errorf("failed to get projects: %w", err)
		}
		// 对齐上游 GetProjects：未指定项目参数时只处理已 checkout 的项目，
		// 工作树缺失的项目直接跳过，而不是在并发收集时产生 "真实失败"。
		projects = filterSyncedProjects(projects)
		log.Debug("共获取到 %d 个已同步的项目", len(projects))
	} else {
		log.Debug("根据参数获取项目: %v", args)
		projects, err = manager.GetProjectsByArgs(args, originalDir)
		if err != nil {
			log.Error("根据参数获取项目失败: %v", err)
			return fmt.Errorf("failed to get projects by args: %w", err)
		}
		if err := requireSyncedProjects(projects); err != nil {
			log.Error("指定的项目未同步: %v", err)
			return err
		}
		log.Debug("共获取到 %d 个项目", len(projects))
	}

	// 重复的项目参数只处理一次，避免同一项目被并发处理两次。
	projects = dedupeProjects(projects)

	// --set-upstream：为各项目的当前分支设置上游跟踪关系（repo-go 扩展选项）
	if opts.SetUpstream != "" {
		var upstreamErrs []error
		for _, p := range projects {
			remoteName := p.RemoteName
			if remoteName == "" {
				remoteName = "origin"
			}
			upstreamRef := fmt.Sprintf("%s/%s", remoteName, opts.SetUpstream)
			if _, err := p.GitRepo.RunCommand("branch", "--set-upstream-to", upstreamRef); err != nil {
				log.Error("项目 %s 设置上游跟踪failed: %v", p.Name, err)
				upstreamErrs = append(upstreamErrs, fmt.Errorf("project %s: set upstream %s: %w", p.Name, upstreamRef, err))
			} else if !opts.Quiet {
				log.Debug("项目 %s 已设置上游跟踪到 %s", p.Name, upstreamRef)
			}
		}
		if len(upstreamErrs) > 0 {
			return errors.Join(upstreamErrs...)
		}
		return nil
	}

	type branchResult struct {
		ProjectName   string
		CurrentBranch string
		Branches      []string
		Published     map[string]bool // 分支是否已发布
		Err           error
	}

	// 规范化并发数：-j <= 0 会导致 makechan panic，回退到默认 8（与其他命令一致）
	if opts.Jobs <= 0 {
		opts.Jobs = 8
	}
	log.Debug("正在failed to get project分支信息，并行任务数: %d...", opts.Jobs)

	// 创建进度显示器
	prog := progress.NewProgress("Fetching branches", len(projects), opts.Quiet)

	results := make(chan branchResult, len(projects))
	sem := make(chan struct{}, opts.Jobs)
	var wg sync.WaitGroup

	for _, p := range projects {
		p := p
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()

			log.Debug("failed to get project %s 的分支信息...", p.Name)

			// 当前分支使用完整 symbolic-ref HEAD 再剥离 refs/heads/：
			// --abbrev-ref/--short 在同名 tag 存在时会返回 "heads/main" 歧义形式。
			// detached HEAD 时 symbolic-ref 失败，当前分支记为空串（对齐上游
			// CurrentBranch=None），分支列表仍可正常展示。
			currentBranch := ""
			if currentBranchBytes, cerr := p.GitRepo.RunCommand("symbolic-ref", "HEAD"); cerr == nil {
				if ref := strings.TrimSpace(string(currentBranchBytes)); strings.HasPrefix(ref, "refs/heads/") {
					currentBranch = strings.TrimPrefix(ref, "refs/heads/")
				}
			} else {
				log.Debug("项目 %s 处于 detached HEAD，当前分支记为未检出", p.Name)
			}

			branchArgs := []string{"branch", "--list"}
			if opts.All {
				branchArgs = append(branchArgs, "-a")
			}
			if opts.Remote {
				branchArgs = append(branchArgs, "-r")
			}

			branchesOutputBytes, err := p.GitRepo.RunCommand(branchArgs...)
			if err != nil {
				log.Error("failed to get project %s 的分支列表failed %v", p.Name, err)
				results <- branchResult{ProjectName: p.Name, Err: err}
				return
			}

			branches := strings.Split(strings.TrimSpace(string(branchesOutputBytes)), "\n")

			log.Debug("项目 %s 当前分支: %s, 共有 %d 个分支", p.Name, currentBranch, len(branches))

			// 更新进度
			prog.Update(p.Name)

			results <- branchResult{ProjectName: p.Name, CurrentBranch: currentBranch, Branches: branches}
		}()
	}
	// 启动一个goroutine 来关闭结果通道
	go func() {
		wg.Wait()
		close(results)
	}()

	branchInfo := make(map[string][]string)
	currentBranches := make(map[string]bool)
	successCount := 0
	failCount := 0
	var collectErrs []error

	// 收集结果
	for res := range results {
		if res.Err != nil {
			failCount++
			log.Error("获取项目 %s 的分支信息failed: %v", res.ProjectName, res.Err)
			collectErrs = append(collectErrs, fmt.Errorf("project %s: %w", res.ProjectName, res.Err))
			continue
		}

		successCount++
		if res.CurrentBranch != "" {
			currentBranches[res.CurrentBranch] = true
		}

		for _, rawBranch := range res.Branches {
			branch := parseBranchListLine(rawBranch)
			if branch == "" {
				continue
			}
			branchInfo[branch] = append(branchInfo[branch], res.ProjectName)
		}
	}

	log.Debug("共处理 %d 个项目，成功: %d，failed: %d", len(projects), successCount, failCount)

	// 显示分支信息：结果 payload 写 stdout，即使 -q 也不吞；只有诊断日志受 -q 影响。
	if !opts.Quiet {
		// 完成进度
		prog.Finish("")
	}
	printBranchReport(os.Stdout, branchInfo, currentBranches, len(projects), opts)

	// 分支信息收集失败必须使命令返回非零，不能只打日志后静默成功
	if len(collectErrs) > 0 {
		return errors.Join(collectErrs...)
	}

	return nil
}

// parseBranchListLine 解析 `git branch --list` 的一行输出：
//   - 去掉首尾空白；
//   - 剥离 "* "（当前分支）/"+ "（被其他 worktree 检出）标记；
//   - detached HEAD 的伪条目 ("(HEAD detached at ...)") 不是本地分支，返回空串。
func parseBranchListLine(line string) string {
	line = strings.TrimSpace(line)
	if line == "" {
		return ""
	}
	if strings.HasPrefix(line, "* ") || strings.HasPrefix(line, "+ ") {
		line = strings.TrimSpace(line[2:])
	}
	if line == "" || strings.HasPrefix(line, "(HEAD detached") {
		return ""
	}
	return line
}

// printBranchReport 按上游 "toplevel branches" 汇总格式输出分支清单到 w：
// 当前分支用 * 标记并高亮，其余分支标注所属项目。
func printBranchReport(w io.Writer, branchInfo map[string][]string, currentBranches map[string]bool, projectCount int, opts *BranchOptions) {
	branchNames := make([]string, 0, len(branchInfo))
	for branch := range branchInfo {
		branchNames = append(branchNames, branch)
	}
	sort.Strings(branchNames)

	useColor := color.ShouldUseColor(opts.Color)
	coloring := color.NewBranchColoring(useColor)

	fmt.Fprintln(w, "分支信息汇总")

	for _, branch := range branchNames {
		// --current：只显示当前分支
		if opts.Current && !currentBranches[branch] {
			continue
		}
		projs := branchInfo[branch]
		prefix := " "
		var branchText string
		if currentBranches[branch] {
			prefix = "*"
			branchText = coloring.Current(branch)
		} else {
			branchText = coloring.Local(branch)
		}

		if len(projs) == projectCount {
			fmt.Fprintf(w, "%s %-30s | 所有项目\n", prefix, branchText)
		} else {
			fmt.Fprintf(w, "%s %-30s | 在项目 %s\n", prefix, branchText, strings.Join(projs, ", "))
		}
	}

	fmt.Fprintf(w, "\n共有 %d 个分支\n", len(branchNames))
}

// projectUnsynced 判断项目是否尚未同步：工作树为空或目录不存在。
// 对齐上游 Project.Exists 语义（只有目录存在才算已 checkout）。
func projectUnsynced(p *project.Project) bool {
	if p == nil || p.Worktree == "" {
		return true
	}
	info, err := os.Stat(p.Worktree)
	if err != nil {
		return true
	}
	return !info.IsDir()
}

// filterSyncedProjects 仅保留工作树存在的项目（对齐上游 GetProjects 无项目参数的
// Exists 过滤）：未同步项目不应参与 branch/checkout/abandon，也不应计入失败。
func filterSyncedProjects(projects []*project.Project) []*project.Project {
	synced := make([]*project.Project, 0, len(projects))
	for _, p := range projects {
		if !projectUnsynced(p) {
			synced = append(synced, p)
		}
	}
	return synced
}

// requireSyncedProjects 校验显式指定的项目均已同步，返回含项目名与工作树路径的
// 清晰错误（对齐上游 NoSuchProjectError）。
func requireSyncedProjects(projects []*project.Project) error {
	errs := make([]error, 0)
	for _, p := range projects {
		if projectUnsynced(p) {
			errs = append(errs, fmt.Errorf("project %s is not synced (worktree %s does not exist)", p.Name, p.Worktree))
		}
	}
	return errors.Join(errs...)
}
