package commands

import (
	"fmt"
	"runtime"
	"strings"
	"sync"

	"github.com/leopardxu/repo-go/internal/config"
	"github.com/leopardxu/repo-go/internal/git"
	"github.com/leopardxu/repo-go/internal/logger"
	"github.com/leopardxu/repo-go/internal/manifest"
	"github.com/leopardxu/repo-go/internal/project"
	"github.com/spf13/cobra"
)

// InfoOptions 包含info命令的选项
type InfoOptions struct {
	Diff            bool
	Overview        bool
	CurrentBranch   bool
	NoCurrentBranch bool
	LocalOnly       bool
	Jobs            int
	Verbose         bool
	Quiet           bool
	Config          *config.Config
	CommonManifestOptions
}

// infoStats 用于统计info命令的执行结果
type infoStats struct {
	mu      sync.Mutex
	success int
	failed  int
}

// InfoCmd 返回info命令
func InfoCmd() *cobra.Command {
	opts := &InfoOptions{}

	cmd := &cobra.Command{
		Use:   "info [-dl] [-o [-c]] [<project>...]",
		Short: "Get info on the manifest branch, current branch or unmerged branches",
		Long:  `Show detailed information about projects including branch info.`,
		RunE: func(_ *cobra.Command, args []string) error {
			return runInfo(opts, args)
		},
	}

	// 添加命令行选项
	cmd.Flags().BoolVarP(&opts.Diff, "diff", "d", false, "show full info and commit diff including remote branches")
	cmd.Flags().BoolVarP(&opts.Overview, "overview", "o", false, "show overview of all local commits")
	cmd.Flags().BoolVarP(&opts.CurrentBranch, "current-branch", "c", false, "consider only checked out branches")
	cmd.Flags().BoolVar(&opts.NoCurrentBranch, "no-current-branch", false, "consider all local branches")
	cmd.Flags().BoolVarP(&opts.LocalOnly, "local-only", "l", false, "disable all remote operations")
	cmd.Flags().IntVarP(&opts.Jobs, "jobs", "j", min(runtime.NumCPU(), 8), "number of jobs to run in parallel")
	cmd.Flags().BoolVarP(&opts.Verbose, "verbose", "v", false, "show all output")
	cmd.Flags().BoolVarP(&opts.Quiet, "quiet", "q", false, "only show errors")
	AddManifestFlags(cmd, &opts.CommonManifestOptions)
	return cmd
}

