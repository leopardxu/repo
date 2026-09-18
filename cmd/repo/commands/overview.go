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
	manifestObj, err := parser.ParseFromFile(cfg.ManifestName, manifest.SplitGroups(cfg.Groups))
	if err != nil {
		return fmt.Errorf("failed to parse manifest: %w", err)
	}

	manager := project.NewManagerFromManifest(manifestObj, cfg)
	var projects []*project.Project
	if len(args) == 0 {
		projects, err = manager.GetProjectsInGroups(nil)
	} else {
		projects, err = manager.GetProjectsByNames(args)
	}
	if err != nil {
		return fmt.Errorf("failed to get projects: %w", err)
	}

	type overviewEntry struct {
		Project *project.Project
		Branch  string
		Commits string
		Count   int
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

		// 尚未推送的本地提交：manifest revision..HEAD
		var commits string
		count := 0
		if p.Revision != "" {
			logOut, lErr := p.GitRepo.RunCommand("log", "--oneline", p.Revision+"..HEAD")
			if lErr == nil {
				commits = strings.TrimSpace(string(logOut))
				if commits != "" {
					count = strings.Count(commits, "\n") + 1
				}
			} else {
				log.Debug("项目 %s 计算 %s..HEAD failed: %v", p.Name, p.Revision, lErr)
			}
		}
		// 默认仅显示有未推送提交的项目；--verbose 显示全部
		if count == 0 && !opts.Verbose {
			continue
		}
		entries = append(entries, &overviewEntry{Project: p, Branch: branch, Commits: commits, Count: count})
	}

	sort.Slice(entries, func(i, j int) bool { return entries[i].Project.Name < entries[j].Project.Name })

	if len(entries) == 0 {
		log.Info("没有未推送的本地提交")
		return nil
	}

	for _, e := range entries {
		if e.Count > 0 {
			log.Info("project %s (%s): %d 个未推送提交", e.Project.Name, e.Branch, e.Count)
			if e.Commits != "" {
				log.Info("%s", e.Commits)
			}
		} else {
			log.Info("project %s (%s): 无未推送提交", e.Project.Name, e.Branch)
		}
	}
	return nil
}
