package commands

import (
	"fmt"

	"github.com/leopardxu/repo-go/internal/config"
	"github.com/leopardxu/repo-go/internal/logger"
	"github.com/leopardxu/repo-go/internal/manifest"
	"github.com/leopardxu/repo-go/internal/project"
	"github.com/leopardxu/repo-go/internal/repo_sync"
	"github.com/spf13/cobra"
)

// CheckoutOptions holds the options for the checkout command
// 简化参数结构体，与原生git-repo保持一致
type CheckoutOptions struct {
	JobsCheckout int
	Detach       bool // --detach：分离 HEAD 检出
	Quiet        bool
	Verbose      bool
	Config       *config.Config
	CommonManifestOptions
}

// CheckoutCmd creates the checkout command
func CheckoutCmd() *cobra.Command {
	opts := &CheckoutOptions{}
	cmd := &cobra.Command{
		Use:  "checkout <branchname> [<project>...]",
		Args: cobra.MinimumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return fmt.Errorf("failed to load config: %w", err)
			}
			opts.Config = cfg
			return runCheckout(opts, args)
		},
	}
	cmd.Flags().IntVarP(&opts.JobsCheckout, "jobs", "j", 8, "number of projects to checkout in parallel")
	cmd.Flags().BoolVarP(&opts.Detach, "detach", "d", false, "detach HEAD after checkout")
	cmd.Flags().BoolVarP(&opts.Quiet, "quiet", "q", false, "only show errors")
	cmd.Flags().BoolVarP(&opts.Verbose, "verbose", "v", false, "show all output")
	AddManifestFlags(cmd, &opts.CommonManifestOptions)
	return cmd
}

// runCheckout executes the checkout command logic
func runCheckout(opts *CheckoutOptions, args []string) error {
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

	if len(args) < 1 {
		return fmt.Errorf("missing branch name")
	}
	// 上游 repo checkout <branchname> [<project>...]：分支名为第一个位置参数
	branchName := args[0]
	projectNames := args[1:]
	cfg := opts.Config

	parser := manifest.NewParser()
	manifestObj, err := parser.ParseFromFile(cfg.ManifestName, manifest.SplitGroups(cfg.Groups))
	if err != nil {
		log.Error("failed to parse manifest file: %v", err)
		return fmt.Errorf("failed to parse manifest: %w", err)
	}

	manager := project.NewManagerFromManifest(manifestObj, cfg)
	var projects []*project.Project
	if len(projectNames) == 0 {
		log.Debug("获取所有项目")
		projects, err = manager.GetProjectsInGroups(nil)
		if err != nil {
			log.Error("failed to get project列表failed: %v", err)
			return fmt.Errorf("failed to get projects: %w", err)
		}
	} else {
		// 按上游语义解析项目参数（名字 + 路径回溯；"." 表示当前目录所在项目）
		log.Debug("解析项目参数: %v", projectNames)
		projects, err = manager.GetProjectsByArgs(projectNames, originalDir)
		if err != nil {
			log.Error("解析项目参数failed: %v", err)
			return fmt.Errorf("failed to get projects: %w", err)
		}
	}
	syncOpts := &repo_sync.Options{
		JobsCheckout: opts.JobsCheckout,
		Detach:       opts.Detach,
		Quiet:        opts.Quiet,
		Verbose:      opts.Verbose,
	}

	engine := repo_sync.NewEngine(syncOpts, nil, log)
	// 设置分支名称
	engine.SetBranchName(branchName)
	// 执行检出操作
	err = engine.CheckoutBranch(projects)
	if err != nil {
		log.Error("检出分支失 %v", err)
		return err
	}

	// 获取检出结
	success, failed := engine.GetCheckoutStats()

	if !opts.Quiet {
		log.Info("检出分支 '%s' 完成: %d 成功, %d failed", branchName, success, failed)
	}

	if failed > 0 {
		return fmt.Errorf("checkout failed for %d projects", failed)
	}

	return nil
}
