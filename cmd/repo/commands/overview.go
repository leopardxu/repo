package commands

import (
	"fmt"
	"sort"
	"strings"

	"github.com/leopardxu/repo-go/internal/config"
	"github.com/leopardxu/repo-go/internal/logger"
	"github.com/leopardxu/repo-go/internal/manifest"
	"github.com/leopardxu/repo-go/internal/project"
	"github.com/spf13/cobra"
)

// OverviewOptions 包含 overview 命令的选项
type OverviewOptions struct {
	Quiet   bool
	Verbose bool
	Jobs    int
	Config  *config.Config
	CommonManifestOptions
}

// OverviewCmd 返回 overview 命令
// 显示各项目当前分支及尚未推送的本地提交（相对 manifest revision）。
func OverviewCmd() *cobra.Command {
	opts := &OverviewOptions{}
	cmd := &cobra.Command{
		Use:   "overview [<project>...]",
		Short: "Show overview of uncommitted/unpushed work across projects",
		Long: `Show an overview of local commits that have not been uploaded yet,
grouped by project. For each checked-out project it prints the current branch
and the commits ahead of the manifest revision.`,
		RunE: func(_ *cobra.Command, args []string) error {
			return runOverview(opts, args)
		},
	}
	cmd.Flags().BoolVarP(&opts.Verbose, "verbose", "v", false, "show all projects (including those with no unpushed commits)")
	cmd.Flags().BoolVarP(&opts.Quiet, "quiet", "q", false, "only show errors")
	cmd.Flags().IntVarP(&opts.Jobs, "jobs", "j", 8, "number of jobs to run in parallel")
	AddManifestFlags(cmd, &opts.CommonManifestOptions)
	return cmd
}

// runOverview 执行 overview 命令
func runOverview(opts *OverviewOptions, args []string) error {
	log := logger.NewDefaultLogger()
	if opts.Verbose {
		log.SetLevel(logger.LogLevelDebug)
	} else if opts.Quiet {
		log.SetLevel(logger.LogLevelError)
	} else {
		log.SetLevel(logger.LogLevelInfo)
	}

	originalDir, err := EnsureRepoRoot(log)
	if err != nil {
		return fmt.Errorf("failed to locate repo root: %w", err)
	}
	defer func() {
		if err := RestoreWorkDir(originalDir, log); err != nil {
			log.Warn("恢复工作目录failed: %v", err)
		}
	}()

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}
	opts.Config = cfg

	parser := manifest.NewParser()
	manifestObj, err := parser.ParseFromFile(cfg.ManifestName, manifest.SplitGroups(cfg.EffectiveGroups()))
	if err != nil {
		return fmt.Errorf("failed to parse manifest: %w", err)
	}

	manager := project.NewManagerFromManifest(manifestObj, cfg)
	var projects []*project.Project
	if len(args) == 0 {
		projects, err = manager.GetProjectsInGroups(nil)
	} else {
		projects, err = manager.GetProjectsByArgs(args, originalDir)
	}
	if err != nil {
		return fmt.Errorf("failed to get projects: %w", err)
	}

	type overviewEntry struct {
		Project *project.Project
		Branch  string
		Commits string
		Count   int
		Note    string
	}

	var entries []*overviewEntry
	for _, p := range projects {
		if p.Worktree == "" {
			continue
		}
		// 当前分支
		branchOut, bErr := p.GitRepo.RunCommand("rev-parse", "--abbrev-ref", "HEAD")
		if bErr != nil {
			log.Debug("项目 %s 获取分支failed: %v", p.Name, bErr)
			continue
		}
		branch := strings.TrimSpace(string(branchOut))

		// 尚未推送的本地提交：manifest revision..HEAD。
		// base 必须先解析为提交（远端跟踪 ref/SHA 优先），解析失败时显式标注，
		// 不得静默当作 0 个提交。
		base, ok := overviewResolveBase(p, manifestObj.Remotes, log)
		if !ok {
			note := fmt.Sprintf("无法解析 manifest revision %q，无法统计未推送提交", p.Revision)
			log.Warn("项目 %s %s", p.Name, note)
			entries = append(entries, &overviewEntry{Project: p, Branch: branch, Note: note})
			continue
		}
		var commits string
		count := 0
		logOut, lErr := p.GitRepo.RunCommand("log", "--oneline", base+"..HEAD")
		if lErr != nil {
			note := fmt.Sprintf("计算 %s..HEAD failed: %v", base, lErr)
			log.Warn("项目 %s %s", p.Name, note)
			entries = append(entries, &overviewEntry{Project: p, Branch: branch, Note: note})
			continue
		}
		commits = strings.TrimSpace(string(logOut))
		if commits != "" {
			count = strings.Count(commits, "\n") + 1
		}
		// 默认仅显示有未推送提交的项目；--verbose 显示全部
		if count == 0 && !opts.Verbose {
			continue
		}
		entries = append(entries, &overviewEntry{Project: p, Branch: branch, Commits: commits, Count: count})
	}

	sort.Slice(entries, func(i, j int) bool { return entries[i].Project.Name < entries[j].Project.Name })

	if len(entries) == 0 {
		readCmdResultLine("没有未推送的本地提交")
		return nil
	}

	for _, e := range entries {
		if e.Note != "" {
			readCmdResultLine("project %s (%s): %s", readCmdProjectRelpath(e.Project), e.Branch, e.Note)
			continue
		}
		if e.Count > 0 {
			readCmdResultLine("project %s (%s): %d 个未推送提交", readCmdProjectRelpath(e.Project), e.Branch, e.Count)
			if e.Commits != "" {
				readCmdResultLine("%s", e.Commits)
			}
		} else {
			readCmdResultLine("project %s (%s): 无未推送提交", readCmdProjectRelpath(e.Project), e.Branch)
		}
	}
	return nil
}

// overviewResolveBase 解析 overview 的比较基准：SHA 与 refs/tags 原样，
// 分支名与 refs/heads/X 先 refs/remotes/<remote>/X，再回退 refs/heads/X；
// 裸 revision 不作为 tag（tags 仅显式 refs/tags/ 前缀）。remotes 用于把清单
// remote name 映射成实际 git remote 名（有 alias 时用 alias）。解析成功后
// 返回提交 id。解析失败返回 ok=false，由调用方标注而不是静默当 0。
func overviewResolveBase(p *project.Project, remotes []manifest.Remote, log logger.Logger) (string, bool) {
	if p == nil || p.GitRepo == nil {
		return "", false
	}
	remote := readCmdRemoteRefName(p.RemoteName, remotes)
	if remote == "" {
		remote = "origin"
	}
	candidates := readCmdRevisionCandidates(p.Revision, remote, true)
	for _, candidate := range candidates {
		out, err := p.GitRepo.RunCommand("rev-parse", "--verify", candidate+"^{commit}")
		if err != nil {
			log.Debug("项目 %s 解析 revision 候选 %s failed: %v", p.Name, candidate, err)
			continue
		}
		if id := strings.TrimSpace(string(out)); id != "" {
			return id, true
		}
	}
	return "", false
}
