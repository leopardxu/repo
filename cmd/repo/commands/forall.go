package commands

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"

	"github.com/leopardxu/repo-go/internal/config"
	"github.com/leopardxu/repo-go/internal/logger"
	"github.com/leopardxu/repo-go/internal/manifest"
	"github.com/leopardxu/repo-go/internal/project"
	"github.com/spf13/cobra"
)

// ForallOptions holds the options for the forall command
type ForallOptions struct {
	Command       string
	Parallel      bool
	PrintHeader   bool // -p：执行前打印项目头（对齐上游 repo forall -p）
	Jobs          int
	IgnoreErrors  bool   // 已废弃（默认即继续）；保留兼容
	AbortOnErrors bool   // -e：首个failed即中止（对齐上游 repo forall -e）
	Regex         string // -r：仅对名称匹配正则的项目执行
	InverseRegex  string // -i：仅对名称不匹配正则的项目执行
	IgnoreMissing bool   // --ignore-missing：跳过未 checkout 的项目
	Interactive   bool   // --interactive：串行执行（需 TTY）
	Quiet         bool
	Verbose       bool
	Groups        string
	Config        *config.Config
	CommonManifestOptions
}

// ForallCmd creates the forall command
func ForallCmd() *cobra.Command {
	opts := &ForallOptions{}
	cmd := &cobra.Command{
		Use:   "forall [<project>...] -c <command> [<arg>...]",
		Short: "Run a shell command in each project",
		Long:  `Executes the same shell command in the working directory of each specified project.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return fmt.Errorf("failed to load config: %w", err)
			}
			opts.Config = cfg

			// 分离项目名与命令：-c 指定命令（字符串），`--` 之后的参数作为命令的 argv。
			// 对齐上游 repo forall -c <command> 语义：-- 后的参数附加到命令，而非覆盖 -c。
			projectNames := args
			commandIndex := cmd.ArgsLenAtDash()
			if commandIndex != -1 {
				projectNames = args[:commandIndex]
				afterDash := strings.Join(args[commandIndex:], " ")
				if opts.Command != "" {
					if afterDash != "" {
						opts.Command = opts.Command + " " + afterDash
					}
				} else {
					opts.Command = afterDash
				}
			}

			if opts.Command == "" {
				return fmt.Errorf("command (-c) is required")
			}

			return runForall(opts, projectNames)
		},
	}

	// Add flags
	cmd.Flags().StringVarP(&opts.Command, "command", "c", "", "command and arguments to execute")
	cmd.Flags().BoolVarP(&opts.PrintHeader, "print-header", "p", false, "print project name header before command output")
	cmd.Flags().IntVarP(&opts.Jobs, "jobs", "j", 8, "number of jobs to run in parallel")
	cmd.Flags().BoolVar(&opts.Parallel, "parallel", true, "run commands in parallel (default: true)")
	cmd.Flags().BoolVar(&opts.IgnoreErrors, "ignore-errors", false, "continue executing even if a command fails (deprecated: default behavior)")
	cmd.Flags().BoolVarP(&opts.AbortOnErrors, "abort-on-errors", "e", false, "abort on first command failure (aligns with upstream repo forall -e)")
	cmd.Flags().StringVarP(&opts.Regex, "regex", "r", "", "only run on projects whose name matches the regex")
	cmd.Flags().StringVarP(&opts.InverseRegex, "inverse-regex", "i", "", "only run on projects whose name does NOT match the regex")
	cmd.Flags().BoolVar(&opts.IgnoreMissing, "ignore-missing", false, "skip projects that are not checked out")
	cmd.Flags().BoolVar(&opts.Interactive, "interactive", false, "run serially (for interactive commands needing a TTY)")
	cmd.Flags().BoolVarP(&opts.Quiet, "quiet", "q", false, "only show errors")
	cmd.Flags().BoolVarP(&opts.Verbose, "verbose", "v", false, "show commands being executed")
	cmd.Flags().StringVarP(&opts.Groups, "groups", "g", "", "restrict execution to projects in specified groups (comma-separated)")
	AddManifestFlags(cmd, &opts.CommonManifestOptions)

	return cmd
}

// forallStats tracks command execution statistics
type forallStats struct {
	mu      sync.Mutex
	Success int
	Failed  int
}

// runForall executes the forall command logic
func runForall(opts *ForallOptions, projectNames []string) error {
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

	// 加载清单
	log.Debug("Loading manifest file")
	parser := manifest.NewParser()
	manifestObj, err := parser.ParseFromFile(opts.Config.ManifestName, manifest.SplitGroups(opts.Config.Groups))
	if err != nil {
		log.Error("Failed to parse manifest: %v", err)
		return fmt.Errorf("failed to parse manifest: %w", err)
	}

	// 创建项目管理器
	log.Debug("Creating project manager")
	manager := project.NewManagerFromManifest(manifestObj, opts.Config)

	// 获取要处理的项目
	log.Debug("Getting projects to operate on")
	var projects []*project.Project
	var groupsArg []string
	if opts.Groups != "" {
		groupsArg = manifest.SplitGroups(opts.Groups)
	}

	if len(projectNames) == 0 {
		projects, err = manager.GetProjectsInGroups(groupsArg)
		if err != nil {
			log.Error("Failed to get projects: %v", err)
			return fmt.Errorf("failed to get projects: %w", err)
		}
	} else {
		// 过滤指定的项目
		filteredProjects, err := manager.GetProjectsByNames(projectNames)
		if err != nil {
			log.Error("Failed to get projects by name: %v", err)
			return fmt.Errorf("failed to get projects by name: %w", err)
		}
		if len(groupsArg) > 0 {
			for _, p := range filteredProjects {
				if p.IsInAnyGroup(groupsArg) {
					projects = append(projects, p)
				}
			}
		} else {
			projects = filteredProjects
		}
	}

	// -r/--regex 与 -i/--inverse-regex：按项目名正则过滤（对齐上游 repo forall -r/-i）
	if opts.Regex != "" || opts.InverseRegex != "" {
		matched := make([]*project.Project, 0, len(projects))
		for _, p := range projects {
			if opts.Regex != "" {
				if ok, _ := regexp.MatchString(opts.Regex, p.Name); !ok {
					continue
				}
			}
			if opts.InverseRegex != "" {
				if ok, _ := regexp.MatchString(opts.InverseRegex, p.Name); ok {
					continue
				}
			}
			matched = append(matched, p)
		}
		projects = matched
	}

	// 执行命令
	log.Debug("Executing command '%s' in %d projects", opts.Command, len(projects))

	// REPO_COUNT：对齐上游 repo forall，设置项目总数到环境变量
	os.Setenv("REPO_COUNT", fmt.Sprintf("%d", len(projects)))

	type forallResult struct {
		Name string
		Err  error
	}

	// 设置并发控制
	maxConcurrency := opts.Jobs
	if maxConcurrency <= 0 {
		maxConcurrency = 8
	}
	// 默认并行（受 -j 控制）；--parallel=false 时退化为串行
	if !opts.Parallel {
		maxConcurrency = 1
	}
	// --interactive：交互式命令需 TTY，强制串行
	if opts.Interactive {
		maxConcurrency = 1
	}

	// 创建通道和等待组
	sem := make(chan struct{}, maxConcurrency)
	results := make(chan forallResult, len(projects))
	var wg sync.WaitGroup
	stats := forallStats{}

	// 取消上下文：-e/--abort-on-errors 时首个failed即取消后续未启动任务（默认继续收集所有结果，对齐上游）
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 确定使用的 shell（跨平台）
	// shellPath/shellArg 在 getShell() 中返回

	// 并发执行命令
	for i, p := range projects {
		idx := i + 1          // REPO_I is 1-based
		if p.Worktree == "" { // 跳过没有working directory的项
			log.Debug("Skipping project %s (no worktree)", p.Name)
			continue
		}
		// --ignore-missing：跳过未 checkout 的working directory
		if opts.IgnoreMissing {
			if _, statErr := os.Stat(p.Worktree); statErr != nil {
				log.Debug("Skipping project %s (worktree not checked out)", p.Name)
				continue
			}
		}

		// 检查是否已取消（避免在已failed后继续派发）
		select {
		case <-ctx.Done():
			log.Debug("已取消，停止派发后续项目")
			goto drain
		default:
		}

		wg.Add(1)
		sem <- struct{}{}
		go func(proj *project.Project, repoI int) {
			defer wg.Done()
			defer func() { <-sem }()

			log.Debug("Executing command in project %s", proj.Name)

			// -p：执行前打印项目头
			if opts.PrintHeader {
				fmt.Printf("\nproject %s/\n", proj.Name)
			}

			shellPath, shellArg := getShell()
			cmd := exec.CommandContext(ctx, shellPath, shellArg, opts.Command)
			cmd.Dir = proj.Worktree
			cmd.Stdout = os.Stdout
			cmd.Stderr = os.Stderr
			cmd.Env = buildForallEnv(proj, repoI)
			err := cmd.Run()
			results <- forallResult{Name: proj.Name, Err: err}
		}(p, idx)
	}

drain:
	// 关闭结果通道
	go func() {
		wg.Wait()
		close(results)
	}()

	// 处理结果
	for res := range results {
		if res.Err != nil {
			errMsg := fmt.Sprintf("Error in %s: %v", res.Name, res.Err)
			log.Error("%s", errMsg)
			stats.mu.Lock()
			stats.Failed++
			stats.mu.Unlock()
			if opts.AbortOnErrors {
				// -e/--abort-on-errors：取消后续未启动任务并返回（默认继续收集所有结果，对齐上游）
				cancel()
				return fmt.Errorf("command failed in project %s", res.Name)
			}
		} else {
			log.Debug("Command executed successfully in project %s", res.Name)
			stats.mu.Lock()
			stats.Success++
			stats.mu.Unlock()
		}
	}

	// 输出统计信息
	log.Info("Command execution complete. Success: %d, Failed: %d", stats.Success, stats.Failed)

	// 如果有failed的项目，返回错
	if stats.Failed > 0 {
		return fmt.Errorf("forall command failed in %d projects", stats.Failed)
	}

	return nil
}

// getShell 返回用于执行命令的 shell 路径与其 -c 等价参数（跨平台）。
// 优先使用 $SHELL；Unix 回退 /bin/sh（-c），Windows 回退 cmd.exe（/c）。
func getShell() (string, string) {
	if s := os.Getenv("SHELL"); s != "" {
		return s, "-c"
	}
	if runtime.GOOS == "windows" {
		return "cmd.exe", "/c"
	}
	return "/bin/sh", "-c"
}

// buildForallEnv 构建 forall 命令的环境变量，导出上游 repo 约定的 REPO_* 变量。
// 以当前进程环境为基础，追加项目相关变量。
func buildForallEnv(proj *project.Project, repoI int) []string {
	env := os.Environ()

	// 过滤掉已存在的 GIT_PAGER，确保下面的覆盖生效（避免重复键导致行为不确定）。
	// forall 在多项目并发执行命令时，stdout 为终端，git log/diff/show 等会启动 less
	// 分页器并等待按键输入；任一项目阻塞即导致 wg.Wait() 无法返回、close(results)
	// 无法触发，整个 forall 永久挂起。设置 GIT_PAGER=cat 让 git 直接流式输出。
	filtered := env[:0]
	for _, kv := range env {
		if strings.HasPrefix(kv, "GIT_PAGER=") {
			continue
		}
		filtered = append(filtered, kv)
	}
	env = filtered

	remoteName := proj.RemoteName
	if remoteName == "" {
		remoteName = "origin"
	}

	// 计算工作树绝对路径（REPO_OUT_PWD）
	outPwd := proj.Worktree
	if abs, err := filepath.Abs(proj.Worktree); err == nil {
		outPwd = abs
	}

	appendVars := []string{
		"REPO_PATH=" + proj.Relpath,
		"REPO_PROJECT=" + proj.Name,
		"REPO_REMOTE=" + remoteName,
		"REPO_LREV=" + proj.Revision,
		"REPO_RREV=" + proj.Revision,
		"REPO_OUT_PWD=" + outPwd,
		"REPO_I=" + fmt.Sprintf("%d", repoI),
		"REPO_UPSTREAM=" + proj.Revision,
		"REPO_DEST_BRANCH=" + proj.Revision,
		// REPO_TOPDIR：repo 客户端顶层目录（对齐上游 repo forall 约定）
	}
	// REPO_TOPDIR 指向 repo 根目录；定位failed时省略该变量而非写入错误值。
	if repoRoot, rerr := config.GetRepoRoot(); rerr == nil {
		appendVars = append(appendVars, "REPO_TOPDIR="+repoRoot)
	}
	appendVars = append(appendVars,
		// 禁用交互式分页器，避免 forall 在并发执行时因 less 等待按键而挂起
		"GIT_PAGER=cat",
	)
	env = append(env, appendVars...)
	return env
}
