package commands

import (
	"fmt"
	"runtime"
	"strings"
	"sync"

	"github.com/leopardxu/repo-go/internal/config"
	"github.com/leopardxu/repo-go/internal/git"
	"github.com/leopardxu/repo-go/internal/logger"
	"github.com/leopardxu/repo-go/internal/manifest"
	"github.com/leopardxu/repo-go/internal/project"
	"github.com/spf13/cobra"
)

// InfoOptions 包含info命令的选项
type InfoOptions struct {
	Diff            bool
	Overview        bool
	CurrentBranch   bool
	NoCurrentBranch bool
	LocalOnly       bool
	Jobs            int
	Verbose         bool
	Quiet           bool
	Config          *config.Config
	CommonManifestOptions
}

// infoStats 用于统计info命令的执行结果
type infoStats struct {
	mu      sync.Mutex
	success int
	failed  int
}

// InfoCmd 返回info命令
func InfoCmd() *cobra.Command {
	opts := &InfoOptions{}

	cmd := &cobra.Command{
		Use:   "info [-dl] [-o [-c]] [<project>...]",
		Short: "Get info on the manifest branch, current branch or unmerged branches",
		Long:  `Show detailed information about projects including branch info.`,
		RunE: func(_ *cobra.Command, args []string) error {
			return runInfo(opts, args)
		},
	}

	// 添加命令行选项
	cmd.Flags().BoolVarP(&opts.Diff, "diff", "d", false, "show full info and commit diff including remote branches")
	cmd.Flags().BoolVarP(&opts.Overview, "overview", "o", false, "show overview of all local commits")
	cmd.Flags().BoolVarP(&opts.CurrentBranch, "current-branch", "c", false, "consider only checked out branches")
	cmd.Flags().BoolVar(&opts.NoCurrentBranch, "no-current-branch", false, "consider all local branches")
	cmd.Flags().BoolVarP(&opts.LocalOnly, "local-only", "l", false, "disable all remote operations")
	cmd.Flags().IntVarP(&opts.Jobs, "jobs", "j", min(runtime.NumCPU(), 8), "number of jobs to run in parallel")
	cmd.Flags().BoolVarP(&opts.Verbose, "verbose", "v", false, "show all output")
	cmd.Flags().BoolVarP(&opts.Quiet, "quiet", "q", false, "only show errors")
	cmd.Flags().StringVarP(&opts.Groups, "groups", "g", "", "restrict execution to projects in specified groups (comma-separated)")
	AddManifestFlags(cmd, &opts.CommonManifestOptions)
	return cmd
}

