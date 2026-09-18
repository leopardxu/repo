package commands

import (
	"fmt"

	"sync"

	"github.com/leopardxu/repo-go/internal/config"
	"github.com/leopardxu/repo-go/internal/logger"
	"github.com/leopardxu/repo-go/internal/manifest"
	"github.com/leopardxu/repo-go/internal/project"
	"github.com/spf13/cobra"
)

// RebaseOptions 包含rebase命令的选项
type RebaseOptions struct {
	CommonManifestOptions
	Abort       bool
	Continue    bool
	Skip        bool
	Interactive bool
	Autosquash  bool
	Onto        string
	Force       bool
	FailFast    bool
	AutoStash   bool
	Whitespace  string
	Verbose     bool
	Quiet       bool
	Jobs        int
}

// rebaseStats 用于统计rebase命令的执行结果
type rebaseStats struct {
	mu      sync.Mutex
	success int
	failed  int
	total   int
}

// RebaseCmd 返回rebase命令
func RebaseCmd() *cobra.Command {
	opts := &RebaseOptions{}

	cmd := &cobra.Command{
		Use:   "rebase {[<project>...] | -i <project>...}",
		Short: "Rebase local branches on upstream branch",
		Long: `'repo rebase' uses git rebase to move local changes in the current topic branch
to the HEAD of the upstream history, useful when you have made commits in a
topic branch but need to incorporate new upstream changes "underneath" them.`,
		RunE: func(_ *cobra.Command, args []string) error {
			return runRebase(opts, args)
		},
	}

	// 添加命令行选项
	cmd.Flags().BoolVar(&opts.Abort, "abort", false, "abort current rebase")
	cmd.Flags().BoolVar(&opts.Continue, "continue", false, "continue current rebase")
	cmd.Flags().BoolVar(&opts.Skip, "skip", false, "skip current patch and continue")
	cmd.Flags().BoolVarP(&opts.Interactive, "interactive", "i", false, "interactive rebase")
	cmd.Flags().BoolVar(&opts.Autosquash, "autosquash", false, "automatically squash fixup commits")
	cmd.Flags().StringVar(&opts.Onto, "onto", "", "rebase onto given branch instead of upstream")
	cmd.Flags().BoolVarP(&opts.Force, "force-rebase", "f", false, "force rebase even if branch is up to date (passes --force-rebase to git rebase)")
	cmd.Flags().BoolVar(&opts.FailFast, "fail-fast", false, "stop rebasing after first error is hit")
	cmd.Flags().BoolVar(&opts.AutoStash, "auto-stash", false, "stash local modifications before starting")
	cmd.Flags().StringVar(&opts.Whitespace, "whitespace", "", "pass --whitespace to git rebase")
	cmd.Flags().BoolVarP(&opts.Verbose, "verbose", "v", false, "show all output")
	cmd.Flags().BoolVarP(&opts.Quiet, "quiet", "q", false, "only show errors")
	AddManifestFlags(cmd, &opts.CommonManifestOptions)
	cmd.Flags().IntVarP(&opts.Jobs, "jobs", "j", 8, "number of jobs to run in parallel")

	return cmd
}