// runInfo 执行info命令
// runInfo executes the info command logic
func runInfo(opts *InfoOptions, args []string) error {
	// 初始化日志记录器
	log := logger.NewDefaultLogger()
	if opts.Verbose {
		log.SetLevel(logger.LogLevelDebug)
	} else if opts.Quiet {
		log.SetLevel(logger.LogLevelError)
	} else {
		log.SetLevel(logger.LogLevelInfo)
	}

	log.Debug("Starting info command")

	// -c/--current-branch 与 --no-current-branch 互斥（前者仅当前分支，后者所有本地分支）
	if opts.CurrentBranch && opts.NoCurrentBranch {
		return fmt.Errorf("cannot specify both --current-branch and --no-current-branch")
	}

	// 确保在repo根目录下执行
	originalDir, err := EnsureRepoRoot(log)
	if err != nil {
		log.Error("Failed to locate repo root: %v", err)
		return err
	}
	defer func() {
		if err := RestoreWorkDir(originalDir, log); err != nil {
			log.Warn("恢复工作目录failed: %v", err)
		}
	}()

	// 加载配置
	cfg, err := config.Load() // 声明err
	if err != nil {
		log.Error("Failed to load config: %v", err)
		return err
	}
	opts.Config = cfg // 分配加载的配置

	// 加载manifest
	log.Debug("Loading manifest from %s", cfg.ManifestName)
	parser := manifest.NewParser()
	manifestObj, err := parser.ParseFromFile(cfg.ManifestName, manifest.SplitGroups(cfg.Groups)) // 重用err
	if err != nil {
		log.Error("Failed to parse manifest: %v", err)
		return err
	}

	// 创建项目管理器
	manager := project.NewManagerFromManifest(manifestObj, cfg)

	// 声明projects变量
	var projects []*project.Project

	// 获取要操作的项目
	if len(args) == 0 {
		log.Debug("Getting all projects")
		projects, err = manager.GetProjectsInGroups(nil) // 使用=，使用nil
		if err != nil {
			log.Error("Failed to get projects: %v", err)
			return err
		}
	} else {
		log.Debug("Getting projects by names: %v", args)
		projects, err = manager.GetProjectsByNames(args) // 使用=
		if err != nil {
			log.Error("Failed to get projects by name: %v", err)
			return err
		}
	}

	log.Debug("Found %d projects to process", len(projects))

	// -c/--current-branch（默认）：仅显示当前分支（默认模式）或仅当前分支提交（-o）
	// --no-current-branch：列出所有本地分支（默认模式）或考虑所有本地分支提交（-o --all）
	// --local-only：禁用远程操作（不解析 @{upstream}）
	if opts.LocalOnly {
		log.Debug("--local-only：禁用远程操作")
	}
	if opts.NoCurrentBranch {
		log.Debug("--no-current-branch：考虑所有本地分支")
	} else {
		log.Debug("-c：仅考虑当前分支")
	}

	// 显示 manifest 级信息（对齐上游 repo info 头部）
	log.Debug("Manifest URL: %s", cfg.ManifestURL)
	log.Debug("Manifest branch: %s", cfg.ManifestBranch)
	if shaOut, shaErr := git.NewRunner().RunInDir(".repo/manifests", "rev-parse", "HEAD"); shaErr == nil {
		if sha := strings.TrimSpace(string(shaOut)); sha != "" {
			log.Debug("Manifest SHA: %s", sha)
		}
	}

	// 并发failed to get project信息
	type infoResult struct {
		Project *project.Project
		Output  string
		Err     error
	}

	results := make(chan infoResult, len(projects))
	jobs := opts.Jobs
	if jobs < 1 {
		jobs = 8
	}
	sem := make(chan struct{}, jobs) // 控制并发数（-j）
	var wg sync.WaitGroup
	stats := &infoStats{}

	for _, p := range projects {
		wg.Add(1)
		sem <- struct{}{}
		go func(proj *project.Project) {
			defer func() {
				<-sem
				wg.Done()
			}()

			var output string
			var err error
			var outputBytes []byte

			log.Debug("Processing project %s", proj.Name)

			// 根据选项显示不同信息
			// -c/--current-branch 影响：默认模式仅显示当前分支、-o 模式仅当前分支提交；
			// --no-current-branch 则列出所有本地分支 / 给 log 追加 --all。
			// --local-only 禁用远程操作（diff 模式下不解析 @{upstream}）
			switch {
			case opts.Diff:
				log.Debug("Getting diff for project %s", proj.Name)
				// --local-only 时仅显示 HEAD 提交，不与 @{upstream} 比较
				if opts.LocalOnly {
					outputBytes, err = proj.GitRepo.RunCommand("log", "--oneline", "-10")
				} else {
					outputBytes, err = proj.GitRepo.RunCommand("log", "--oneline", "HEAD..@{upstream}")
				}
				if err == nil {
					output = strings.TrimSpace(string(outputBytes))
				}
			case opts.Overview:
				log.Debug("Getting overview for project %s", proj.Name)
				// --no-current-branch：考虑所有本地分支（追加 --all）
				logArgs := []string{"log", "--oneline", "-10"}
				if opts.NoCurrentBranch {
					logArgs = append(logArgs, "--all")
				}
				outputBytes, err = proj.GitRepo.RunCommand(logArgs...)
				if err == nil {
					output = strings.TrimSpace(string(outputBytes))
				}
			default:
				// 默认显示项目元数据（对齐上游 repo info）
				// -c/--current-branch（默认）：仅当前分支；--no-current-branch：列出所有本地分支
				log.Debug("Getting metadata for project %s", proj.Name)
				remoteName := proj.RemoteName
				if remoteName == "" {
					remoteName = "origin"
				}
				var sb strings.Builder
				fmt.Fprintf(&sb, "Project: %s\n", proj.Name)
				fmt.Fprintf(&sb, "Path: %s\n", proj.Path)
				fmt.Fprintf(&sb, "Remote name: %s\n", remoteName)
				fmt.Fprintf(&sb, "Manifest revision: %s\n", proj.Revision)
				if opts.NoCurrentBranch {
					if branches, bErr := proj.ListLocalBranches(); bErr == nil && len(branches) > 0 {
						fmt.Fprintf(&sb, "Local branches: %s\n", strings.Join(branches, ", "))
					}
				} else {
					currentBranch := ""
					if bOut, bErr := proj.GitRepo.RunCommand("rev-parse", "--abbrev-ref", "HEAD"); bErr == nil {
						currentBranch = strings.TrimSpace(string(bOut))
					}
					if currentBranch != "" {
						fmt.Fprintf(&sb, "Current branch: %s\n", currentBranch)
					}
				}
				output = strings.TrimRight(sb.String(), "\n")
			}

			results <- infoResult{Project: proj, Output: output, Err: err}
		}(p)
	}

	// 等待所有goroutine完成后关闭结果通道
	go func() {
		wg.Wait()
		close(results)
	}()

	// 收集并显示结果
	for res := range results {
		if res.Err != nil {
			stats.mu.Lock()
			stats.failed++
			stats.mu.Unlock()

			log.Error("Error getting info for %s: %v", res.Project.Name, res.Err)
			continue
		}

		stats.mu.Lock()
		stats.success++
		stats.mu.Unlock()

		if res.Output != "" {
			log.Info("--- %s ---\n%s", res.Project.Name, res.Output)
		} else if !opts.Quiet {
			log.Info("--- %s ---\n(No changes)", res.Project.Name)
		}
	}

	// 显示统计信息
	log.Debug("Info command completed: %d successful, %d failed", stats.success, stats.failed)

	if stats.failed > 0 {
		return fmt.Errorf("%d projects failed", stats.failed)
	}

	return nil
}