// runInfo 执行info命令
// runInfo executes the info command logic
func runInfo(opts *InfoOptions, args []string) error {
	// 初始化日志记录器
	log := logger.NewDefaultLogger()
	if opts.Verbose {
		log.SetLevel(logger.LogLevelDebug)
	} else if opts.Quiet {
		log.SetLevel(logger.LogLevelError)
	} else {
		log.SetLevel(logger.LogLevelInfo)
	}

	log.Debug("Starting info command")

	// -c/--current-branch 与 --no-current-branch 互斥（前者仅当前分支，后者所有本地分支）
	if opts.CurrentBranch && opts.NoCurrentBranch {
		return fmt.Errorf("cannot specify both --current-branch and --no-current-branch")
	}

	// 确保在repo根目录下执行
	originalDir, err := EnsureRepoRoot(log)
	if err != nil {
		log.Error("Failed to locate repo root: %v", err)
		return err
	}
	defer func() {
		if err := RestoreWorkDir(originalDir, log); err != nil {
			log.Warn("恢复工作目录failed: %v", err)
		}
	}()

	// 加载配置
	cfg, err := config.Load() // 声明err
	if err != nil {
		log.Error("Failed to load config: %v", err)
		return err
	}
	opts.Config = cfg // 分配加载的配置

	// 加载manifest（显式 --groups 优先，否则 cfg.Groups；只解析一次）
	log.Debug("Loading manifest from %s", cfg.ManifestName)
	parser := manifest.NewParser()
	manifestObj, err := parser.ParseFromFile(cfg.ManifestName, readCmdEffectiveGroups(opts.Groups, cfg))
	if err != nil {
		log.Error("Failed to parse manifest: %v", err)
		return err
	}

	// 创建项目管理器
	manager := project.NewManagerFromManifest(manifestObj, cfg)

	// 获取要操作的项目（位置参数走上游名字+路径回溯语义；无参 = 全部）
	var projects []*project.Project
	if len(args) == 0 {
		log.Debug("Getting all projects")
		projects, err = manager.GetProjectsInGroups(nil)
	} else {
		log.Debug("Resolving projects by args: %v", args)
		projects, err = manager.GetProjectsByArgs(args, originalDir)
	}
	if err != nil {
		log.Error("Failed to get projects: %v", err)
		return err
	}

	log.Debug("Found %d projects to process", len(projects))

	// -c/--current-branch（默认）：仅显示当前分支（默认模式）或仅当前分支提交（-o）
	// --no-current-branch：列出所有本地分支（默认模式）或考虑所有本地分支提交（-o --all）
	// --local-only：禁用远程操作（diff 使用已有远端引用，不访问网络）
	if opts.LocalOnly {
		log.Debug("--local-only：禁用远程操作（使用已有远端引用）")
	}
	if opts.NoCurrentBranch {
		log.Debug("--no-current-branch：考虑所有本地分支")
	} else {
		log.Debug("-c：仅考虑当前分支")
	}

	// 显示 manifest 级信息（对齐上游 repo info 头部）
	log.Debug("Manifest URL: %s", cfg.ManifestURL)
	log.Debug("Manifest branch: %s", cfg.ManifestBranch)
	if shaOut, shaErr := git.NewRunner().RunInDir(".repo/manifests", "rev-parse", "HEAD"); shaErr == nil {
		if sha := strings.TrimSpace(string(shaOut)); sha != "" {
			log.Debug("Manifest SHA: %s", sha)
		}
	}

	// 并发failed to get project信息
	type infoResult struct {
		Project *project.Project
		Output  string
		Missing bool
		Err     error
	}

	results := make(chan infoResult, len(projects))
	jobs := opts.Jobs
	if jobs < 1 {
		jobs = 8
	}
	sem := make(chan struct{}, jobs) // 控制并发数（-j）
	var wg sync.WaitGroup
	stats := &infoStats{}

	for _, p := range projects {
		wg.Add(1)
		sem <- struct{}{}
		go func(proj *project.Project) {
			defer func() {
				<-sem
				wg.Done()
			}()

			var output string
			var err error
			var outputBytes []byte

			log.Debug("Processing project %s", proj.Name)

			// 未初始化项目跳过（不视为失败）；镜像项目按裸库目录判定，
			// 便于展示 Relpath 而非绝对 .git 路径。
			if !readCmdProjectExists(proj) || proj.GitRepo == nil {
				results <- infoResult{Project: proj, Missing: true}
				return
			}

			// 根据选项显示不同信息
			// -c/--current-branch 影响：默认模式仅显示当前分支、-o 模式仅当前分支提交；
			// --no-current-branch 则列出所有本地分支 / 给 log 追加 --all。
			// --local-only 禁用远程操作（diff 模式下不访问网络，只使用已有远端引用）
			switch {
			case opts.Diff:
				log.Debug("Getting diff for project %s", proj.Name)
				// 默认比较目标顺序：项目 revision（SHA/tag/本地分支）→
				// refs/remotes/<remote>/<revision> → cfg.ManifestBranch 远端引用。
				// --local-only 时只使用仓库已有引用，不访问网络（不刷新远端）。
				target, terr := infoResolveDiffTarget(proj, cfg)
				if terr != nil {
					// detached HEAD / 无远端跟踪引用：明确标注无法比较远端，不伪造本地提交
					log.Warn("项目 %s 无法比较远端: %v", proj.Name, terr)
					output = infoDiffUnavailable(terr.Error())
					break
				}
				outputBytes, err = proj.GitRepo.RunCommand(infoDiffArgs(target)...)
				if err == nil {
					local, remote := infoSplitDiffCommits(string(outputBytes))
					output = infoFormatDiff(local, remote)
				} else {
					// HEAD 无效（如空仓库）时同样标注无法比较远端，不使整命令失败
					log.Warn("项目 %s 无法比较远端 %s: %v", proj.Name, target, err)
					output = infoDiffUnavailable(err.Error())
					err = nil
				}
			case opts.Overview:
				log.Debug("Getting overview for project %s", proj.Name)
				// --no-current-branch：考虑所有本地分支（追加 --all）
				logArgs := []string{"log", "--oneline", "-10"}
				if opts.NoCurrentBranch {
					logArgs = append(logArgs, "--all")
				}
				outputBytes, err = proj.GitRepo.RunCommand(logArgs...)
				if err == nil {
					output = strings.TrimSpace(string(outputBytes))
				}
			default:
				// 默认显示项目元数据（对齐上游 repo info）
				// -c/--current-branch（默认）：仅当前分支；--no-current-branch：列出所有本地分支
				log.Debug("Getting metadata for project %s", proj.Name)
				remoteName := proj.RemoteName
				if remoteName == "" {
					remoteName = "origin"
				}
				var sb strings.Builder
				fmt.Fprintf(&sb, "Project: %s\n", proj.Name)
				fmt.Fprintf(&sb, "Path: %s\n", readCmdProjectRelpath(proj))
				fmt.Fprintf(&sb, "Remote name: %s\n", remoteName)
				fmt.Fprintf(&sb, "Manifest revision: %s\n", proj.Revision)
				if opts.NoCurrentBranch {
					if branches, bErr := proj.ListLocalBranches(); bErr == nil && len(branches) > 0 {
						fmt.Fprintf(&sb, "Local branches: %s\n", strings.Join(branches, ", "))
					}
				} else {
					currentBranch := ""
					if bOut, bErr := proj.GitRepo.RunCommand("rev-parse", "--abbrev-ref", "HEAD"); bErr == nil {
						currentBranch = strings.TrimSpace(string(bOut))
					}
					if currentBranch != "" {
						fmt.Fprintf(&sb, "Current branch: %s\n", currentBranch)
					}
				}
				output = strings.TrimRight(sb.String(), "\n")
			}

			results <- infoResult{Project: proj, Output: output, Err: err}
		}(p)
	}

	// 等待所有goroutine完成后关闭结果通道
	go func() {
		wg.Wait()
		close(results)
	}()

	// 收集并显示结果
	for res := range results {
		if res.Missing {
			log.Debug("跳过未检出项目 %s (missing)", res.Project.Name)
			continue
		}
		if res.Err != nil {
			stats.mu.Lock()
			stats.failed++
			stats.mu.Unlock()

			log.Error("Error getting info for %s: %v", res.Project.Name, res.Err)
			continue
		}

		stats.mu.Lock()
		stats.success++
		stats.mu.Unlock()

		if res.Output != "" {
			readCmdResultLine("--- %s ---", res.Project.Name)
			readCmdResultLine("%s", res.Output)
		} else {
			readCmdResultLine("--- %s ---", res.Project.Name)
			readCmdResultLine("(No changes)")
		}
	}

	// 显示统计信息
	log.Debug("Info command completed: %d successful, %d failed", stats.success, stats.failed)

	if stats.failed > 0 {
		return fmt.Errorf("%d projects failed", stats.failed)
	}

	return nil
}

