package commands

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"sort"
	"strings"
	"sync"

	"github.com/leopardxu/repo-go/internal/config"
	"github.com/leopardxu/repo-go/internal/logger"
	"github.com/leopardxu/repo-go/internal/manifest"
	"github.com/leopardxu/repo-go/internal/project"
	"github.com/spf13/cobra"
)

// StatusOptions 包含status命令的选项
type StatusOptions struct {
	CommonManifestOptions
	Jobs      int
	Orphans   bool
	Quiet     bool
	Verbose   bool
	Branch    bool
	Unshelved bool // -u/--unshelved：上游已废弃，接受但无效果
	Config    *config.Config
}

// statusStats 用于统计状态检查结果
type statusStats struct {
	mu      sync.Mutex
	total   int
	success int
	failed  int
}

// increment 增加统计计数
func (s *statusStats) increment(success bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.total++
	if success {
		s.success++
	} else {
		s.failed++
	}
}

// StatusCmd 返回status命令
func StatusCmd() *cobra.Command {
	opts := &StatusOptions{
		Jobs: min(runtime.NumCPU(), 8), // 对齐上游 DEFAULT_LOCAL_JOBS = min(cpu,8)
	}

	cmd := &cobra.Command{
		Use:   "status [<project>...]",
		Short: "Show the working tree status",
		Long:  `Show the status of the working tree. This includes projects with uncommitted changes, projects with unpushed commits, and projects on different branches than specified in the manifest.`,
		RunE: func(_ *cobra.Command, args []string) error {
			// 创建日志记录器
			log := logger.NewDefaultLogger()

			// 根据选项设置日志级别
			if opts.Quiet {
				log.SetLevel(logger.LogLevelError)
			} else if opts.Verbose {
				log.SetLevel(logger.LogLevelDebug)
			} else {
				log.SetLevel(logger.LogLevelInfo)
			}

			return runStatus(opts, args, log)
		},
	}

	// 添加命令行选项
	cmd.Flags().IntVarP(&opts.Jobs, "jobs", "j", opts.Jobs, "number of jobs to run in parallel (default: based on number of CPU cores)")
	cmd.Flags().BoolVarP(&opts.Orphans, "orphans", "o", false, "include objects in working directory outside of repo projects")
	cmd.Flags().BoolVarP(&opts.Branch, "branch", "b", false, "display the current branch name")
	cmd.Flags().BoolVarP(&opts.Unshelved, "unshelved", "u", false, "show unshelved changes (deprecated, no effect)")
	cmd.Flags().BoolVarP(&opts.Quiet, "quiet", "q", false, "only show errors")
	cmd.Flags().BoolVarP(&opts.Verbose, "verbose", "v", false, "show all output including debug logs")
	AddManifestFlags(cmd, &opts.CommonManifestOptions)

	return cmd
}

