package commands

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/leopardxu/repo-go/internal/config"
	"github.com/leopardxu/repo-go/internal/logger"
	"github.com/leopardxu/repo-go/internal/manifest"
	"github.com/leopardxu/repo-go/internal/project"
	"github.com/leopardxu/repo-go/internal/repo_sync"
	"github.com/spf13/cobra"
)

// SyncOptions 包含sync命令的选项
type SyncOptions struct {
	Jobs                   int
	JobsNetwork            int
	JobsCheckout           int
	CurrentBranch          bool
	NoCurrentBranch        bool
	Detach                 bool
	ForceSync              bool
	ForceRemoveDirty       bool
	ForceOverwrite         bool
	ForceBroken            bool // 继续同步即使项目已损坏
	LocalOnly              bool
	NetworkOnly            bool
	Prune                  bool
	NoPrune                bool // 显式禁用prune
	Quiet                  bool
	Verbose                bool // 是否显示详细日志
	SmartSync              bool
	Tags                   bool
	NoCloneBundle          bool
	FetchSubmodules        bool
	NoTags                 bool
	OptimizedFetch         bool
	RetryFetches           int
	Groups                 string
	FailFast               bool
	NoManifestUpdate       bool
	ManifestServerUsername string
	ManifestServerPassword string
	ManifestServerURL      string // manifest服务器URL
	NoManifestServer       bool   // 禁用manifest服务器
	UseSuperproject        bool
	NoUseSuperproject      bool
	HyperSync              bool
	SmartTag               string
	NoThisManifestOnly     bool
	AutoGC                 bool   // sync后自动运行git gc
	NoAutoGC               bool   // 禁用自动gc
	GitLFS                 bool   // 是否启用Git LFS支持
	DefaultRemote          string // 默认远程仓库名称，用于解决分支匹配多个远程的问题
	Reference              string // 本地参考仓库路径（仓库本身或参考仓库目录），用于加速克隆
	Dissociate             bool   // 克隆后解除对参考仓库的 alternates 依赖
	Config                 *config.Config
	CommonManifestOptions
}

// SyncCmd 返回sync命令
func SyncCmd() *cobra.Command {
	opts := &SyncOptions{
		Jobs:         runtime.NumCPU() * 2,
		RetryFetches: 3,
	}

	cmd := &cobra.Command{
		Use:   "sync",
		Short: "Update working tree to the latest revision",
		Long:  `Synchronize the local repository with the remote repositories.`,
		RunE: func(cmd *cobra.Command, args []string) error {
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

			// 如果设置了日志文件，配置日志输出
			logFile := os.Getenv("REPO_LOG_FILE")
			if logFile != "" {
				if err := log.SetDebugFile(logFile); err != nil {
					fmt.Printf("warning: failed to set log file %s: %v\n", logFile, err)
				}
			}

			return runSync(opts, args, log, cmd)
		},
	}

	// 添加命令行选项
	registerSyncFlags(cmd, opts, true)

	return cmd
}

