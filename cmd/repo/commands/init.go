package commands

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/leopardxu/repo-go/internal/git"
	"github.com/leopardxu/repo-go/internal/hook"
	"github.com/leopardxu/repo-go/internal/logger"
	"github.com/leopardxu/repo-go/internal/manifest"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
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

	// changedFlags 记录命令行显式设置的 flag（含位置参数与环境变量注入）。
	// re-init 时未显式指定的字段从旧 .repo/config.json 继承，避免
	// `repo init -u URL` 把 mirror/reference/depth/groups 等重置为默认值。
	changedFlags map[string]bool
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
			// 记录显式设置的 flag：re-init 时未显式指定的字段从旧配置继承。
			opts.changedFlags = make(map[string]bool)
			cmd.Flags().Visit(func(f *pflag.Flag) {
				opts.changedFlags[f.Name] = true
			})

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
				opts.changedFlags["manifest-url"] = true
			}

			// 上游 _RegisteredEnvironmentOptions：对应 flag 未显式设置时回退到环境变量
			if opts.ManifestURL == "" {
				if v := os.Getenv("REPO_MANIFEST_URL"); v != "" {
					opts.ManifestURL = v
					opts.changedFlags["manifest-url"] = true
				}
			}
			if opts.Reference == "" {
				if v := os.Getenv("REPO_MIRROR_LOCATION"); v != "" {
					opts.Reference = v
					opts.changedFlags["reference"] = true
				}
			}
			if !cmd.Flags().Changed("git-lfs") {
				if v := os.Getenv("REPO_GIT_LFS"); v != "" {
					if b, err := strconv.ParseBool(v); err == nil {
						opts.GitLFS = b
					} else {
						opts.GitLFS = true
					}
					opts.changedFlags["git-lfs"] = true
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

			// 正反 flag 同时显式指定时，必须在归一化前报错
			if err := validateChangedFlagConflicts(cmd); err != nil {
				return err
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

// commitSHAPattern 匹配完整提交 SHA：40 位或 64 位小写十六进制。
// 只接受完整长度，避免 "deadbeef" 之类短串被误判为提交而触发 detached checkout。
var commitSHAPattern = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)

// isCommitSHA 判断分支参数是否为一个提交 SHA。
func isCommitSHA(branch string) bool {
	return commitSHAPattern.MatchString(strings.TrimSpace(branch))
}

// ensureWithinWorkspace 校验路径位于当前工作目录内，防止误删仓库外目录。
func ensureWithinWorkspace(dir string) error {
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("failed to get current directory: %w", err)
	}
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return fmt.Errorf("failed to resolve path %s: %w", dir, err)
	}
	relPath, err := filepath.Rel(cwd, absDir)
	if err != nil || relPath == ".." || strings.HasPrefix(relPath, ".."+string(filepath.Separator)) {
		return fmt.Errorf("path %s is not under repo root, operation refused", dir)
	}
	return nil
}

// cloneManifestRepo 克隆或更新清单仓库。
// 清单仓库始终使用普通 clone：--mirror 仅作用于清单内项目仓库，裸镜像的
// .repo/manifests 没有工作区，无法解析清单文件。
// 使用 git -C <dir> 在指定目录执行命令，避免 os.Chdir 的线程安全问题。
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
		if err := ensureWithinWorkspace(manifestsDir); err != nil {
			return err
		}
		if err := os.RemoveAll(manifestsDir); err != nil {
			return fmt.Errorf("failed to clean manifests directory: %w", err)
		}
		if err := os.MkdirAll(manifestsDir, 0755); err != nil {
			return fmt.Errorf("failed to recreate manifests directory: %w", err)
		}
	}

	return cloneManifestRepoFresh(gitRunner, manifestsDir, cfg)
}

