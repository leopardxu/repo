package commands

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/leopardxu/repo-go/internal/config"
	"github.com/leopardxu/repo-go/internal/git"
	"github.com/leopardxu/repo-go/internal/logger"
	"github.com/leopardxu/repo-go/internal/manifest"
	"github.com/spf13/cobra"
)

// DiffManifestsOptions 包含diff-manifests命令的选项
type DiffManifestsOptions struct {
	CommonManifestOptions
	Raw      bool
	NoColor  bool
	Verbose  bool
	Quiet    bool
	Name     bool
	Path     bool
	Revision bool
	Groups   bool
	All      bool
}

// DiffManifestsCmd 返回diff-manifests命令
func DiffManifestsCmd() *cobra.Command {
	opts := &DiffManifestsOptions{}

	cmd := &cobra.Command{
		Use:   "diffmanifests manifest1.xml [manifest2.xml]",
		Short: "Show differences between project revisions of manifests",
		Long: `The repo diffmanifests command shows differences between project revisions of
manifest1 and manifest2. if manifest2 is not specified, current manifest.xml
will be used instead. Both absolute and relative paths may be used for
manifests. Relative paths start from project's ".repo/manifests" folder.

The --raw option Displays the diff in a way that facilitates parsing, the
project pattern will be <status> <path> <revision from> [<revision to>] and the
commit pattern will be <status> <onelined log> with status values respectively :

  A = Added project
  R = Removed project
  C = Changed project
  U = Project with unreachable revision(s) (revision(s) not found)

for project, and

   A = Added commit
   R = Removed commit

for a commit.

Only changed projects may contain commits, and commit status always starts with
a space, and are part of last printed project. Unreachable revisions may occur
if project is not up to date or if repo has not been initialized with all the
groups, in which case some projects won't be synced and their revisions won't be
available.`,
		RunE: func(_ *cobra.Command, args []string) error {
			return runDiffManifests(opts, args)
		},
		Args: cobra.RangeArgs(1, 2),
	}

	// 添加命令行选项
	cmd.Flags().BoolVar(&opts.Raw, "raw", false, "display raw diff")
	cmd.Flags().BoolVar(&opts.NoColor, "no-color", false, "does not display the diff in color")
	cmd.Flags().BoolVarP(&opts.Verbose, "verbose", "v", false, "show all output")
	cmd.Flags().BoolVarP(&opts.Quiet, "quiet", "q", false, "only show errors")
	AddManifestFlags(cmd, &opts.CommonManifestOptions)
	cmd.Flags().BoolVarP(&opts.Name, "name", "n", false, "diff project names only")
	cmd.Flags().BoolVarP(&opts.Path, "path", "p", false, "diff project paths only")
	cmd.Flags().BoolVarP(&opts.Revision, "revision", "r", false, "diff project revisions only")
	cmd.Flags().BoolVarP(&opts.Groups, "groups", "g", false, "diff project groups only")
	cmd.Flags().BoolVarP(&opts.All, "all", "a", false, "diff all project attributes")

	return cmd
}

// defaultDiffManifestPath 是未指定第二个清单时优先使用的当前合并清单。
const defaultDiffManifestPath = ".repo/manifest.xml"

// diffSecondManifestInput 返回未显式给出第二个清单时的输入：优先当前合并
// 快照 `.repo/manifest.xml`，只有该文件不存在时才回退配置的 ManifestName
// （清单仓库中的源清单不是合并结果，仅作兜底）。
func diffSecondManifestInput(cfg *config.Config) string {
	if _, err := resolveDiffManifestPath(defaultDiffManifestPath); err == nil {
		return defaultDiffManifestPath
	}
	if cfg != nil {
		if name := strings.TrimSpace(cfg.ManifestName); name != "" {
			return name
		}
	}
	return defaultDiffManifestPath
}