// registerSyncFlags 注册 sync 系列命令的完整命令行选项。
// sync 与 smartsync 共用同一份注册逻辑，避免两处手工维护的 flag 列表
// 漂移（历史上 smartsync 曾缺失 --force-overwrite 导致 unknown flag）。
// includeSmartSyncFlag 控制 --smart-sync 本身是否注册：
// smartsync 命令自动启用 SmartSync，无需该 flag。
func registerSyncFlags(cmd *cobra.Command, opts *SyncOptions, includeSmartSyncFlag bool) {
	cmd.Flags().IntVarP(&opts.Jobs, "jobs", "j", opts.Jobs, "number of parallel jobs (default: based on number of CPU cores)")
	cmd.Flags().IntVar(&opts.JobsNetwork, "jobs-network", opts.Jobs, "number of network jobs to run in parallel")
	cmd.Flags().IntVar(&opts.JobsCheckout, "jobs-checkout", opts.Jobs, "number of local checkout jobs to run in parallel")
	cmd.Flags().BoolVarP(&opts.CurrentBranch, "current-branch", "c", false, "fetch only current branch")
	cmd.Flags().BoolVar(&opts.NoCurrentBranch, "no-current-branch", false, "fetch all branches from server")
	cmd.Flags().BoolVarP(&opts.Detach, "detach", "d", false, "detach projects back to manifest revision")
	cmd.Flags().BoolVar(&opts.ForceSync, "force-sync", false, "overwrite local changes (no short option)")
	cmd.Flags().BoolVar(&opts.ForceRemoveDirty, "force-remove-dirty", false, "force remove projects with uncommitted modifications")
	cmd.Flags().BoolVar(&opts.ForceOverwrite, "force-overwrite", false, "overwrite local changes unconditionally (stale working trees)")
	cmd.Flags().BoolVarP(&opts.ForceBroken, "force-broken", "f", false, "continue syncing other projects if a project sync fails")
	cmd.Flags().BoolVarP(&opts.LocalOnly, "local-only", "l", false, "only update working tree, don't fetch")
	cmd.Flags().BoolVar(&opts.NoManifestUpdate, "no-manifest-update", false, "use the existing manifest checkout as-is")
	cmd.Flags().BoolVarP(&opts.NetworkOnly, "network-only", "n", false, "fetch only, don't update working tree")
	cmd.Flags().BoolVarP(&opts.Prune, "prune", "p", false, "prune refs that no longer exist on the remote (fetch --prune)")
	cmd.Flags().BoolVar(&opts.NoPrune, "no-prune", false, "do not prune refs that no longer exist on the remote")
	cmd.Flags().BoolVarP(&opts.Quiet, "quiet", "q", false, "only show errors")
	cmd.Flags().BoolVarP(&opts.Verbose, "verbose", "v", false, "show all output including debug logs")
	if includeSmartSyncFlag {
		cmd.Flags().BoolVarP(&opts.SmartSync, "smart-sync", "s", false, "smart sync using manifest from the latest known good build")
	}
	cmd.Flags().BoolVarP(&opts.Tags, "tags", "t", false, "fetch tags")
	cmd.Flags().BoolVar(&opts.NoCloneBundle, "no-clone-bundle", false, "disable use of /clone.bundle on HTTP/HTTPS")
	cmd.Flags().BoolVar(&opts.FetchSubmodules, "fetch-submodules", false, "fetch submodules")
	cmd.Flags().BoolVar(&opts.NoTags, "no-tags", false, "don't fetch tags")
	cmd.Flags().BoolVar(&opts.OptimizedFetch, "optimized-fetch", false, "only fetch projects fixed to sha1 if revision does not exist locally")
	cmd.Flags().IntVar(&opts.RetryFetches, "retry-fetches", opts.RetryFetches, "number of times to retry fetches")
	cmd.Flags().StringVarP(&opts.Groups, "groups", "g", "", "restrict to projects matching the specified groups")
	cmd.Flags().BoolVar(&opts.FailFast, "fail-fast", false, "stop syncing after first error is hit")
	cmd.Flags().BoolVar(&opts.UseSuperproject, "use-superproject", false, "use the manifest superproject to sync projects")
	cmd.Flags().BoolVar(&opts.NoUseSuperproject, "no-use-superproject", false, "disable use of manifest superprojects")
	cmd.Flags().BoolVar(&opts.HyperSync, "hyper-sync", false, "only update projects changed on git server (repo-go extension)")
	cmd.Flags().StringVar(&opts.SmartTag, "smart-tag", "", "smart sync using manifest from a known tag")
	cmd.Flags().BoolVar(&opts.OuterManifest, "outer-manifest", false, "operate starting at the outermost manifest")
	cmd.Flags().BoolVar(&opts.NoOuterManifest, "no-outer-manifest", false, "do not operate on outer manifests")
	cmd.Flags().BoolVar(&opts.ThisManifestOnly, "this-manifest-only", false, "only operate on this (sub)manifest")
	cmd.Flags().BoolVar(&opts.NoThisManifestOnly, "all-manifests", false, "operate on this manifest and its submanifests")
	cmd.Flags().StringVarP(&opts.ManifestServerUsername, "manifest-server-username", "u", "", "username to authenticate with the manifest server")
	cmd.Flags().StringVarP(&opts.ManifestServerPassword, "manifest-server-password", "w", "", "password to authenticate with the manifest server")
	cmd.Flags().StringVar(&opts.ManifestServerURL, "manifest-server-url", "", "manifest server URL")
	cmd.Flags().BoolVar(&opts.NoManifestServer, "no-manifest-server", false, "do not use the manifest server")
	cmd.Flags().BoolVar(&opts.AutoGC, "auto-gc", false, "run git gc --auto after syncing")
	cmd.Flags().BoolVar(&opts.NoAutoGC, "no-auto-gc", false, "do not run git gc --auto after syncing")
	cmd.Flags().BoolVar(&opts.GitLFS, "git-lfs", false, "启用 Git LFS 支持 (repo-go extension)")
	cmd.Flags().StringVar(&opts.DefaultRemote, "default-remote", "", "设置默认远程仓库名称，用于解决分支匹配多个远程的问题 (repo-go extension)")
	cmd.Flags().StringVar(&opts.Reference, "reference", "", "location of mirror directory (a single repo or a directory of per-project reference repos)")
	cmd.Flags().BoolVar(&opts.Dissociate, "dissociate", false, "dissociate from reference mirrors after clone")
}