// cloneManifestRepoFresh 在空目录上执行清单仓库初始克隆。
func cloneManifestRepoFresh(gitRunner git.Runner, manifestsDir string, cfg *RepoConfig) error {
	branch := strings.TrimSpace(cfg.ManifestBranch)
	commitSHA := isCommitSHA(branch)

	args := []string{"clone"}
	if cfg.ManifestDepth > 0 {
		args = append(args, fmt.Sprintf("--depth=%d", cfg.ManifestDepth))
	}
	// HEAD 视为远端默认分支，不传 -b；提交 SHA 先普通 clone 再 checkout
	if branch != "" && branch != "HEAD" && !commitSHA {
		args = append(args, "-b", branch)
	}
	if cfg.Reference != "" {
		args = append(args, fmt.Sprintf("--reference=%s", cfg.Reference))
		if cfg.Dissociate {
			args = append(args, "--dissociate")
		}
	}
	if cfg.PartialClone {
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

	// -b HEAD：解析克隆后的默认分支并记录到配置
	if branch == "HEAD" {
		if resolved, err := resolveDefaultBranch(gitRunner, manifestsDir); err == nil && resolved != "" {
			cfg.ManifestBranch = resolved
		}
	}

	// -b <commit SHA>：先 clone，再 fetch 指定 upstream ref 并 checkout 该提交
	if commitSHA {
		if err := fetchManifestUpstreamRef(gitRunner, manifestsDir, cfg); err != nil {
			return err
		}
		if _, err := gitRunner.RunInDir(manifestsDir, "checkout", branch); err != nil {
			return fmt.Errorf("failed to checkout manifest commit %s: %w", branch, err)
		}
	}

	return nil
}

// fetchManifestUpstreamRef 在清单仓库中获取 --manifest-upstream-branch 指定的
// ref，供提交 SHA checkout 使用（该选项此前完全未被读取）。
func fetchManifestUpstreamRef(gitRunner git.Runner, manifestsDir string, cfg *RepoConfig) error {
	upstream := strings.TrimSpace(cfg.ManifestUpstreamBranch)
	if upstream == "" {
		return nil
	}
	refspec := fmt.Sprintf("refs/heads/%s:refs/remotes/origin/%s", upstream, upstream)
	fetchArgs := []string{"fetch"}
	if cfg.ManifestDepth > 0 {
		fetchArgs = append(fetchArgs, fmt.Sprintf("--depth=%d", cfg.ManifestDepth))
	}
	fetchArgs = append(fetchArgs, "origin", refspec)
	if _, err := gitRunner.RunInDir(manifestsDir, fetchArgs...); err != nil {
		// 兼容 upstream 以 ref/sha 形式给出（非分支名）
		if _, ferr := gitRunner.RunInDir(manifestsDir, "fetch", "origin", upstream); ferr != nil {
			return fmt.Errorf("failed to fetch manifest upstream ref %q: %w", upstream, err)
		}
	}
	return nil
}

// resolveRemoteDefaultBranch 使用 ls-remote --symref 直接向远端解析默认分支。
// 换 URL 后本地的 refs/remotes/origin/HEAD 可能仍指向旧仓库的分支，因此
// fetch 之后必须询问远端，而不是读旧 origin/HEAD。
func resolveRemoteDefaultBranch(gitRunner git.Runner, dir, url string) (string, error) {
	if strings.TrimSpace(url) == "" {
		return "", fmt.Errorf("manifest URL is empty")
	}
	out, err := gitRunner.RunInDir(dir, "ls-remote", "--symref", url, "HEAD")
	if err != nil {
		return "", fmt.Errorf("failed to query remote default branch: %w", err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		// 形如 "ref: refs/heads/main\tHEAD"
		if !strings.HasPrefix(line, "ref:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) >= 2 && strings.HasPrefix(fields[1], "refs/heads/") {
			return strings.TrimPrefix(fields[1], "refs/heads/"), nil
		}
	}
	return "", fmt.Errorf("remote %s did not advertise a default branch", url)
}

// resolveDefaultBranch 解析清单仓库的远端默认分支（-b HEAD 语义）。
func resolveDefaultBranch(gitRunner git.Runner, manifestsDir string) (string, error) {
	if out, err := gitRunner.RunInDir(manifestsDir, "symbolic-ref", "--short", "refs/remotes/origin/HEAD"); err == nil {
		branch := strings.TrimSpace(string(out))
		if branch != "" {
			return strings.TrimPrefix(branch, "origin/"), nil
		}
	}
	out, err := gitRunner.RunInDir(manifestsDir, "symbolic-ref", "--short", "HEAD")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// normalizeHeadManifestBranch 将 -b HEAD 归一化为清单仓库 origin/HEAD 指向的
// 实际分支。diffInitConfig 比较前使用该值，保证同一条 `repo init -b HEAD`
// 命令重复执行时不因 "HEAD" vs 实际分支而被要求确认。仓库尚未克隆时原样返回，
// 由 clone 阶段解析。
func normalizeHeadManifestBranch(runner git.Runner, manifestsDir, branch string) string {
	if strings.TrimSpace(branch) != "HEAD" {
		return branch
	}
	if _, err := os.Stat(filepath.Join(manifestsDir, ".git")); err != nil {
		return branch
	}
	resolved, err := resolveDefaultBranch(runner, manifestsDir)
	if err != nil || resolved == "" {
		return branch
	}
	return resolved
}

// loadExistingRepoConfig 读取现有 .repo/config.json（用于判断克隆参数是否变化）。
func loadExistingRepoConfig(manifestsDir string) (*RepoConfig, bool) {
	configPath := filepath.Join(filepath.Dir(manifestsDir), "config.json")
	data, err := os.ReadFile(configPath)
	if err != nil {
		return nil, false
	}
	var cfg RepoConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, false
	}
	return &cfg, true
}

// flagChanged 判断 flag 是否被显式设置（含位置参数/环境变量）。
func (o *InitOptions) flagChanged(name string) bool {
	return o.changedFlags != nil && o.changedFlags[name]
}

// flagTouched 判断任一同义 flag（如正/反 flag 对）是否被显式设置。
func (o *InitOptions) flagTouched(names ...string) bool {
	for _, name := range names {
		if o.flagChanged(name) {
			return true
		}
	}
	return false
}

// applyRepoConfigInheritance 将旧配置中“本次未显式指定”的字段继承到 opts。
// 这是 re-init 幂等的关键：`repo init -u URL` 在已初始化客户端上不得把
// mirror/reference/depth/groups 等重置为 flag 默认值或强制重克隆。
func applyRepoConfigInheritance(opts *InitOptions, old *RepoConfig) {
	if old == nil {
		return
	}
	inheritString := func(changed bool, dst *string, src string) {
		if !changed && src != "" {
			*dst = src
		}
	}
	inheritBool := func(changed bool, dst *bool, src bool) {
		if !changed {
			*dst = src
		}
	}
	inheritInt := func(changed bool, dst *int, src int) {
		if !changed {
			*dst = src
		}
	}

	inheritString(opts.flagTouched("manifest-url"), &opts.ManifestURL, old.ManifestURL)
	inheritString(opts.flagTouched("manifest-branch"), &opts.ManifestBranch, old.ManifestBranch)
	inheritString(opts.flagTouched("manifest-upstream-branch"), &opts.ManifestUpstreamBranch, old.ManifestUpstreamBranch)
	inheritString(opts.flagTouched("manifest-name"), &opts.ManifestName, old.ManifestName)
	// groups 需要区分“未指定”与“显式空串”：未显式 -g 时原样继承旧值
	// （即使为空），显式 -g "" 则保留空串，不能回落到默认 "default"。
	if !opts.flagTouched("groups") {
		opts.Groups = old.Groups
	}
	inheritString(opts.flagTouched("platform"), &opts.Platform, old.Platform)

	inheritBool(opts.flagTouched("mirror"), &opts.Mirror, old.Mirror)
	inheritBool(opts.flagTouched("archive"), &opts.Archive, old.Archive)
	inheritBool(opts.flagTouched("worktree"), &opts.Worktree, old.Worktree)
	inheritBool(opts.flagTouched("use-local-gitdirs"), &opts.UseLocalGitdirs, old.UseLocalGitdirs)
	inheritString(opts.flagTouched("reference"), &opts.Reference, old.Reference)
	inheritBool(opts.flagTouched("no-smart-cache"), &opts.NoSmartCache, old.NoSmartCache)
	inheritBool(opts.flagTouched("dissociate"), &opts.Dissociate, old.Dissociate)
	inheritInt(opts.flagTouched("depth"), &opts.Depth, old.Depth)
	inheritInt(opts.flagTouched("manifest-depth"), &opts.ManifestDepth, old.ManifestDepth)

	inheritBool(opts.flagTouched("partial-clone", "no-partial-clone"), &opts.PartialClone, old.PartialClone)
	inheritString(opts.flagTouched("partial-clone-exclude"), &opts.PartialCloneExclude, old.PartialCloneExclude)
	inheritString(opts.flagTouched("clone-filter"), &opts.CloneFilter, old.CloneFilter)
	inheritBool(opts.flagTouched("use-superproject", "no-use-superproject"), &opts.UseSuperproject, old.UseSuperproject)
	inheritBool(opts.flagTouched("clone-bundle", "no-clone-bundle"), &opts.CloneBundle, old.CloneBundle)
	inheritBool(opts.flagTouched("git-lfs", "no-git-lfs"), &opts.GitLFS, old.GitLFS)

	inheritString(opts.flagTouched("repo-url"), &opts.RepoURL, old.RepoURL)
	inheritString(opts.flagTouched("repo-rev", "repo-branch"), &opts.RepoRev, old.RepoRev)
	inheritBool(opts.flagTouched("no-repo-verify"), &opts.NoRepoVerify, old.NoRepoVerify)
	inheritBool(opts.flagTouched("standalone-manifest"), &opts.StandaloneManifest, old.StandaloneManifest)
	inheritBool(opts.flagTouched("submodules"), &opts.Submodules, old.Submodules)
	inheritBool(opts.flagTouched("current-branch", "no-current-branch"), &opts.CurrentBranch, old.CurrentBranch)
	inheritBool(opts.flagTouched("tags", "no-tags"), &opts.Tags, old.Tags)
}

// discardInheritedModeConflicts 在继承旧配置后、validateOptions 之前清理
// 与新显式选项冲突的“继承值”。只有本次显式设置的模式开关才触发丢弃，
// 且绝不丢弃同样被显式设置的选项——那样真正的命令行冲突仍由
// validateOptions 报错，而不会因为继承的历史配置把用户的新选择废掉。
func discardInheritedModeConflicts(opts *InitOptions) {
	clearInherited := func(dst *bool, names ...string) {
		if !opts.flagTouched(names...) {
			*dst = false
		}
	}

	if opts.Mirror && opts.flagChanged("mirror") {
		clearInherited(&opts.Archive, "archive")
		clearInherited(&opts.PartialClone, "partial-clone", "no-partial-clone")
		clearInherited(&opts.Worktree, "worktree")
		clearInherited(&opts.UseLocalGitdirs, "use-local-gitdirs")
		clearInherited(&opts.UseSuperproject, "use-superproject", "no-use-superproject")
	}
	if opts.Archive && opts.flagChanged("archive") {
		clearInherited(&opts.Mirror, "mirror")
		clearInherited(&opts.UseSuperproject, "use-superproject", "no-use-superproject")
		clearInherited(&opts.UseLocalGitdirs, "use-local-gitdirs")
	}
	if opts.Worktree && opts.flagChanged("worktree") {
		clearInherited(&opts.Mirror, "mirror")
		clearInherited(&opts.UseLocalGitdirs, "use-local-gitdirs")
	}
	if opts.UseLocalGitdirs && opts.flagChanged("use-local-gitdirs") {
		clearInherited(&opts.Mirror, "mirror")
		clearInherited(&opts.Archive, "archive")
		clearInherited(&opts.Worktree, "worktree")
	}
	if opts.PartialClone && opts.flagTouched("partial-clone", "no-partial-clone") {
		clearInherited(&opts.Mirror, "mirror")
	}
	if opts.UseSuperproject && opts.flagTouched("use-superproject", "no-use-superproject") {
		clearInherited(&opts.Mirror, "mirror")
		clearInherited(&opts.Archive, "archive")
	}
	if opts.StandaloneManifest && opts.flagChanged("standalone-manifest") {
		// standalone 与 manifest-branch / 非默认 manifest-name 互斥；
		// 显式选择 standalone 时丢弃继承来的冲突值。
		if !opts.flagTouched("manifest-branch") {
			opts.ManifestBranch = ""
		}
		if !opts.flagTouched("manifest-upstream-branch") {
			opts.ManifestUpstreamBranch = ""
		}
		if !opts.flagTouched("manifest-name") {
			opts.ManifestName = "default.xml"
		}
	}
}

// deriveCloneBundle 在继承完成后重新应用上游推导：
// partial clone 开启且未显式设置 clone-bundle 时默认关闭 clone bundle。
func deriveCloneBundle(opts *InitOptions) {
	if opts.PartialClone && !opts.flagTouched("clone-bundle", "no-clone-bundle") {
		opts.CloneBundle = false
	}
}

// resolveEffectiveGroups 计算本次 init 最终写入配置的 groups：
//   - 首次初始化，或本次显式指定 -p/--platform 或 -g/--groups 时，按平台
//     语义重建 platform-* 组；
//   - 普通 re-init 原样继承旧 Groups，保留用户手写的 platform 组，避免
//     每次 init 都按 --platform 默认值重写。
func resolveEffectiveGroups(opts *InitOptions, initialized bool) (string, error) {
	if initialized && !opts.flagTouched("platform", "groups") {
		return initNormalizeEffectiveGroups(opts.Groups), nil
	}
	groups, err := mergePlatformGroups(opts.Groups, opts.Platform, opts.Mirror, opts.flagChanged("groups"))
	if err != nil {
		return "", err
	}
	return initNormalizeEffectiveGroups(groups), nil
}

// initNormalizeEffectiveGroups 保证写入配置的 groups 非空：空值归一为 "default"，
// 与 (*config.Config).EffectiveGroups 的空值回退语义一致，避免 config.json
// 落盘空组后各命令解析清单时行为不一致（上游未配置组时按 default 解析）。
func initNormalizeEffectiveGroups(groups string) string {
	if strings.TrimSpace(groups) == "" {
		return "default"
	}
	return groups
}

// manifestCloneParamsChanged 判断会影响清单仓库克隆方式的参数是否变化。
// 变化时必须安全重克隆，否则浅克隆/部分克隆/引用仓库等状态无法就地调整。
func manifestCloneParamsChanged(oldCfg, newCfg RepoConfig) bool {
	if oldCfg.ManifestDepth != newCfg.ManifestDepth {
		return true
	}
	if oldCfg.PartialClone != newCfg.PartialClone {
		return true
	}
	if oldCfg.PartialClone && oldCfg.CloneFilter != newCfg.CloneFilter {
		return true
	}
	if oldCfg.Reference != newCfg.Reference {
		return true
	}
	if oldCfg.Dissociate != newCfg.Dissociate {
		return true
	}
	return false
}

// updateExistingManifestRepo 对已存在的清单仓库执行 URL 校正 + fetch + checkout。
// 使用 git -C <dir> 避免线程不安全的 os.Chdir。
// 对齐上游 Sync_LocalHalf 语义：checkout 后 reset --hard 到已获取的远程顶端，
// 否则 re-init 会停留在本地旧提交，manifest.xml 永远是陈旧内容。
func updateExistingManifestRepo(gitRunner git.Runner, manifestsDir string, cfg *RepoConfig) error {
	// 克隆方式参数变化：在确认路径安全后重克隆
	if oldCfg, ok := loadExistingRepoConfig(manifestsDir); ok && manifestCloneParamsChanged(*oldCfg, *cfg) {
		return recloneManifestRepo(gitRunner, manifestsDir, cfg)
	}

	// origin URL 与配置不一致时校正，避免 config 新 URL + origin 旧 URL
	if cfg.ManifestURL != "" {
		out, err := gitRunner.RunInDir(manifestsDir, "config", "--get", "remote.origin.url")
		currentURL := strings.TrimSpace(string(out))
		if err != nil || currentURL == "" {
			if _, aerr := gitRunner.RunInDir(manifestsDir, "remote", "add", "origin", cfg.ManifestURL); aerr != nil {
				return fmt.Errorf("failed to add manifest origin remote: %w", aerr)
			}
		} else if currentURL != cfg.ManifestURL {
			if _, serr := gitRunner.RunInDir(manifestsDir, "remote", "set-url", "origin", cfg.ManifestURL); serr != nil {
				return fmt.Errorf("failed to update manifest origin URL: %w", serr)
			}
		}
	}

	// fetch 更新
	if _, err := gitRunner.RunInDir(manifestsDir, "fetch", "--all"); err != nil {
		return fmt.Errorf("failed to fetch manifest repository: %w", err)
	}

	// 提交 SHA：fetch 指定 upstream ref 后 checkout（可能为 detached HEAD）
	if isCommitSHA(cfg.ManifestBranch) {
		if err := fetchManifestUpstreamRef(gitRunner, manifestsDir, cfg); err != nil {
			return err
		}
		if _, err := gitRunner.RunInDir(manifestsDir, "checkout", strings.TrimSpace(cfg.ManifestBranch)); err != nil {
			return fmt.Errorf("failed to checkout manifest commit %s: %w", cfg.ManifestBranch, err)
		}
		return nil
	}

	// 未指定分支：无法确定目标 ref，仅完成 fetch（保持当前分支）
	branch := strings.TrimSpace(cfg.ManifestBranch)
	if branch == "" {
		return nil
	}
	// -b HEAD：fetch 后直接向远端解析默认分支。换 URL 时本地
	// refs/remotes/origin/HEAD 可能仍指向旧仓库分支，不能依赖它。
	if branch == "HEAD" {
		resolved, err := resolveRemoteDefaultBranch(gitRunner, manifestsDir, cfg.ManifestURL)
		if err != nil {
			if cfg.ManifestURL != "" {
				return fmt.Errorf("failed to resolve default manifest branch: %w", err)
			}
			// 没有 URL（极端情况）时退回本地 origin/HEAD
			resolved, err = resolveDefaultBranch(gitRunner, manifestsDir)
			if err != nil {
				return fmt.Errorf("failed to resolve default manifest branch: %w", err)
			}
		}
		if resolved == "" {
			return fmt.Errorf("failed to resolve default manifest branch: remote returned empty branch")
		}
		branch = resolved
		cfg.ManifestBranch = resolved
	}
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

// recloneManifestRepo 安全地删除并重新克隆清单仓库（路径必须在 workspace 内）。
func recloneManifestRepo(gitRunner git.Runner, manifestsDir string, cfg *RepoConfig) error {
	if err := ensureWithinWorkspace(manifestsDir); err != nil {
		return err
	}
	if err := os.RemoveAll(manifestsDir); err != nil {
		return fmt.Errorf("failed to remove manifests directory for reclone: %w", err)
	}
	if err := os.MkdirAll(manifestsDir, 0755); err != nil {
		return fmt.Errorf("failed to recreate manifests directory: %w", err)
	}
	return cloneManifestRepo(gitRunner, cfg)
}

// allPlatformNames 是 --platform 支持的具体平台组（对齐上游 platformize 列表）。
var allPlatformNames = []string{"linux", "darwin", "windows"}

// mergePlatformGroups 按上游语义把 --platform 合并进有效组：
//   - auto：非 mirror 时追加 platform-<host OS>（mirror 模式上游不追加）；
//   - all：追加 platform-linux/darwin/windows；
//   - none：不追加；
//   - 具体平台：追加 platform-<x>，非法值报错。
//
// preserveExplicitPlatform 为 true 时保留 groups 中用户显式输入的 platform-*
// 组（只补不删）；为 false 时先剔除全部 platform-* 再按 --platform 追加——
// 这用于清理从旧 config 继承的过期平台组，避免重复 init 累积或残留平台组。
func mergePlatformGroups(groups, platform string, mirror, preserveExplicitPlatform bool) (string, error) {
	name := strings.TrimSpace(platform)
	if name == "" {
		name = "auto"
	}

	parts := manifest.SplitGroups(groups)
	filtered := make([]string, 0, len(parts)+len(allPlatformNames))
	for _, g := range parts {
		if preserveExplicitPlatform || !strings.HasPrefix(g, "platform-") {
			filtered = append(filtered, g)
		}
	}

	switch name {
	case "auto":
		if !mirror {
			filtered = append(filtered, "platform-"+runtime.GOOS)
		}
	case "all":
		for _, p := range allPlatformNames {
			filtered = append(filtered, "platform-"+p)
		}
	case "none":
		// 不追加任何平台组
	default:
		valid := false
		for _, p := range allPlatformNames {
			if p == name {
				valid = true
				break
			}
		}
		if !valid {
			return "", fmt.Errorf("invalid platform flag: %q", platform)
		}
		filtered = append(filtered, "platform-"+name)
	}

	// 去重并保留首次出现顺序
	seen := make(map[string]bool, len(filtered))
	out := make([]string, 0, len(filtered))
	for _, g := range filtered {
		if g == "" || seen[g] {
			continue
		}
		seen[g] = true
		out = append(out, g)
	}
	return strings.Join(out, ","), nil
}

// validateChangedFlagConflicts 检查正反 flag 是否被同时显式指定。
// 必须在 applyNegationFlags 之前执行：归一化会把正 flag 的默认 true 置为
// false，从而掩盖"两个 flag 都显式给出"的冲突。
func validateChangedFlagConflicts(cmd *cobra.Command) error {
	pairs := [][2]string{
		{"current-branch", "no-current-branch"},
		{"tags", "no-tags"},
		{"partial-clone", "no-partial-clone"},
		{"use-superproject", "no-use-superproject"},
		{"clone-bundle", "no-clone-bundle"},
		{"git-lfs", "no-git-lfs"},
		{"outer-manifest", "no-outer-manifest"},
	}
	for _, pair := range pairs {
		if cmd.Flags().Changed(pair[0]) && cmd.Flags().Changed(pair[1]) {
			return fmt.Errorf("cannot specify both --%s and --%s", pair[0], pair[1])
		}
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

	// --platform 合法性（非法值直接报错）
	if _, err := mergePlatformGroups(opts.Groups, opts.Platform, opts.Mirror, opts.flagChanged("groups")); err != nil {
		return err
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
	// opts.changedFlags 非 nil 时只比较显式指定的字段：未指定字段在 re-init
	// 中从旧配置继承，不应触发覆盖确认（等价于“同一条命令重复执行不确认”）。
	// 测试可通过 nil map 关闭该过滤，直接比较值。
	add := func(flags []string, name, oldVal, newVal string) {
		if opts.changedFlags != nil {
			touched := false
			for _, flag := range flags {
				if opts.changedFlags[flag] {
					touched = true
					break
				}
			}
			if !touched {
				return
			}
		}
		if oldVal != newVal {
			diffs = append(diffs, configFieldDiff{name: name, existing: oldVal, current: newVal})
		}
	}
	add([]string{"manifest-url"}, "manifest-url", existing.ManifestURL, opts.ManifestURL)
	add([]string{"manifest-branch"}, "manifest-branch", existing.ManifestBranch, opts.ManifestBranch)
	add([]string{"manifest-name"}, "manifest-name", existing.ManifestName, opts.ManifestName)
	add([]string{"groups"}, "groups", existing.Groups, opts.Groups)
	add([]string{"platform"}, "platform", existing.Platform, opts.Platform)
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

	// 归一化 --no-* 否定 flag：必须在 validateOptions 之前，否则
	// --no-current-branch / --no-clone-bundle（正 flag 默认 true）会被
	// 误判为"正反同时指定"而永久报错。
	applyNegationFlags(opts)

	// re-init 时未显式指定的字段从旧 .repo/config.json 继承：避免
	// `repo init -u URL` 重置 mirror/reference/depth/groups 等并强制重克隆。
	oldCfg, initialized := loadExistingRepoConfig(filepath.Join(".repo", "manifests"))
	if initialized {
		applyRepoConfigInheritance(opts, oldCfg)
	}

	// 继承值可能与本轮显式选择的模式冲突（例如旧配置 mirror=true，
	// 本次显式 --partial-clone）：丢弃冲突的继承值，再交给 validateOptions。
	discardInheritedModeConflicts(opts)
	// 继承完成后重新应用 partial-clone ⇒ clone-bundle=false 推导。
	deriveCloneBundle(opts)

	// -b HEAD 归一化为实际分支，保证重复执行同一条命令时 diffInitConfig
	// 不误判为配置变化。仅在清单 URL 未变化时使用本地 origin/HEAD；
	// 换 URL 后旧 origin/HEAD 可能指向旧仓库，保持 HEAD 交由 clone/fetch
	// 阶段用 ls-remote --symref 解析。
	urlUnchanged := !initialized || oldCfg == nil || oldCfg.ManifestURL == opts.ManifestURL
	if initialized && urlUnchanged {
		opts.ManifestBranch = normalizeHeadManifestBranch(git.NewRunner(), filepath.Join(".repo", "manifests"), opts.ManifestBranch)
	}

	// 按上游语义把 --platform 合并进有效组（写入 cfg.Groups，cfg.Platform 保留原值）。
	// 仅首次初始化或本次显式 -p/-g 时重建 platform-*；普通 re-init 原样继承。
	mergedGroups, err := resolveEffectiveGroups(opts, initialized)
	if err != nil {
		log.Error("平台选项验证failed: %v", err)
		return err
	}
	opts.Groups = mergedGroups

	// 验证选项冲突
	if err := validateOptions(opts); err != nil {
		log.Error("选项验证failed: %v", err)
		return err
	}

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

	// init 保留完整清单（ParseFromFile 已展开 include 并把其项目合并进
	// Projects），不在此按组预过滤；组过滤交给 sync 按 -g/配置动态执行。
	// 写盘前清空 Includes，输出扁平清单，避免二次解析/合并导致项目丢失。
	if err := writeManifestXML(manifestObj, log); err != nil {
		log.Error("failed to write manifest file: %v", err)
		return err
	}
	log.Info("已将清单保存到 %s", filepath.Join(".repo", "manifest.xml"))

	// 保存配置
	log.Info("保存 repo 配置...")
	if err := saveRepoConfig(cfg); err != nil {
		log.Error("failed to save config: %v", err)
		return fmt.Errorf("failed to save config: %w", err)
	}

	// 处理多清单选项：外部清单合并后重写扁平清单
	if opts.OuterManifest {
		log.Debug("加载外部清单...")

		outerManifestPath := filepath.Join("..", ".repo", "manifest.xml")
		if _, err := os.Stat(outerManifestPath); err == nil {
			log.Debug("解析外部清单: %s", outerManifestPath)
			outerManifest, err := parser.ParseFromFile(outerManifestPath, nil)
			if err != nil {
				log.Error("解析外部清单failed: %v", err)
				return fmt.Errorf("failed to parse outer manifest: %w", err)
			}

			log.Debug("合并外部清单...")
			merger := manifest.NewMerger(parser, filepath.Join(".repo"))
			mergedManifest, err := merger.Merge([]*manifest.Manifest{outerManifest, manifestObj})
			if err != nil {
				log.Error("合并外部清单failed: %v", err)
				return fmt.Errorf("failed to merge with outer manifest: %w", err)
			}
			manifestObj = mergedManifest

			if err := writeManifestXML(manifestObj, log); err != nil {
				log.Error("failed to write merged manifest file: %v", err)
				return err
			}
			log.Info("外部清单合并成功")
		} else {
			log.Debug("未找到外部清单 %s", outerManifestPath)
		}
	}

	// --this-manifest-only：include 已在解析期展开，这里仅记录语义选择
	if opts.ThisManifestOnly {
		log.Debug("仅处理当前清单")
	}

	log.Debug("Repo 初始化完成")
	return nil
}

// writeManifestXML 把清单以扁平 XML 写入 .repo/manifest.xml。
// 写盘前清空 Includes：include 内容已在解析期合并进 Projects，输出扁平清单
// 可保证再次 Parse 时项目/修订/属性不丢失；若残留 include 标签会重复展开。
func writeManifestXML(m *manifest.Manifest, log logger.Logger) error {
	if m == nil {
		return fmt.Errorf("manifest is nil")
	}
	m.Includes = nil
	// 扁平清单把项目与 remove-project 的相对顺序改成“项目在前、remove 在后”。
	// 任何残留 remove 都可能在二次解析时重新生效（删除刚写出的项目）或
	// 因目标不存在而报错，因此写盘前一律清除；解析期已消费的细项无需保留。
	m.RemoveProjects = nil

	manifestPath := filepath.Join(".repo", "manifest.xml")

	data, err := m.ToXML()
	if err != nil {
		return fmt.Errorf("failed to convert manifest to XML: %w", err)
	}

	// 使用临时文件 + rename 覆盖目标：rename 会替换软链本身，而不是跟随
	// 软链写入清单仓库（旧版本可能留下 .repo/manifest.xml -> manifests/<name>
	// 的软链）。同时保证写入的原子性，避免半写文件。
	dir := filepath.Dir(manifestPath)
	tmp, err := os.CreateTemp(dir, ".manifest-*.tmp")
	if err != nil {
		return fmt.Errorf("failed to create temp manifest file: %w", err)
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }

	if _, err := tmp.WriteString(data); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("failed to write temp manifest file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("failed to close temp manifest file: %w", err)
	}
	if err := os.Chmod(tmpName, 0644); err != nil {
		cleanup()
		return fmt.Errorf("failed to set manifest file mode: %w", err)
	}
	if err := os.Rename(tmpName, manifestPath); err != nil {
		cleanup()
		return fmt.Errorf("failed to replace manifest file: %w", err)
	}
	log.Debug("已将清单写入 %s", manifestPath)
	return nil
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
