package commands

import (
	"fmt"
	"os"
	"sort"
	"strings"
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

// readCmdEffectiveGroups 计算命令的有效组过滤：显式 --groups 优先，否则使用
// cfg.EffectiveGroups()（空 Groups 归一为 "default"）。结果只用于一次
// ParseFromFile，避免解析后再用另一套组做二次过滤。
func readCmdEffectiveGroups(explicit string, cfg *config.Config) []string {
	if strings.TrimSpace(explicit) != "" {
		return manifest.SplitGroups(explicit)
	}
	if cfg == nil {
		return nil
	}
	return manifest.SplitGroups(cfg.EffectiveGroups())
}

// readCmdIsCommitID 判断字符串是否形如对象 ID（7-64 位小写十六进制）。
// 这类 revision 属于不可变引用，解析时原样使用，不做远端分支映射。
func readCmdIsCommitID(rev string) bool {
	if len(rev) < 7 || len(rev) > 64 {
		return false
	}
	for _, r := range rev {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// readCmdIsImmutableRevision 判断 revision 是否应原样解析：对象 ID 与
// refs/tags/* 不随远端分支映射（对齐上游 Remote.ToLocal 对 IsId/IsTag 的处理）。
func readCmdIsImmutableRevision(rev string) bool {
	rev = strings.TrimSpace(rev)
	if rev == "" {
		return false
	}
	return readCmdIsCommitID(rev) || strings.HasPrefix(rev, "refs/tags/")
}

// readCmdRemoteRefName 返回 refs/remotes/ 前缀使用的实际 git remote 名。
// 清单项目引用的 remote 可能是 name 或 alias；项目加载时声明了 alias 的
// remote 会以 alias 作为本地 git remote 名，因此这里优先返回 alias。
func readCmdRemoteRefName(name string, remotes []manifest.Remote) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	for _, r := range remotes {
		if r.Name != name && (r.Alias == "" || r.Alias != name) {
			continue
		}
		if alias := strings.TrimSpace(r.Alias); alias != "" {
			return alias
		}
		return strings.TrimSpace(r.Name)
	}
	return name
}

// readCmdRevisionCandidates 返回 manifest revision 的解析候选顺序：
//   - SHA 与 refs/tags/*（以及其它非 refs/heads 的完整引用）原样使用；
//   - 分支名与 refs/heads/X：先 refs/remotes/<remote>/X，再回退 refs/heads/X。
//
// 裸 revision 只按分支名处理，绝不当作 tag（tag 仅支持显式 refs/tags/ 前缀）；
// 本地回退使用完整 refs/heads/ 形式，避免 git 的 DWIM 把同名 tag 解析出来。
// remote 为空时回退 origin。includeLocal=false 时不追加本地回退（info -d
// 需要避免本地分支优先于远端跟踪引用）。
func readCmdRevisionCandidates(revision, remote string, includeLocal bool) []string {
	rev := strings.TrimSpace(revision)
	if rev == "" {
		return []string{}
	}
	candidates := make([]string, 0, 2)
	if readCmdIsImmutableRevision(rev) {
		return []string{rev}
	}
	if strings.HasPrefix(rev, "refs/") && !strings.HasPrefix(rev, "refs/heads/") {
		// refs/changes/... 等完整引用不映射到远端分支，按原样解析。
		return []string{rev}
	}
	remote = strings.TrimSpace(remote)
	if remote == "" {
		remote = "origin"
	}
	branch := strings.TrimPrefix(rev, "refs/heads/")
	if branch != "" {
		candidates = append(candidates, "refs/remotes/"+remote+"/"+branch)
	}
	if includeLocal && branch != "" {
		candidates = append(candidates, "refs/heads/"+branch)
	}
	return candidates
}

// readCmdProjectMissing 判断项目是否未检出：工作树为空或对应目录不存在。
// 只读命令遇到未检出项目应跳过并提示，而不是按失败处理。
func readCmdProjectMissing(p *project.Project) bool {
	if p == nil || p.Worktree == "" {
		return true
	}
	info, err := os.Stat(p.Worktree)
	if err != nil {
		return true
	}
	return !info.IsDir()
}

// buildDiffArgs 构造 git diff 参数：基础比较对象为 HEAD（工作树+索引对 HEAD），
// --cached 时比较索引与 HEAD。格式选项保持透传。
func buildDiffArgs(opts *DiffOptions) []string {
	args := []string{"diff"}
	if opts.Cached {
		args = append(args, "--cached")
	}
	if opts.Unified > 0 {
		args = append(args, fmt.Sprintf("--unified=%d", opts.Unified))
	}
	if opts.NameOnly {
		args = append(args, "--name-only")
	}
	if opts.NameStatus {
		args = append(args, "--name-status")
	}
	if opts.Stat {
		args = append(args, "--stat")
	}
	args = append(args, "HEAD", "--")
	return args
}

// 并发执行diff操作
type diffResult struct {
	Project *project.Project
	Name    string
	Output  string
	Missing bool
	Err     error
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

	// 确保在repo根目录下执行（保留原始目录用于项目参数路径解析）
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
	cfg, err := config.Load()
	if err != nil {
		log.Error("failed to load config: %v", err)
		return fmt.Errorf("failed to load config: %w", err)
	}
	opts.Config = cfg

	log.Debug("解析清单文件")
	groups := readCmdEffectiveGroups(opts.Groups, cfg)
	mf, err := manifest.NewParser().ParseFromFile(cfg.ManifestName, groups)
	if err != nil {
		log.Error("failed to parse manifest file: %v", err)
		return fmt.Errorf("failed to parse manifest: %w", err)
	}

	log.Debug("创建项目管理器")
	manager := project.NewManagerFromManifest(mf, cfg)

	var projects []*project.Project
	if len(projectNames) == 0 {
		projects, err = manager.GetProjectsInGroups(nil)
	} else {
		// 按上游语义解析项目参数（名字 + 路径回溯；"." 表示当前目录所在项目）
		projects, err = manager.GetProjectsByArgs(projectNames, originalDir)
	}
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

	diffArgs := buildDiffArgs(opts)

	// 并发执行diff操作
	for _, p := range projects {
		wg.Add(1)
		sem <- struct{}{}
		go func(proj *project.Project) {
			defer wg.Done()
			defer func() { <-sem }()

			// 未检出项目：跳过并提示，不视为失败
			if readCmdProjectMissing(proj) {
				results <- diffResult{Project: proj, Name: proj.Name, Missing: true}
				return
			}

			log.Debug("对项目 %s 执行 diff 操作", proj.Name)
			outBytes, err := proj.GitRepo.RunCommand(diffArgs...)
			results <- diffResult{Project: proj, Name: proj.Name, Output: string(outBytes), Err: err}
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
		if !res.Missing {
			successCount++
		}
	}
	sort.Slice(allResults, func(i, j int) bool { return allResults[i].Name < allResults[j].Name })

	for _, res := range allResults {
		if res.Missing {
			// 结果走 stdout，-q 不得吞掉
			path := readCmdProjectRelpath(res.Project)
			if path == "" {
				path = res.Name
			}
			readCmdResultLine("project %s/", path)
			readCmdResultLine("  missing (run \"repo sync\")")
			continue
		}
		if res.Err != nil {
			log.Error("项目 %s 执行 diff failed: %v", res.Name, res.Err)
			continue
		}
		if res.Output != "" {
			readCmdResultLine("--- %s ---", res.Name)
			fmt.Fprint(os.Stdout, res.Output)
			if !strings.HasSuffix(res.Output, "\n") {
				fmt.Fprintln(os.Stdout)
			}
		} else {
			readCmdResultLine("--- %s ---", res.Name)
			readCmdResultLine("(无变更)")
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
	cmd.Flags().StringVarP(&opts.Groups, "groups", "g", "", "restrict execution to projects in specified groups (comma-separated)")
	AddManifestFlags(cmd, &opts.CommonManifestOptions)
	return cmd
}
