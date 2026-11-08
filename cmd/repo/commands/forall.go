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
	"github.com/spf13/pflag"
)

// forallSafeExecPattern 对齐上游 repo forall：命令名仅由字母、数字及 _/.- 组成时
// 直接 exec（shell=False），否则命令含 shell 特性（空格/管道/重定向等），交给 shell 执行。
var forallSafeExecPattern = regexp.MustCompile(`^[a-zA-Z0-9_/.-]+$`)

// ForallOptions holds the options for the forall command
type ForallOptions struct {
	Command       []string // -c 之后的所有 argv（保留参数边界）
	Parallel      bool
	PrintHeader   bool // -p：执行前打印项目头（对齐上游 repo forall -p）
	Jobs          int
	IgnoreErrors  bool // 已废弃（默认即继续）；保留兼容
	AbortOnErrors bool // -e：首个failed即中止（对齐上游 repo forall -e）
	Regex         bool // -r：位置参数作为正则/通配模式，匹配项目名或相对路径
	InverseRegex  bool // -i：位置参数作为正则，选择不匹配的项目
	IgnoreMissing bool // --ignore-missing：跳过未 checkout 的项目
	Interactive   bool // --interactive：串行执行（需 TTY）
	Quiet         bool
	Verbose       bool
	Groups        string
	Help          bool
	Config        *config.Config
	CommonManifestOptions
}

// ForallCmd creates the forall command.
//
// 对齐上游 repo forall：-c/--command 使用 optparse callback 语义，-c 之后的所有
// token（包括以 - 开头的）都属于命令 argv，因此本命令禁用 cobra 的 flag 解析，
// 由 splitForallArgs 在 RunE 中手动拆分后再解析 -c 之前的 flag。
func ForallCmd() *cobra.Command {
	opts := &ForallOptions{}
	cmd := &cobra.Command{
		Use:                "forall [<project>...] -c <command> [<arg>...]",
		Short:              "Run a shell command in each project",
		Long:               `Executes the same shell command in the working directory of each specified project.`,
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			flagArgs, command, hasCommand := splitForallArgs(args)

			fs := newForallFlagSet(opts)
			if err := fs.Parse(flagArgs); err != nil {
				return fmt.Errorf("解析参数failed: %w", err)
			}
			if opts.Help {
				return cmd.Help()
			}
			if !hasCommand || len(command) == 0 {
				return fmt.Errorf("command (-c) is required")
			}
			opts.Command = command

			cfg, err := config.Load()
			if err != nil {
				return fmt.Errorf("failed to load config: %w", err)
			}
			opts.Config = cfg

			return runForall(opts, fs.Args())
		},
	}

	// 以下 flag 仅用于生成帮助文本；实际解析由 newForallFlagSet 完成
	// （DisableFlagParsing 下 cobra 不会解析它们）。
	cmd.Flags().StringArrayVarP(&opts.Command, "command", "c", nil, "command and arguments to execute")
	cmd.Flags().BoolVarP(&opts.PrintHeader, "print-header", "p", false, "print project name header before command output")
	cmd.Flags().IntVarP(&opts.Jobs, "jobs", "j", 8, "number of jobs to run in parallel")
	cmd.Flags().BoolVar(&opts.Parallel, "parallel", true, "run commands in parallel (default: true)")
	cmd.Flags().BoolVar(&opts.IgnoreErrors, "ignore-errors", false, "continue executing even if a command fails (deprecated: default behavior)")
	cmd.Flags().BoolVarP(&opts.AbortOnErrors, "abort-on-errors", "e", false, "abort on first command failure (aligns with upstream repo forall -e)")
	cmd.Flags().BoolVarP(&opts.Regex, "regex", "r", false, "treat positional arguments as regex patterns matched against project name/path")
	cmd.Flags().BoolVarP(&opts.InverseRegex, "inverse-regex", "i", false, "treat positional arguments as regex patterns and select non-matching projects")
	cmd.Flags().BoolVar(&opts.IgnoreMissing, "ignore-missing", false, "skip projects that are not checked out")
	cmd.Flags().BoolVar(&opts.Interactive, "interactive", false, "run serially (for interactive commands needing a TTY)")
	cmd.Flags().BoolVarP(&opts.Quiet, "quiet", "q", false, "only show errors")
	cmd.Flags().BoolVarP(&opts.Verbose, "verbose", "v", false, "show commands being executed")
	cmd.Flags().StringVarP(&opts.Groups, "groups", "g", "", "restrict execution to projects in specified groups (comma-separated)")
	cmd.Flags().BoolVarP(&opts.Help, "help", "h", false, "show help")
	AddManifestFlags(cmd, &opts.CommonManifestOptions)

	return cmd
}

