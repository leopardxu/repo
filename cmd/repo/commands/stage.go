package commands

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"github.com/leopardxu/repo-go/internal/config"
	"github.com/leopardxu/repo-go/internal/logger"
	"github.com/leopardxu/repo-go/internal/manifest"
	"github.com/leopardxu/repo-go/internal/project"
	"github.com/spf13/cobra"
)

// stageUsageText 是 stage 命令的用法说明（仅指定项目而没有文件/选项时打印）。
const stageUsageText = "usage: repo stage [<project>...] [<file>...]\n" +
	"       repo stage [--all|--update|--interactive|--patch] [<project>...]"

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

	// --interactive/--patch 需要 TTY：无终端时 git add -i/-p 会失败，提前给出明确错误
	if (opts.Interactive || opts.Patch) && !stdinIsTTY() {
		log.Error("--interactive/--patch 需要交互式终端")
		return fmt.Errorf("--interactive/--patch require a TTY")
	}

	// 无参数且没有任何会自行选择文件的选项：直接打印用法，避免无谓加载清单
	if len(args) == 0 && !opts.All && !opts.Update && !opts.Interactive && !opts.Patch {
		log.Error("%s", stageUsageText)
		return fmt.Errorf("no files specified; %s", stageUsageText)
	}

	log.Debug("开始暂存文件")

	// 加载清单：必须用未按组过滤的完整清单构建 manager，
	// 这样 allProjects 能包含全部项目（含未选中组），文件归属检查才能识别
	// “文件属于未选中的项目”，而不是误判为不属于任何项目。
	log.Debug("正在加载清单文件: %s", opts.Config.ManifestName)
	parser := manifest.NewParser()
	manifestObj, err := parser.ParseFromFile(opts.Config.ManifestName, nil)
	if err != nil {
		log.Error("failed to parse manifest: %v", err)
		return fmt.Errorf("failed to parse manifest: %w", err)
	}
	log.Debug("成功加载清单，包含 %d 个项目", len(manifestObj.Projects))

	// 创建项目管理器
	log.Debug("正在初始化项目管理器...")
	manager := project.NewManagerFromManifest(manifestObj, opts.Config)

	// 规范化路径：工作树可能是相对 repo 根目录的路径，比较前统一为绝对路径，
	// 避免 TopDir 为 "." 时与用户传入的绝对路径无法匹配。
	repoRoot := manager.TopDir
	if repoRoot == "" {
		repoRoot = originalDir
	}
	if absRoot, aerr := filepath.Abs(repoRoot); aerr == nil {
		repoRoot = absRoot
	}
	baseDir := originalDir
	if absBase, aerr := filepath.Abs(baseDir); aerr == nil {
		baseDir = absBase
	}

	// 区分项目参数与文件 pathspec：前导的项目名/工作树路径属于项目选择，
	// 首个非项目参数起全部视为文件（对齐 stage [<project>...] [<file>...]）。
	allProjects := dedupeStageProjects(manager.GetProjects())
	projectArgs, files := splitStageArgs(allProjects, args, baseDir, repoRoot)
	log.Debug("解析参数: %d 个项目参数，%d 个文件 pathspec", len(projectArgs), len(files))

	// 仅指定项目而没有文件、也未使用会自行选择文件的选项时：打印用法并报错，
	// 不能静默地“成功暂存 0 个文件”。
	if len(files) == 0 && !opts.All && !opts.Update && !opts.Interactive && !opts.Patch {
		log.Error("%s", stageUsageText)
		return fmt.Errorf("no files specified; %s", stageUsageText)
	}

	// 获取要处理的项目：显式项目参数按上游语义解析（名字 + 路径）；
	// 否则按配置组过滤（清单本身已完整解析，组过滤只在选择阶段生效）。
	groups := stageSelectionGroups(opts.Config)
	log.Debug("选择要处理的项目: projectArgs=%v groups=%v", projectArgs, groups)
	projects, err := selectStageProjects(manager, projectArgs, originalDir, groups)
	if err != nil {
		log.Error("failed to get projects: %v", err)
		return fmt.Errorf("failed to get projects: %w", err)
	}
	log.Debug("共获取到 %d 个项目", len(projects))

	// 将文件 pathspec 归属到持有该路径的项目。归属判断基于 manager 的全部项目：
	// 文件属于未选中的嵌套项目时必须报错，不能交给父项目假成功。
	assigned, unassigned := assignStageFiles(allProjects, projects, files, baseDir, repoRoot, len(projectArgs) > 0)
	if len(unassigned) > 0 {
		msgs := make([]string, 0, len(unassigned))
		for _, u := range unassigned {
			msgs = append(msgs, u.String())
		}
		log.Error("以下路径无法暂存: %s", strings.Join(msgs, "; "))
		return fmt.Errorf("cannot stage paths: %s", strings.Join(msgs, "; "))
	}

	// --all 且没有文件 pathspec：每个项目暂存整个工作树，补上嵌套项目排除项。
	stageAllNestedExcludes(assigned, allProjects, projects, files, opts, repoRoot)

	// 使用goroutine池并发执行stage
	// 规范化并发数：-j <= 0 会导致 makechan panic，回退到默认 8（与其他命令一致）
	jobs := opts.Jobs
	if jobs <= 0 {
		jobs = 8
	}
	// --interactive/--patch 需要终端 TTY，必须串行执行避免竞争
	if opts.Interactive || opts.Patch {
		jobs = 1
	}
	log.Debug("开始暂存文件，并行任务 %d...", jobs)

	var wg sync.WaitGroup
	errChan := make(chan error, len(projects))
	resultChan := make(chan string, len(projects))
	sem := make(chan struct{}, jobs) // 使用信号量控制并发数

	for _, p := range projects {
		p := p // 创建副本避免闭包问题

		// 只有拥有该文件路径的项目才执行 pathspec，其余项目直接跳过，
		// 避免对每个项目都报 "pathspec did not match" 错误。
		projectFiles := assigned[p]
		if len(files) > 0 && len(projectFiles) == 0 {
			log.Debug("项目 %s 不包含指定文件，跳过", p.Name)
			continue
		}
		if p.Worktree == "" {
			log.Debug("项目 %s 没有工作树，跳过", p.Name)
			continue
		}

		// 交互模式对齐上游 stage -i：只对存在未提交修改的项目启动交互
		if opts.Interactive || opts.Patch {
			clean, cerr := p.GitRepo.IsClean()
			if cerr != nil {
				log.Warn("检查项目 %s 状态failed: %v", p.Name, cerr)
			} else if clean {
				log.Debug("项目 %s 没有未提交修改，跳过交互", p.Name)
				continue
			}
		}

		stageArgs := buildStageArgs(opts, projectFiles)

		wg.Add(1)
		sem <- struct{}{} // 获取信号

		go func() {
			defer wg.Done()
			defer func() { <-sem }() // 释放信号

			log.Debug("在项目 %s 中执行 git %s...", p.Name, strings.Join(stageArgs, " "))
			run := func() (string, error) {
				if opts.Interactive || opts.Patch {
					// 交互命令必须直连终端，否则 git add -i/-p 无法工作
					return "", p.GitRepo.RunInteractive(stageArgs...)
				}
				out, rerr := p.GitRepo.RunCommand(stageArgs...)
				if rerr != nil {
					return "", rerr
				}
				return strings.TrimSpace(string(out)), nil
			}

			output, rerr := run()
			if rerr != nil {
				log.Error("项目 %s 暂存failed: %v", p.Name, rerr)
				errChan <- fmt.Errorf("project %s: %w", p.Name, rerr)
				stats.increment(false)
				return
			}

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

	// 输出结果：命令 payload 走 stdout（可被调用方捕获），诊断日志仍走 logger。
	for result := range resultChan {
		fmt.Fprintf(os.Stdout, "%s\n", result)
	}

	// 显示统计信息（诊断输出，保持走 logger/默认 stderr）
	log.Info("暂存操作完成，总计: %d，成功 %d，failed %d", stats.total, stats.success, stats.failed)

	// 没有任何项目被处理时不能报告成功（例如项目全部被 pathspec 归属过滤）
	if stats.total == 0 {
		if opts.Interactive || opts.Patch {
			return fmt.Errorf("no projects have uncommitted modifications")
		}
		return fmt.Errorf("no projects staged: no matching files in selected projects")
	}

	// 如果有错误，返回汇总错误
	if len(errs) > 0 {
		return fmt.Errorf("%d projects failed: %w", len(errs), errors.Join(errs...))
	}

	return nil
}

// splitStageArgs 将参数拆分为项目选择参数与文件 pathspec。
//
// 规则对齐上游 stage [<project>...] [<file>...]：从前往后消费参数，只要参数
// 精确等于某个项目名或项目工作树路径就视为项目选择；遇到第一个非项目参数后，
// 其余参数全部视为文件 pathspec。仅做精确匹配（不做父目录回溯），避免把项目内
// 的普通文件路径误判成项目参数。
func splitStageArgs(projects []*project.Project, args []string, baseDir, repoRoot string) (projectArgs, files []string) {
	if baseDir == "" {
		baseDir, _ = os.Getwd()
	}
	if repoRoot == "" {
		repoRoot = baseDir
	}

	i := 0
	for ; i < len(args); i++ {
		if !isProjectSelector(projects, args[i], baseDir, repoRoot) {
			break
		}
		projectArgs = append(projectArgs, args[i])
	}
	files = args[i:]
	return projectArgs, files
}

// isProjectSelector 判断参数是否精确选中某个项目（项目名或工作树路径）。
// "." 永远是文件 pathspec：cwd 在项目根时它表示该项目工作树下的当前目录，
// 不能被工作树路径匹配误判成项目选择参数。
func isProjectSelector(projects []*project.Project, arg, baseDir, repoRoot string) bool {
	if arg == "" || isDotSpec(arg) {
		return false
	}
	absArg := arg
	if !filepath.IsAbs(absArg) {
		absArg = filepath.Join(baseDir, absArg)
	}
	absArg = filepath.Clean(absArg)

	for _, p := range projects {
		if p.Name == arg {
			return true
		}
		wt := projectWorktree(p, repoRoot)
		if wt != "" && filepath.Clean(wt) == absArg {
			return true
		}
	}
	return false
}

// projectWorktree 返回项目工作树的绝对路径（相对路径基于 repo 根目录解析）。
func projectWorktree(p *project.Project, repoRoot string) string {
	wt := p.Worktree
	if wt == "" {
		wt = p.Path
	}
	if wt == "" {
		return ""
	}
	if !filepath.IsAbs(wt) {
		wt = filepath.Join(repoRoot, wt)
	}
	return filepath.Clean(wt)
}

// stageUnassigned 描述一个无法归属到选中项目的文件 pathspec。
// Owner 是全部项目中最深的归属项目；为 nil 表示不属于任何项目。
type stageUnassigned struct {
	File  string
	Owner *project.Project
}

// String 返回人类可读的失败原因。
func (u stageUnassigned) String() string {
	if u.Owner == nil {
		return fmt.Sprintf("%s: not inside any project", u.File)
	}
	return fmt.Sprintf("%s: belongs to project %q which is not selected", u.File, u.Owner.Name)
}

// assignStageFiles 把文件 pathspec 归属到项目：只有路径位于项目工作树内时才归属，
// 并转换为项目内相对路径（命令在该项目工作树中执行）。
//
// allProjects 是清单中的全部项目（未按组过滤），selected 是本次命令选中的项目。
// 归属判断必须基于全部项目，否则选中父项目时嵌套子项目的文件会被父项目“假成功”吞掉。
// explicitSelection 表示用户显式给出了项目参数（只有此时才启用单项目相对路径回退，
// 否则不在项目内的相对路径一律按 unowned 报错）。
// 返回的 unassigned 列出无法归属到选中项目的 pathspec（含不属于任何项目，
// 以及属于未选中项目两种情况）。
func assignStageFiles(allProjects, selected []*project.Project, files []string, baseDir, repoRoot string, explicitSelection bool) (map[*project.Project][]string, []stageUnassigned) {
	assigned := make(map[*project.Project][]string, len(selected))
	if len(files) == 0 {
		return assigned, nil
	}
	if baseDir == "" {
		baseDir, _ = os.Getwd() //nolint:errcheck // 获取失败时 baseDir 为空，后续按相对路径处理
	}
	if repoRoot == "" {
		repoRoot = baseDir
	}

	selectedSet := make(map[*project.Project]struct{}, len(selected))
	for _, p := range selected {
		selectedSet[p] = struct{}{}
	}

	unassigned := make([]stageUnassigned, 0)
	for _, f := range files {
		if f == "" {
			continue
		}
		absFile := resolveStagePath(f, baseDir)

		// "." 表示当前目录：对每个工作树位于该目录之下（或就是该目录）的选中项目
		// 暂存 "."。cwd 在项目根时同样按 pathspec 处理，而不是项目选择器。
		if isDotSpec(f) {
			dirAssigned := false
			for _, p := range selected {
				wt := projectWorktree(p, repoRoot)
				if _, ok := subdirOf(absFile, wt); ok {
					// 父项目暂存 "." 时必须排除其内部嵌套项目的工作树路径，
					// 否则父仓库会把子项目目录写成 gitlink。
					appendStagePaths(assigned, allProjects, p, repoRoot, ".")
					dirAssigned = true
					continue
				}
			}
			if dirAssigned {
				continue
			}
			// cwd 在某个项目工作树内部（非根）时落回普通归属逻辑，
			// 相对路径会退化为工作树内的子目录。
		}

		// 在全部项目中寻找最深（路径最长）的归属项目，避免嵌套项目重复暂存同一文件。
		if owners := deepestStageOwners(allProjects, absFile, repoRoot); len(owners) > 0 {
			unassigned = append(unassigned, assignStageOwners(allProjects, owners, selectedSet, f, absFile, repoRoot, assigned)...)
			continue
		}

		// 回退：仅当用户显式给出项目参数且只选中一个项目时，允许相对该项目工作树的路径
		// （如 `repo stage proj src/a.go`）。未显式选择项目时，不在任何项目工作树内的
		// 相对路径必须按 unowned 报错，不能猜成某个项目。
		// 不做文件 Stat（支持已删除文件与 git pathspec glob），但项目工作树必须已检出，
		// 否则无法判断路径归属，按 unowned 报错。
		if explicitSelection && len(selected) == 1 && !filepath.IsAbs(f) {
			wt := projectWorktree(selected[0], repoRoot)
			if wt != "" {
				if info, statErr := os.Stat(wt); statErr == nil && info.IsDir() {
					if _, ok := pathWithinWorktree(wt, f, wt); ok {
						// 命中回退后先解析为绝对路径，再对全部项目重新做最深归属检查：
						// 该相对路径可能实际落在未选中的嵌套子项目里，
						// 绝不能交给被选中的父项目假成功。
						absRel := resolveStagePath(f, wt)
						owners := deepestStageOwners(allProjects, absRel, repoRoot)
						if len(owners) == 0 {
							appendStagePaths(assigned, allProjects, selected[0], repoRoot, f)
							continue
						}
						unassigned = append(unassigned, assignStageOwners(allProjects, owners, selectedSet, f, absRel, repoRoot, assigned)...)
						continue
					}
				}
			}
		}
		unassigned = append(unassigned, stageUnassigned{File: f})
	}
	return assigned, unassigned
}

// deepestStageOwners 返回包含 absPath 的全部项目中最深（工作树路径最长）的项目；
// 同深度项目全部返回，交由调用方逐个判断是否选中。
func deepestStageOwners(projects []*project.Project, absPath, repoRoot string) []*project.Project {
	var owners []*project.Project
	bestLen := -1
	for _, p := range projects {
		wt := projectWorktree(p, repoRoot)
		if wt == "" {
			continue
		}
		if _, ok := pathWithinWorktree(wt, absPath, ""); !ok {
			continue
		}
		if len(wt) > bestLen {
			bestLen = len(wt)
			owners = []*project.Project{p}
		} else if len(wt) == bestLen {
			owners = append(owners, p)
		}
	}
	return owners
}

// assignStageOwners 把 pathspec 归属到最深项目集合：选中项目写入相对路径，
// 未选中项目记录为 unassigned（避免父项目吞掉嵌套/未选中项目的文件）。
func assignStageOwners(allProjects, owners []*project.Project, selected map[*project.Project]struct{}, f, absFile, repoRoot string, assigned map[*project.Project][]string) []stageUnassigned {
	var unassigned []stageUnassigned
	for _, p := range owners {
		if _, ok := selected[p]; !ok {
			unassigned = append(unassigned, stageUnassigned{File: f, Owner: p})
			continue
		}
		wt := projectWorktree(p, repoRoot)
		relPath, ok := pathWithinWorktree(wt, absFile, "")
		if !ok {
			continue
		}
		// 含嵌套项目的父项目：任何 pathspec（包括子目录）都要排除嵌套项目，
		// 避免父仓库把子项目目录写成 gitlink。
		appendStagePaths(assigned, allProjects, p, repoRoot, relPath)
	}
	return unassigned
}

// appendStagePaths 把 pathspec 追加到项目 p 的暂存列表，并在 p 工作树内含嵌套项目时
// 一并追加排除项（对子目录 pathspec 同样生效），确保父仓库不会把嵌套项目写成 gitlink。
// 排除项按项目去重，多个 pathspec 不会重复累加同一排除项。
func appendStagePaths(assigned map[*project.Project][]string, allProjects []*project.Project, p *project.Project, repoRoot string, paths ...string) {
	assigned[p] = append(assigned[p], paths...)
	// 排除项始终放在 pathspec 列表末尾：多次追加路径后仍保持
	// “正向 pathspec + 末尾 exclude”的稳定顺序。
	kept := assigned[p][:0]
	for _, spec := range assigned[p] {
		if !strings.HasPrefix(spec, ":(exclude)") {
			kept = append(kept, spec)
		}
	}
	assigned[p] = kept
	for _, exclude := range nestedProjectExcludes(allProjects, p, repoRoot) {
		if !containsStageSpec(assigned[p], exclude) {
			assigned[p] = append(assigned[p], exclude)
		}
	}
}

// containsStageSpec 判断 pathspec 列表中是否已包含 want。
func containsStageSpec(specs []string, want string) bool {
	for _, spec := range specs {
		if spec == want {
			return true
		}
	}
	return false
}

// stageAllNestedExcludes 在 --all/-A 且没有显式文件 pathspec 时，为每个选中项目
// 追加 nestedProjectExcludes 排除项：父项目暂存整个工作树时不能把内部嵌套项目
// （包括未选中的嵌套项目）写成 gitlink。
func stageAllNestedExcludes(assigned map[*project.Project][]string, allProjects, selected []*project.Project, files []string, opts *StageOptions, repoRoot string) {
	if len(files) > 0 || opts == nil || !opts.All {
		return
	}
	for _, p := range selected {
		excludes := nestedProjectExcludes(allProjects, p, repoRoot)
		if len(excludes) == 0 {
			continue
		}
		assigned[p] = append(assigned[p], excludes...)
	}
}

// nestedProjectExcludes 返回 parent 工作树内嵌套的其他项目工作树路径，
// 以 git pathspec 排除形式 (:(exclude)path) 表示，供父项目暂存 "." 或子目录
// pathspec 时使用，避免父仓库写入 gitlink。
func nestedProjectExcludes(allProjects []*project.Project, parent *project.Project, repoRoot string) []string {
	parentWT := projectWorktree(parent, repoRoot)
	if parentWT == "" {
		return nil
	}
	excludes := make([]string, 0)
	seen := make(map[string]struct{})
	for _, q := range allProjects {
		if q == parent {
			continue
		}
		qwt := projectWorktree(q, repoRoot)
		if qwt == "" {
			continue
		}
		rel, ok := subdirOf(parentWT, qwt)
		if !ok || rel == "." {
			continue
		}
		spec := ":(exclude)" + filepath.ToSlash(rel)
		if _, dup := seen[spec]; dup {
			continue
		}
		seen[spec] = struct{}{}
		excludes = append(excludes, spec)
	}
	return excludes
}

// subdirOf 判断 target 是否等于 base 或位于 base 之下，返回相对 base 的路径。
func subdirOf(base, target string) (string, bool) {
	if base == "" || target == "" {
		return "", false
	}
	rel, err := filepath.Rel(base, target)
	if err != nil {
		return "", false
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return rel, true
}

// dedupeStageProjects 按项目身份（name + 工作树/路径）去重，保持原顺序。
func dedupeStageProjects(projects []*project.Project) []*project.Project {
	if len(projects) < 2 {
		return projects
	}
	seen := make(map[string]struct{}, len(projects))
	deduped := make([]*project.Project, 0, len(projects))
	for _, p := range projects {
		if p == nil {
			continue
		}
		keyPath := p.Worktree
		if keyPath == "" {
			keyPath = p.Path
		}
		key := p.Name + "\x00" + keyPath
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		deduped = append(deduped, p)
	}
	return deduped
}

// stageSelectionGroups 返回 stage 选择项目时使用的组过滤：stage 没有 -g 选项，
// 一律使用配置的有效组（空值回退 "default"），避免空 Groups 时误选全部项目。
func stageSelectionGroups(cfg *config.Config) []string {
	return manifest.SplitGroups(cfg.EffectiveGroups())
}

// selectStageProjects 返回本次命令实际处理的项目集合：
// 显式项目参数按上游语义解析（名字 + 路径）；否则按配置组过滤全部项目。
func selectStageProjects(manager *project.Manager, projectArgs []string, baseDir string, groups []string) ([]*project.Project, error) {
	var (
		projects []*project.Project
		err      error
	)
	if len(projectArgs) == 0 {
		projects, err = manager.GetProjectsInGroups(groups)
	} else {
		projects, err = manager.GetProjectsByArgs(projectArgs, baseDir)
	}
	if err != nil {
		return nil, err
	}
	return dedupeStageProjects(projects), nil
}

// isDotSpec 判断 pathspec 是否为当前目录 "."。
func isDotSpec(f string) bool {
	return !filepath.IsAbs(f) && filepath.Clean(f) == "."
}

// resolveStagePath 把文件参数解析为绝对且规范的路径。
func resolveStagePath(f, baseDir string) string {
	p := f
	if !filepath.IsAbs(p) {
		p = filepath.Join(baseDir, p)
	}
	return filepath.Clean(p)
}

// pathWithinWorktree 判断文件参数（基于 baseDir 解析）是否位于工作树 wt 内，
// 是则返回相对工作树的路径。
func pathWithinWorktree(wt, file, baseDir string) (string, bool) {
	absFile := file
	if !filepath.IsAbs(absFile) {
		absFile = filepath.Join(baseDir, absFile)
	}
	absFile = filepath.Clean(absFile)

	rel, err := filepath.Rel(wt, absFile)
	if err != nil {
		return "", false
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return rel, true
}

// buildStageArgs 构建 git add 参数。
func buildStageArgs(opts *StageOptions, files []string) []string {
	args := []string{"add"}
	if opts.All {
		args = append(args, "--all")
	}
	if opts.Interactive {
		args = append(args, "--interactive")
	}
	if opts.Patch {
		args = append(args, "--patch")
	}
	if opts.Force {
		args = append(args, "--force")
	}
	if opts.Update {
		args = append(args, "--update")
	}
	if opts.Verbose {
		args = append(args, "--verbose")
	}
	if len(files) > 0 {
		// -- 分隔，避免文件路径以 - 开头被当作选项
		args = append(args, "--")
		args = append(args, files...)
	}
	return args
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
