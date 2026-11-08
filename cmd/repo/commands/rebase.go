package commands

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	"github.com/leopardxu/repo-go/internal/config"
	"github.com/leopardxu/repo-go/internal/logger"
	"github.com/leopardxu/repo-go/internal/manifest"
	"github.com/leopardxu/repo-go/internal/project"
	"github.com/spf13/cobra"
)

// RebaseOptions 包含rebase命令的选项
type RebaseOptions struct {
	CommonManifestOptions
	Abort       bool
	Continue    bool
	Skip        bool
	Interactive bool
	Autosquash  bool
	Onto        string
	Force       bool
	FailFast    bool
	AutoStash   bool
	Whitespace  string
	Verbose     bool
	Quiet       bool
	Jobs        int
	// interactiveTTY 供测试注入 TTY 判定；nil 时使用真实 stdin/stdout。
	interactiveTTY func() bool
}

// rebaseStats 用于统计rebase命令的执行结果
type rebaseStats struct {
	mu      sync.Mutex
	success int
	failed  int
	total   int
}

// rebaseTarget 描述单个项目的 rebase 目标（项目 + 已解析的上游 ref）
type rebaseTarget struct {
	Project  *project.Project
	Upstream string
}

// RebaseCmd 返回rebase命令
func RebaseCmd() *cobra.Command {
	opts := &RebaseOptions{}

	cmd := &cobra.Command{
		Use:   "rebase [<project>...]",
		Short: "Rebase local branches on upstream branch",
		Long: `Rebase local branches on upstream branch.

'%prog' uses git rebase to move local changes in the current topic branch to
the HEAD of the upstream history, useful when you have made commits in a topic
branch but need to incorporate new upstream changes "underneath" them.`,
		RunE: func(_ *cobra.Command, args []string) error {
			return runRebase(opts, args)
		},
	}

	// 添加命令行选项
	cmd.Flags().BoolVar(&opts.Abort, "abort", false, "abort current rebase")
	cmd.Flags().BoolVar(&opts.Continue, "continue", false, "continue current rebase")
	cmd.Flags().BoolVar(&opts.Skip, "skip", false, "skip current patch and continue")
	cmd.Flags().BoolVarP(&opts.Interactive, "interactive", "i", false, "interactive rebase")
	cmd.Flags().BoolVar(&opts.Autosquash, "autosquash", false, "automatically squash fixup commits")
	cmd.Flags().StringVar(&opts.Onto, "onto", "", "rebase onto given branch instead of upstream")
	cmd.Flags().BoolVarP(&opts.Force, "force-rebase", "f", false, "force rebase even if branch is up to date (passes --force-rebase to git rebase)")
	cmd.Flags().BoolVar(&opts.FailFast, "fail-fast", false, "stop rebasing after first error is hit")
	cmd.Flags().BoolVar(&opts.AutoStash, "auto-stash", false, "stash local modifications before starting")
	cmd.Flags().StringVar(&opts.Whitespace, "whitespace", "", "pass --whitespace to git rebase")
	cmd.Flags().BoolVarP(&opts.Verbose, "verbose", "v", false, "show all output")
	cmd.Flags().BoolVarP(&opts.Quiet, "quiet", "q", false, "only show errors")
	AddManifestFlags(cmd, &opts.CommonManifestOptions)
	cmd.Flags().IntVarP(&opts.Jobs, "jobs", "j", 8, "number of jobs to run in parallel")

	return cmd
}

