package commands

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"

	"github.com/leopardxu/repo-go/internal/config"
	"github.com/leopardxu/repo-go/internal/git"
	"github.com/leopardxu/repo-go/internal/logger"
	"github.com/leopardxu/repo-go/internal/manifest"
	"github.com/leopardxu/repo-go/internal/project"
	"github.com/spf13/cobra"
)

// GrepOptions holds the options for the grep command
type GrepOptions struct {
	IgnoreCase       bool
	FixedStrings     bool
	ExtendedRegexp   bool // -E/--extended-regexp：扩展正则（git grep 透传）
	AfterContext     int  // -A/--after-context：后置上下文行数（git grep 透传）
	BeforeContext    int  // -B/--before-context：前置上下文行数（git grep 透传）
	Context          int  // -C/--context：上下文行数（git grep 透传）
	LineNumber       bool
	FilesWithMatches bool
	WordRegexp       bool
	InvertMatch      bool
	Count            bool
	Text             bool   // -a/--text：将二进制文件视为文本
	IgnoreBinary     bool   // -I：不匹配二进制文件
	NoFilename       bool   // -h：不在输出前缀文件名
	Null             bool   // -z/--null：NUL 分隔
	Cached           bool   // --cached：搜索索引而非工作区
	Color            string // --color：auto/always/never
	Quiet            bool
	Verbose          bool
	Jobs             int
	Pattern          string
	Groups           string
	Config           *config.Config
	CommonManifestOptions
}

// grepStats tracks grep execution statistics
type grepStats struct {
	mu      sync.Mutex
	Success int
	Failed  int
	Matches int
}

// GrepCmd creates the grep command
func GrepCmd() *cobra.Command {
	opts := &GrepOptions{}
	cmd := &cobra.Command{
		Use:   "grep <pattern> [<project>...]",
		Short: "Print lines matching a pattern",
		Long:  `Looks for specified patterns in the working tree files of the specified projects.`,
		Args:  cobra.MinimumNArgs(1), // Requires at least the pattern
		RunE: func(_ *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return fmt.Errorf("failed to load config: %w", err)
			}
			opts.Config = cfg
			opts.Pattern = args[0]
			return runGrep(opts, args[1:])
		},
	}

	// 预注册 --help（仅长选项，无 -h 短选项），避免 cobra 默认占用 -h；
	// 这样 -h 可对齐上游 git grep 语义：抑制文件名前缀（--no-filename）。
	// cobra InitDefaultHelpFlag 见到 help 已存在便不再添加 -h/--help。
	cmd.Flags().Bool("help", false, "help for grep")

	// Add flags
	cmd.Flags().BoolVarP(&opts.IgnoreCase, "ignore-case", "i", false, "ignore case distinctions")
	cmd.Flags().BoolVarP(&opts.FixedStrings, "fixed-strings", "F", false, "interpret pattern as fixed string")
	cmd.Flags().BoolVarP(&opts.ExtendedRegexp, "extended-regexp", "E", false, "interpret pattern as extended regular expression (git grep passthrough)")
	cmd.Flags().IntVarP(&opts.AfterContext, "after-context", "A", 0, "show <n> trailing context lines (git grep passthrough)")
	cmd.Flags().IntVarP(&opts.BeforeContext, "before-context", "B", 0, "show <n> leading context lines (git grep passthrough)")
	cmd.Flags().IntVarP(&opts.Context, "context", "C", 0, "show <n> context lines, before and after (git grep passthrough)")
	cmd.Flags().BoolVarP(&opts.LineNumber, "line-number", "n", false, "prefix matching lines with line number")
	cmd.Flags().BoolVarP(&opts.FilesWithMatches, "files-with-matches", "l", false, "show only file names containing matches")
	cmd.Flags().BoolVarP(&opts.WordRegexp, "word-regexp", "w", false, "match patterns only at word boundaries")
	cmd.Flags().BoolVarP(&opts.InvertMatch, "invert-match", "v", false, "select non-matching lines")
	cmd.Flags().BoolVarP(&opts.Count, "count", "c", false, "show the number of matches per file")
	cmd.Flags().BoolVarP(&opts.Text, "text", "a", false, "treat binary files as text")
	cmd.Flags().BoolVarP(&opts.IgnoreBinary, "ignore-binary", "I", false, "do not match binary files")
	cmd.Flags().BoolVarP(&opts.NoFilename, "no-filename", "h", false, "suppress file name prefix")
	cmd.Flags().BoolVarP(&opts.Null, "null", "z", false, "NUL separator")
	cmd.Flags().BoolVar(&opts.Cached, "cached", false, "search in index instead of working tree")
	// 命令级 --color 有意遮蔽根持久 --color 旗标：对齐 git grep 语义，接受
	// auto/always/never 三值并透传给 git grep --color=<mode>（根旗标仅 true/false）。
	// 不改名，保持与上游 git grep 一致的选项面。
	cmd.Flags().StringVar(&opts.Color, "color", "auto", "control color usage: auto, always, never")
	cmd.Flags().BoolVarP(&opts.Quiet, "quiet", "q", false, "only show errors (suppress match output)")
	cmd.Flags().BoolVar(&opts.Verbose, "verbose", false, "show detailed output")
	cmd.Flags().IntVarP(&opts.Jobs, "jobs", "j", 8, "number of jobs to run in parallel")
	cmd.Flags().StringVarP(&opts.Groups, "groups", "g", "", "restrict execution to projects in specified groups (comma-separated)")
	AddManifestFlags(cmd, &opts.CommonManifestOptions)

	return cmd
}

