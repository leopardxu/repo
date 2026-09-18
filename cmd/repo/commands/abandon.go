package commands

import (
	"fmt"
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
	All     bool // 删除所有分
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

It is equivalent to "git branch -D <branchname>".`,
		Args: cobra.MinimumNArgs(0),
		RunE: func(_ *cobra.Command, args []string) error {
			return runAbandon(opts, args)
		},
	}

	// 添加命令行选项
	cmd.Flags().BoolVar(&opts.All, "all", false, "delete all branches in all projects")
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

	branchName := ""
	if len(args) > 0 {
		branchName = args[0]
	}
	projectNames := []string{}
	if len(args) > 1 {
		projectNames = args[1:]
	}

	// 无分支名且未指定 --all 时报错：否则会向 AbandonTopics 传入空 topic，
	// 触发"删除所有项目所有本地分支"路径，造成数据丢失。
	if opts.All && branchName != "" {
		return fmt.Errorf("--all cannot be combined with a branch name")
	}
	if !opts.All && branchName == "" {
		return fmt.Errorf("branch name required (or use --all to abandon all topic branches)")
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
	manifestObj, err := parser.ParseFromFile(cfg.ManifestName, manifest.SplitGroups(cfg.Groups))
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
		log.Debug("共获取到 %d 个项目", len(projects))
	} else {
		// 按上游语义解析项目参数（名字 + 路径回溯；"." 表示当前目录所在项目）
		log.Debug("解析项目参数: %v", projectNames)
		projects, err = manager.GetProjectsByArgs(projectNames, originalDir)
		if err != nil {
			log.Error("解析项目参数failed: %v", err)
			return fmt.Errorf("failed to get projects: %w", err)
		}
		log.Debug("共获取到 %d 个项目", len(projects))
	}

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
	branchesToAbandon := []string{branchName}
	if !opts.All && branchName != "" {
		branchesToAbandon = strings.Fields(branchName)
	}

	var allResults []repo_sync.AbandonResult
	failedCount := 0
	notFoundCount := 0
	resultCount := 0
	for _, b := range branchesToAbandon {
		results := engine.AbandonTopics(projects, b)
		allResults = append(allResults, results...)
		for _, r := range results {
			if r.NotFound {
				// 分支不存在：不计失败（上游 AbandonBranch 返回 None）
				notFoundCount++
				continue
			}
			resultCount++
			if !r.Success {
				failedCount++
			}
		}
	}

	// 输出结果汇总
	repo_sync.PrintAbandonSummary(allResults, opts.DryRun, log)

	// 上游语义：所有项目都没有该分支时报 "no project has local branch(es)"
	if failedCount == 0 && resultCount == 0 && notFoundCount > 0 {
		return fmt.Errorf("no project has local branch(es) : %s", branchName)
	}

	// 有failed则返回非 0（不再吞错误）
	if failedCount > 0 {
		return fmt.Errorf("abandon failed for %d branch/project operations", failedCount)
	}
	return nil
}