// newForallFlagSet 构造用于解析 forall flag 的独立 FlagSet（-c 之前的参数）。
func newForallFlagSet(opts *ForallOptions) *pflag.FlagSet {
	fs := pflag.NewFlagSet("forall", pflag.ContinueOnError)
	fs.BoolVarP(&opts.PrintHeader, "print-header", "p", false, "print project name header")
	fs.IntVarP(&opts.Jobs, "jobs", "j", opts.Jobs, "number of jobs to run in parallel")
	fs.BoolVar(&opts.Parallel, "parallel", opts.Parallel, "run commands in parallel")
	fs.BoolVar(&opts.IgnoreErrors, "ignore-errors", opts.IgnoreErrors, "continue on command failure (deprecated)")
	fs.BoolVarP(&opts.AbortOnErrors, "abort-on-errors", "e", opts.AbortOnErrors, "abort on first command failure")
	fs.BoolVarP(&opts.Regex, "regex", "r", opts.Regex, "match projects by regex pattern")
	fs.BoolVarP(&opts.InverseRegex, "inverse-regex", "i", opts.InverseRegex, "select non-matching projects")
	fs.BoolVar(&opts.IgnoreMissing, "ignore-missing", opts.IgnoreMissing, "skip projects not checked out")
	fs.BoolVar(&opts.Interactive, "interactive", opts.Interactive, "run serially")
	fs.BoolVarP(&opts.Quiet, "quiet", "q", opts.Quiet, "only show errors")
	fs.BoolVarP(&opts.Verbose, "verbose", "v", opts.Verbose, "show commands being executed")
	fs.StringVarP(&opts.Groups, "groups", "g", opts.Groups, "restrict to projects in groups")
	fs.BoolVarP(&opts.Help, "help", "h", false, "show help")
	fs.BoolVar(&opts.OuterManifest, "outer-manifest", opts.OuterManifest, "operate starting at the outermost manifest")
	fs.BoolVar(&opts.NoOuterManifest, "no-outer-manifest", opts.NoOuterManifest, "do not operate on outer manifests")
	fs.BoolVar(&opts.ThisManifestOnly, "this-manifest-only", opts.ThisManifestOnly, "only operate on this (sub)manifest")
	fs.BoolVar(&opts.AllManifests, "all-manifests", opts.AllManifests, "operate on this manifest and its submanifests")
	return fs
}

// splitForallArgs 将原始参数拆分为 -c 之前的 flag/项目参数与 -c 之后的命令 argv。
//
// 对齐上游 repo forall 的 -c callback：-c 之后的所有 token（包括以 - 开头的）都属于
// 命令，且保留 token 边界（不 Join）。`--` 之后不再识别 -c（optparse 语义），
// 此时全部参数按位置参数处理。
func splitForallArgs(args []string) (flagArgs, command []string, hasCommand bool) {
	for i, arg := range args {
		if arg == "--" {
			// `--` 之前没有 -c：后续 token 全部是位置参数，不再拆分命令。
			return args, nil, false
		}
		if arg == "-c" || arg == "--command" {
			return args[:i], args[i+1:], true
		}
		if strings.HasPrefix(arg, "--command=") {
			rest := args[i+1:]
			cmd := make([]string, 0, len(rest)+1)
			cmd = append(cmd, strings.TrimPrefix(arg, "--command="))
			cmd = append(cmd, rest...)
			return args[:i], cmd, true
		}
		if strings.HasPrefix(arg, "-c") && len(arg) > 2 && !strings.HasPrefix(arg, "--") {
			// -cVALUE / -c=VALUE 形式
			value := arg[2:]
			value = strings.TrimPrefix(value, "=")
			cmd := make([]string, 0, len(args)-i)
			cmd = append(cmd, value)
			cmd = append(cmd, args[i+1:]...)
			return args[:i], cmd, true
		}
	}
	return args, nil, false
}