// runStatus 执行status命令
func runStatus(opts *StatusOptions, args []string, log logger.Logger) error {
	// 创建统计对象
	stats := &statusStats{}

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

	// 加载配置
	cfg, err := config.Load()
	if err != nil {
		log.Error("failed to load config: %v", err)
		return fmt.Errorf("failed to load config: %w", err)
	}
	opts.Config = cfg

	// 加载清单
	log.Debug("正在加载清单文件: %s", cfg.ManifestName)
	parser := manifest.NewParser()
	manifest, err := parser.ParseFromFile(cfg.ManifestName, manifest.SplitGroups(cfg.Groups))
	if err != nil {
		log.Error("failed to parse manifest: %v", err)
		return fmt.Errorf("failed to parse manifest: %w", err)
	}
	log.Debug("成功加载清单，包含 %d 个项目", len(manifest.Projects))

	// 创建项目管理器
	log.Debug("正在初始化项目管理器...")
	manager := project.NewManagerFromManifest(manifest, cfg)

	// 获取要处理的项目
	var projects []*project.Project

	if len(args) == 0 {
		// 如果没有指定项目，则处理所有项目
		log.Debug("获取所有项目..")
		projects, err = manager.GetProjectsInGroups(nil)
		if err != nil {
			log.Error("failed to get projectfailed: %v", err)
			return fmt.Errorf("failed to get projects: %w", err)
		}
		log.Debug("共获取到 %d 个项目", len(projects))
	} else {
		// 否则，只处理指定的项目
		log.Debug("根据名称failed to get project: %v", args)
		projects, err = manager.GetProjectsByNames(args)
		if err != nil {
			log.Error("根据名称failed to get projectfailed: %v", err)
			return fmt.Errorf("failed to get projects by name: %w", err)
		}
		log.Debug("共获取到 %d 个项目", len(projects))
	}

	// 使用goroutine池并发failed to get project状态
	// 规范化并发数：-j <= 0 会导致 makechan panic，回退到默认 8（与其他命令一致）
	if opts.Jobs <= 0 {
		opts.Jobs = 8
	}
	log.Debug("开始检查项目状态，并行任务数 %d...", opts.Jobs)

	type statusResult struct {
		Project *project.Project
		Branch  string
		Status  string
		Err     error
	}

	var wg sync.WaitGroup
	results := make(chan statusResult, len(projects))
	sem := make(chan struct{}, opts.Jobs) // 使用信号量控制并发数

	for _, p := range projects {
		p := p // 创建副本避免闭包问题
		wg.Add(1)
		sem <- struct{}{} // 获取信号

		go func() {
			defer wg.Done()
			defer func() { <-sem }() // 释放信号

			log.Debug("正在检查项目 %s 的状态...", p.Name)

			status, err := p.GetStatus()
			if err != nil {
				log.Error("failed to get project %s 状态failed %v", p.Name, err)
				stats.increment(false)
			} else {
				stats.increment(true)
				log.Debug("项目 %s 状态检查完成", p.Name)
			}

			// -b/--branch：获取当前分支名用于显示
			var branch string
			if opts.Branch {
				if bOut, bErr := p.GitRepo.RunCommand("rev-parse", "--abbrev-ref", "HEAD"); bErr == nil {
					branch = strings.TrimSpace(string(bOut))
				}
			}

			results <- statusResult{
				Project: p,
				Branch:  branch,
				Status:  status,
				Err:     err,
			}
		}()
	}

	// 启动一个goroutine 来关闭结果通道
	go func() {
		wg.Wait()
		close(results)
	}()

	// 处理结果
	var errs []error
	for res := range results {
		if res.Err != nil {
			errs = append(errs, fmt.Errorf("项目 %s: %w", res.Project.Name, res.Err))
			continue
		}

		// 默认跳过无变更的项目以减少噪声；--verbose 显示全部；-b 显示所有项目的分支
		if strings.TrimSpace(res.Status) == "" && !opts.Verbose && !opts.Branch {
			continue
		}
		// 输出格式对齐上游 repo status：project <path>/ 头，可选 branch 行，再是状态输出
		log.Info("project %s/", res.Project.Path)
		if opts.Branch && res.Branch != "" {
			log.Info("branch %s", res.Branch)
		}
		if s := strings.TrimRight(res.Status, "\n"); s != "" {
			log.Info("%s", s)
		}
	}

	// --orphans：列出 repoRoot 顶层中不属于任何 manifest 项目的目录（工作树孤儿）
	if opts.Orphans {
		relPaths := make([]string, 0, len(manifest.Projects))
		for _, mp := range manifest.Projects {
			rel := mp.Path
			if rel == "" {
				rel = mp.Name
			}
			relPaths = append(relPaths, rel)
		}
		if orphans, oerr := computeOrphanDirs(cfg.RepoRoot, relPaths); oerr != nil {
			log.Warn("扫描 orphan 目录failed: %v", oerr)
		} else if len(orphans) > 0 {
			log.Info("orphan projects (in working tree, not in manifest):")
			for _, name := range orphans {
				log.Info("project %s/", name)
			}
		}
	}

	// 显示统计信息
	log.Info("状态检查操作完成，总计: %d，成功 %d，failed %d", stats.total, stats.success, stats.failed)

	// 如果有错误，返回汇总错误
	if len(errs) > 0 {
		return fmt.Errorf("%d projects failed: %w", len(errs), errors.Join(errs...))
	}

	return nil
}

// computeOrphanDirs 列出 repoRoot 顶层目录中不属于任何 manifest 项目的目录
// （即"孤儿"工作树目录）。一个顶层目录被视为被项目覆盖当且仅当它与某项目相对
// 路径相等，或是某项目相对路径的前缀段（支持 "external/foo" 这类嵌套项目）。
// 始终跳过 .repo。结果按目录名排序以保证确定性。
func computeOrphanDirs(repoRoot string, projectRelPaths []string) ([]string, error) {
	entries, err := os.ReadDir(repoRoot)
	if err != nil {
		return nil, err
	}
	var orphans []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if name == ".repo" {
			continue
		}
		covered := false
		for _, rel := range projectRelPaths {
			if rel == name || strings.HasPrefix(rel, name+"/") {
				covered = true
				break
			}
		}
		if !covered {
			orphans = append(orphans, name)
		}
	}
	sort.Strings(orphans)
	return orphans, nil
}
