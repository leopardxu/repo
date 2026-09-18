package commands

import (
	"fmt"
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
	manifestObj, err := parser.ParseFromFile(cfg.ManifestName, manifest.SplitGroups(cfg.Groups))
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
		log.Debug("共获取到 %d 个项目", len(projects))
	} else {
		log.Debug("根据名称failed to get project: %v", args)
		projects, err = manager.GetProjectsByNames(args)
		if err != nil {
			log.Error("根据名称failed to get projectfailed: %v", err)
			return fmt.Errorf("failed to get projects by names: %w", err)
		}
		log.Debug("共获取到 %d 个项目", len(projects))
	}

	// --set-upstream：为各项目的当前分支设置上游跟踪关系（repo-go 扩展选项）
	if opts.SetUpstream != "" {
		for _, p := range projects {
			remoteName := p.RemoteName
			if remoteName == "" {
				remoteName = "origin"
			}
			upstreamRef := fmt.Sprintf("%s/%s", remoteName, opts.SetUpstream)
			if _, err := p.GitRepo.RunCommand("branch", "--set-upstream-to", upstreamRef); err != nil {
				log.Warn("项目 %s 设置上游跟踪failed: %v", p.Name, err)
			} else if !opts.Quiet {
				log.Debug("项目 %s 已设置上游跟踪到 %s", p.Name, upstreamRef)
			}
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

			currentBranchBytes, err := p.GitRepo.RunCommand("rev-parse", "--abbrev-ref", "HEAD")
			if err != nil {
				log.Error("failed to get project %s 的当前分支failed: %v", p.Name, err)
				results <- branchResult{ProjectName: p.Name, Err: err}
				return
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

			currentBranch := strings.TrimSpace(string(currentBranchBytes))
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

	// 收集结果
	for res := range results {
		if res.Err != nil {
			failCount++
			log.Error("failed to get project %s 的分支信息failed: %v", res.ProjectName, res.Err)
			continue
		}

		successCount++
		currentBranches[res.CurrentBranch] = true

		for _, branch := range res.Branches {
			branch = strings.TrimSpace(branch)
			if branch == "" {
				continue
			}

			// 处理分支名称，移除前导的 '*' 或空格
			if strings.HasPrefix(branch, "* ") {
				branch = strings.TrimPrefix(branch, "* ")
			} else if strings.HasPrefix(branch, "  ") {
				branch = strings.TrimPrefix(branch, "  ")
			}

			branchInfo[branch] = append(branchInfo[branch], res.ProjectName)
		}
	}

	log.Debug("共处理 %d 个项目，成功: %d，failed: %d", len(projects), successCount, failCount)
	// 对分支名称进行排序，以便有序显示
	var branchNames []string
	for branch := range branchInfo {
		branchNames = append(branchNames, branch)
	}
	sort.Strings(branchNames)

	// 显示分支信息
	if !opts.Quiet {
		// 完成进度
		prog.Finish("")

		// 创建颜色输出器
		useColor := color.ShouldUseColor(opts.Color)
		coloring := color.NewBranchColoring(useColor)

		log.Info("分支信息汇总")

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
				// 当前分支用绿色显示
				branchText = coloring.Current(branch)
			} else {
				// 普通分支
				branchText = coloring.Local(branch)
			}

			// 注意：log.Info 的第二个参数是普通参数而非格式串，
			// 无需对 % 转义（转义会导致输出中出现字面 %%）
			branchDisplay := branchText

			if len(projs) == len(projects) {
				log.Info("%s %-30s | 所有项目", prefix, branchDisplay)
			} else {
				log.Info("%s %-30s | 在项目 %s", prefix, branchDisplay, strings.Join(projs, ", "))
			}
		}

		log.Info("\n共有 %d 个分支", len(branchNames))
	}

	return nil
}