// parseForallCommand 返回命令 argv 以及是否需要 shell 执行。
// 对齐上游：cmd[0] 仅由安全字符组成时直接 exec argv（保留参数边界）；
// 否则命令含 shell 特性，交由 shell -c 执行。
func parseForallCommand(command []string) (argv []string, useShell bool) {
	if len(command) == 0 {
		return nil, false
	}
	argv = append([]string(nil), command...)
	return argv, !forallSafeExecPattern.MatchString(command[0])
}

// forallStats tracks command execution statistics
type forallStats struct {
	mu      sync.Mutex
	Success int
	Failed  int
}

// runForall executes the forall command logic
func runForall(opts *ForallOptions, projectArgs []string) error {
	// 初始化日志记录器
	log := logger.NewDefaultLogger()
	switch {
	case opts.Verbose:
		log.SetLevel(logger.LogLevelDebug)
	case opts.Quiet:
		log.SetLevel(logger.LogLevelError)
	default:
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

	// 组过滤：显式 --groups 优先，否则使用配置中的 effective groups（空值即 default）；只解析一次。
	effectiveGroups := opts.Groups
	if effectiveGroups == "" {
		effectiveGroups = opts.Config.EffectiveGroups()
	}

	// 加载清单
	log.Debug("Loading manifest file")
	parser := manifest.NewParser()
	manifestObj, err := parser.ParseFromFile(opts.Config.ManifestName, manifest.SplitGroups(effectiveGroups))
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
	switch {
	case opts.Regex || opts.InverseRegex:
		// -r/-i：位置参数是正则/通配模式，匹配项目名或相对路径（大小写不敏感）。
		all, gerr := manager.GetProjectsInGroups(nil)
		if gerr != nil {
			log.Error("Failed to get projects: %v", gerr)
			return fmt.Errorf("failed to get projects: %w", gerr)
		}
		projects, err = filterForallProjectsByPatterns(all, projectArgs, opts.InverseRegex)
		if err != nil {
			log.Error("Invalid regex: %v", err)
			return err
		}
	case len(projectArgs) == 0:
		projects, err = manager.GetProjectsInGroups(nil)
		if err != nil {
			log.Error("Failed to get projects: %v", err)
			return fmt.Errorf("failed to get projects: %w", err)
		}
	default:
		// 项目参数支持名称、"."、相对/绝对路径（对齐上游 GetProjects）。
		projects, err = manager.GetProjectsByArgs(projectArgs, originalDir)
		if err != nil {
			log.Error("Failed to get projects by args: %v", err)
			return fmt.Errorf("failed to get projects by args: %w", err)
		}
	}

	// 执行命令
	log.Debug("Executing command '%s' in %d projects", strings.Join(opts.Command, " "), len(projects))

	// REPO_COUNT：对齐上游 repo forall，设置项目总数到环境变量
	if err := os.Setenv("REPO_COUNT", fmt.Sprintf("%d", len(projects))); err != nil {
		log.Warn("设置 REPO_COUNT failed: %v", err)
	}

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
	// 对齐上游：-j 1 时进入交互模式
	if maxConcurrency == 1 {
		opts.Interactive = true
	}

	// 创建通道和等待组
	sem := make(chan struct{}, maxConcurrency)
	results := make(chan forallResult, len(projects))
	var wg sync.WaitGroup
	stats := forallStats{}

	// 取消上下文：-e/--abort-on-errors 时首个failed即取消后续未启动任务（默认继续收集所有结果，对齐上游）
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 并发执行命令
dispatch:
	for i, p := range projects {
		idx := i + 1 // REPO_I is 1-based
		worktree := forallWorktree(manager.TopDir, p)
		if worktree == "" { // 跳过没有working directory的项
			log.Debug("Skipping project %s (no worktree)", p.Name)
			continue
		}
		// --ignore-missing：跳过未 checkout 的working directory
		if _, statErr := os.Stat(worktree); statErr != nil {
			if opts.IgnoreMissing {
				log.Debug("Skipping project %s (worktree not checked out)", p.Name)
				continue
			}
			// 对齐上游：未 checkout 且未指定 --ignore-missing 时报告跳过并计为失败
			results <- forallResult{Name: p.Name, Err: fmt.Errorf("skipping %s/: working tree not found", p.Relpath)}
			continue
		}

		// 检查是否已取消（避免在已failed后继续派发）
		select {
		case <-ctx.Done():
			log.Debug("已取消，停止派发后续项目")
			break dispatch
		default:
		}

		wg.Add(1)
		sem <- struct{}{}
		go func(proj *project.Project, repoI int, dir string) {
			defer wg.Done()
			defer func() { <-sem }()

			log.Debug("Executing command in project %s", proj.Name)

			// -p：执行前打印项目头
			if opts.PrintHeader {
				fmt.Printf("\nproject %s/\n", proj.Name)
			}

			err := runForallCommand(ctx, opts, proj, repoI, dir)
			results <- forallResult{Name: proj.Name, Err: err}
		}(p, idx, worktree)
	}

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
				return fmt.Errorf("command failed in project %s: %w", res.Name, res.Err)
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

// runForallCommand 在给定目录执行 forall 命令。
// 直接 exec argv（保留参数边界）；仅当命令含 shell 特性时才使用 shell。
func runForallCommand(ctx context.Context, opts *ForallOptions, p *project.Project, repoI int, dir string) error {
	argv, useShell := parseForallCommand(opts.Command)
	if len(argv) == 0 {
		return fmt.Errorf("empty command")
	}

	var cmd *exec.Cmd
	if useShell {
		shellPath, shellArg := getShell()
		// 对齐上游 shell=True 形式：shell -c <command> <$0=command> <$1...>
		args := make([]string, 0, len(argv)+2)
		args = append(args, shellArg, argv[0], argv[0])
		args = append(args, argv[1:]...)
		cmd = exec.CommandContext(ctx, shellPath, args...)
	} else {
		cmd = exec.CommandContext(ctx, argv[0], argv[1:]...)
	}
	cmd.Dir = dir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if opts.Interactive {
		cmd.Stdin = os.Stdin
	}
	cmd.Env = buildForallEnv(p, repoI)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("command in %s failed: %w", p.Name, err)
	}
	return nil
}

// filterForallProjectsByPatterns 按上游 FindProjects 语义过滤项目：
// 位置参数按大小写不敏感正则解释，匹配项目名或相对路径；
// inverse 为 true 时选择不匹配任何模式的项目。非法正则返回错误。
func filterForallProjectsByPatterns(projects []*project.Project, patterns []string, inverse bool) ([]*project.Project, error) {
	compiled := make([]*regexp.Regexp, 0, len(patterns))
	for _, pattern := range patterns {
		re, err := regexp.Compile("(?i)" + pattern)
		if err != nil {
			return nil, fmt.Errorf("invalid regex %q: %w", pattern, err)
		}
		compiled = append(compiled, re)
	}

	matched := make([]*project.Project, 0, len(projects))
	for _, p := range projects {
		hit := false
		for _, re := range compiled {
			if re.MatchString(p.Name) || re.MatchString(p.Relpath) {
				hit = true
				break
			}
		}
		if hit != inverse {
			matched = append(matched, p)
		}
	}
	return matched, nil
}

// forallWorktree 返回 forall 命令的工作目录：普通项目为工作树绝对路径
// （相对路径基于 repo 顶层目录解析；TopDir 为空时保持原值），镜像项目没有
// 工作树，对齐上游 MirrorSafe 语义使用裸仓库 Gitdir/Objdir。
func forallWorktree(topDir string, p *project.Project) string {
	if p != nil && p.Mirror {
		return forallMirrorGitdir(p)
	}
	wt := p.Worktree
	if wt == "" {
		wt = p.Path
	}
	if wt == "" {
		return ""
	}
	if filepath.IsAbs(wt) {
		return filepath.Clean(wt)
	}
	if topDir == "" {
		return wt
	}
	return filepath.Clean(filepath.Join(topDir, wt))
}

// forallMirrorGitdir 返回镜像项目的裸仓库目录（Gitdir，回退 Objdir）绝对路径。
// 非镜像项目返回空串。
func forallMirrorGitdir(p *project.Project) string {
	if p == nil || !p.Mirror {
		return ""
	}
	dir := p.Gitdir
	if dir == "" {
		dir = p.Objdir
	}
	if dir == "" {
		return ""
	}
	if abs, err := filepath.Abs(dir); err == nil {
		return filepath.Clean(abs)
	}
	return filepath.Clean(dir)
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
// 以当前进程环境为基础，追加项目相关变量：
//   - REPO_LREV       解析后的 commit SHA
//   - REPO_RREV       manifest 中的 revision 表达式（原样）
//   - REPO_UPSTREAM   manifest 中的 upstream
//   - REPO_DEST_BRANCH manifest 中的 dest-branch
func buildForallEnv(proj *project.Project, repoI int) []string {
	env := os.Environ()

	// 镜像项目以裸仓库为工作目录，必须显式导出 GIT_DIR；同时过滤进程已有的
	// GIT_DIR，避免重复键导致 git 读取到错误仓库。
	mirrorGitdir := forallMirrorGitdir(proj)

	// 过滤掉已存在的 GIT_PAGER，确保下面的覆盖生效（避免重复键导致行为不确定）。
	// forall 在多项目并发执行命令时，stdout 为终端，git log/diff/show 等会启动 less
	// 分页器并等待按键输入；任一项目阻塞即导致 wg.Wait() 无法返回、close(results)
	// 无法触发，整个 forall 永久挂起。设置 GIT_PAGER=cat 让 git 直接流式输出。
	filtered := env[:0]
	for _, kv := range env {
		if strings.HasPrefix(kv, "GIT_PAGER=") {
			continue
		}
		if mirrorGitdir != "" && strings.HasPrefix(kv, "GIT_DIR=") {
			continue
		}
		filtered = append(filtered, kv)
	}
	env = filtered

	remoteName := proj.RemoteName
	if remoteName == "" {
		remoteName = "origin"
	}

	// REPO_OUT_PWD 必须是项目实际工作目录的绝对路径：镜像项目没有工作树，
	// 不能对空 Worktree 调用 filepath.Abs（那会得到当前进程目录）。
	outPwd := mirrorGitdir
	if outPwd == "" {
		wt := proj.Worktree
		if wt == "" {
			wt = proj.Path
		}
		if wt != "" {
			outPwd = wt
			if abs, err := filepath.Abs(wt); err == nil {
				outPwd = abs
			}
		}
	}

	appendVars := []string{
		"REPO_PATH=" + proj.Relpath,
		"REPO_PROJECT=" + proj.Name,
		"REPO_REMOTE=" + remoteName,
		"REPO_LREV=" + resolveForallLREV(proj),
		"REPO_RREV=" + proj.Revision,
		"REPO_OUT_PWD=" + outPwd,
		"REPO_I=" + fmt.Sprintf("%d", repoI),
		"REPO_UPSTREAM=" + proj.Upstream,
		"REPO_DEST_BRANCH=" + proj.DestBranch,
		"REPO_PROJECT_FETCH_URL=" + proj.RemoteURL,
	}
	if mirrorGitdir != "" {
		appendVars = append(appendVars, "GIT_DIR="+mirrorGitdir)
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

// resolveForallLREV 把 manifest revision 解析为具体 commit SHA（对齐上游 REPO_LREV）。
// 解析失败时回退到 RevisionID/Revision，不返回错误（环境变量缺省为空）。
func resolveForallLREV(p *project.Project) string {
	if p == nil {
		return ""
	}
	if forallLooksLikeCommitSHA(p.RevisionID) {
		return p.RevisionID
	}
	if forallLooksLikeCommitSHA(p.Revision) {
		return p.Revision
	}
	if p.GitRepo == nil || p.GitRepo.Runner == nil || p.Revision == "" {
		return p.RevisionID
	}

	rev := p.Revision
	remote := p.RemoteName
	if remote == "" {
		remote = "origin"
	}

	candidates := make([]string, 0, 3)
	if strings.HasPrefix(rev, "refs/heads/") {
		branch := strings.TrimPrefix(rev, "refs/heads/")
		candidates = append(candidates, remote+"/"+branch, "refs/heads/"+branch)
	} else if strings.HasPrefix(rev, "refs/") {
		candidates = append(candidates, rev)
	} else {
		candidates = append(candidates, remote+"/"+rev, "refs/heads/"+rev)
	}

	for _, candidate := range candidates {
		out, err := p.GitRepo.Runner.RunInDir(p.GitRepo.Path, "rev-parse", "--verify", "--quiet", candidate+"^0")
		if err != nil {
			continue
		}
		if sha := strings.TrimSpace(string(out)); sha != "" {
			return sha
		}
	}
	return p.RevisionID
}

// forallLooksLikeCommitSHA 判断字符串是否为完整 commit SHA（40 位 hex；兼容 64 位）。
func forallLooksLikeCommitSHA(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
			return false
		}
	}
	return true
}
