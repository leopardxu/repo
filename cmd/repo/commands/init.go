package commands

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/leopardxu/repo-go/internal/git"
	"github.com/leopardxu/repo-go/internal/hook"
	"github.com/leopardxu/repo-go/internal/logger"
	"github.com/leopardxu/repo-go/internal/manifest"
	"github.com/spf13/cobra"
)

// RepoConfig 表示repo配置
type RepoConfig struct {
	ManifestURL            string `json:"manifest_url"`
	ManifestBranch         string `json:"manifest_branch"`
	ManifestUpstreamBranch string `json:"manifest_upstream_branch"`
	ManifestName           string `json:"manifest_name"`
	Groups                 string `json:"groups"`
	Platform               string `json:"platform"`
	Mirror                 bool   `json:"mirror"`
	Archive                bool   `json:"archive"`
	Worktree               bool   `json:"worktree"`
	UseLocalGitdirs        bool   `json:"use_local_gitdirs"`
	Reference              string `json:"reference"`
	NoSmartCache           bool   `json:"no_smart_cache"`
	Dissociate             bool   `json:"dissociate"`
	Depth                  int    `json:"depth"`
	ManifestDepth          int    `json:"manifest_depth"`
	PartialClone           bool   `json:"partial_clone"`
	PartialCloneExclude    string `json:"partial_clone_exclude"`
	CloneFilter            string `json:"clone_filter"`
	UseSuperproject        bool   `json:"use_superproject"`
	CloneBundle            bool   `json:"clone_bundle"`
	GitLFS                 bool   `json:"git_lfs"`
	RepoURL                string `json:"repo_url"`
	RepoRev                string `json:"repo_rev"`
	NoRepoVerify           bool   `json:"no_repo_verify"`
	StandaloneManifest     bool   `json:"standalone_manifest"`
	Submodules             bool   `json:"submodules"`
	CurrentBranch          bool   `json:"current_branch"`
	Tags                   bool   `json:"tags"`
}

// InitOptions 包含init命令的选项
type InitOptions struct {
	CommonManifestOptions
	Verbose                bool
	Quiet                  bool
	Debug                  bool
	ManifestURL            string
	ManifestBranch         string
	ManifestName           string
	Groups                 string
	Platform               string
	ManifestUpstreamBranch string // --manifest-upstream-branch：提交版本时定位 commit 的 git ref
	ManifestDepth          int    // --manifest-depth：清单仓库自身浅克隆深度（0=全量）
	Submodules             bool
	StandaloneManifest     bool
	CurrentBranch          bool
	NoCurrentBranch        bool
	Tags                   bool
	NoTags                 bool
	Mirror                 bool
	Archive                bool
	Worktree               bool
	UseLocalGitdirs        bool // --use-local-gitdirs：跳过 .repo/projects/ 使用标准 Git 布局
	Reference              string
	NoSmartCache           bool
	Dissociate             bool
	Depth                  int
	PartialClone           bool
	NoPartialClone         bool
	PartialCloneExclude    string
	CloneFilter            string
	UseSuperproject        bool
	NoUseSuperproject      bool
	CloneBundle            bool
	NoCloneBundle          bool
	GitLFS                 bool
	NoGitLFS               bool
	RepoURL                string
	RepoRev                string
	RepoBranch             string // --repo-branch：--repo-rev 的隐藏别名（对齐上游 SUPPRESS_HELP）
	NoRepoVerify           bool
	ConfigName             bool
	Force                  bool // --force：跳过"覆盖已有初始化配置"的人工确认

	// 内部标记：是否对清单做了合并（影响 manifest.xml 软链决策）。
	// init 不再按组预过滤，故 Filtered 恒为 false，仅 Merged 控制是否写盘合并清单。
	Merged   bool
	Filtered bool // 已废弃：init 不再预过滤，恒为 false；保留以兼容字段
}