// runRebase 执行rebase命令
func runRebase(opts *RebaseOptions, args []string) error {
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

	log.Debug("开始执行rebase操作")

	// 加载配置
	log.Debug("正在加载配置...")
	cfg, err := config.Load()
	if err != nil {
		log.Error("failed to load config: %v", err)
		return fmt.Errorf("failed to load config: %w", err)
	}

	// 加载清单
	log.Debug("正在解析清单文件...")
	parser := manifest.NewParser()
	manifestObj, err := parser.ParseFromFile(cfg.ManifestName, manifest.SplitGroups(cfg.EffectiveGroups()))
	if err != nil {
		log.Error("failed to parse manifest file: %v", err)
		return fmt.Errorf("failed to parse manifest: %w", err)
	}

	// 子清单作用域 flag（--outer-manifest/--this-manifest-only/--all-manifests）已接受，
	// 但 repo-go 在 init 时合并所有 include 为扁平 manifest，未保留 manifest 树，
	// 故不在此按层做差异化处理（统一作用于全部已合并项目）。详见 AddManifestFlags 注释。

	// 创建项目管理器
	log.Debug("正在创建项目管理器...")
	manager := project.NewManagerFromManifest(manifestObj, cfg)

	var projects []*project.Project

	// 获取项目列表（名字 + 路径回溯；"." 表示当前目录所在项目）
	if len(args) == 0 {
		log.Debug("获取所有项目")
		projects, err = manager.GetProjectsInGroups(nil)
		if err != nil {
			log.Error("获取所有项目failed: %v", err)
			return fmt.Errorf("failed to get projects: %w", err)
		}
	} else {
		log.Debug("获取指定的项目: %v", args)
		projects, err = manager.GetProjectsByArgs(args, originalDir)
		if err != nil {
			log.Error("获取指定项目failed: %v", err)
			return fmt.Errorf("failed to get projects by name: %w", err)
		}
	}

	// 重复的项目参数只处理一次，避免同一项目被并发 rebase 两次。
	projects = dedupeProjects(projects)

	// 对齐上游 GetProjects：无项目参数时只处理已 checkout 的项目；显式参数
	// 则要求项目均已同步。过滤必须在统计与 -i 计数之前完成。
	projects, err = rebaseSelectProjects(projects, len(args) != 0)
	if err != nil {
		log.Error("筛选 rebase 项目failed: %v", err)
		return err
	}

	log.Debug("找到 %d 个项目需要执行rebase操作", len(projects))

	// 交互式 rebase 需要编辑器：仅允许单项目串行执行；非 TTY 时要求通过
	// GIT_SEQUENCE_EDITOR/GIT_EDITOR 提供编辑器。计数使用过滤后的项目。
	if err := checkRebaseInteractive(opts, len(projects)); err != nil {
		log.Error("交互式 rebase 前置检查失败: %v", err)
		return err
	}

	// 构建rebase命令选项
	log.Debug("构建rebase命令选项...")
	// 基础参数（不含上游，上游在派发时按项目追加）
	baseArgs := buildRebaseBaseArgs(opts, log)

	needUpstream := !opts.Abort && !opts.Continue && !opts.Skip

	// 解析每个项目的上游 ref（对齐上游 Rebase.Execute）：
	//   - detached HEAD：单项目报错，多项目跳过；
	//   - 当前分支没有跟踪任何远程分支：单项目报错，多项目跳过。
	// 绝不静默回退到 manifest 修订版本。在派发前完成解析，便于并行执行时
	// 也能正确跳过无法 rebase 的项目。
	targets, err := resolveRebaseTargets(projects, needUpstream, log)
	if err != nil {
		log.Error("解析 rebase 上游失败: %v", err)
		return err
	}

	// 创建统计对象
	stats := &rebaseStats{total: len(targets)}

	// 设置并发控制
	maxWorkers := opts.Jobs
	if maxWorkers <= 0 {
		maxWorkers = 8
	}
	// 交互式 rebase 需要 TTY/编辑器，并发会产生交错或失败，强制串行。
	if opts.Interactive {
		maxWorkers = 1
	}

	buildArgs := func(t rebaseTarget) []string {
		rebaseArgs := make([]string, len(baseArgs))
		copy(rebaseArgs, baseArgs)
		if needUpstream && t.Upstream != "" {
			rebaseArgs = append(rebaseArgs, t.Upstream)
		}
		return rebaseArgs
	}

	var failedProjects []string

	// --fail-fast 必须真正串行：逐个执行，首个失败后立即返回，不再派发后续项目。
	if opts.FailFast || maxWorkers == 1 {
		log.Debug("串行执行rebase操作（fail-fast=%v, 并发数=%d）", opts.FailFast, maxWorkers)
		for _, t := range targets {
			log.Debug("正在对项目 %s 执行rebase操作...", t.Project.Name)
			output, rerr := rebaseInvoke(t.Project, buildArgs(t), opts.Interactive)
			if rerr != nil {
				stats.failed++
				log.Error("项目 %s rebasefailed: %v", t.Project.Name, rerr)
				if len(output) > 0 {
					log.Error("Git 输出:\n%s", string(output))
				}
				if opts.FailFast {
					return fmt.Errorf("failed to rebase project %s: %w", t.Project.Name, rerr)
				}
				failedProjects = append(failedProjects, t.Project.Name)
				continue
			}
			stats.success++
			if !opts.Quiet {
				log.Debug("项目 %s rebase成功", t.Project.Name)
			}
			// git 输出是结果 payload，写 stdout，不被 -q 吞掉。
			rebaseWriteOutput(os.Stdout, t.Project.Name, string(output))
		}

		return finishRebase(stats, failedProjects, log)
	}

	// 并发执行rebase操作
	log.Debug("开始并发执行rebase操作，并发数: %d", maxWorkers)
	type rebaseResult struct {
		Project *project.Project
		Output  string
		Err     error
	}

	results := make(chan rebaseResult, len(targets))
	sem := make(chan struct{}, maxWorkers)
	var wg sync.WaitGroup

	for _, target := range targets {
		t := target
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()

			log.Debug("正在对项目 %s 执行rebase操作...", t.Project.Name)
			outputBytes, rerr := rebaseInvoke(t.Project, buildArgs(t), opts.Interactive)
			output := string(outputBytes)

			// 失败日志统一在结果处理阶段输出一次，避免并行路径重复打印。
			if rerr != nil {
				stats.mu.Lock()
				stats.failed++
				stats.mu.Unlock()
			} else {
				if !opts.Quiet {
					log.Debug("项目 %s rebase成功", t.Project.Name)
				}
				stats.mu.Lock()
				stats.success++
				stats.mu.Unlock()
			}

			results <- rebaseResult{Project: t.Project, Output: output, Err: rerr}
		}()
	}

	// 等待所有goroutine完成
	log.Debug("等待所有rebase任务完成...")
	wg.Wait()
	close(results)

	// 处理结果
	log.Debug("处理rebase结果...")
	for res := range results {
		if res.Err != nil {
			failedProjects = append(failedProjects, res.Project.Name)
			log.Error("项目 %s rebasefailed: %v", res.Project.Name, res.Err)
			if res.Output != "" {
				log.Error("Git 输出:\n%s", res.Output)
			}
			continue
		}

		// git 输出是结果 payload，写 stdout，不被 -q 吞掉。
		rebaseWriteOutput(os.Stdout, res.Project.Name, res.Output)
	}

	return finishRebase(stats, failedProjects, log)
}

