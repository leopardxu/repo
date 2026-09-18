package commands

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"
	"sync"

	"github.com/leopardxu/repo-go/internal/config"
	"github.com/leopardxu/repo-go/internal/logger"
	"github.com/leopardxu/repo-go/internal/manifest"
	"github.com/leopardxu/repo-go/internal/project"
	"github.com/spf13/cobra"
)

// StageOptions 包含stage命令的选项
type StageOptions struct {
	All              bool
	Interactive      bool
	Verbose          bool
	Quiet            bool
	OuterManifest    bool
	NoOuterManifest  bool
	ThisManifestOnly bool
	Patch            bool
	Force            bool
	Update           bool // -u/--update：更新已跟踪文件
	Jobs             int
	Config           *config.Config
	CommonManifestOptions
}

// stageStats 用于统计暂存结果
type stageStats struct {
	mu      sync.Mutex
	total   int
	success int
	failed  int
}

// increment 增加统计计数
func (s *stageStats) increment(success bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.total++
	if success {
		s.success++
	} else {
		s.failed++
	}
}

// StageCmd 返回stage命令
func StageCmd() *cobra.Command {
	opts := &StageOptions{
		Jobs: runtime.NumCPU() * 2,
	}

	cmd := &cobra.Command{
		Use:   "stage [<project>...] [<file>...]",
		Short: "Stage file contents to the index",
		Long:  `Stage file contents to the index (equivalent to 'git add').`,
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

			// 加载配置
			cfg, err := config.Load()
			if err != nil {
				log.Error("failed to load config: %v", err)
				return fmt.Errorf("failed to load config: %w", err)
			}
			opts.Config = cfg

			return runStage(opts, args, log)
		},
	}

	// 添加命令行选项
	cmd.Flags().BoolVarP(&opts.All, "all", "A", false, "stage all files")
	cmd.Flags().BoolVarP(&opts.Interactive, "interactive", "i", false, "interactive staging")
	cmd.Flags().BoolVarP(&opts.Verbose, "verbose", "v", false, "show all output including debug logs")
	cmd.Flags().BoolVarP(&opts.Quiet, "quiet", "q", false, "only show errors")
	cmd.Flags().BoolVarP(&opts.Patch, "patch", "p", false, "select hunks interactively")
	cmd.Flags().BoolVarP(&opts.Force, "force", "f", false, "allow adding otherwise ignored files")
	cmd.Flags().BoolVarP(&opts.Update, "update", "u", false, "update tracked files")
	cmd.Flags().IntVarP(&opts.Jobs, "jobs", "j", opts.Jobs, "number of jobs to run in parallel (default: based on number of CPU cores)")
	// 添加清单相关的标志
	AddManifestFlags(cmd, &opts.CommonManifestOptions)

	return cmd
}

