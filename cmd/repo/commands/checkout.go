package commands

import (
	"fmt"
	"io"
	"os"
	"runtime"

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
	// 显式 --groups 优先于 config 中的 groups（其他命令同语义）
	cmd.Flags().StringVarP(&opts.Groups, "groups", "g", "", "restrict to projects matching the specified groups")
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

	// 组过滤：显式 --groups 优先，否则使用配置的有效组（未配置时归一为
	// "default"，与其他命令一致）；一次性解析清单，避免解析后再用另一套组二次过滤。
	groups := cfg.EffectiveGroups()
	if opts.Groups != "" {
		groups = opts.Groups
	}
	parser := manifest.NewParser()
	manifestObj, err := parser.ParseFromFile(cfg.ManifestName, manifest.SplitGroups(groups))
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
		// 对齐上游 GetProjects：未指定项目参数时跳过未 checkout 的项目。
		projects = filterSyncedProjects(projects)
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
	}
	// 重复的项目参数只处理一次，避免同一项目被并发检出两次。
	projects = dedupeProjects(projects)

	// 并发数归一化：0/负数会让 engine 里 make(chan struct{}, JobsCheckout) panic，
	// 统一回退到 CPU 数（至少 1）。
	jobs := normalizeCheckoutJobs(opts.JobsCheckout)
	syncOpts := &repo_sync.Options{
		JobsCheckout: jobs,
		Detach:       opts.Detach,
		Quiet:        opts.Quiet,
		Verbose:      opts.Verbose,
	}

	engine := repo_sync.NewEngine(syncOpts, nil, log)
	// 设置分支名称
	engine.SetBranchName(branchName)
	// 执行检出操作
	runErr := engine.CheckoutBranch(projects)

	// 获取检出统计并输出摘要。摘要属于结果 payload：写 stdout，即使 -q 也
	// 不吞；诊断日志才受 -q 影响。部分失败时也要先给出摘要。
	success, failed := engine.GetCheckoutStats()
	printCheckoutSummary(os.Stdout, branchName, success, failed)

	if runErr != nil {
		log.Error("检出分支失 %v", runErr)
		return runErr
	}

	if failed > 0 {
		return fmt.Errorf("checkout failed for %d projects", failed)
	}

	return nil
}

// printCheckoutSummary 把检出摘要写到 w（结果 payload，调用方传 os.Stdout）。
// 该输出不受 -q 影响。
func printCheckoutSummary(w io.Writer, branchName string, success, failed int) {
	fmt.Fprintf(w, "检出分支 '%s' 完成: %d 成功, %d failed\n", branchName, success, failed)
}

// normalizeCheckoutJobs 归一化检出并发数：
// <=0 时回退到 CPU 数（至少 1），避免 engine 中 make(chan struct{}, 0/负数) panic 或死锁。
func normalizeCheckoutJobs(jobs int) int {
	if jobs > 0 {
		return jobs
	}
	n := runtime.NumCPU()
	if n < 1 {
		n = 1
	}
	return n
}