// buildRebaseBaseArgs 构造 git rebase 的基础参数（不含上游 ref）。
// --abort/--continue/--skip 为互斥的一次性动作，优先于其它选项。
func buildRebaseBaseArgs(opts *RebaseOptions, log logger.Logger) []string {
	baseArgs := []string{"rebase"}

	switch {
	case opts.Abort:
		baseArgs = append(baseArgs, "--abort")
		log.Debug("添加--abort选项")
		return baseArgs
	case opts.Continue:
		baseArgs = append(baseArgs, "--continue")
		log.Debug("添加--continue选项")
		return baseArgs
	case opts.Skip:
		baseArgs = append(baseArgs, "--skip")
		log.Debug("添加--skip选项")
		return baseArgs
	}

	if opts.Interactive {
		baseArgs = append(baseArgs, "--interactive")
		log.Debug("添加--interactive选项")
	}

	if opts.Autosquash {
		baseArgs = append(baseArgs, "--autosquash")
		log.Debug("添加--autosquash选项")
	}

	if opts.Onto != "" {
		baseArgs = append(baseArgs, "--onto", opts.Onto)
		log.Debug("添加--onto %s选项", opts.Onto)
	}

	// --force-rebase 为 git rebase 原生支持的参数，透传给 git rebase。
	if opts.Force {
		baseArgs = append(baseArgs, "--force-rebase")
	}

	if opts.Whitespace != "" {
		baseArgs = append(baseArgs, "--whitespace", opts.Whitespace)
		log.Debug("添加--whitespace %s选项", opts.Whitespace)
	}

	if opts.AutoStash {
		baseArgs = append(baseArgs, "--autostash")
		log.Debug("添加--autostash选项")
	}

	// -q 透传给 git rebase
	if opts.Quiet {
		baseArgs = append(baseArgs, "--quiet")
	}

	return baseArgs
}

// rebaseInteractiveTTY 判断当前进程 stdin/stdout 是否连接到终端。
func rebaseInteractiveTTY() bool {
	return rebaseFileIsTTY(os.Stdin) && rebaseFileIsTTY(os.Stdout)
}