// InitCmd 返回init命令
func InitCmd() *cobra.Command {
	opts := &InitOptions{}

	cmd := &cobra.Command{
		Use:   "init [options] [manifest url]",
		Short: "Initialize a repo client checkout in the current directory",
		Long:  `Initialize a repository client checkout in the current directory.`,
		// 对齐上游 ValidateOptions：init 最多接受 1 个位置参数（manifest url）
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// --repo-branch 是 --repo-rev 的隐藏别名（上游 dest=repo_rev, SUPPRESS_HELP）
			if cmd.Flags().Changed("repo-branch") {
				opts.RepoRev = opts.RepoBranch
			}

			// 位置参数 URL 与 --manifest-url 互斥（对齐上游 ValidateOptions）
			if len(args) > 0 {
				if cmd.Flags().Changed("manifest-url") {
					return fmt.Errorf("--manifest-url option and URL argument both specified: only use one to select the manifest URL")
				}
				opts.ManifestURL = args[0]
			}

			// 上游 _RegisteredEnvironmentOptions：对应 flag 未显式设置时回退到环境变量
			if opts.ManifestURL == "" {
				if v := os.Getenv("REPO_MANIFEST_URL"); v != "" {
					opts.ManifestURL = v
				}
			}
			if opts.Reference == "" {
				if v := os.Getenv("REPO_MIRROR_LOCATION"); v != "" {
					opts.Reference = v
				}
			}
			if !cmd.Flags().Changed("git-lfs") {
				if v := os.Getenv("REPO_GIT_LFS"); v != "" {
					if b, err := strconv.ParseBool(v); err == nil {
						opts.GitLFS = b
					} else {
						opts.GitLFS = true
					}
				}
			}

			// 对齐上游：--partial-clone 时默认关闭 clone-bundle（除非显式 --clone-bundle）
			if opts.PartialClone && !cmd.Flags().Changed("clone-bundle") {
				opts.CloneBundle = false
			}

			// --reference 展开 ~（对齐上游 os.path.expanduser）
			if opts.Reference != "" {
				if expanded, err := expandUser(opts.Reference); err == nil {
					opts.Reference = expanded
				}
			}

			return runInit(opts)
		},
	}

	// 日志选项
	cmd.Flags().BoolVarP(&opts.Verbose, "verbose", "v", false, "show all output")
	cmd.Flags().BoolVarP(&opts.Quiet, "quiet", "q", false, "only show errors")
	cmd.Flags().BoolVar(&opts.Debug, "debug", false, "show debug output")

	// 清单选项
	cmd.Flags().StringVarP(&opts.ManifestURL, "manifest-url", "u", "", "manifest repository location")
	cmd.Flags().StringVarP(&opts.ManifestBranch, "manifest-branch", "b", "", "manifest branch or revision (use HEAD for default)")
	cmd.Flags().StringVar(&opts.ManifestUpstreamBranch, "manifest-upstream-branch", "", "when a commit is provided to --manifest-branch, this is the name of the git ref in which the commit can be found")
	cmd.Flags().StringVarP(&opts.ManifestName, "manifest-name", "m", "default.xml", "initial manifest file")
	cmd.Flags().StringVarP(&opts.Groups, "groups", "g", "default", "restrict manifest projects to ones with specified group(s) [default|all|G1,G2,G3|G4,-G5,-G6]")
	cmd.Flags().StringVarP(&opts.Platform, "platform", "p", "auto", "restrict manifest projects to ones with a specified platform group [auto|all|none|linux|darwin|...]")
	cmd.Flags().BoolVar(&opts.Submodules, "submodules", false, "sync any submodules associated with the manifest repo")
	cmd.Flags().BoolVar(&opts.StandaloneManifest, "standalone-manifest", false, "download the manifest as a static file rather then create a git checkout of the manifest repo")
	cmd.Flags().IntVar(&opts.ManifestDepth, "manifest-depth", 0, "create a shallow clone of the manifest repo with given depth (0 for full clone); see git clone")

	// 清单检出选项（仅影响清单仓库，不影响清单内项目）
	cmd.Flags().BoolVarP(&opts.CurrentBranch, "current-branch", "c", true, "fetch only current manifest branch from server (default)")
	cmd.Flags().BoolVar(&opts.NoCurrentBranch, "no-current-branch", false, "fetch all manifest branches from server")
	cmd.Flags().BoolVar(&opts.Tags, "tags", false, "fetch tags in the manifest")
	cmd.Flags().BoolVar(&opts.NoTags, "no-tags", false, "don't fetch tags in the manifest")

	// 检出模式
	cmd.Flags().BoolVar(&opts.Mirror, "mirror", false, "create a replica of the remote repositories rather than a client working directory")
	cmd.Flags().BoolVar(&opts.Archive, "archive", false, "checkout an archive instead of a git repository for each project. See git archive.")
	cmd.Flags().BoolVar(&opts.Worktree, "worktree", false, "use git-worktree to manage projects")
	cmd.Flags().BoolVar(&opts.UseLocalGitdirs, "use-local-gitdirs", false, "bypass .repo/projects/ and use standard Git layout in working tree")

	// 项目检出优化
	cmd.Flags().StringVar(&opts.Reference, "reference", "", "location of mirror directory")
	cmd.Flags().BoolVar(&opts.NoSmartCache, "no-smart-cache", false, "disable CIX smart cache feature")
	cmd.Flags().BoolVar(&opts.Dissociate, "dissociate", false, "dissociate from reference mirrors after clone")
	cmd.Flags().IntVar(&opts.Depth, "depth", 0, "create a shallow clone with given depth; use 0 for a full clone")
	cmd.Flags().BoolVar(&opts.PartialClone, "partial-clone", false, "perform partial clone (https://git-scm.com/docs/partial-clone)")
	cmd.Flags().BoolVar(&opts.NoPartialClone, "no-partial-clone", false, "disable use of partial clone (https://git-scm.com/docs/partial-clone)")
	cmd.Flags().StringVar(&opts.PartialCloneExclude, "partial-clone-exclude", "", "exclude the specified projects (a comma-delimited project names) from partial clone (https://git-scm.com/docs/partial-clone)")
	cmd.Flags().StringVar(&opts.CloneFilter, "clone-filter", "blob:none", "filter for use with --partial-clone [default: blob:none]")
	cmd.Flags().BoolVar(&opts.UseSuperproject, "use-superproject", false, "use the manifest superproject to sync projects; implies -c")
	cmd.Flags().BoolVar(&opts.NoUseSuperproject, "no-use-superproject", false, "disable use of manifest superprojects")
	cmd.Flags().BoolVar(&opts.CloneBundle, "clone-bundle", true, "enable use of /clone.bundle on HTTP/HTTPS (default if not --partial-clone)")
	cmd.Flags().BoolVar(&opts.NoCloneBundle, "no-clone-bundle", false, "disable use of /clone.bundle on HTTP/HTTPS (default if --partial-clone)")
	cmd.Flags().BoolVar(&opts.GitLFS, "git-lfs", false, "enable Git LFS support")
	cmd.Flags().BoolVar(&opts.NoGitLFS, "no-git-lfs", false, "disable Git LFS support")

	// repo 版本选项
	cmd.Flags().StringVar(&opts.RepoURL, "repo-url", "", "repo repository location")
	cmd.Flags().StringVar(&opts.RepoRev, "repo-rev", "", "repo branch or revision")
	// --repo-branch 是 --repo-rev 的隐藏别名（对齐上游 dest=repo_rev, SUPPRESS_HELP）
	cmd.Flags().StringVar(&opts.RepoBranch, "repo-branch", "", "")
	if f := cmd.Flags().Lookup("repo-branch"); f != nil {
		f.Hidden = true
	}
	cmd.Flags().BoolVar(&opts.NoRepoVerify, "no-repo-verify", false, "do not verify repo source code")

	// 其他选项
	cmd.Flags().BoolVar(&opts.ConfigName, "config-name", false, "Always prompt for name/e-mail")
	cmd.Flags().BoolVar(&opts.Force, "force", false, "force overwrite of an existing initialized client without prompting")

	// 多清单选项
	AddManifestFlags(cmd, &opts.CommonManifestOptions)

	return cmd
}

