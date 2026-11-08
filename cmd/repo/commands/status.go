package commands

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
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

// readCmdResultLine 把一行结果 payload 写入 stdout。结果数据（项目路径、分支、
// porcelain 状态、缺失提示等）必须走 stdout；logger 仅承载诊断信息（stderr），
// 这样 `repo status >file` 等重定向能得到干净结果，且 -q 不会吞掉结果。
func readCmdResultLine(format string, args ...interface{}) {
	fmt.Fprintf(os.Stdout, format+"\n", args...)
}

// readCmdProjectRelpath 返回用于展示的项目相对路径。
// 镜像项目的 Path/Gitdir 可能是绝对裸库路径，展示必须用 Relpath；普通项目
// Relpath 同样存在。两者都为空时才回退 Name。
func readCmdProjectRelpath(p *project.Project) string {
	if p == nil {
		return ""
	}
	if p.Relpath != "" {
		return p.Relpath
	}
	if p.Path != "" {
		return p.Path
	}
	return p.Name
}

// readCmdProjectExists 判断项目仓库是否已存在：镜像项目检查裸库 Gitdir/Objdir，
// 普通项目检查工作树。用于 list 默认过滤（对齐上游 Project.Exists）。
func readCmdProjectExists(p *project.Project) bool {
	if p == nil {
		return false
	}
	if p.Mirror {
		repoDir := p.Gitdir
		if repoDir == "" {
			repoDir = p.Objdir
		}
		if repoDir == "" {
			return false
		}
		info, err := os.Stat(repoDir)
		return err == nil && info.IsDir()
	}
	if p.Worktree == "" {
		return false
	}
	info, err := os.Stat(p.Worktree)
	return err == nil && info.IsDir()
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
	cmd.Flags().StringVarP(&opts.Groups, "groups", "g", "", "restrict execution to projects in specified groups (comma-separated)")
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

	// 加载清单（显式 --groups 优先，否则 cfg.Groups；只解析一次）
	log.Debug("正在加载清单文件: %s", cfg.ManifestName)
	parser := manifest.NewParser()
	manifestObj, err := parser.ParseFromFile(cfg.ManifestName, readCmdEffectiveGroups(opts.Groups, cfg))
	if err != nil {
		log.Error("failed to parse manifest: %v", err)
		return fmt.Errorf("failed to parse manifest: %w", err)
	}
	log.Debug("成功加载清单，包含 %d 个项目", len(manifestObj.Projects))

	// 创建项目管理器
	log.Debug("正在初始化项目管理器...")
	manager := project.NewManagerFromManifest(manifestObj, cfg)

	// 获取要处理的项目（位置参数走上游名字+路径回溯语义）
	var projects []*project.Project
	if len(args) == 0 {
		log.Debug("获取所有项目..")
		projects, err = manager.GetProjectsInGroups(nil)
	} else {
		log.Debug("解析项目参数: %v", args)
		projects, err = manager.GetProjectsByArgs(args, originalDir)
	}
	if err != nil {
		log.Error("failed to get projectfailed: %v", err)
		return fmt.Errorf("failed to get projects: %w", err)
	}
	log.Debug("共获取到 %d 个项目", len(projects))

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
		Missing bool
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

			// 未检出项目：输出 missing 提示，不视为失败
			if readCmdProjectMissing(p) {
				results <- statusResult{Project: p, Missing: true}
				return
			}

			status, err := p.GetStatus()
			if err != nil {
				log.Error("failed to get project %s 状态failed %v", p.Name, err)
				stats.increment(false)
			} else {
				stats.increment(true)
				log.Debug("项目 %s 状态检查完成", p.Name)
			}

			// 始终解析当前分支：干净项目也要显示 project/branch 头（对齐上游）。
			// detached HEAD 返回空串，渲染为 (*** NO BRANCH ***)。
			branch := statusBranchName(p)

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

	// 收集结果并按 manifest 项目顺序输出，保证确定性（上游使用 ordered 并行结果）。
	collected := make([]statusResult, 0, len(projects))
	for res := range results {
		collected = append(collected, res)
	}
	order := make(map[*project.Project]int, len(projects))
	for i, p := range projects {
		order[p] = i
	}
	sort.SliceStable(collected, func(i, j int) bool {
		return order[collected[i].Project] < order[collected[j].Project]
	})

	// 处理结果
	var errs []error
	rendered := 0
	cleanDetached := 0
	for _, res := range collected {
		rendered++
		if res.Missing {
			readCmdResultLine("project %s/", readCmdProjectRelpath(res.Project))
			readCmdResultLine("  missing (run \"repo sync\")")
			continue
		}
		if res.Err != nil {
			errs = append(errs, fmt.Errorf("项目 %s: %w", res.Project.Name, res.Err))
			continue
		}

		status := strings.TrimRight(res.Status, "\n")
		clean := strings.TrimSpace(status) == ""
		// 上游语义：干净且 detached（无分支）的项目视为 CLEAN，不输出项目头；
		// 干净但在分支上的项目仍输出 project/branch 头。-b/--verbose 是 repo-go
		// 扩展，强制显示 CLEAN 项目。
		if clean && res.Branch == "" && !opts.Verbose && !opts.Branch {
			cleanDetached++
			continue
		}

		readCmdResultLine("%s", statusProjectHeaderLine(readCmdProjectRelpath(res.Project), res.Branch, opts.Quiet))
		if !opts.Quiet && status != "" {
			readCmdResultLine("%s", status)
		}
	}
	// 与上游一致：所有项目均为 CLEAN（干净 detached）且非 quiet 时输出汇总。
	if !opts.Quiet && rendered > 0 && cleanDetached == rendered {
		readCmdResultLine("nothing to commit (working directory clean)")
	}

	// --orphans：递归扫描工作树中未被任何 manifest 项目覆盖的文件/目录
	if opts.Orphans {
		relPaths := make([]string, 0, len(manifestObj.Projects))
		for _, mp := range manifestObj.Projects {
			rel := mp.Path
			if rel == "" {
				rel = mp.Name
			}
			relPaths = append(relPaths, rel)
		}
		orphans, oerr := computeOrphanDirs(cfg.RepoRoot, relPaths)
		if oerr != nil {
			log.Warn("扫描 orphan 目录failed: %v", oerr)
		} else if len(orphans) > 0 {
			readCmdResultLine("Objects not within a project (orphans):")
			for _, entry := range orphans {
				readCmdResultLine(" --\t%s", entry)
			}
		} else {
			readCmdResultLine("No orphan files or directories")
		}
	}

	// 统计信息属于诊断输出（stderr），不污染 stdout 结果 payload
	log.Info("状态检查操作完成，总计: %d，成功 %d，failed %d", stats.total, stats.success, stats.failed)

	// 如果有错误，返回汇总错误
	if len(errs) > 0 {
		return fmt.Errorf("%d projects failed: %w", len(errs), errors.Join(errs...))
	}

	return nil
}