// runStage 执行stage命令
func runStage(opts *StageOptions, args []string, log logger.Logger) error {
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

	// 创建统计对象
	stats := &stageStats{}

	if len(args) == 0 && !opts.All && !opts.Interactive && !opts.Patch && !opts.Update {
		log.Error("未指定文件且未使用 --all/--interactive/--patch/--update 选项")
		return fmt.Errorf("no files specified and none of --all/--interactive/--patch/--update used")
	}

	// --interactive/--patch 需要 TTY：无终端时 git add -i/-p 会失败，提前给出明确错误
	if (opts.Interactive || opts.Patch) && !stdinIsTTY() {
		log.Error("--interactive/--patch 需要交互式终端")
		return fmt.Errorf("--interactive/--patch require a TTY")
	}

	log.Debug("开始暂存文件")

	// 加载清单
	log.Debug("正在加载清单文件: %s", opts.Config.ManifestName)
	parser := manifest.NewParser()
	manifest, err := parser.ParseFromFile(opts.Config.ManifestName, manifest.SplitGroups(opts.Config.Groups))
	if err != nil {
		log.Error("failed to parse manifest: %v", err)
		return fmt.Errorf("failed to parse manifest: %w", err)
	}
	log.Debug("成功加载清单，包含 %d 个项目", len(manifest.Projects))

	// 创建项目管理器
	log.Debug("正在初始化项目管理器...")
	manager := project.NewManagerFromManifest(manifest, opts.Config)

	// 确定文件和项目列表
	var files []string
	var projectNames []string

	// 解析参数，区分项目名和文件名
	log.Debug("解析命令行参..")
	if len(args) > 0 {
		// 消费所有前导的有效项目名，其余参数为文件路径（对齐上游 stage [<project>...] [<file>...]）。
		i := 0
		for i < len(args) {
			projs, perr := manager.GetProjectsByNames([]string{args[i]})
			if perr != nil || len(projs) == 0 {
				break
			}
			projectNames = append(projectNames, args[i])
			i++
		}
		if i > 0 {
			files = args[i:]
			log.Debug("指定 %d 个项目，文件数量: %d", len(projectNames), len(files))
		} else {
			// 所有参数都是文件名
			files = args
			log.Debug("未指定项目，文件数量: %d", len(files))
		}
	}

	// 获取要处理的项目
	var projects []*project.Project
	if len(projectNames) == 0 {
		// 如果没有指定项目，则处理所有项
		log.Debug("获取所有项..")
		projects, err = manager.GetProjectsInGroups(nil)
		if err != nil {
			log.Error("failed to get projectfailed: %v", err)
			return fmt.Errorf("failed to get projects: %w", err)
		}
		log.Debug("共获取到 %d 个项目", len(projects))
	} else {
		// 否则，只处理指定的项目
		log.Debug("根据名称failed to get project: %v", projectNames)
		projects, err = manager.GetProjectsByNames(projectNames)
		if err != nil {
			log.Error("根据名称failed to get projectfailed: %v", err)
			return fmt.Errorf("failed to get projects: %w", err)
		}
		log.Debug("共获取到 %d 个项目", len(projects))
	}

	// 构建stage命令选项（实际上是git add命令）
	log.Debug("构建git add命令参数...")
	stageArgs := []string{"add"}

	if opts.All {
		stageArgs = append(stageArgs, "--all")
	}

	if opts.Interactive {
		stageArgs = append(stageArgs, "--interactive")
	}

	if opts.Patch {
		stageArgs = append(stageArgs, "--patch")
	}

	if opts.Force {
		stageArgs = append(stageArgs, "--force")
	}

	if opts.Update {
		stageArgs = append(stageArgs, "--update")
	}

	if opts.Verbose {
		stageArgs = append(stageArgs, "--verbose")
	}

	// 添加文件参数
	if len(files) > 0 {
		stageArgs = append(stageArgs, files...)
	}

	// 使用goroutine池并发执行stage
	// 规范化并发数：-j <= 0 会导致 makechan panic，回退到默认 8（与其他命令一致）
	if opts.Jobs <= 0 {
		opts.Jobs = 8
	}
	log.Debug("开始暂存文件，并行任务 %d...", opts.Jobs)

	// --interactive/--patch 需要终端 TTY，必须串行执行避免竞争
	maxJobs := opts.Jobs
	if opts.Interactive || opts.Patch {
		maxJobs = 1
		log.Debug("交互/补丁模式，使用串行执行")
	}

	var wg sync.WaitGroup
	errChan := make(chan error, len(projects))
	resultChan := make(chan string, len(projects))
	sem := make(chan struct{}, maxJobs) // 使用信号量控制并发数

	for _, p := range projects {
		p := p // 创建副本避免闭包问题
		wg.Add(1)
		sem <- struct{}{} // 获取信号

		go func() {
			defer wg.Done()
			defer func() { <-sem }() // 释放信号

			log.Debug("在项目 %s 中执行 git add 命令...", p.Name)
			outputBytes, err := p.GitRepo.RunCommand(stageArgs...)
			if err != nil {
				log.Error("项目 %s 暂存failed: %v", p.Name, err)
				errChan <- fmt.Errorf("project %s: %w", p.Name, err)
				stats.increment(false)
				return
			}

			output := strings.TrimSpace(string(outputBytes))
			if output != "" {
				resultChan <- fmt.Sprintf("项目 %s:\n%s", p.Name, output)
			} else {
				resultChan <- fmt.Sprintf("项目 %s: 文件暂存成功", p.Name)
			}
			stats.increment(true)
			log.Debug("项目 %s 暂存完成", p.Name)
		}()
	}

	// 启动一goroutine 来关闭结果通道
	go func() {
		wg.Wait()
		close(errChan)
		close(resultChan)
	}()

	// 处理错误
	var errs []error
	for err := range errChan {
		errs = append(errs, err)
	}

	// 输出结果
	for result := range resultChan {
		log.Info("%s", result)
	}

	// 显示统计信息
	log.Info("暂存操作完成，总计: %d，成功 %d，failed %d", stats.total, stats.success, stats.failed)

	// 如果有错误，返回汇总错误
	if len(errs) > 0 {
		return fmt.Errorf("%d projects failed: %w", len(errs), errors.Join(errs...))
	}

	return nil
}

// stdinIsTTY 判断标准输入是否连接到交互终端（跨平台启发式，无第三方依赖）：
// 管道/普通文件重定向不是字符设备；/dev/null 是字符设备但不是终端。
// /dev/stdin -> /proc/self/fd/0 -> 真实输入源，需两级 Readlink 才能拿到最终目标；
// 解析失败时保守放行（git 自身会拒绝哑终端）
func stdinIsTTY() bool {
	fi, err := os.Stdin.Stat()
	if err != nil || fi.Mode()&os.ModeCharDevice == 0 {
		return false
	}
	target, lerr := os.Readlink("/dev/stdin")
	if lerr == nil && strings.HasPrefix(target, "/proc/") {
		if t2, e2 := os.Readlink(target); e2 == nil {
			target = t2
		}
	}
	if lerr == nil && target != "" {
		return target != "/dev/null" && !strings.HasSuffix(target, "/null")
	}
	return true
}
