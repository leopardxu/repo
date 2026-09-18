package commands

import (
	"fmt"
	"sort"

	"sync"

	"github.com/leopardxu/repo-go/internal/config"
	"github.com/leopardxu/repo-go/internal/logger"
	"github.com/leopardxu/repo-go/internal/manifest"
	"github.com/leopardxu/repo-go/internal/project"
	"github.com/spf13/cobra"
)

// DiffOptions holds the options for the diff command
type DiffOptions struct {
	Quiet      bool
	Verbose    bool
	Cached     bool
	Unified    int
	NameOnly   bool
	NameStatus bool
	Stat       bool
	Jobs       int
	Config     *config.Config
	CommonManifestOptions
}

// 加载配置
func loadConfig() (*config.Config, error) {
	return config.Load()
}

// 解析清单
func loadManifest(cfg *config.Config) (*manifest.Manifest, error) {
	parser := manifest.NewParser()
	return parser.ParseFromFile(cfg.ManifestName, manifest.SplitGroups(cfg.Groups))
}

// failed to get project列表
func getProjects(manager *project.Manager, projectNames []string) ([]*project.Project, error) {
	if len(projectNames) == 0 {
		return manager.GetProjectsInGroups(nil)
	}
	return manager.GetProjectsByNames(projectNames)
}

// 并发执行diff操作
type diffResult struct {
	Name   string
	Output string
	Err    error
}

func runDiff(opts *DiffOptions, projectNames []string) error {
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

	log.Debug("加载配置")
	cfg, err := loadConfig()
	if err != nil {
		log.Error("failed to load config: %v", err)
		return fmt.Errorf("failed to load config: %w", err)
	}
	opts.Config = cfg

	log.Debug("解析清单文件")
	mf, err := loadManifest(cfg)
	if err != nil {
		log.Error("failed to parse manifest file: %v", err)
		return fmt.Errorf("failed to parse manifest: %w", err)
	}

	log.Debug("failed to get project管理器")
	manager := project.NewManagerFromManifest(mf, cfg)
	log.Debug("failed to get project列表")
	projects, err := getProjects(manager, projectNames)
	if err != nil {
		log.Error("failed to get project列表failed: %v", err)
		return fmt.Errorf("failed to get projects: %w", err)
	}

	log.Debug("开始对 %d 个项目执行 diff 操作", len(projects))

	maxConcurrency := opts.Jobs
	if maxConcurrency <= 0 {
		maxConcurrency = 8
	}
	sem := make(chan struct{}, maxConcurrency)
	results := make(chan diffResult, len(projects))
	var wg sync.WaitGroup

	// 构建 diff 参数（透传上游 repo diff 支持的格式选项）
	diffArgs := []string{"diff"}
	if opts.Cached {
		diffArgs = append(diffArgs, "--cached")
	}
	if opts.Unified > 0 {
		diffArgs = append(diffArgs, fmt.Sprintf("--unified=%d", opts.Unified))
	}
	if opts.NameOnly {
		diffArgs = append(diffArgs, "--name-only")
	}
	if opts.NameStatus {
		diffArgs = append(diffArgs, "--name-status")
	}
	if opts.Stat {
		diffArgs = append(diffArgs, "--stat")
	}

	// 并发执行diff操作
	for _, p := range projects {
		wg.Add(1)
		sem <- struct{}{}
		go func(proj *project.Project) {
			defer wg.Done()
			defer func() { <-sem }()
			log.Debug("对项目 %s 执行 diff 操作", proj.Name)
			outBytes, err := proj.GitRepo.RunCommand(diffArgs...)
			out := string(outBytes)
			results <- diffResult{Name: proj.Name, Output: out, Err: err}
		}(p)
	}

	// 等待所有diff操作完成并关闭结果通道
	go func() {
		wg.Wait()
		close(results)
	}()

	// 收集结果并按项目名排序输出（保证顺序确定，便于可复现）
	var allResults []diffResult
	successCount := 0
	errorCount := 0

	for res := range results {
		allResults = append(allResults, res)
		if res.Err != nil {
			errorCount++
			continue
		}
		successCount++
	}
	sort.Slice(allResults, func(i, j int) bool { return allResults[i].Name < allResults[j].Name })

	for _, res := range allResults {
		if res.Err != nil {
			log.Error("项目 %s 执行 diff failed: %v", res.Name, res.Err)
			continue
		}
		if res.Output != "" {
			log.Info("--- %s ---\n%s", res.Name, res.Output)
		} else if !opts.Quiet {
			log.Info("--- %s ---\n(无变更)", res.Name)
		}
	}

	log.Debug("diff 操作完成: %d 成功, %d failed", successCount, errorCount)

	if errorCount > 0 {
		return fmt.Errorf("diff failed for %d projects", errorCount)
	}

	return nil
}

// DiffCmd creates the diff command
func DiffCmd() *cobra.Command {
	opts := &DiffOptions{}
	cmd := &cobra.Command{
		Use:   "diff [<project>...]",
		Short: "Show changes between commit, working tree, etc",
		Long:  `Shows changes between the working tree and the index or a commit.`,
		RunE: func(_ *cobra.Command, args []string) error {
			return runDiff(opts, args)
		},
	}

	cmd.Flags().BoolVarP(&opts.Quiet, "quiet", "q", false, "only show errors")
	cmd.Flags().BoolVarP(&opts.Verbose, "verbose", "v", false, "show all output")
	cmd.Flags().BoolVarP(&opts.Cached, "cached", "c", false, "show diff of staged changes")
	cmd.Flags().IntVarP(&opts.Unified, "unified", "u", 0, "generate diffs with <n> lines context")
	cmd.Flags().BoolVar(&opts.NameOnly, "name-only", false, "show only names of changed files")
	cmd.Flags().BoolVar(&opts.NameStatus, "name-status", false, "show names and status of changed files")
	cmd.Flags().BoolVar(&opts.Stat, "stat", false, "show diffstat")
	cmd.Flags().IntVarP(&opts.Jobs, "jobs", "j", 8, "number of jobs to run in parallel")
	AddManifestFlags(cmd, &opts.CommonManifestOptions)
	return cmd
}