// infoDiffArgs 构造 -d 的三点比较参数：HEAD...<target>（target 为
// refs/remotes/... 远端引用）。target 为空表示没有可比较的远端目标。
// --left-right 用于区分本地独有（<）与远端独有（>）提交，对齐上游
// info.py:_GetDiffCommits 的一次 history walk 语义。
func infoDiffArgs(target string) []string {
	target = strings.TrimSpace(target)
	if target == "" {
		return nil
	}
	return []string{"log", "--oneline", "--left-right", "HEAD..." + target, "--"}
}

// infoDiffTargetRef 构造远端跟踪引用 refs/remotes/<remote>/<branch>。
// remote 为空时回退 origin；branch 为空时返回空串；refs/heads/ 前缀会被
// 去掉（对齐上游 manifest merge 分支处理），其它完整引用原样返回。
func infoDiffTargetRef(remoteName, branch string) string {
	branch = strings.TrimSpace(branch)
	branch = strings.TrimPrefix(branch, "refs/heads/")
	if branch == "" {
		return ""
	}
	if strings.HasPrefix(branch, "refs/") {
		return branch
	}
	remote := strings.TrimSpace(remoteName)
	if remote == "" {
		remote = "origin"
	}
	return "refs/remotes/" + remote + "/" + branch
}