// saveRepoConfig 保存repo配置
func saveRepoConfig(cfg *RepoConfig) error {
	// 确保.repo目录存在
	if err := os.MkdirAll(".repo", 0755); err != nil {
		return fmt.Errorf("failed to create .repo directory: %w", err)
	}

	// 序列化配置
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to serialize config: %w", err)
	}

	// 写入配置文件
	configPath := filepath.Join(".repo", "config.json")
	if err := os.WriteFile(configPath, data, 0644); err != nil {
		return fmt.Errorf("failed to write config file: %w", err)
	}

	return nil
}

// loadGitConfig 已删除：原函数为死代码（无调用方）且含 err 重赋值 bug。
// Git 用户配置检查由 promptForUserInfo / init 的 --config-name 路径处理。

// promptForUserInfo 提示用户输入信息
func promptForUserInfo(log logger.Logger) error {
	gitRunner := git.NewRunner()

	// 检查用户名
	output, err := gitRunner.Run("config", "--get", "user.name")
	if err != nil {
		// 读取失败按未设置处理，转空输出以触发用户名提示
		log.Debug("读取 git user.name failed（按未设置处理）: %v", err)
		output = nil
	}
	if strings.TrimSpace(string(output)) == "" { // 添加string()转换
		fmt.Print("Enter your name: ")
		var name string
		if _, err := fmt.Scanln(&name); err != nil {
			// 输入失败（如管道无输入/EOF）按未提供处理，跳过设置
			log.Debug("读取用户名输入failed（按未提供处理）: %v", err)
		}
		if name != "" {
			if _, err := gitRunner.Run("config", "--global", "user.name", name); err != nil {
				return fmt.Errorf("failed to set git user.name: %w", err)
			}
		}
	}

	// 检查邮箱
	output, err = gitRunner.Run("config", "--get", "user.email")
	if err != nil {
		// 读取失败按未设置处理，转空输出以触发邮箱提示
		log.Debug("读取 git user.email failed（按未设置处理）: %v", err)
		output = nil
	}
	if strings.TrimSpace(string(output)) == "" { // 添加string()转换
		fmt.Print("Enter your email: ")
		var email string
		if _, err := fmt.Scanln(&email); err != nil {
			// 输入失败（如管道无输入/EOF）按未提供处理，跳过设置
			log.Debug("读取邮箱输入failed（按未提供处理）: %v", err)
		}
		if email != "" {
			if _, err := gitRunner.Run("config", "--global", "user.email", email); err != nil {
				return fmt.Errorf("failed to set git user.email: %w", err)
			}
		}
	}

	return nil
}

// cloneManifestRepo 克隆或更新清单仓库。
// 使用 git -C <dir> 在指定目录执行命令，避免 os.Chdir 的线程安全问题。
// 超时后通过 context 取消子进程，避免 goroutine 泄漏。
func cloneManifestRepo(gitRunner git.Runner, cfg *RepoConfig) error {
	manifestsDir := filepath.Join(".repo", "manifests")

	// 确保 .repo/manifests 目录存在
	if err := os.MkdirAll(manifestsDir, 0755); err != nil {
		return fmt.Errorf("failed to create manifests directory: %w", err)
	}

	// 检查是否已存在有效的 git 仓库
	gitDirPath := filepath.Join(manifestsDir, ".git")
	if _, err := os.Stat(gitDirPath); err == nil {
		// 已有 git 仓库，执行 fetch + checkout 更新（对齐上游 repo manifestProject.Sync_NetworkHalf）
		return updateExistingManifestRepo(gitRunner, manifestsDir, cfg)
	}

	// 目录存在但不是 git 仓库（可能残留），清空后重新克隆
	if entries, err := os.ReadDir(manifestsDir); err == nil && len(entries) > 0 {
		// 安全检查：确保要删除的目录在 repo 根目录下
		cwd, _ := os.Getwd()
		absManifestsDir, _ := filepath.Abs(manifestsDir)
		relPath, relErr := filepath.Rel(cwd, absManifestsDir)
		if relErr != nil || strings.HasPrefix(relPath, "..") {
			return fmt.Errorf("manifests目录 %s is not under repo root, deletion refused", manifestsDir)
		}
		if err := os.RemoveAll(manifestsDir); err != nil {
			return fmt.Errorf("failed to clean manifests directory: %w", err)
		}
		if err := os.MkdirAll(manifestsDir, 0755); err != nil {
			return fmt.Errorf("failed to recreate manifests directory: %w", err)
		}
	}

	// 构建克隆命令
	args := []string{"clone"}

	if cfg.Mirror {
		args = append(args, "--mirror")
	}
	if cfg.ManifestDepth > 0 && !cfg.Mirror {
		args = append(args, fmt.Sprintf("--depth=%d", cfg.ManifestDepth))
	}
	if cfg.ManifestBranch != "" && !cfg.Mirror {
		args = append(args, "-b", cfg.ManifestBranch)
	}
	if cfg.Reference != "" {
		args = append(args, fmt.Sprintf("--reference=%s", cfg.Reference))
		if cfg.Dissociate {
			args = append(args, "--dissociate")
		}
	}
	if cfg.PartialClone && !cfg.Mirror {
		args = append(args, "--filter="+cfg.CloneFilter)
	}

	args = append(args, cfg.ManifestURL, manifestsDir)

	// 使用带超时的 context 执行克隆，避免 goroutine 泄漏
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	type result struct {
		output []byte
		err    error
	}
	resultChan := make(chan result, 1)

	go func() {
		// 使用 RunWithTimeout 通过 context 控制超时
		output, err := gitRunner.RunWithTimeout(5*time.Minute, args...)
		resultChan <- result{output: output, err: err}
	}()

	select {
	case <-ctx.Done():
		return fmt.Errorf("manifest repository clone timed out, please check network connection or increase timeout")
	case res := <-resultChan:
		if res.err != nil {
			errStr := res.err.Error()
			if strings.Contains(errStr, "Permission denied (publickey)") {
				return fmt.Errorf("SSH authentication failed: please ensure your SSH key is properly configured and added to the git server\nOriginal error: %w", res.err)
			}
			if strings.Contains(errStr, "fatal: repository '") {
				return fmt.Errorf("invalid or inaccessible manifest repository URL %w\nplease check URL correctness and network accessibility", res.err)
			}
			if strings.Contains(errStr, "Could not read from remote repository") {
				return fmt.Errorf("failed to read from remote repository %s\nplease check permissions and network connection", res.err)
			}
			return fmt.Errorf("failed to clone manifest repository: %w", res.err)
		}
	}

	// 如果需要子模块，使用 git -C 在指定目录执行（避免 os.Chdir）
	if cfg.Submodules {
		if _, err := gitRunner.RunInDir(manifestsDir, "submodule", "update", "--init", "--recursive"); err != nil {
			return fmt.Errorf("failed to initialize submodules: %w", err)
		}
	}

	return nil
}