// runGrep executes the grep command logic
func runGrep(opts *GrepOptions, projectNames []string) error {
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
	log.Debug("正在加载清单文件...")
	parser := manifest.NewParser()
	manifestObj, err := parser.ParseFromFile(opts.Config.ManifestName, manifest.SplitGroups(opts.Config.Groups))
	if err != nil {
		log.Error("failed to parse manifest file: %v", err)
		return fmt.Errorf("failed to parse manifest: %w", err)
	}

	// 创建项目管理器
	log.Debug("正在创建项目管理器...")
	manager := project.NewManagerFromManifest(manifestObj, opts.Config)

	// 获取要处理的项目
	log.Debug("正在获取要处理的项目...")
	var projects []*project.Project
	var groupsArg []string
	if opts.Groups != "" {
		groupsArg = manifest.SplitGroups(opts.Groups)
	}

	if len(projectNames) == 0 {
		log.Debug("获取所有项目...")
		projects, err = manager.GetProjectsInGroups(groupsArg)
		if err != nil {
			log.Error("failed to get projectfailed: %v", err)
			return fmt.Errorf("failed to get projects: %w", err)
		}
	} else {
		log.Debug("根据名称failed to get project: %v", projectNames)
		// 过滤指定的项目
		filteredProjects, err := manager.GetProjectsByNames(projectNames)
		if err != nil {
			log.Error("根据名称failed to get projectfailed: %v", err)
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

	// 构建 git grep 参数
	log.Debug("构建 git grep 参数...")
	grepArgs := []string{"grep"}
	if opts.Cached {
		grepArgs = append(grepArgs, "--cached")
	}
	if opts.IgnoreCase {
		grepArgs = append(grepArgs, "-i")
	}
	if opts.FixedStrings {
		grepArgs = append(grepArgs, "-F")
	}
	if opts.ExtendedRegexp {
		grepArgs = append(grepArgs, "-E")
	}
	if opts.AfterContext > 0 {
		grepArgs = append(grepArgs, fmt.Sprintf("-A%d", opts.AfterContext))
	}
	if opts.BeforeContext > 0 {
		grepArgs = append(grepArgs, fmt.Sprintf("-B%d", opts.BeforeContext))
	}
	if opts.Context > 0 {
		grepArgs = append(grepArgs, fmt.Sprintf("-C%d", opts.Context))
	}
	if opts.LineNumber {
		grepArgs = append(grepArgs, "-n")
	}
	if opts.FilesWithMatches {
		grepArgs = append(grepArgs, "-l")
	}
	if opts.WordRegexp {
		grepArgs = append(grepArgs, "-w")
	}
	if opts.InvertMatch {
		grepArgs = append(grepArgs, "-v")
	}
	if opts.Count {
		grepArgs = append(grepArgs, "-c")
	}
	if opts.Text {
		grepArgs = append(grepArgs, "-a")
	}
	if opts.IgnoreBinary {
		grepArgs = append(grepArgs, "-I")
	}
	if opts.NoFilename {
		grepArgs = append(grepArgs, "-h")
	}
	if opts.Null {
		grepArgs = append(grepArgs, "-z")
	}
	// 颜色：按 --color flag（auto/always/never），默认 auto（仅 TTY 时着色）
	colorMode := opts.Color
	if colorMode == "" {
		colorMode = "auto"
	}
	grepArgs = append(grepArgs, "--color="+colorMode)
	grepArgs = append(grepArgs, "-e", opts.Pattern)

	// 在每个项目中并发执行 grep
	log.Debug("正在 %d 个项目中搜索 '%s'...", len(projects), opts.Pattern)

	type grepResult struct {
		project *project.Project
		output  []byte
		err     error
	}

	// 创建工作池
	maxWorkers := opts.Jobs
	if maxWorkers <= 0 {
		maxWorkers = 8
	}

	sem := make(chan struct{}, maxWorkers)
	results := make(chan grepResult, len(projects))
	var wg sync.WaitGroup
	stats := grepStats{}

	// 跟踪有working directory的项目数量
	validProjects := 0

	for _, p := range projects {
		if p.Worktree == "" {
			log.Debug("跳过项目 %s (无working directory)", p.Name)
			continue
		}

		validProjects++
		wg.Add(1)
		sem <- struct{}{}
		go func(p *project.Project) {
			defer wg.Done()
			defer func() { <-sem }()

			log.Debug("在项目 %s 中执行 grep...", p.Name)
			output, err := p.GitRepo.RunCommand(grepArgs...)
			results <- grepResult{p, output, err}
		}(p)
	}

	// 关闭结果通道
	go func() {
		wg.Wait()
		close(results)
	}()

	// 收集结果并按项目名排序输出（保证顺序确定）
	var allResults []grepResult
	for res := range results {
		allResults = append(allResults, res)
	}
	sort.Slice(allResults, func(i, j int) bool { return allResults[i].project.Name < allResults[j].project.Name })

	var errList []error

	for _, res := range allResults {
		if res.err != nil {
			// git grep 退出码 1 表示无匹配（非错误）；其他退出码才是真正failed
			var gitErr *git.GitCommandError
			if errors.As(res.err, &gitErr) && gitErr.ExitCode == 1 {
				log.Debug("项目 %s 中没有找到匹配项", res.project.Name)
				stats.mu.Lock()
				stats.Success++
				stats.mu.Unlock()
				continue
			}
			errList = append(errList, fmt.Errorf("error grepping in %s: %w", res.project.Name, res.err))
			stats.mu.Lock()
			stats.Failed++
			stats.mu.Unlock()
			continue
		}

		if len(res.output) > 0 {
			// 有匹配结果（仅用于调试；无匹配时上游静默退出 1）
			lines := strings.Split(strings.TrimSpace(string(res.output)), "\n")
			log.Debug("项目 %s 中找%d 个匹配项", res.project.Name, len(lines))

			stats.mu.Lock()
			stats.Success++
			stats.Matches += len(lines)
			stats.mu.Unlock()

			// -q 抑制匹配输出（仅统计），非 -q 时打印匹配行
			if !opts.Quiet {
				for _, line := range lines {
					fmt.Printf("%s:%s\n", res.project.Name, line)
				}
			}
		} else {
			log.Debug("项目 %s 中没有找到匹配项", res.project.Name)
			stats.mu.Lock()
			stats.Success++
			stats.mu.Unlock()
		}
	}

	// 输出错误信息
	if len(errList) > 0 {
		log.Error("%d 个项目中执行 grep failed", len(errList))
		for _, err := range errList {
			log.Error("%v", err)
		}
	}

	// 输出统计信息（仅调试级别；上游 grep 只输出匹配行）
	log.Debug("搜索完成. 处理项目: %d, 成功: %d, failed: %d, 找到匹配 %d",
		validProjects, stats.Success, stats.Failed, stats.Matches)

	// 如果有failed的项目，返回错误
	if stats.Failed > 0 {
		return fmt.Errorf("grep command failed in %d projects", stats.Failed)
	}

	// 无匹配项时静默退出码 1（对齐上游 repo grep：无匹配时不打印任何错误，仅退出非 0）
	if stats.Matches == 0 {
		os.Exit(1)
	}

	return nil
}