// runRebase 执行rebase命令
func runRebase(opts *RebaseOptions, args []string) error {
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

	log.Debug("开始执行rebase操作")

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

	// 子清单作用域 flag（--outer-manifest/--this-manifest-only/--all-manifests）已接受，
	// 但 repo-go 在 init 时合并所有 include 为扁平 manifest，未保留 manifest 树，
	// 故不在此按层做差异化处理（统一作用于全部已合并项目）。详见 AddManifestFlags 注释。

	// 创建项目管理器
	log.Debug("正在创建项目管理器...")
	manager := project.NewManagerFromManifest(manifestObj, cfg)

	var projects []*project.Project

	// failed to get project列表
	log.Debug("正在failed to get project列表...")
	if len(args) == 0 {
		log.Debug("获取所有项目")
		projects, err = manager.GetProjectsInGroups(nil)
		if err != nil {
			log.Error("获取所有项目失 %v", err)
			return fmt.Errorf("failed to get projects: %w", err)
		}
	} else {
		log.Debug("获取指定的项 %v", args)
		projects, err = manager.GetProjectsByNames(args)
		if err != nil {
			log.Error("获取指定项目failed: %v", err)
			return fmt.Errorf("failed to get projects by name: %w", err)
		}
	}

	log.Debug("找到 %d 个项目需要执行rebase操作", len(projects))

	// 构建rebase命令选项
	log.Debug("构建rebase命令选项...")
	// 基础参数（不含上游，上游按项目在 goroutine 内追加）
	baseArgs := []string{"rebase"}

	if opts.Abort {
		baseArgs = append(baseArgs, "--abort")
		log.Debug("添加--abort选项")
	} else if opts.Continue {
		baseArgs = append(baseArgs, "--continue")
		log.Debug("添加--continue选项")
	} else if opts.Skip {
		baseArgs = append(baseArgs, "--skip")
		log.Debug("添加--skip选项")
	} else {
		if opts.Interactive {
			baseArgs = append(baseArgs, "--interactive")
			log.Debug("添加--interactive选项")
		}

		if opts.Autosquash {
			baseArgs = append(baseArgs, "--autosquash")
			log.Debug("添加--autosquash选项")
		}

		if opts.Onto != "" {
			baseArgs = append(baseArgs, "--onto", opts.Onto)
			log.Debug("添加--onto %s选项", opts.Onto)
		}

		// --force-rebase 为 git rebase 原生支持的参数，透传给 git rebase。
		if opts.Force {
			baseArgs = append(baseArgs, "--force-rebase")
		}

		if opts.Whitespace != "" {
			baseArgs = append(baseArgs, "--whitespace", opts.Whitespace)
			log.Debug("添加--whitespace %s选项", opts.Whitespace)
		}

		if opts.AutoStash {
			baseArgs = append(baseArgs, "--autostash")
			log.Debug("添加--autostash选项")
		}

		// -q 透传给 git rebase
		if opts.Quiet {
			baseArgs = append(baseArgs, "--quiet")
		}
	}

	// 创建统计对象
	stats := &rebaseStats{total: len(projects)}

	// 并发执行rebase操作
	log.Debug("开始并发执行rebase操作...")
	type rebaseResult struct {
		Project *project.Project
		Output  string
		Err     error
	}

	// 设置并发控制
	maxWorkers := opts.Jobs
	if maxWorkers <= 0 {
		maxWorkers = 8
	}
	// 交互式 rebase 需要 TTY/编辑器，并发会产生交错或failed，强制串行。
	if opts.Interactive {
		maxWorkers = 1
	}
	log.Debug("设置并发数为: %d", maxWorkers)

	results := make(chan rebaseResult, len(projects))
	sem := make(chan struct{}, maxWorkers)
	var wg sync.WaitGroup

	for _, p := range projects {
		wg.Add(1)
		p := p
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()

			log.Debug("正在对项目 %s 执行rebase操作...", p.Name)

			// 按项目构建完整 rebase 参数：基础参数 + 上游 <remote>/<revision>
			// 上游按项目不同，故必须在 goroutine 内追加
			rebaseArgs := make([]string, len(baseArgs))
			copy(rebaseArgs, baseArgs)
			if !opts.Abort && !opts.Continue && !opts.Skip {
				remoteName := p.RemoteName
				if remoteName == "" {
					remoteName = "origin"
				}
				revision := p.Revision
				if revision != "" {
					upstreamRef := fmt.Sprintf("%s/%s", remoteName, revision)
					rebaseArgs = append(rebaseArgs, upstreamRef)
					log.Debug("项目 %s rebase 上游: %s", p.Name, upstreamRef)
				}
			}

			outputBytes, err := p.GitRepo.RunCommand(rebaseArgs...)
			output := string(outputBytes)

			if err != nil {
				// 显示详细的错误信息
				log.Error("项目 %s rebasefailed: %v", p.Name, err)
				if output != "" {
					log.Error("Git 输出:\n%s", output)
				}

				// 更新统计信息
				stats.mu.Lock()
				stats.failed++
				stats.mu.Unlock()
			} else {
				if !opts.Quiet {
					log.Debug("项目 %s rebase成功", p.Name)
				}

				// 更新统计信息
				stats.mu.Lock()
				stats.success++
				stats.mu.Unlock()
			}

			results <- rebaseResult{
				Project: p,
				Output:  output,
				Err:     err,
			}
		}()
	}

	// 等待所有goroutine完成
	log.Debug("等待所有rebase任务完成...")
	wg.Wait()
	close(results)

	// 处理结果
	log.Debug("处理rebase结果...")
	var hasError bool
	var failedProjects []string

	// 收集所有结
	for res := range results {
		if res.Err != nil {
			hasError = true
			failedProjects = append(failedProjects, res.Project.Name)

			if opts.Verbose || !opts.Quiet {
				log.Error("项目 %s rebasefailed", res.Project.Name)
				if res.Output != "" {
					log.Error("Git 输出:\n%s", res.Output)
				}
			}

			if opts.FailFast {
				log.Error("由于设置了fail-fast选项，在首次错误后停止")
				return fmt.Errorf("failed to rebase project %s: %w", res.Project.Name, res.Err)
			}
			continue
		}

		if !opts.Quiet {
			log.Debug("\n项目 %s:", res.Project.Name)
			if res.Output != "" {
				log.Info("%s", res.Output)
			} else {
				log.Info("Rebase完成")
			}
		}
	}

	// 输出统计信息
	log.Info("Rebase操作完成: 总计 %d 个项目, 成功 %d 个, failed %d 个",
		stats.total, stats.success, stats.failed)

	if hasError {
		log.Error("以下项目 rebase failed:")
		for _, name := range failedProjects {
			log.Error("  - %s", name)
		}
		log.Error("\n请检查错误信息并手动解决冲突，然后使用 'repo rebase --continue' 继续")
		return fmt.Errorf("some projects failed to rebase")
	}
	return nil
}