// updateExistingManifestRepo 对已存在的清单仓库执行 fetch + checkout 更新。
// 使用 git -C <dir> 避免线程不安全的 os.Chdir。
// 对齐上游 Sync_LocalHalf 语义：checkout 后 reset --hard 到已获取的远程顶端，
// 否则 re-init 会停留在本地旧提交，manifest.xml 永远是陈旧内容。
func updateExistingManifestRepo(gitRunner git.Runner, manifestsDir string, cfg *RepoConfig) error {
	// fetch 更新
	if _, err := gitRunner.RunInDir(manifestsDir, "fetch", "--all"); err != nil {
		return fmt.Errorf("failed to fetch manifest repository: %w", err)
	}

	// mirror 模式没有 working directory，不需要 checkout
	if cfg.Mirror {
		return nil
	}

	// 未指定分支：无法确定目标 ref，仅完成 fetch（保持当前分支）
	if cfg.ManifestBranch == "" {
		return nil
	}
	branch := cfg.ManifestBranch
	remoteRef := "refs/remotes/origin/" + branch

	// 远程跟踪分支缺失时按显式 refspec 补取：clone -b 产生的单分支 refspec
	// 拿不到其他分支。仍失败说明远端没有该分支，必须报错而非静默沿用旧分支，
	// 否则 config.json 记录的分支与实际检出的清单不一致
	if _, err := gitRunner.RunInDir(manifestsDir, "rev-parse", "--verify", "--quiet", remoteRef); err != nil {
		if _, ferr := gitRunner.RunInDir(
			manifestsDir,
			"fetch", "origin",
			fmt.Sprintf("refs/heads/%s:%s", branch, remoteRef),
		); ferr != nil {
			return fmt.Errorf("manifest branch %q not found in remote %s: %w", branch, cfg.ManifestURL, ferr)
		}
	}

	_, localErr := gitRunner.RunInDir(manifestsDir, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch)
	if localErr != nil {
		// 本地分支不存在：从远程跟踪分支创建
		if _, err := gitRunner.RunInDir(manifestsDir, "checkout", "-b", branch, remoteRef); err != nil {
			return fmt.Errorf("failed to create branch %s from %s: %w", branch, remoteRef, err)
		}
		return nil
	}

	// 本地分支已存在：checkout 后对齐到远程顶端
	if _, err := gitRunner.RunInDir(manifestsDir, "checkout", branch); err != nil {
		return fmt.Errorf("failed to checkout branch %s: %w", branch, err)
	}
	if _, err := gitRunner.RunInDir(manifestsDir, "reset", "--hard", remoteRef); err != nil {
		return fmt.Errorf("failed to reset branch %s to %s: %w", branch, remoteRef, err)
	}
	return nil
}

// validateOptions 验证选项冲突（对齐上游 Init.ValidateOptions）
func validateOptions(opts *InitOptions) error {
	// 检查必填字段：manifest URL（对齐上游，必须在 clone 前给出清晰错误）
	if opts.ManifestURL == "" {
		return fmt.Errorf("manifest URL is required (use -u or positional argument)")
	}

	// 检出模式互斥
	if opts.Mirror && opts.Archive {
		return fmt.Errorf("--mirror and --archive cannot be used together")
	}
	if opts.Mirror && opts.UseSuperproject {
		return fmt.Errorf("--mirror and --use-superproject cannot be used together")
	}
	if opts.Archive && opts.UseSuperproject {
		return fmt.Errorf("--archive and --use-superproject cannot be used together")
	}
	// mirror 模式不兼容 partial-clone / worktree（对齐上游 repo）
	if opts.Mirror && opts.PartialClone {
		return fmt.Errorf("--mirror and --partial-clone cannot be used together")
	}
	if opts.Mirror && opts.Worktree {
		return fmt.Errorf("--mirror and --worktree cannot be used together")
	}

	// standalone-manifest 不能与 manifest-branch 或非默认 manifest-name 同用
	if opts.StandaloneManifest && (opts.ManifestBranch != "" || opts.ManifestName != "default.xml") {
		return fmt.Errorf("--manifest-branch and --manifest-name cannot be used with --standalone-manifest")
	}

	// manifest-upstream-branch 必须配合 manifest-branch
	if opts.ManifestUpstreamBranch != "" && opts.ManifestBranch == "" {
		return fmt.Errorf("--manifest-upstream-branch cannot be used without --manifest-branch")
	}

	// 正反 flag 互斥
	if opts.CurrentBranch && opts.NoCurrentBranch {
		return fmt.Errorf("cannot specify both --current-branch and --no-current-branch")
	}
	if opts.Tags && opts.NoTags {
		return fmt.Errorf("cannot specify both --tags and --no-tags")
	}
	if opts.PartialClone && opts.NoPartialClone {
		return fmt.Errorf("cannot specify both --partial-clone and --no-partial-clone")
	}
	if opts.UseSuperproject && opts.NoUseSuperproject {
		return fmt.Errorf("cannot specify both --use-superproject and --no-use-superproject")
	}
	if opts.CloneBundle && opts.NoCloneBundle {
		return fmt.Errorf("cannot specify both --clone-bundle and --no-clone-bundle")
	}
	if opts.GitLFS && opts.NoGitLFS {
		return fmt.Errorf("cannot specify both --git-lfs and --no-git-lfs")
	}
	if opts.OuterManifest && opts.NoOuterManifest {
		return fmt.Errorf("cannot specify both --outer-manifest and --no-outer-manifest")
	}
	return nil
}