// statusBranchName 返回当前分支短名；detached HEAD 或解析失败返回空串。
func statusBranchName(p *project.Project) string {
	out, err := p.GitRepo.Runner.RunInDir(p.GitRepo.Path, "symbolic-ref", "-q", "HEAD")
	if err != nil {
		return ""
	}
	ref := strings.TrimSpace(string(out))
	if !strings.HasPrefix(ref, "refs/heads/") {
		return ""
	}
	return strings.TrimPrefix(ref, "refs/heads/")
}

// statusProjectHeaderLine 生成上游格式的项目头：
//   - project 头使用 "%-40s" 填充（参数为 "path/ "），branch 紧接同一行；
//   - detached 项目显示 (*** NO BRANCH ***)；
//   - quiet 只输出项目头（无 branch、无文件明细）。
func statusProjectHeaderLine(relPath, branch string, quiet bool) string {
	line := fmt.Sprintf("project %-40s", relPath+"/ ")
	if quiet {
		return line
	}
	if branch == "" {
		return line + "(*** NO BRANCH ***)"
	}
	return line + "branch " + branch
}

// computeOrphanDirs 递归扫描 repoRoot，返回工作树中不属于任何 manifest 项目的
// 文件与目录（相对 repoRoot 的路径；目录带尾部 "/"）。未被覆盖的目录整体报告，
// 若某目录是项目的祖先目录则继续深入，确保 "dir/subdir/proj3" 这类嵌套孤儿不会
// 因父目录被覆盖而漏报。覆盖判定按项目相对路径的完整路径段前缀（精确分隔符），
// ".repo" 始终跳过。目录判定跟随符号链接（os.Stat），并用 realpath 去重防止
// 符号链接循环。结果排序以保证确定性。
func computeOrphanDirs(repoRoot string, projectRelPaths []string) ([]string, error) {
	projDirs := make(map[string]bool, len(projectRelPaths)+1)
	projParents := make(map[string]bool, len(projectRelPaths))
	for _, rel := range projectRelPaths {
		rel = filepath.ToSlash(filepath.Clean(rel))
		rel = strings.TrimPrefix(rel, "./")
		if rel == "" || rel == "." || rel == ".." {
			continue
		}
		projDirs[rel] = true
		for head := path.Dir(rel); head != "." && head != "/" && head != ""; head = path.Dir(head) {
			projParents[head] = true
		}
	}
	projDirs[".repo"] = true

	orphans := make([]string, 0)
	// visited 记录已遍历目录的 realpath，防止符号链接形成循环导致无限递归。
	visited := make(map[string]bool)
	var walk func(relDir string) error
	walk = func(relDir string) error {
		dir := repoRoot
		if relDir != "" {
			dir = filepath.Join(repoRoot, filepath.FromSlash(relDir))
		}
		if realDir, err := filepath.EvalSymlinks(dir); err == nil {
			realDir = filepath.Clean(realDir)
			if visited[realDir] {
				return nil
			}
			visited[realDir] = true
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			return err
		}
		for _, e := range entries {
			rel := e.Name()
			if relDir != "" {
				rel = path.Join(relDir, e.Name())
			}
			// 必须跟随符号链接判断目录：DirEntry.IsDir 对指向目录的符号链接
			// 返回 false，会把符号链接项目/父目录误报为孤儿文件。
			info, statErr := os.Stat(filepath.Join(dir, e.Name()))
			if statErr != nil || !info.IsDir() {
				orphans = append(orphans, rel)
				continue
			}
			if projDirs[rel] {
				continue
			}
			if projParents[rel] {
				if err := walk(rel); err != nil {
					return err
				}
				continue
			}
			orphans = append(orphans, rel+"/")
		}
		return nil
	}
	if err := walk(""); err != nil {
		return nil, err
	}
	sort.Strings(orphans)
	return orphans, nil
}