// infoDiffTargetCandidates 返回 -d 的比较目标尝试顺序（对齐本次审计要求）：
//  1. SHA/refs/tags 等不可变 revision 直接使用；
//  2. 分支名与 refs/heads/X 先 refs/remotes/<remote>/X，再回退 refs/heads/X；
//  3. 最后回退清单分支 cfg.ManifestBranch 的远端跟踪引用。
//
// 裸 revision 只按分支名处理（tag 仅支持显式 refs/tags/ 前缀），本地回退用
// 完整 refs/heads/ 形式，避免 git 的 DWIM 把同名 tag 解析出来。三者按顺序
// 去重后返回。注意这里只构造候选，是否可用由调用方 rev-parse 验证。
func infoDiffTargetCandidates(remoteName, manifestBranch, revision string) []string {
	candidates := make([]string, 0, 4)
	seen := make(map[string]bool, 4)
	appendCandidate := func(ref string) {
		ref = strings.TrimSpace(ref)
		if ref == "" || seen[ref] {
			return
		}
		seen[ref] = true
		candidates = append(candidates, ref)
	}
	for _, ref := range readCmdRevisionCandidates(revision, remoteName, true) {
		appendCandidate(ref)
	}
	appendCandidate(infoDiffTargetRef(remoteName, manifestBranch))
	return candidates
}

// infoResolveDiffTarget 按候选顺序解析项目可用的比较目标：优先不可变 revision
// （SHA/显式 refs/tags），其次 refs/remotes/<remote>/<revision> 跟踪引用，
// 再回退 refs/heads/<revision>，最后回退 cfg.ManifestBranch。全部不可用时
// 返回错误（调用方据此标注"无法比较远端"）。--local-only 时调用方不刷新远端，
// 因此这里只读取仓库已有引用，绝不访问网络。
func infoResolveDiffTarget(proj *project.Project, cfg *config.Config) (string, error) {
	if proj == nil || proj.GitRepo == nil {
		return "", fmt.Errorf("项目未检出，无法解析比较目标")
	}
	manifestBranch := ""
	if cfg != nil {
		manifestBranch = cfg.ManifestBranch
	}
	candidates := infoDiffTargetCandidates(proj.RemoteName, manifestBranch, proj.Revision)
	if len(candidates) == 0 {
		return "", fmt.Errorf("项目 %s 没有可用的 revision 或远端分支", proj.Name)
	}
	var lastErr error
	for _, candidate := range candidates {
		if _, err := proj.GitRepo.RunCommand("rev-parse", "--verify", candidate+"^{commit}"); err != nil {
			lastErr = err
			continue
		}
		return candidate, nil
	}
	return "", fmt.Errorf("无法解析比较目标 %s: %w", strings.Join(candidates, ", "), lastErr)
}

// infoDiffUnavailable 生成无法比较远端时的输出（不伪造本地提交）。
func infoDiffUnavailable(reason string) string {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "无法解析远端比较目标"
	}
	return "无法比较远端: " + reason
}

// infoSplitDiffCommits 把 --left-right 的 git log 输出拆成 local/remote 两组提交。
func infoSplitDiffCommits(output string) (local []string, remote []string) {
	local = make([]string, 0)
	remote = make([]string, 0)
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimRight(line, "\r")
		switch {
		case strings.HasPrefix(line, "<"):
			if c := strings.TrimSpace(line[1:]); c != "" {
				local = append(local, c)
			}
		case strings.HasPrefix(line, ">"):
			if c := strings.TrimSpace(line[1:]); c != "" {
				remote = append(remote, c)
			}
		}
	}
	return local, remote
}

// infoFormatDiff 生成 -d 的 Local Commits / Remote Commits 两段输出。
func infoFormatDiff(local, remote []string) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Local Commits: %d (on current branch)\n", len(local))
	for _, c := range local {
		sb.WriteString(c)
		sb.WriteByte('\n')
	}
	sb.WriteString("----------------------------\n")
	fmt.Fprintf(&sb, "Remote Commits: %d\n", len(remote))
	for _, c := range remote {
		sb.WriteString(c)
		sb.WriteByte('\n')
	}
	return strings.TrimRight(sb.String(), "\n")
}