// applyNegationFlags 归一化 --no-* 否定 flag：单独传反 flag 时置零正 flag 的默认值。
// validateOptions 已保证正反不会同时为 true，这里处理"仅传反 flag"的语义。
func applyNegationFlags(opts *InitOptions) {
	if opts.NoCurrentBranch {
		opts.CurrentBranch = false
	}
	if opts.NoTags {
		opts.Tags = false
	}
	if opts.NoPartialClone {
		opts.PartialClone = false
	}
	if opts.NoUseSuperproject {
		opts.UseSuperproject = false
	}
	if opts.NoCloneBundle {
		opts.CloneBundle = false // --clone-bundle 默认 true，必须显式置零
	}
	if opts.NoGitLFS {
		opts.GitLFS = false
	}
	if opts.NoOuterManifest {
		opts.OuterManifest = false
	}
}

// warnUnsupported 对尚未实现或不适用于 repo-go 单二进制的 init 选项集中告警。
// 不中断流程，仅让用户明确这些 flag 当前无效果。
func warnUnsupported(opts *InitOptions, log logger.Logger) {
	if opts.Archive {
		log.Warn("--archive 尚未在 init 实现，将忽略；archive 检出请在 sync 阶段进行（待实现）")
	}
	if opts.Worktree {
		log.Warn("--worktree 尚未实现，将忽略（使用标准工作树检出）")
	}
	if opts.UseLocalGitdirs {
		log.Warn("--use-local-gitdirs 尚未实现，将忽略（仍使用 .repo/projects/ 布局）")
	}
	if opts.StandaloneManifest {
		log.Warn("--standalone-manifest 尚未实现，将克隆标准 manifest 仓库")
	}
	if opts.PartialCloneExclude != "" {
		log.Warn("--partial-clone-exclude 尚未在 init 生效，将在 sync 阶段按项目过滤（待实现）")
	}
	if opts.RepoURL != "" || opts.RepoRev != "" {
		log.Warn("--repo-url/--repo-rev 不适用于单二进制 repo-go（无 launcher 自更新），忽略")
	}
	if opts.NoRepoVerify {
		log.Warn("--no-repo-verify 不适用于单二进制 repo-go，忽略")
	}
	if opts.NoSmartCache {
		log.Warn("--no-smart-cache 为内部特性，当前未生效")
	}
}

// configFieldDiff 标记一个发生变化的 init 关键配置字段
type configFieldDiff struct {
	name     string // 字段名（展示用）
	existing string // 现有配置值
	current  string // 新 init 选项值
}

// diffInitConfig 比较现有 init 配置与新 init 选项的关键字段，返回发生变化的字段列表。
// 仅比较决定"切换到不同清单"的标识性字段（URL/分支/清单名/groups/platform），
// 克隆调优类选项（depth/dissociate 等）不纳入，避免刷新调优参数时反复确认。
func diffInitConfig(existing RepoConfig, opts *InitOptions) []configFieldDiff {
	var diffs []configFieldDiff
	add := func(name, oldVal, newVal string) {
		if oldVal != newVal {
			diffs = append(diffs, configFieldDiff{name: name, existing: oldVal, current: newVal})
		}
	}
	add("manifest-url", existing.ManifestURL, opts.ManifestURL)
	add("manifest-branch", existing.ManifestBranch, opts.ManifestBranch)
	add("manifest-name", existing.ManifestName, opts.ManifestName)
	add("groups", existing.Groups, opts.Groups)
	add("platform", existing.Platform, opts.Platform)
	return diffs
}

// confirmOverwriteInit 在当前目录已存在初始化配置时，若关键配置发生变化，
// 提示用户确认是否覆盖。opts.Force 为 true 时跳过确认。
// 关键配置未变化（视为刷新）时不提示；非交互输入（stdin 关闭/无数据）默认拒绝。
func confirmOverwriteInit(opts *InitOptions, log logger.Logger) error {
	configPath := filepath.Join(".repo", "config.json")
	data, err := os.ReadFile(configPath)
	if err != nil {
		if os.IsNotExist(err) {
			// 当前目录未初始化，无需确认
			return nil
		}
		return fmt.Errorf("读取现有初始化配置failed: %w", err)
	}

	// 已存在配置文件，解析以比较关键字段
	var existing RepoConfig
	if uerr := json.Unmarshal(data, &existing); uerr != nil {
		// 配置文件损坏：无法精确比较，按"已初始化且需确认"处理（existing 为零值，diff 多半非空）
		log.Warn("现有配置文件解析failed（%v），将提示确认覆盖", uerr)
	}

	diffs := diffInitConfig(existing, opts)
	if len(diffs) == 0 {
		// 关键配置未变化，视为刷新，无需确认
		return nil
	}

	if opts.Force {
		log.Warn("检测到初始化配置变化，已通过 --force 跳过确认，将覆盖现有配置")
		return nil
	}

	fmt.Println("检测到当前目录已初始化 repo 客户端，且关键配置发生变化：")
	for _, d := range diffs {
		fmt.Printf("  %s: %q -> %q\n", d.name, d.existing, d.current)
	}
	fmt.Println("继续将覆盖 .repo/config.json 并重新克隆清单仓库。")
	fmt.Print("是否覆盖现有初始化配置？[y/N] ")

	// 复用 fmt 内部的共享 stdin 缓冲读取器，与 promptForUserInfo 保持一致，
	// 避免与后续 --config-name 提示的 stdin 读取产生缓冲竞争。
	var answer string
	_, err = fmt.Scanln(&answer)
	answer = strings.ToLower(strings.TrimSpace(answer))
	if answer != "y" && answer != "yes" {
		if err != nil {
			return fmt.Errorf("检测到初始化配置变化但未获得确认；非交互环境请使用 --force 强制覆盖")
		}
		return fmt.Errorf("用户取消，未覆盖现有初始化配置")
	}
	return nil
}

