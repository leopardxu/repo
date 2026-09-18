package commands

import (
	"fmt"
	"strings"

	"github.com/leopardxu/repo-go/internal/config"
	"github.com/leopardxu/repo-go/internal/logger"
	"github.com/leopardxu/repo-go/internal/manifest"
	"github.com/leopardxu/repo-go/internal/project"
	"github.com/leopardxu/repo-go/internal/repo_sync"
	"github.com/spf13/cobra"
)

// CherryPickOptions holds the options for the cherry-pick command
type CherryPickOptions struct {
	All      bool
	X        bool // -x：记录原始提交
	FF       bool // --ff：允许 fast-forward
	Continue bool // --continue：恢复中断的 cherry-pick
	Abort    bool // --abort：放弃当前 cherry-pick
	Skip     bool // --skip：跳过当前补丁
	Jobs     int
	Quiet    bool
	Verbose  bool
	Config   *config.Config
	CommonManifestOptions
}

// CherryPickCmd creates the cherry-pick command
func CherryPickCmd() *cobra.Command {
	opts := &CherryPickOptions{}
	cmd := &cobra.Command{
		Use:   "cherry-pick <commit> [<project>...]",
		Short: "Cherry-pick a commit onto the current branch",
		Long: `Applies the changes introduced by the named commit(s) onto the current branch.

Note: this is a repo-go extension; upstream repo has no standalone cherry-pick
subcommand (use "repo download -c" to download and cherry-pick a Gerrit change).`,
		Args: cobra.MinimumNArgs(0),
		RunE: func(_ *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return fmt.Errorf("failed to load config: %w", err)
			}
			opts.Config = cfg
			return runCherryPick(opts, args)
		},
	}
	cmd.Flags().BoolVar(&opts.All, "all", false, "cherry-pick in all projects")
	cmd.Flags().BoolVarP(&opts.X, "x", "x", false, "record the original commit in the cherry-pick commit message")
	cmd.Flags().BoolVar(&opts.FF, "ff", false, "allow fast-forward when cherry-picking")
	cmd.Flags().BoolVar(&opts.Continue, "continue", false, "resume an interrupted cherry-pick")
	cmd.Flags().BoolVar(&opts.Abort, "abort", false, "abort the current cherry-pick")
	cmd.Flags().BoolVar(&opts.Skip, "skip", false, "skip the current patch and continue")
	cmd.Flags().IntVarP(&opts.Jobs, "jobs", "j", 8, "number of projects to cherry-pick in parallel")
	cmd.Flags().BoolVarP(&opts.Quiet, "quiet", "q", false, "only show errors")
	cmd.Flags().BoolVarP(&opts.Verbose, "verbose", "v", false, "show all output")
	AddManifestFlags(cmd, &opts.CommonManifestOptions)
	return cmd
}

// runCherryPick executes the cherry-pick command logic
func runCherryPick(opts *CherryPickOptions, args []string) error {
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

	cfg := opts.Config

	// 解析 commit 与项目名：恢复模式（--continue/--abort/--skip）无需 commit 参数
	var commit string
	var projectNames []string
	if opts.Continue || opts.Abort || opts.Skip {
		projectNames = args
	} else {
		if len(args) < 1 {
			return fmt.Errorf("commit hash required (or use --continue/--abort/--skip to resume)")
		}
		commit = args[0]
		projectNames = args[1:]
		log.Debug("正在应用 cherry-pick '%s'", commit)
		// 当前仅支持本地 commit；Gerrit change 路径（按 change-id 下载后应用）尚未实现
		if strings.HasPrefix(commit, "I") && len(commit) == 41 {
			log.Warn("检测到 Change-Id 格式参数；Gerrit change 路径尚未实现，将按本地 commit 处理（可能failed）")
		}
	}

	// 一个提交属于单个项目：默认应用到所有项目是危险且无意义的，必须显式指定项目或 --all。
	if len(projectNames) == 0 && !opts.All {
		return fmt.Errorf("specify at least one project (or use --all to cherry-pick in all projects)")
	}

	parser := manifest.NewParser()
	manifestObj, err := parser.ParseFromFile(cfg.ManifestName, manifest.SplitGroups(cfg.Groups))
	if err != nil {
		log.Error("failed to parse manifest file: %v", err)
		return fmt.Errorf("failed to parse manifest: %w", err)
	}

	manager := project.NewManagerFromManifest(manifestObj, cfg)
	var projects []*project.Project
	if opts.All {
		log.Debug("获取所有项目")
		projects, err = manager.GetProjectsInGroups(nil)
		if err != nil {
			log.Error("failed to get project列表failed: %v", err)
			return fmt.Errorf("failed to get projects: %w", err)
		}
	} else {
		log.Debug("获取指定项目: %v", projectNames)
		projects, err = manager.GetProjectsByNames(projectNames)
		if err != nil {
			log.Error("获取指定项目failed: %v", err)
			return fmt.Errorf("failed to get projects by name: %w", err)
		}
	}

	// 恢复模式：在每个项目中跑 git cherry-pick --continue/--abort/--skip
	if opts.Continue || opts.Abort || opts.Skip {
		return resumeCherryPick(opts, projects, log)
	}

	log.Debug("开始在 %d 个项目中应用 cherry-pick", len(projects))

	// 使用 repo_sync 包中的 Engine 进行 cherry-pick 操作
	syncOpts := &repo_sync.Options{
		Jobs:         opts.Jobs,
		Quiet:        opts.Quiet,
		Verbose:      opts.Verbose,
		CherryPickX:  opts.X,
		CherryPickFF: opts.FF,
	}

	engine := repo_sync.NewEngine(syncOpts, nil, log)
	engine.SetCommitHash(commit)
	err = engine.CherryPickCommit(projects)
	if err != nil {
		log.Error("Cherry-pick failed: %v", err)
		return err
	}

	success, failed := engine.GetCherryPickStats()

	if !opts.Quiet {
		log.Info("Cherry-pick 提交 '%s' 完成: %d 成功, %d failed", commit, success, failed)
	}

	if failed > 0 {
		return fmt.Errorf("cherry-pick failed for %d projects", failed)
	}

	return nil
}

// resumeCherryPick 在每个项目中恢复/放弃/跳过中断的 cherry-pick（git cherry-pick --continue/--abort/--skip）。
func resumeCherryPick(opts *CherryPickOptions, projects []*project.Project, log logger.Logger) error {
	subcmd := "--continue"
	switch {
	case opts.Abort:
		subcmd = "--abort"
	case opts.Skip:
		subcmd = "--skip"
	}
	success, failed := 0, 0
	for _, p := range projects {
		if p.Worktree == "" {
			continue
		}
		if _, err := p.GitRepo.RunCommand("cherry-pick", subcmd); err != nil {
			failed++
			log.Error("项目 %s cherry-pick %s failed: %v", p.Name, subcmd, err)
		} else {
			success++
			if !opts.Quiet {
				log.Info("项目 %s cherry-pick %s 成功", p.Name, subcmd)
			}
		}
	}
	log.Info("Cherry-pick %s 完成: %d 成功, %d failed", subcmd, success, failed)
	if failed > 0 {
		return fmt.Errorf("cherry-pick %s failed for %d projects", subcmd, failed)
	}
	return nil
}
