package commands

import (
	"github.com/leopardxu/repo-go/internal/logger"
	"github.com/spf13/cobra"
)

// smartSyncDefaultJobs 与上游 smartsync 的默认并行数保持一致：8。
const smartSyncDefaultJobs = 8

// SmartSyncCmd 返回 smartsync 命令。
//
// 对齐上游 repo 的 Smartsync 设计：`smartsync` 等价于 `sync -s`，
// 选项集为 sync._Options(show_smart=False)，即不暴露 -s/--smart-sync、
// --no-smart-sync/--head 与 -t/--smart-tag（smart-sync 由命令自身开启）。
// 所有选项均为可选，`repo smartsync` 不带任何参数即可执行；清单未声明
// manifest-server 时告警并退化为常规同步（SmartSyncOptional）。
func SmartSyncCmd() *cobra.Command {
	opts := &SyncOptions{
		SmartSyncOptional: true,
		PruneDefaultOn:    true,
	}

	cmd := &cobra.Command{
		Use:   "smartsync [<project>...]",
		Short: "Update working tree to the latest known good revision",
		Long:  `The 'repo smartsync' command is a shortcut for sync -s.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			log := logger.NewDefaultLogger()

			// 根据选项设置日志级别
			if opts.Quiet {
				log.SetLevel(logger.LogLevelError)
			} else if opts.Verbose {
				log.SetLevel(logger.LogLevelDebug)
			} else {
				log.SetLevel(logger.LogLevelInfo)
			}

			// 自动启用 SmartSync 模式（这是 smartsync 命令的核心特性）
			opts.SmartSync = true

			// 帮助文本中的默认值由上游文案给出；这里在未显式指定 -j 时
			// 填入 8，随后 runSync 仍允许 manifest sync-j / 配置文件覆盖。
			if !cmd.Flags().Changed("jobs") {
				opts.Jobs = smartSyncDefaultJobs
			}

			// 调用 sync 命令的执行逻辑
			return runSync(opts, args, log, cmd)
		},
	}

	registerSmartSyncFlags(cmd, opts)
	return cmd
}

// registerSmartSyncFlags 注册与上游 smartsync 一致的命令行选项。
//
// 选项顺序、短选项与帮助文本对齐上游 `repo smartsync --help`：
// - -p 是 manifest-server-password（不是 prune），--prune 没有短选项；
// - -t 在此命令中不占用（--tags 仅长选项）；
// - --nmu 是 --no-manifest-update 的别名；
// - --load-aiconfig 与 --hyper-sync 为 CIX 扩展。
func registerSmartSyncFlags(cmd *cobra.Command, opts *SyncOptions) {
	cmd.Flags().IntVarP(&opts.Jobs, "jobs", "j", 0, "number of jobs to run in parallel (default: 8; based on number of CPU cores)")
	cmd.Flags().IntVar(&opts.JobsNetwork, "jobs-network", 0, "number of network jobs to run in parallel (defaults to --jobs)")
	cmd.Flags().IntVar(&opts.JobsCheckout, "jobs-checkout", 0, "number of local checkout jobs to run in parallel (defaults to --jobs)")
	cmd.Flags().BoolVarP(&opts.ForceBroken, "force-broken", "f", false, "obsolete option (to be deleted in the future)")
	cmd.Flags().BoolVar(&opts.FailFast, "fail-fast", false, "stop syncing after first error is hit")
	cmd.Flags().BoolVar(&opts.ForceSync, "force-sync", false, "overwrite an existing git directory if it needs to point to a different object directory. WARNING: this may cause loss of data")
	cmd.Flags().BoolVar(&opts.ForceRemoveDirty, "force-remove-dirty", false, "force remove projects with uncommitted modifications if projects no longer exist in the manifest. WARNING: this may cause loss of data")
	cmd.Flags().BoolVar(&opts.ForceOverwrite, "force-overwrite", false, "DANGOUR: DO NOT USE UNLESS YOU KNOW WHAT YOUR ARE DOING,force cleanup local uncomitted changes")
	cmd.Flags().BoolVarP(&opts.LocalOnly, "local-only", "l", false, "only update working tree, don't fetch")
	cmd.Flags().BoolVar(&opts.NoManifestUpdate, "no-manifest-update", false, "use the existing manifest checkout as-is. (do not update to the latest revision)")
	cmd.Flags().BoolVar(&opts.NoManifestUpdate, "nmu", false, "use the existing manifest checkout as-is. (do not update to the latest revision)")
	cmd.Flags().BoolVarP(&opts.NetworkOnly, "network-only", "n", false, "fetch only, don't update working tree")
	cmd.Flags().BoolVarP(&opts.Detach, "detach", "d", false, "detach projects back to manifest revision")
	cmd.Flags().BoolVarP(&opts.CurrentBranch, "current-branch", "c", false, "fetch only current branch from server")
	cmd.Flags().BoolVar(&opts.NoCurrentBranch, "no-current-branch", false, "fetch all branches from server")
	cmd.Flags().StringVarP(&opts.ManifestName, "manifest-name", "m", "", "temporary manifest to use for this sync")
	cmd.Flags().BoolVar(&opts.CloneBundle, "clone-bundle", false, "enable use of /clone.bundle on HTTP/HTTPS")
	cmd.Flags().BoolVar(&opts.NoCloneBundle, "no-clone-bundle", false, "disable use of /clone.bundle on HTTP/HTTPS")
	cmd.Flags().StringVarP(&opts.ManifestServerUsername, "manifest-server-username", "u", "", "username to authenticate with the manifest server")
	cmd.Flags().StringVarP(&opts.ManifestServerPassword, "manifest-server-password", "p", "", "password to authenticate with the manifest server")
	cmd.Flags().BoolVar(&opts.FetchSubmodules, "fetch-submodules", false, "fetch submodules from server")
	cmd.Flags().BoolVar(&opts.UseSuperproject, "use-superproject", false, "use the manifest superproject to sync projects; implies -c")
	cmd.Flags().BoolVar(&opts.NoUseSuperproject, "no-use-superproject", false, "disable use of manifest superprojects")
	cmd.Flags().BoolVar(&opts.Tags, "tags", false, "fetch tags")
	cmd.Flags().BoolVar(&opts.NoTags, "no-tags", false, "don't fetch tags (default)")
	cmd.Flags().BoolVar(&opts.OptimizedFetch, "optimized-fetch", false, "only fetch projects fixed to sha1 if revision does not exist locally")
	cmd.Flags().IntVar(&opts.RetryFetches, "retry-fetches", 0, "number of times to retry fetches on transient errors")
	cmd.Flags().BoolVar(&opts.Prune, "prune", false, "delete refs that no longer exist on the remote (default)")
	cmd.Flags().BoolVar(&opts.NoPrune, "no-prune", false, "do not delete refs that no longer exist on the remote")
	cmd.Flags().BoolVar(&opts.HyperSync, "hyper-sync", false, "only update projects changed on git server by checking against CIX Manifest Service")
	cmd.Flags().BoolVar(&opts.LoadAIConfig, "load-aiconfig", false, "load cix ai config")

	// Logging options
	cmd.Flags().BoolVarP(&opts.Verbose, "verbose", "v", false, "show all output")
	cmd.Flags().BoolVarP(&opts.Quiet, "quiet", "q", false, "only show errors")

	// Multi-manifest options
	cmd.Flags().BoolVar(&opts.OuterManifest, "outer-manifest", false, "operate starting at the outermost manifest")
	cmd.Flags().BoolVar(&opts.NoOuterManifest, "no-outer-manifest", false, "do not operate on outer manifests")
	cmd.Flags().BoolVar(&opts.ThisManifestOnly, "this-manifest-only", false, "only operate on this (sub)manifest")
	cmd.Flags().BoolVar(&opts.NoThisManifestOnly, "no-this-manifest-only", false, "operate on this manifest and its submanifests")
	cmd.Flags().BoolVar(&opts.NoThisManifestOnly, "all-manifests", false, "operate on this manifest and its submanifests")

	// repo Version options
	cmd.Flags().BoolVar(&opts.NoRepoVerify, "no-repo-verify", false, "do not verify repo source code")
}