// runInit 执行init命令
func runInit(opts *InitOptions) error {
	// 创建日志记录
	log := logger.NewDefaultLogger()
	if opts.Debug {
		log.SetLevel(logger.LogLevelDebug)
	} else if opts.Verbose {
		log.SetLevel(logger.LogLevelInfo)
	} else if opts.Quiet {
		log.SetLevel(logger.LogLevelError)
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

	log.Debug("初始化repo 客户端..")

	// 验证选项冲突
	if err := validateOptions(opts); err != nil {
		log.Error("选项验证failed: %v", err)
		return err
	}

	// 归一化 --no-* 否定 flag：单独传反 flag 时置零正 flag 默认值
	applyNegationFlags(opts)

	// 告警尚未实现/不适用的选项（不中断）
	warnUnsupported(opts, log)

	// 已初始化且关键配置发生变化时，提示用户确认覆盖；--force 可跳过
	if err := confirmOverwriteInit(opts, log); err != nil {
		log.Error("%v", err)
		return err
	}

	// 创建配置
	cfg := &RepoConfig{
		ManifestURL:            opts.ManifestURL,
		ManifestBranch:         opts.ManifestBranch,
		ManifestUpstreamBranch: opts.ManifestUpstreamBranch,
		ManifestName:           opts.ManifestName,
		Groups:                 opts.Groups,
		Platform:               opts.Platform,
		Mirror:                 opts.Mirror,
		Archive:                opts.Archive,
		Worktree:               opts.Worktree,
		UseLocalGitdirs:        opts.UseLocalGitdirs,
		Reference:              opts.Reference,
		NoSmartCache:           opts.NoSmartCache,
		Dissociate:             opts.Dissociate,
		Depth:                  opts.Depth,
		ManifestDepth:          opts.ManifestDepth,
		PartialClone:           opts.PartialClone,
		PartialCloneExclude:    opts.PartialCloneExclude,
		CloneFilter:            opts.CloneFilter,
		UseSuperproject:        opts.UseSuperproject,
		CloneBundle:            opts.CloneBundle,
		GitLFS:                 opts.GitLFS,
		RepoURL:                opts.RepoURL,
		RepoRev:                opts.RepoRev,
		NoRepoVerify:           opts.NoRepoVerify,
		StandaloneManifest:     opts.StandaloneManifest,
		Submodules:             opts.Submodules,
		CurrentBranch:          opts.CurrentBranch,
		Tags:                   opts.Tags,
	}

	// 处理配置名称提示
	if opts.ConfigName {
		log.Debug("提示用户输入 Git 用户信息")
		if err := promptForUserInfo(log); err != nil {
			log.Error("提示用户信息failed: %v", err)
			return fmt.Errorf("failed to prompt for user info: %w", err)
		}
	} else {
		// 只检查Git是否安装，不强制要求配置用户信息
		log.Debug("检查 Git 是否已安装")
		gitRunner := git.NewRunner()
		if _, err := gitRunner.Run("--version"); err != nil {
			log.Error("Git 未安装 %v", err)
			return fmt.Errorf("git not found: %w", err)
		}
	}

	// 配置 Git 运行
	gitRunner := git.NewRunner()
	if opts.Debug {
		gitRunner.SetVerbose(true)
	} else {
		gitRunner.SetVerbose(opts.Verbose)
		gitRunner.SetQuiet(opts.Quiet)
	}

	// 设置Git LFS
	if opts.GitLFS {
		log.Info("安装 Git LFS...")
		if _, err := gitRunner.Run("lfs", "install"); err != nil {
			log.Error("failed to install Git LFS: %v", err)
			return fmt.Errorf("failed to install Git LFS: %w", err)
		}
		log.Info("Git LFS 安装成功")
	}

	// 创建.repo目录结构
	log.Info("创建 repo 目录结构...")
	currentDir, err := os.Getwd()
	if err != nil {
		log.Error("获取当前目录failed: %v", err)
		return fmt.Errorf("failed to get current directory: %w", err)
	}

	// 初始化Git配置和hooks
	log.Debug("初始化repo 目录结构和Git hooks")
	if err := initRepoStructure(currentDir); err != nil {
		log.Error("初始化repo 结构failed: %v", err)
		return fmt.Errorf("failed to initialize repo structure: %w", err)
	}
	log.Info("repo 目录结构创建成功")

	// 克隆清单仓库
	log.Info("克隆清单仓库...")
	if err := cloneManifestRepo(gitRunner, cfg); err != nil {
		log.Error("failed to clone manifest repository: %v", err)
		return fmt.Errorf("failed to clone manifest repository: %w", err)
	}
	log.Info("清单仓库克隆成功")

	// 解析清单文件
	log.Info("解析清单文件...")
	parser := manifest.NewParser()
	parser.SetSilentMode(!opts.Verbose && !opts.Debug) // 根据verbose和debug选项控制警告日志输出
	manifestPath := filepath.Join(".repo", "manifests", cfg.ManifestName)
	log.Debug("解析清单文件: %s", manifestPath)
	// 解析时不传组过滤（nil），保留全部项目；组过滤由 sync 按 -g 执行
	manifestObj, err := parser.ParseFromFile(manifestPath, nil)
	if err != nil {
		log.Error("failed to parse manifest file: %v", err)
		return fmt.Errorf("failed to parse manifest: %w", err)
	}
	log.Info("清单文件解析成功，包含 %d 个项目", len(manifestObj.Projects))

	// 不在此按组过滤并固化写盘：上游 repo init 保留完整清单（含所有 include 项目），
	// 组过滤交给 sync 时按 -g / 配置动态执行。预过滤会把非默认组（如 groups="cix"）
	// 的项目在 init 阶段删除，导致后续 sync 始终为空。opts.Groups 仅写入配置供 sync 读取。

	// 处理include标签
	if len(manifestObj.Includes) > 0 && !opts.ThisManifestOnly {
		log.Debug("处理 %d 个包含的清单文件", len(manifestObj.Includes))

		// 创建清单合并
		merger := manifest.NewMerger(parser, filepath.Join(".repo", "manifests"))

		// 加载所有包含的清单
		includedManifests := []*manifest.Manifest{manifestObj}

		for _, include := range manifestObj.Includes {
			includePath := filepath.Join(".repo", "manifests", include.Name)
			log.Debug("加载包含的清单 %s", include.Name)

			// 检查包含的清单文件是否存在
			if _, err := os.Stat(includePath); os.IsNotExist(err) {
				log.Error("included manifest file does not exist: %s", includePath)
				return fmt.Errorf("included manifest file does not exist: %s", includePath)
			}

			// 解析包含清单时不传组过滤（nil），保留全部项目；组过滤由 sync 执行
			includeManifest, err := parser.ParseFromFile(includePath, nil)
			if err != nil {
				log.Error("failed to parse included manifest%s failed: %v", include.Name, err)
				return fmt.Errorf("failed to parse included manifest%s failed: %w", include.Name, err)
			}

			if includeManifest == nil {
				log.Error("包含的清单文件 %s 解析结果为空", include.Name)
				return fmt.Errorf("包含的清单文件 %s 解析结果为空", include.Name)
			}

			log.Debug("包含的清单 %s 包含 %d 个项目", include.Name, len(includeManifest.Projects))

			includedManifests = append(includedManifests, includeManifest)
		}

		// 合并清单
		log.Info("合并 %d 个清单文件", len(includedManifests))

		mergedManifest, err := merger.Merge(includedManifests)
		if err != nil {
			log.Error("failed to merge manifests: %v", err)
			return fmt.Errorf("failed to merge manifests: %w", err)
		}

		// 更新清单对象
		manifestObj = mergedManifest

		log.Info("合并后的清单包含 %d 个项目", len(manifestObj.Projects))
		opts.Merged = true

		// Remove existing manifest.xml if it is a symlink (legacy from old repo-go versions).
		// If left as a symlink, WriteFile would follow it and overwrite the manifests git repo file.
		mergedPath := filepath.Join(".repo", "manifest.xml")
		if fi, err := os.Lstat(mergedPath); err == nil && (fi.Mode()&os.ModeSymlink != 0) {
			os.Remove(mergedPath)
		}
		log.Debug("保存合并后的清单到 %s", mergedPath)
		mergedData, err := manifestObj.ToXML()
		if err != nil {
			log.Error("failed to convert merged manifest to XML: %v", err)
			return fmt.Errorf("failed to convert merged manifest to XML: %w", err)
		}

		if err := os.WriteFile(mergedPath, []byte(mergedData), 0644); err != nil {
			log.Error("failed to write merged manifest file: %v", err)
			return fmt.Errorf("failed to write merged manifest file: %w", err)
		}

		log.Debug("已将合并后的清单保存到 %s", mergedPath)
	} else {
		// 无 include：直接把解析出的清单（完整、未过滤）写入 .repo/manifest.xml
		// Remove existing manifest.xml if it is a symlink (legacy from old repo-go versions).
		mergedPath := filepath.Join(".repo", "manifest.xml")
		if fi, err := os.Lstat(mergedPath); err == nil && (fi.Mode()&os.ModeSymlink != 0) {
			os.Remove(mergedPath)
		}
		log.Debug("保存清单到 %s", mergedPath)
		mergedData, err := manifestObj.ToXML()
		if err != nil {
			log.Error("failed to convert manifest to XML: %v", err)
			return fmt.Errorf("failed to convert manifest to XML: %w", err)
		}
		if err := os.WriteFile(mergedPath, []byte(mergedData), 0644); err != nil {
			log.Error("failed to write manifest file: %v", err)
			return fmt.Errorf("failed to write manifest file: %w", err)
		}
		log.Info("已将清单保存到 %s", mergedPath)
	}

	// 保存配置
	log.Info("保存 repo 配置...")
	if err := saveRepoConfig(cfg); err != nil {
		log.Error("failed to save config: %v", err)
		return fmt.Errorf("failed to save config: %w", err)
	}

	// 处理多清单选项
	if opts.OuterManifest {
		// 实现加载外部清单的逻辑
		log.Debug("加载外部清单...")

		// 查找外部清单
		outerManifestPath := filepath.Join("..", ".repo", "manifest.xml")
		if _, err := os.Stat(outerManifestPath); err == nil {
			// 加载外部清单
			log.Debug("解析外部清单: %s", outerManifestPath)
			outerManifest, err := parser.ParseFromFile(outerManifestPath, nil)
			if err != nil {
				log.Error("解析外部清单failed: %v", err)
				return fmt.Errorf("failed to parse outer manifest: %w", err)
			}

			// 合并外部清单
			log.Debug("合并外部清单...")
			merger := manifest.NewMerger(parser, filepath.Join(".repo"))
			mergedManifest, err := merger.Merge([]*manifest.Manifest{outerManifest, manifestObj})
			if err != nil {
				log.Error("合并外部清单failed: %v", err)
				return fmt.Errorf("failed to merge with outer manifest: %w", err)
			}

			// 更新清单对象
			manifestObj = mergedManifest

			// 保存合并后的清单
			mergedPath := filepath.Join(".repo", "manifest.xml")
			log.Debug("保存合并后的清单到 %s", mergedPath)
			mergedData, err := manifestObj.ToXML()
			if err != nil {
				log.Error("failed to convert merged manifest to XML: %v", err)
				return fmt.Errorf("failed to convert merged manifest to XML: %w", err)
			}

			if err := os.WriteFile(mergedPath, []byte(mergedData), 0644); err != nil {
				log.Error("failed to write merged manifest file: %v", err)
				return fmt.Errorf("failed to write merged manifest: %w", err)
			}
			log.Info("外部清单合并成功")
		} else {
			log.Debug("未找到外部清单 %s", outerManifestPath)
		}
	}

	if opts.ThisManifestOnly {
		// 实现仅处理当前清单的逻辑
		log.Debug("仅处理当前清单")
		// 移除所有include标签
		manifestObj.Includes = nil
	}

	if opts.AllManifests {
		// 实现处理所有清单的逻辑
		log.Debug("处理所有清单")
		// 确保处理所有include标签
		if len(manifestObj.Includes) > 0 && !opts.ThisManifestOnly {
			merger := manifest.NewMerger(parser, filepath.Join(".repo", "manifests"))
			includedManifests := []*manifest.Manifest{manifestObj}

			for _, include := range manifestObj.Includes {
				includePath := filepath.Join(".repo", "manifests", include.Name)
				log.Debug("加载包含的清单 %s", include.Name)

				includeManifest, err := parser.ParseFromFile(includePath, nil)
				if err != nil {
					log.Error("failed to parse included manifest%s failed: %v", include.Name, err)
					return fmt.Errorf("failed to parse included manifest %s: %w", include.Name, err)
				}

				includedManifests = append(includedManifests, includeManifest)
			}

			// 合并清单：合并结果本身不再被后续逻辑消费（manifest.xml 已在
			// 前面写出），此处调用仅用于校验合并是否成功，失败即返回错误
			log.Debug("合并所有清单..")
			if _, err := merger.Merge(includedManifests); err != nil {
				log.Error("failed to merge manifests: %v", err)
				return fmt.Errorf("failed to merge manifests: %w", err)
			}
			log.Info("所有清单合并成功")
		}
	}

	log.Debug("Repo 初始化完成")

	// 2.4 若未做合并/过滤，把 .repo/manifest.xml 软链到 manifests/<name>（对齐上游 repo）
	// Windows 创建软链可能因权限failed，failed时回退为已写入的普通文件并告警
	if !opts.Merged && !opts.Filtered {
		linkManifestXML(cfg.ManifestName, log)
	}
	return nil
}

// linkManifestXML 在未做合并/过滤时，把 .repo/manifest.xml 替换为指向
// manifests/<name> 的符号链接（对齐上游 repo 的清单软链行为）。
// 软链创建failed（如 Windows 无权限）时回退为已写入的普通文件，仅告警不报错。
func linkManifestXML(manifestName string, log logger.Logger) {
	if manifestName == "" {
		manifestName = "default.xml"
	}
	target := filepath.Join("manifests", manifestName)
	linkPath := filepath.Join(".repo", "manifest.xml")

	// 确认 manifests/<name> 存在
	if _, err := os.Stat(filepath.Join(".repo", target)); err != nil {
		log.Debug("软链目标不存在，保持普通文件 manifest.xml: %s", target)
		return
	}

	// 移除已写入的普通文件 manifest.xml
	if err := os.Remove(linkPath); err != nil {
		log.Debug("移除现有 manifest.xml failed（可能本就是软链）: %v", err)
	}

	// failed to create symlink（相对路径，便于仓库整体迁移）
	if err := os.Symlink(target, linkPath); err != nil {
		// Windows 常因缺少权限/开发者模式failed，回退：重新复制原始文件
		log.Warn("创建 manifest.xml 软链failed（%v），回退为普通文件", err)
		src, rerr := os.ReadFile(filepath.Join(".repo", target))
		if rerr == nil {
			if werr := os.WriteFile(linkPath, src, 0644); werr != nil {
				log.Debug("回退复制 manifest.xml failed: %v", werr)
			}
		}
		return
	}
	log.Debug("已将 .repo/manifest.xml 软链到 %s", target)
}

// initRepoStructure 初始化repo目录结构和配置
func initRepoStructure(repoDir string) error {
	// 创建.repo目录结构
	dirs := []string{
		".repo",
		".repo/manifests",
		// ".repo/project-objects",
		// ".repo/repo",
		// ".repo/projects",
		".repo/hooks",
	}

	for _, dir := range dirs {
		if err := os.MkdirAll(filepath.Join(repoDir, dir), 0755); err != nil {
			return fmt.Errorf("failed to create directory %s: %w", dir, err)
		}
	}

	// 初始化Git hooks
	if err := hook.InitHooks(repoDir); err != nil {
		return fmt.Errorf("failed to initialize hooks: %w", err)
	}

	// 创建repo.git配置文件
	if err := hook.CreateRepoGitConfig(repoDir); err != nil {
		return fmt.Errorf("failed to create repo.git config: %w", err)
	}

	// 创建repo.gitconfig配置文件
	if err := hook.CreateRepoGitconfig(repoDir); err != nil {
		return fmt.Errorf("failed to create repo.gitconfig: %w", err)
	}

	// 记录钩子目录路径，用于后续同步到各个项目
	// hooksDir := filepath.Join(repoDir, ".repo", "hooks")
	// fmt.Printf("已初始化钩子脚本目录: %s\n", hooksDir)

	return nil
}

// expandUser 展开路径中的前导 ~，对齐上游 Python 的 os.path.expanduser。
// 仅处理 "~" 与 "~/"；"~user" 形式在 Windows 无意义，原样返回。
func expandUser(path string) (string, error) {
	if !strings.HasPrefix(path, "~") {
		return path, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return path, err
	}
	if path == "~" {
		return home, nil
	}
	if strings.HasPrefix(path, "~"+string(filepath.Separator)) || strings.HasPrefix(path, "~/") {
		return filepath.Join(home, path[2:]), nil
	}
	return path, nil
}