// rebaseFileIsTTY 判断文件是否为字符设备（终端）。
func rebaseFileIsTTY(f *os.File) bool {
	if f == nil {
		return false
	}
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

// rebaseEditorConfigured 判断是否通过环境变量提供了交互编辑器。
// 设置 GIT_SEQUENCE_EDITOR 或 GIT_EDITOR 的非 TTY 场景（CI/脚本）可正常
// 驱动 git rebase -i，不应被 TTY 校验拒绝。
func rebaseEditorConfigured() bool {
	return strings.TrimSpace(os.Getenv("GIT_SEQUENCE_EDITOR")) != "" ||
		strings.TrimSpace(os.Getenv("GIT_EDITOR")) != ""
}

// checkRebaseInteractive 校验交互式 rebase 的前置条件：
// 仅支持单个项目（串行）；无 TTY 时必须显式配置 GIT_SEQUENCE_EDITOR/GIT_EDITOR，
// 否则编辑器无法工作。完全无编辑器且无 TTY 时才拒绝。
func checkRebaseInteractive(opts *RebaseOptions, projectCount int) error {
	if opts == nil || !opts.Interactive {
		return nil
	}
	if projectCount != 1 {
		return fmt.Errorf("interactive rebase requires exactly one project, got %d", projectCount)
	}
	tty := rebaseInteractiveTTY
	if opts.interactiveTTY != nil {
		tty = opts.interactiveTTY
	}
	if !tty() && !rebaseEditorConfigured() {
		return fmt.Errorf("interactive rebase requires a terminal or GIT_SEQUENCE_EDITOR/GIT_EDITOR")
	}
	return nil
}

// rebaseSelectProjects 按上游 GetProjects 语义筛选项目：
//   - 未显式指定项目：跳过未 checkout（工作树不存在）的项目；
//   - 显式指定项目：要求全部已同步，任一未同步即返回错误。
//
// 供 runRebase 在统计/-i 计数之前调用。
func rebaseSelectProjects(projects []*project.Project, explicit bool) ([]*project.Project, error) {
	if !explicit {
		return filterSyncedProjects(projects), nil
	}
	if err := requireSyncedProjects(projects); err != nil {
		return nil, err
	}
	return projects, nil
}

// resolveRebaseTargets 解析每个项目 rebase 的上游 ref（对齐上游 Rebase.Execute）：
//   - detached HEAD：单项目报错，多项目跳过；
//   - 当前分支未跟踪任何远程分支：单项目报错，多项目跳过。
//
// 绝不回退 manifest revision。交互/恢复模式（needUpstream=false）不解析上游。
func resolveRebaseTargets(projects []*project.Project, needUpstream bool, log logger.Logger) ([]rebaseTarget, error) {
	oneProject := len(projects) == 1
	targets := make([]rebaseTarget, 0, len(projects))
	for _, p := range projects {
		upstream := ""
		if needUpstream {
			current := rebaseCurrentBranch(p)
			if current == "" {
				if oneProject {
					return nil, fmt.Errorf("project %s has a detached HEAD", p.Path)
				}
				log.Debug("项目 %s 处于 detached HEAD 状态，跳过 rebase", p.Name)
				continue
			}
			upstream = rebaseUpstreamRef(p, current)
			if upstream == "" {
				if oneProject {
					return nil, fmt.Errorf("project %s does not track any remote branches", p.Path)
				}
				log.Debug("项目 %s 的当前分支 %s 未跟踪任何远程分支，跳过 rebase", p.Name, current)
				continue
			}
			log.Debug("项目 %s: rebase %s -> %s", p.Name, current, upstream)
		}
		targets = append(targets, rebaseTarget{Project: p, Upstream: upstream})
	}
	return targets, nil
}

// rebaseWriteOutput 把项目 rebase 的 git 输出写到 w（结果 payload 不走 logger）。
// 无输出（例如 git rebase 静默成功）时不产生任何 stdout 内容。
func rebaseWriteOutput(w io.Writer, projectName, output string) {
	if output == "" {
		return
	}
	fmt.Fprintf(w, "\n项目 %s:\n%s", projectName, output)
	if !strings.HasSuffix(output, "\n") {
		fmt.Fprintln(w)
	}
}

// rebaseInvoke 执行单个项目的 rebase：
//   - 交互模式走 RunInteractive，stdin/stdout 直连终端（编辑器与提示可用）；
//   - 非交互模式保持 RunCommand，捕获输出用于汇总。
func rebaseInvoke(p *project.Project, args []string, interactive bool) ([]byte, error) {
	if interactive {
		return nil, p.GitRepo.RunInteractive(args...)
	}
	return p.GitRepo.RunCommand(args...)
}

// finishRebase 输出统计并汇总失败项目
func finishRebase(stats *rebaseStats, failedProjects []string, log logger.Logger) error {
	// 输出统计信息
	log.Info("Rebase操作完成: 总计 %d 个项目, 成功 %d 个, failed %d 个",
		stats.total, stats.success, stats.failed)

	if len(failedProjects) > 0 {
		log.Error("以下项目 rebase failed:")
		for _, name := range failedProjects {
			log.Error("  - %s", name)
		}
		log.Error("\n请检查错误信息并手动解决冲突，然后使用 'repo rebase --continue' 继续")
		return fmt.Errorf("some projects failed to rebase")
	}
	return nil
}

// rebaseCurrentBranch 返回当前分支短名：symbolic-ref HEAD 返回完整 ref
// （refs/heads/main），自行剥离前缀；--short 在同名 tag 存在时会返回
// "heads/main" 歧义形式，不能使用。detached HEAD 时 symbolic-ref 失败，
// 返回空串（对齐上游 CurrentBranch=None）。
func rebaseCurrentBranch(p *project.Project) string {
	out, err := p.GitRepo.RunCommand("symbolic-ref", "HEAD")
	if err != nil {
		return ""
	}
	ref := strings.TrimSpace(string(out))
	if !strings.HasPrefix(ref, "refs/heads/") {
		return ""
	}
	return strings.TrimPrefix(ref, "refs/heads/")
}

// rebaseConfigGet 读取项目 git 配置值；不存在或读取失败返回空串
func rebaseConfigGet(p *project.Project, key string) string {
	out, err := p.GitRepo.RunCommand("config", "--get", key)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// rebaseUpstreamRef 计算项目当前分支 rebase 的上游 ref：
// 仅使用 branch.<name>.merge + branch.<name>.remote（对齐上游 Branch.LocalMerge /
// RemoteSpec.ToLocal）。当前分支未配置跟踪关系时返回空串，绝不回退 manifest
// revision —— 上游在单项目时报错、多项目时跳过，不会静默 rebase 到 manifest。
func rebaseUpstreamRef(p *project.Project, currentBranch string) string {
	branchRemote := ""
	branchMerge := ""
	if currentBranch != "" {
		branchRemote = rebaseConfigGet(p, "branch."+currentBranch+".remote")
		branchMerge = rebaseConfigGet(p, "branch."+currentBranch+".merge")
	}
	return buildRebaseUpstream(branchRemote, branchMerge)
}

// buildRebaseUpstream 根据分支跟踪配置构造 rebase 上游 ref。
//
// branchRemote/branchMerge 任一为空表示当前分支未配置跟踪关系，返回空串。
// 关键约束：
//   - branch.<name>.merge = refs/heads/x 且远端为 origin 时得到 origin/x，
//     绝不得到 origin/refs/heads/x；
//   - remote = "." 时保持本地 refs/heads/x；
//   - 非分支 ref（refs/tags/、refs/remotes/、SHA 等）原样使用。
//
// isRebaseCommitID 判断是否为完整提交 ID（40/64 位小写十六进制）。
func isRebaseCommitID(rev string) bool {
	if len(rev) != 40 && len(rev) != 64 {
		return false
	}
	for _, c := range rev {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func buildRebaseUpstream(branchRemote, branchMerge string) string {
	if branchRemote == "" || branchMerge == "" {
		return ""
	}
	if strings.HasPrefix(branchMerge, "refs/heads/") {
		name := strings.TrimPrefix(branchMerge, "refs/heads/")
		if branchRemote == "." {
			return branchMerge
		}
		return branchRemote + "/" + name
	}
	// 提交 ID 是不可变对象，不需要按远端限定。
	if isRebaseCommitID(branchMerge) {
		return branchMerge
	}
	// git 配置的 merge 一般为完整 ref；兼容裸分支名并按远端映射。
	if !strings.HasPrefix(branchMerge, "refs/") {
		if branchRemote == "." {
			return "refs/heads/" + branchMerge
		}
		return branchRemote + "/" + branchMerge
	}
	// 非分支 ref（refs/tags/、refs/remotes/、SHA 等）原样使用
	return branchMerge
}