// resolveDiffManifestPath 把命令行给出的清单路径解析为可读取的文件。
// 绝对路径原样使用；相对路径优先相对 repo 根目录下的 `.repo/manifests`
// 解析（从任意 cwd 调用都成立），其次相对 repo 根目录，最后回退当前目录，
// 兼容 ".repo/manifest.xml" 这类写法。
func resolveDiffManifestPath(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", fmt.Errorf("manifest path is empty")
	}
	if filepath.IsAbs(name) {
		info, err := os.Stat(name)
		if err != nil || info.IsDir() {
			return "", fmt.Errorf("manifest file not found: %s", name)
		}
		return name, nil
	}
	candidates := make([]string, 0, 3)
	if repoRoot, err := config.GetRepoRoot(); err == nil {
		candidates = append(candidates,
			filepath.Join(repoRoot, ".repo", "manifests", name),
			filepath.Join(repoRoot, name),
		)
	}
	candidates = append(candidates, name)
	for _, candidate := range candidates {
		info, err := os.Stat(candidate)
		if err == nil && !info.IsDir() {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("manifest file not found: %s (looked under .repo/manifests)", name)
}

// runDiffManifests 执行diff-manifests命令
func runDiffManifests(opts *DiffManifestsOptions, args []string) error {
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

	if len(args) < 1 || len(args) > 2 {
		log.Error("需要提供 1 或 2 个清单文件路径")
		return fmt.Errorf("usage: repo diffmanifests manifest1.xml [manifest2.xml]")
	}

	// 未指定第二个清单时优先使用当前合并快照；快照缺失才回退 ManifestName
	manifest2Input := ""
	if len(args) == 2 {
		manifest2Input = args[1]
	} else if _, statErr := resolveDiffManifestPath(defaultDiffManifestPath); statErr == nil {
		manifest2Input = defaultDiffManifestPath
	} else {
		cfg, cerr := config.Load()
		if cerr != nil {
			log.Warn("failed to load config, 无法回退 ManifestName: %v", cerr)
		}
		manifest2Input = diffSecondManifestInput(cfg)
	}

	manifest1Path, err := resolveDiffManifestPath(args[0])
	if err != nil {
		log.Error("%v", err)
		return err
	}
	manifest2Path, err := resolveDiffManifestPath(manifest2Input)
	if err != nil {
		log.Error("%v", err)
		return err
	}

	log.Debug("正在比较清单文件 %s 和 %s", manifest1Path, manifest2Path)

	// 创建清单解析器
	parser := manifest.NewParser()
	// diff 的两个输入都是显式清单文件，不应混入当前检出的 .repo/local_manifests
	parser.SetUseLocalManifests(false)

	// 解析第一个清单文件
	log.Debug("解析第一个清单文件 %s", manifest1Path)
	manifest1, err := parser.ParseFromFile(manifest1Path, nil)
	if err != nil {
		log.Error("解析第一个清单文件失败: %v", err)
		return fmt.Errorf("failed to parse first manifest: %w", err)
	}

	// 解析第二个清单文件
	log.Debug("解析第二个清单文件 %s", manifest2Path)
	manifest2, err := parser.ParseFromFile(manifest2Path, nil)
	if err != nil {
		log.Error("解析第二个清单文件失败: %v", err)
		return fmt.Errorf("failed to parse second manifest: %w", err)
	}

	// 比较清单
	log.Debug("开始比较清单项目")
	diffs := compareManifests(manifest1, manifest2, opts, log)

	// 结果 payload 必须写 stdout（log 只保留诊断信息）
	if len(diffs) == 0 {
		readCmdResultLine("清单文件之间没有发现差异")
	} else {
		readCmdResultLine("发现 %d 处差异", len(diffs))
		for _, diff := range diffs {
			readCmdResultLine("%s", diff)
		}
	}

	return nil
}

// diffRevisionResolver 把一个 manifest 项目的 revision 解析为提交 id。
// 返回 ("", false) 表示 revision 不可解析（对齐上游 unreachable 语义）。
type diffRevisionResolver func(p manifest.Project) (commitID string, ok bool)

// diffRevisionResolution 是解析 revision 的完整结果：Resolved=false 且
// Missing=false 表示不可解析（unreachable）；Missing=true 表示项目未检出，
// 必须按 missing 处理而不是 unreachable。
type diffRevisionResolution struct {
	CommitID string
	Resolved bool
	Missing  bool
}

// diffRevisionStatusResolver 是 compareManifests 使用的可注入解析器。
type diffRevisionStatusResolver func(p manifest.Project) diffRevisionResolution

// diffDefaultRevisionStatusResolver 返回默认解析器：先确认项目已检出（否则
// 记 missing），再按 readCmdRevisionCandidates 的顺序解析 revision——SHA 原样，
// refs/tags/* 当 tag，分支名与 refs/heads/X 先 refs/remotes/<remote>/X，最后
// 回退 refs/heads/X。remotes 用于把清单里的 remote name 映射为实际 git remote
// 名（声明了 alias 的 remote 以 alias 作为本地 remote 名）。
func diffDefaultRevisionStatusResolver(defaultRemote string, remotes []manifest.Remote) diffRevisionStatusResolver {
	runner := git.NewRunner()
	return func(p manifest.Project) diffRevisionResolution {
		dir := pathOrName(p)
		if dir == "" {
			return diffRevisionResolution{}
		}
		// 项目未检出：记 missing（raw 不输出），不得报 unreachable。
		if info, err := os.Stat(dir); err != nil || !info.IsDir() {
			return diffRevisionResolution{Missing: true}
		}
		remote := strings.TrimSpace(p.Remote)
		if remote == "" {
			remote = strings.TrimSpace(defaultRemote)
		}
		remote = readCmdRemoteRefName(remote, remotes)
		candidates := readCmdRevisionCandidates(p.Revision, remote, true)
		for _, candidate := range candidates {
			out, err := runner.RunInDir(dir, "rev-parse", "--verify", candidate+"^{commit}")
			if err != nil {
				continue
			}
			if id := strings.TrimSpace(string(out)); id != "" {
				return diffRevisionResolution{CommitID: id, Resolved: true}
			}
		}
		return diffRevisionResolution{}
	}
}

// diffDefaultRevisionResolver 返回 (string, bool) 形式的默认解析器，
// 保持既有调用面；missing 信息由 diffDefaultRevisionStatusResolver 提供。
func diffDefaultRevisionResolver(defaultRemote string, remotes []manifest.Remote) diffRevisionResolver {
	status := diffDefaultRevisionStatusResolver(defaultRemote, remotes)
	return func(p manifest.Project) (string, bool) {
		res := status(p)
		return res.CommitID, res.Resolved
	}
}

// pathOrName 返回项目的归一化键：优先 path，为空时回退 name，并去掉尾部的
// "/"（清单中 "foo" 与 "foo/" 指向同一路径，不能因此变成 removed + added）。
// diffmanifests 以 path 作为项目键：无显式 path 的项目 path 即 name，因此
// 改名或改路径都会表现为 removed + added（对齐上游 self.paths 以 relpath 为键）。
func pathOrName(p manifest.Project) string {
	key := p.Path
	if strings.TrimSpace(key) == "" {
		key = p.Name
	}
	return strings.TrimRight(strings.TrimSpace(key), "/")
}

// diffManifestsRevisionSelected 判断是否需要比较 revision：显式 --revision
// 或 --all；未指定任何属性 flag 时默认比较 revision（解析为提交 id）。
func diffManifestsRevisionSelected(opts *DiffManifestsOptions) bool {
	if opts.All || opts.Revision {
		return true
	}
	return !opts.Name && !opts.Path && !opts.Groups
}

// diffManifestsGroupsSelected 判断是否需要比较 groups。
func diffManifestsGroupsSelected(opts *DiffManifestsOptions) bool {
	return opts.All || opts.Groups
}

// diffManifestsNameSelected 判断是否需要比较项目名。
func diffManifestsNameSelected(opts *DiffManifestsOptions) bool {
	return opts.All || opts.Name
}

// compareManifests 比较两个清单对象并返回差异列
func compareManifests(manifest1, manifest2 *manifest.Manifest, opts *DiffManifestsOptions, log logger.Logger) []string {
	resolveFrom := diffDefaultRevisionStatusResolver(manifest1.Default.Remote, manifest1.Remotes)
	resolveTo := diffDefaultRevisionStatusResolver(manifest2.Default.Remote, manifest2.Remotes)
	return compareManifestsWithStatusResolvers(manifest1, manifest2, opts, log, resolveFrom, resolveTo)
}

// compareManifestsWithResolvers 以 path 为键比较两个清单。默认比较 revision
// （解析后的提交 id），--groups/--name/--all 追加对应属性比较。resolver 可注入，
// 便于测试在没有真实仓库时验证比较逻辑；该入口无法表达 missing，项目一律按
// 已检出处理（需要 missing 语义时使用 compareManifestsWithStatusResolvers）。
func compareManifestsWithResolvers(manifest1, manifest2 *manifest.Manifest, opts *DiffManifestsOptions, log logger.Logger, resolveFrom, resolveTo diffRevisionResolver) []string {
	wrap := func(resolve diffRevisionResolver) diffRevisionStatusResolver {
		return func(p manifest.Project) diffRevisionResolution {
			id, ok := resolve(p)
			return diffRevisionResolution{CommitID: id, Resolved: ok}
		}
	}
	return compareManifestsWithStatusResolvers(manifest1, manifest2, opts, log, wrap(resolveFrom), wrap(resolveTo))
}

// compareManifestsWithStatusResolvers 是带 missing 语义的比较实现：以 path 为
// 键，revision 字符串相同直接判等；项目未检出记 missing（raw 不输出），且只
// 短路 revision 比较，name/groups 仍继续比较；其余无法解析的 revision 记
// unreachable。
func compareManifestsWithStatusResolvers(manifest1, manifest2 *manifest.Manifest, opts *DiffManifestsOptions, log logger.Logger, resolveFrom, resolveTo diffRevisionStatusResolver) []string {
	log.Debug("准备比较清单，转换为项目映射")
	projects1 := make(map[string]manifest.Project, len(manifest1.Projects))
	for _, p := range manifest1.Projects {
		projects1[pathOrName(p)] = p
	}
	projects2 := make(map[string]manifest.Project, len(manifest2.Projects))
	for _, p := range manifest2.Projects {
		projects2[pathOrName(p)] = p
	}

	log.Debug("第一个清单包含 %d 个项目，第二个清单包含 %d 个项目", len(projects1), len(projects2))

	keys := make([]string, 0, len(projects1)+len(projects2))
	seen := make(map[string]bool, len(projects1)+len(projects2))
	for key := range projects1 {
		if !seen[key] {
			seen[key] = true
			keys = append(keys, key)
		}
	}
	for key := range projects2 {
		if !seen[key] {
			seen[key] = true
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)

	compareRevision := diffManifestsRevisionSelected(opts)
	compareGroups := diffManifestsGroupsSelected(opts)
	compareName := diffManifestsNameSelected(opts)

	diffs := make([]string, 0)
	for _, key := range keys {
		from, in1 := projects1[key]
		to, in2 := projects2[key]
		switch {
		case in1 && !in2:
			log.Debug("项目已移除 %s", key)
			if opts.Raw {
				diffs = append(diffs, fmt.Sprintf("R %s %s", key, from.Revision))
			} else {
				diffs = append(diffs, fmt.Sprintf("removed project: %s (revision %s)", key, from.Revision))
			}
			continue
		case !in1 && in2:
			log.Debug("项目已添加 %s", key)
			if opts.Raw {
				diffs = append(diffs, fmt.Sprintf("A %s %s", key, to.Revision))
			} else {
				diffs = append(diffs, fmt.Sprintf("added project: %s (revision %s)", key, to.Revision))
			}
			continue
		}

		// missing 只短路 revision 比较：项目未检出时无法解析提交，但 name /
		// groups 等清单属性仍可比较，不得整体跳过。raw 模式下 missing 不输出。
		if compareRevision {
			fromRes := resolveFrom(from)
			toRes := resolveTo(to)
			switch {
			case fromRes.Missing || toRes.Missing:
				log.Debug("项目 %s 未检出（missing），跳过 revision 比较", key)
				if !opts.Raw {
					diffs = append(diffs, fmt.Sprintf("missing project: %s (revision %s)", key, to.Revision))
				}
			case strings.TrimSpace(from.Revision) == strings.TrimSpace(to.Revision):
				// revision 字符串相同直接判等，不要求本地可解析。
			case !fromRes.Resolved || !toRes.Resolved:
				log.Debug("项目 %s revision 不可解析: %s / %s", key, from.Revision, to.Revision)
				if opts.Raw {
					diffs = append(diffs, fmt.Sprintf("U %s %s %s", key, from.Revision, to.Revision))
				} else {
					diffs = append(diffs, fmt.Sprintf("unreachable project: %s (revision %s or %s not found)", key, from.Revision, to.Revision))
				}
			case fromRes.CommitID != toRes.CommitID:
				log.Debug("项目 %s revision 已更改 %s -> %s", key, fromRes.CommitID, toRes.CommitID)
				if opts.Raw {
					diffs = append(diffs, fmt.Sprintf("C %s %s %s", key, from.Revision, to.Revision))
				} else {
					diffs = append(diffs, fmt.Sprintf("changed project: %s (revision %s -> %s)", key, from.Revision, to.Revision))
				}
			}
		}
		if compareName && from.Name != to.Name {
			log.Debug("项目 %s 名称已更改 %s -> %s", key, from.Name, to.Name)
			if opts.Raw {
				diffs = append(diffs, fmt.Sprintf("C %s %s %s", key, from.Revision, to.Revision))
			} else {
				diffs = append(diffs, fmt.Sprintf("changed project: %s (name %s -> %s)", key, from.Name, to.Name))
			}
		}
		if compareGroups && from.Groups != to.Groups {
			log.Debug("项目 %s 组已更改: %s -> %s", key, from.Groups, to.Groups)
			if opts.Raw {
				diffs = append(diffs, fmt.Sprintf("C %s %s %s", key, from.Revision, to.Revision))
			} else {
				diffs = append(diffs, fmt.Sprintf("changed project: %s (groups %s -> %s)", key, from.Groups, to.Groups))
			}
		}
	}

	log.Debug("比较完成，发现 %d 处差异", len(diffs))
	return diffs
}