// runSync 执行sync命令
func runSync(opts *SyncOptions, args []string, log logger.Logger, cmd *cobra.Command) error {
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

	// 检查manifest.xml 文件是否存在
	manifestPath := filepath.Join(cfg.RepoRoot, ".repo", "manifest.xml")
	if _, err := os.Stat(manifestPath); os.IsNotExist(err) {
		log.Error("manifest.xml文件不存在，请先运行 'repo init' 命令")
		return fmt.Errorf("manifest.xml文件不存在，请先运行 'repo init' 命令")
	}

	// 如果命令行没有指定groups 参数，则从配置文件中读取
	if opts.Groups == "" && cfg.Groups != "" {
		log.Debug("从配置文件中读取组信息 %s", cfg.Groups)
		opts.Groups = cfg.Groups
		log.Debug("使用配置文件中的组信息 %s", cfg.Groups)
	}

	// 首先更新 manifest 仓库（--local-only 跳过所有网络操作，含 manifest 仓库更新）
	if !opts.NoManifestUpdate && !opts.LocalOnly {
		log.Info("正在更新 manifest 仓库...")
		// 创建临时引擎用于更新 manifest 仓库
		tempEngine := repo_sync.NewEngine(&repo_sync.Options{
			NoManifestUpdate: opts.NoManifestUpdate,
			Config:           cfg,
			Quiet:            opts.Quiet,
			Verbose:          opts.Verbose,
		}, nil, log)

		if err := tempEngine.UpdateManifestRepo(); err != nil {
			log.Warn("更新 manifest 仓库failed: %v", err)
			// 不返回错误，继续执行同步
		} else {
			log.Info("manifest 仓库更新完成")
		}
	}

	// 加载合并后的清单文件(.repo/manifest.xml)，不使用原始仓库列表
	log.Debug("正在加载合并后的清单文件: %s", manifestPath)
	parser := manifest.NewParser()
	var groupsSlice []string
	if opts.Groups != "" {
		// SplitGroups 已按上游语义处理逗号/空白分隔与去空去重
		groupsSlice = manifest.SplitGroups(opts.Groups)
		log.Info("根据以下组过滤清单: %v", groupsSlice)
	} else {
		log.Info("未指定组过滤，将加载所有项目")
	}

	// 解析合并后的清单文件，根据组过滤项目
	manifestObj, err := parser.ParseFromFile(manifestPath, groupsSlice)
	if err != nil {
		log.Error("failed to parse manifest: %v", err)
		return fmt.Errorf("failed to parse manifest: %w", err)
	}
	log.Debug("成功加载清单，包含 %d 个项目", len(manifestObj.Projects))

	// 并发数优先级：命令行 -j > manifest default sync-j > 配置文件 > 内置默认值
	// 仅当用户未显式指定 -j 时应用 manifest/config 默认值（用 Changed 判定，避免哨兵值比较的脆弱性）。
	if !cmd.Flags().Changed("jobs") {
		if manifestObj.Default.SyncJ > 0 {
			log.Info("使用manifest default sync-j: %d", manifestObj.Default.SyncJ)
			opts.Jobs = manifestObj.Default.SyncJ
		} else if cfg.Jobs > 0 {
			log.Info("使用配置文件中的并发数: %d", cfg.Jobs)
			opts.Jobs = cfg.Jobs
		}
	}
	// 未显式指定 --jobs-network/--jobs-checkout 时，跟随 -j
	if !cmd.Flags().Changed("jobs-network") {
		opts.JobsNetwork = opts.Jobs
	}
	if !cmd.Flags().Changed("jobs-checkout") {
		opts.JobsCheckout = opts.Jobs
	}

	// manifest default sync-c：仅当用户未显式指定 -c/--no-current-branch 时生效
	if manifestObj.Default.SyncC && !cmd.Flags().Changed("current-branch") && !cmd.Flags().Changed("no-current-branch") {
		log.Debug("使用manifest default sync-c: true")
		opts.CurrentBranch = true
	}
	// sync-tags 不在此处灌入全局选项：上游语义是 CLI --tags/--no-tags 三态
	// 覆盖，未指定时按项目级 sync-tags（继承 default，缺省 true）逐项目判定，
	// 由引擎 fetchProject 消费

	// 创建项目管理器
	log.Debug("正在初始化项目管理器...")
	manager := project.NewManagerFromManifest(manifestObj, opts.Config)

	var projects []*project.Project
	if len(args) == 0 {
		// 清单已在 ParseFromFile 阶段按组过滤（对齐上游：组过滤在清单加载时一次完成），
		// manager 直接取全部已过滤项目，不再二次过滤，避免与清单过滤语义漂移导致丢项。
		log.Debug("获取所有项目（清单解析阶段已按组过滤）")
		projects = manager.GetProjects()
	} else {
		// 否则，只处理指定的项目
		log.Debug("根据名称failed to get project: %v", args)
		projects, err = manager.GetProjectsByNames(args)
		if err != nil {
			log.Error("根据名称failed to get projectfailed: %v", err)
			return fmt.Errorf("根据名称failed to get projectfailed: %w", err)
		}
		log.Debug("共获取到 %d 个项目", len(projects))
	}

	log.Info("找到 %d 个匹配项目", len(projects))

	// 如果过滤后没有项目，提前返回错误
	if len(projects) == 0 {
		if len(groupsSlice) > 0 {
			log.Warn("在指定组 %v 中未找到匹配的项目，请检查组名是否正确", groupsSlice)
			return fmt.Errorf("在指定组 %v 中未找到匹配的项目", groupsSlice)
		}
		log.Warn("清单中未找到任何项目，请检查清单文件")
		return fmt.Errorf("清单中未找到任何项目")
	}

	// 创建同步引擎
	log.Debug("创建同步引擎...")
	// 使用已经处理好的 groupsSlice，避免重复处理
	if len(groupsSlice) > 0 {
		log.Info("使用以下组过滤项目 %v", groupsSlice)
	} else {
		log.Info("未指定组过滤，将同步所有项目")
	}

	engine := repo_sync.NewEngine(&repo_sync.Options{
		Jobs:                   opts.Jobs,
		JobsNetwork:            opts.JobsNetwork,
		JobsCheckout:           opts.JobsCheckout,
		CurrentBranch:          opts.CurrentBranch && !opts.NoCurrentBranch,
		Detach:                 opts.Detach,
		ForceSync:              opts.ForceSync,
		ForceRemoveDirty:       opts.ForceRemoveDirty,
		ForceOverwrite:         opts.ForceOverwrite,
		ForceBroken:            opts.ForceBroken,
		LocalOnly:              opts.LocalOnly,
		NetworkOnly:            opts.NetworkOnly,
		Prune:                  opts.Prune && !opts.NoPrune,
		Quiet:                  opts.Quiet,
		Verbose:                opts.Verbose,
		SmartSync:              opts.SmartSync,
		Tags:                   opts.Tags && !opts.NoTags,
		NoTags:                 opts.NoTags,
		FetchSubmodules:        opts.FetchSubmodules || cfg.Submodules, // 命令行参数或配置文件
		OptimizedFetch:         opts.OptimizedFetch,
		RetryFetches:           opts.RetryFetches,
		Groups:                 groupsSlice, // 传递已处理的分组信息，确保只克隆指定组的仓
		FailFast:               opts.FailFast,
		NoManifestUpdate:       opts.NoManifestUpdate,
		ManifestServerUsername: opts.ManifestServerUsername,
		ManifestServerPassword: opts.ManifestServerPassword,
		ManifestServerURL:      opts.ManifestServerURL,
		NoManifestServer:       opts.NoManifestServer,
		UseSuperproject:        opts.UseSuperproject && !opts.NoUseSuperproject,
		HyperSync:              opts.HyperSync,
		SmartTag:               opts.SmartTag,
		AutoGC:                 opts.AutoGC && !opts.NoAutoGC,
		GitLFS:                 opts.GitLFS,        // 添加Git LFS支持选项
		DefaultRemote:          opts.DefaultRemote, // 添加默认远程仓库选项
		Reference:              opts.Reference,     // 本地参考仓库路径（仓库或目录）
		Dissociate:             opts.Dissociate,    // 克隆后解除参考仓库依赖
		Config:                 opts.Config,        // 添加Config字段，传递配置信
	}, manifestObj, log)

	// 设置要同步的项目
	engine.SetProjects(projects)

	// 执行同步
	log.Info("开始同步项目，并行任务 %d...", opts.Jobs)
	err = engine.Sync()

	// 处理同步结果
	if err != nil {
		log.Error("同步操作failed: %v", err)
		return err
	}

	log.Info("同步操作成功完成，共同步 %d 个项目", len(projects))

	// 对齐上游 repo sync：完成后打印清单 notice（含 include 清单合并的 notice）
	if manifestObj.Notice != "" {
		log.Info("\n%s", manifestObj.Notice)
	}
	return nil
}
