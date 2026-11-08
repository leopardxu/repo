package commands

import (
	"fmt"
	"os"
	"runtime"
	"strings"

	"github.com/leopardxu/repo-go/internal/config"
	"github.com/leopardxu/repo-go/internal/git"
	"github.com/leopardxu/repo-go/internal/logger"
	"github.com/leopardxu/repo-go/internal/manifest"
	"github.com/leopardxu/repo-go/internal/project"
	"github.com/leopardxu/repo-go/internal/repo_sync"
	"github.com/spf13/cobra"
)

// AbandonOptions 包含abandon命令的选项
type AbandonOptions struct {
	CommonManifestOptions
	DryRun  bool
	All     bool // 删除所有本地分支（含当前与 manifest 分支）
	Jobs    int  // 并行任务
	Verbose bool // 详细输出
	Quiet   bool // 静默模式
	Force   bool // 强制删除
}

func AbandonCmd() *cobra.Command {
	opts := &AbandonOptions{}

	cmd := &cobra.Command{
		Use:   "abandon [--all | <branchname>] [<project>...]",
		Short: "Permanently abandon a development branch",
		Long: `This subcommand permanently abandons a development branch by
deleting it (and all its history) from your local repository.

It is equivalent to "git branch -D <branchname>".

With --all every local branch in every project is abandoned.  The current
branch is detached to the manifest revision before being deleted, and the
manifest revision branch itself is abandoned too (matching upstream repo).`,
		Args: cobra.MinimumNArgs(0),
		RunE: func(_ *cobra.Command, args []string) error {
			return runAbandon(opts, args)
		},
	}

	// 添加命令行选项
	cmd.Flags().BoolVar(&opts.All, "all", false, "delete all local branches in all projects (current branch is detached first; manifest branch is deleted too)")
	cmd.Flags().IntVarP(&opts.Jobs, "jobs", "j", runtime.NumCPU()*2, "number of jobs to run in parallel (default: based on CPU cores)")
	cmd.Flags().BoolVarP(&opts.Verbose, "verbose", "v", false, "show all output")
	cmd.Flags().BoolVarP(&opts.Quiet, "quiet", "q", false, "only show errors")
	cmd.Flags().BoolVar(&opts.Force, "force", false, "force deletion of branch")
	cmd.Flags().BoolVar(&opts.DryRun, "dry-run", false, "only show what would be done")

	// 添加多清单选项
	AddManifestFlags(cmd, &opts.CommonManifestOptions)

	return cmd
}

// runAbandon 执行abandon命令
func runAbandon(opts *AbandonOptions, args []string) error {
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
		return fmt.Errorf("failed to locate repo root: %w", err)
	}
	defer func() {
		if err := RestoreWorkDir(originalDir, log); err != nil {
			log.Warn("恢复工作目录failed: %v", err)
		}
	}()

	// 参数语义对齐上游 abandon.py：
	//   - --all：位置参数全部是项目（--all 与项目组合合法，不能误当作分支名）；
	//   - 非 --all：第一个位置参数为分支名（可空格分隔多个），其余为项目。
	branchName := ""
	projectNames := []string{}
	if opts.All {
		projectNames = args
	} else {
		if len(args) == 0 {
			return fmt.Errorf("branch name required (or use --all to abandon all topic branches)")
		}
		branchName = args[0]
		projectNames = args[1:]
	}

	// 验证分支名格式（与原生git-repo保持一致）
	if !opts.All && branchName != "" {
		// 支持空格分隔的多个分支名
		branches := strings.Fields(branchName)
		for _, branch := range branches {
			if err := git.CheckRefFormat(fmt.Sprintf("heads/%s", branch)); err != nil {
				return fmt.Errorf("%s are not valid branch names", branches)
			}
		}
	}

	if !opts.Quiet {
		if opts.DryRun {
			log.Debug("将要放弃分支 '%s'", branchName)
		} else {
			log.Debug("正在放弃分支 '%s'", branchName)
		}
	}

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
	if len(projectNames) == 0 {
		log.Debug("获取所有项..")
		projects, err = manager.GetProjectsInGroups(nil)
		if err != nil {
			log.Error("failed to get projectfailed: %v", err)
			return fmt.Errorf("failed to get projects: %w", err)
		}
		// 对齐上游 GetProjects：未指定项目参数时跳过未 checkout 的项目。
		projects = filterSyncedProjects(projects)
		log.Debug("共获取到 %d 个已同步的项目", len(projects))
	} else {
		// 按上游语义解析项目参数（名字 + 路径回溯；"." 表示当前目录所在项目）
		log.Debug("解析项目参数: %v", projectNames)
		projects, err = manager.GetProjectsByArgs(projectNames, originalDir)
		if err != nil {
			log.Error("解析项目参数failed: %v", err)
			return fmt.Errorf("failed to get projects: %w", err)
		}
		if err := requireSyncedProjects(projects); err != nil {
			log.Error("指定的项目未同步: %v", err)
			return err
		}
		log.Debug("共获取到 %d 个项目", len(projects))
	}

	// 重复的项目参数只处理一次，避免同一分支被并发重复删除。
	projects = dedupeProjects(projects)

	// 创建引擎并设置选项
	log.Debug("创建同步引擎，并行任务数: %d", opts.Jobs)
	engine := repo_sync.NewEngine(&repo_sync.Options{
		JobsCheckout: opts.Jobs,
		Quiet:        opts.Quiet,
		Verbose:      opts.Verbose,
		DryRun:       opts.DryRun,
		Force:        opts.Force,
	}, manifestObj, log)

	// 执行放弃分支操作
	if !opts.Quiet {
		log.Debug("开始处理 %d 个项目的分支放弃操作...", len(projects))
	}

	// 支持空格分隔的多个分支名：逐个调用 AbandonTopics
	// --all（branchName==""）传入空 topic 表示"所有本地分支"：引擎对齐上游
	// project.GetBranches()/AbandonBranch()，当前分支先 detach 到 manifest
	// 修订版本再删除，manifest 修订版本分支本身也会被删除；dry-run 只报告不删除。
	// 若所有项目都没有任何本地分支，仍按上游报 "no project has local branch(es)"。
	branchesToAbandon := []string{branchName}
	if !opts.All && branchName != "" {
		branchesToAbandon = strings.Fields(branchName)
	}

	var allResults []repo_sync.AbandonResult
	failedCount := 0
	resultCount := 0
	for _, b := range branchesToAbandon {
		results := engine.AbandonTopics(projects, b)
		allResults = append(allResults, results...)
		for _, r := range results {
			if r.NotFound {
				// 分支不存在：不计失败（上游 AbandonBranch 返回 None）
				continue
			}
			resultCount++
			if !r.Success {
				failedCount++
			}
		}
	}

	// 输出结果汇总：payload 写 stdout，即使 -q 也不吞。
	repo_sync.PrintAbandonSummary(os.Stdout, allResults, opts.DryRun)

	// 上游语义：没有任何分支被处理（不存在或无分支可删）时报
	// "no project has local branch(es)"，不能静默成功或伪造成功行。
	if failedCount == 0 && resultCount == 0 {
		label := branchName
		if opts.All {
			// 对齐上游 ValidateOptions：--all 时报告 all local branches。
			label = "'All local branches'"
		}
		return fmt.Errorf("no project has local branch(es) : %s", label)
	}

	// 有failed则返回非 0（不再吞错误）
	if failedCount > 0 {
		return fmt.Errorf("abandon failed for %d branch/project operations", failedCount)
	}
	return nil
}
