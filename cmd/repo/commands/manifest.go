package commands

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/leopardxu/repo-go/internal/config"
	"github.com/leopardxu/repo-go/internal/logger"
	"github.com/leopardxu/repo-go/internal/manifest"
	"github.com/leopardxu/repo-go/internal/project"
	"github.com/spf13/cobra"
)

type ManifestOptions struct {
	CommonManifestOptions
	ManifestName             string
	RevisionAsHEAD           bool
	OutputFile               string
	SuppressUpstreamRevision bool
	SuppressDestBranch       bool
	Snapshot                 bool
	NoCloneBundle            bool
	JSONOutput               bool
	Format                   string // --format: xml 或 json
	NoLocalManifests         bool
	Verbose                  bool
	Quiet                    bool
	Jobs                     int
}

// ManifestCmd 返回manifest命令
func ManifestCmd() *cobra.Command {
	opts := &ManifestOptions{}

	cmd := &cobra.Command{
		Use:   "manifest",
		Short: "Manifest inspection utility",
		Long:  `Manifest inspection utility to view or generate manifest files.`,
		RunE: func(_ *cobra.Command, args []string) error {
			return runManifest(opts, args)
		},
		Args: cobra.NoArgs,
	}

	// 添加命令行选项
	cmd.Flags().StringVarP(&opts.ManifestName, "manifest-name", "m", "", "temporary manifest to use for this sync")
	cmd.Flags().BoolVarP(&opts.RevisionAsHEAD, "revision-as-HEAD", "r", false, "save revisions as current HEAD")
	cmd.Flags().StringVarP(&opts.OutputFile, "output-file", "o", "", "file to save the manifest to. (Filename prefix for multi-tree.)")
	cmd.Flags().BoolVar(&opts.SuppressUpstreamRevision, "suppress-upstream-revision", false, "if in -r mode, do not write the upstream field (only of use if the branch names for a sha1 manifest are sensitive)")
	cmd.Flags().BoolVar(&opts.SuppressDestBranch, "suppress-dest-branch", false, "if in -r mode, do not write the dest-branch field (only of use if the branch names for a sha1 manifest are sensitive)")
	cmd.Flags().BoolVar(&opts.Snapshot, "snapshot", false, "create a manifest snapshot")
	cmd.Flags().BoolVar(&opts.Platform, "platform", false, "platform manifest")
	cmd.Flags().BoolVar(&opts.NoCloneBundle, "no-clone-bundle", false, "disable use of /clone.bundle on HTTP/HTTPS")
	cmd.Flags().BoolVar(&opts.JSONOutput, "json", false, "output manifest in JSON format (hidden alias of --format=json)")
	cmd.Flags().StringVar(&opts.Format, "format", "", "output format: xml or json (default: xml)")
	cmd.Flags().BoolVar(&opts.NoLocalManifests, "no-local-manifests", false, "ignore local manifests")
	cmd.Flags().BoolVarP(&opts.Verbose, "verbose", "v", false, "show all output")
	cmd.Flags().BoolVarP(&opts.Quiet, "quiet", "q", false, "only show errors")
	cmd.Flags().IntVarP(&opts.Jobs, "jobs", "j", 8, "number of jobs to run in parallel")
	AddManifestFlags(cmd, &opts.CommonManifestOptions)

	// --json 是上游已弃用的隐藏别名（上游 SUPPRESS_HELP），隐藏以免误导；
	// 该旗标刚在本函数注册，MarkHidden 不会失败，返回值安全丢弃
	_ = cmd.Flags().MarkHidden("json")

	return cmd
}

// runManifest 执行manifest命令
func runManifest(opts *ManifestOptions, _ []string) error {
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

	log.Debug("开始处理清单文件")

	// 加载配置
	log.Debug("正在加载配置...")
	cfg, err := config.Load()
	if err != nil {
		log.Error("failed to load config: %v", err)
		return fmt.Errorf("failed to load config: %w", err)
	}

	// 加载清单
	log.Debug("正在解析清单文件...")
	manifestName := opts.ManifestName
	if manifestName == "" {
		manifestName = cfg.ManifestName
	}
	parser := manifest.NewParser()
	if opts.NoLocalManifests {
		parser.SetUseLocalManifests(false)
	}
	manifestObj, err := parser.ParseFromFile(manifestName, manifest.SplitGroups(cfg.EffectiveGroups()))
	if err != nil {
		log.Error("failed to parse manifest file: %v", err)
		return fmt.Errorf("failed to parse manifest: %w", err)
	}

	log.Debug("清单文件解析成功，包含 %d 个项目", len(manifestObj.Projects))

	// 2.5 -r/--revision-as-HEAD 在非 snapshot 模式下也可用：把每个项目的 revision
	// 改为当前 HEAD 的真实 sha，并按 suppress 选项清空 upstream/dest-branch 字段。
	if opts.RevisionAsHEAD && !opts.Snapshot {
		log.Debug("将修订版本替换为当前 HEAD sha (-r)...")
		if err := applyRevisionAsHEAD(manifestObj, cfg, opts, log); err != nil {
			return fmt.Errorf("failed to apply revision-as-HEAD: %w", err)
		}
	}

	// 如果需要创建快照
	if opts.Snapshot {
		log.Debug("正在创建清单快照...")
		// 创建快照清单
		snapshotManifest, err := createSnapshotManifest(manifestObj, cfg, opts, log)
		if err != nil {
			log.Error("创建快照清单failed: %v", err)
			return fmt.Errorf("failed to create snapshot manifest: %w", err)
		}

		// 替换原始清单
		manifestObj = snapshotManifest
		log.Debug("清单快照创建成功")
	}

	// --platform：在根级 CustomAttrs 写入 platform="true"，由 ToXML 序列化输出。
	// 覆盖非 snapshot 路径（snapshot 路径在 createSnapshotManifest 内已设置）。
	if opts.Platform {
		if manifestObj.CustomAttrs == nil {
			manifestObj.CustomAttrs = map[string]string{}
		}
		manifestObj.CustomAttrs["platform"] = "true"
		log.Debug("已应用平台模式标记到清单根级属性")
	}

	// 统一解析输出格式：--format 优先，--json 是 --format=json 的隐藏别名。
	// 非法格式无论是否使用 -o 都立即报错（对齐上游 choices 语义）。
	format, err := resolveManifestFormat(opts)
	if err != nil {
		log.Error("%v", err)
		return err
	}

	// 解析器已将 include 内容按文档顺序并入 Projects；输出前清空 Includes，
	// 避免 ToXML/ToJSON 再次展开导致项目重复（写扁平清单）。
	manifestObj.Includes = nil

	// 输出到 stdout 时不得在 payload 之后写任何日志（payload 必须保持纯净），
	// 因此仅对文件输出记录成功信息。
	if opts.OutputFile != "" && opts.OutputFile != "-" {
		log.Debug("正在将清单写入文件: %s", opts.OutputFile)
	}

	if err := writeManifestOutput(manifestObj, format, opts.OutputFile, os.Stdout); err != nil {
		log.Error("输出清单failed: %v", err)
		return err
	}

	if opts.OutputFile != "" && opts.OutputFile != "-" {
		log.Info("清单已写入到文件: %s", opts.OutputFile)
	}

	return nil
}

// resolveManifestFormat 返回最终输出格式（"xml" 或 "json"）。
// --format 优先；为空时 --json 等价于 json，否则默认 xml。
func resolveManifestFormat(opts *ManifestOptions) (string, error) {
	format := opts.Format
	if format == "" {
		if opts.JSONOutput {
			format = "json"
		} else {
			format = "xml"
		}
	}
	if format != "xml" && format != "json" {
		return "", fmt.Errorf("invalid format: %s (must be xml or json)", format)
	}
	return format, nil
}

// writeManifestOutput 按 format 将清单输出到 outputFile。
// 输出文件为空或 "-" 时写 stdout（对齐上游 repo manifest -o - 语义，
// 绝不能创建名为 "-" 的文件）。
func writeManifestOutput(m *manifest.Manifest, format, outputFile string, stdout io.Writer) error {
	toStdout := outputFile == "" || outputFile == "-"

	var payload string
	if format == "json" {
		jsonData, err := m.ToJSON()
		if err != nil {
			return fmt.Errorf("failed to convert manifest to JSON: %w", err)
		}
		payload = jsonData + "\n"
	} else if format == "xml" {
		xml, err := m.ToXML()
		if err != nil {
			return fmt.Errorf("failed to convert manifest to XML: %w", err)
		}
		payload = xml
	} else {
		return fmt.Errorf("invalid format: %s (must be xml or json)", format)
	}

	if toStdout {
		if stdout == nil {
			stdout = os.Stdout
		}
		if _, err := io.WriteString(stdout, payload); err != nil {
			return fmt.Errorf("failed to write manifest to stdout: %w", err)
		}
		return nil
	}

	// 确保输出目录存在（避免 -o sub/dir/manifest.xml 失败）
	outputDir := filepath.Dir(outputFile)
	if outputDir != "" && outputDir != "." {
		if err := os.MkdirAll(outputDir, 0755); err != nil {
			return fmt.Errorf("failed to create output directory: %w", err)
		}
	}
	if err := os.WriteFile(outputFile, []byte(payload), 0644); err != nil {
		return fmt.Errorf("failed to write manifest to file: %w", err)
	}
	return nil
}

// createSnapshotManifest 创建快照清单。
// 快照按 name+path 定位每个项目：普通项目把 revision 锁定为工作树 HEAD sha，
// 镜像项目按清单 revision 在裸库解析；同时按上游语义生成 upstream/dest-branch。
// 任一项目缺失或解析失败都返回错误，绝不静默产出混合了旧 revision 的快照。
func createSnapshotManifest(m *manifest.Manifest, cfg *config.Config, opts *ManifestOptions, log logger.Logger) (*manifest.Manifest, error) {
	snapshotManifest := *m
	// Projects 按值复制（切片独立），并复制每个项目的自定义属性 map，
	// 避免快照过程原地修改调用方清单。
	snapshotManifest.Projects = make([]manifest.Project, len(m.Projects))
	copy(snapshotManifest.Projects, m.Projects)
	for i := range snapshotManifest.Projects {
		snapshotManifest.Projects[i].CustomAttrs = cloneManifestAttrs(snapshotManifest.Projects[i].CustomAttrs)
	}
	snapshotManifest.CustomAttrs = cloneManifestAttrs(m.CustomAttrs)

	log.Info("开始创建清单快照（%d 个项目）", len(snapshotManifest.Projects))

	// 复用 -r 的锁定逻辑：revision=HEAD sha，并生成 upstream/dest-branch。
	if err := applyRevisionAsHEAD(&snapshotManifest, cfg, opts, log); err != nil {
		log.Error("创建清单快照failed: %v", err)
		return nil, fmt.Errorf("failed to create snapshot manifest: %w", err)
	}

	if opts.Platform {
		if snapshotManifest.CustomAttrs == nil {
			snapshotManifest.CustomAttrs = map[string]string{}
		}
		snapshotManifest.CustomAttrs["platform"] = "true"
		log.Info("已应用平台模式到清单")
	}

	log.Info("清单快照创建完成: %d 个项目", len(snapshotManifest.Projects))
	return &snapshotManifest, nil
}

// cloneManifestAttrs 复制属性 map（nil 保持 nil）。
func cloneManifestAttrs(src map[string]string) map[string]string {
	if src == nil {
		return nil
	}
	dst := make(map[string]string, len(src))
	for k, v := range src {
		dst[k] = v
	}
	return dst
}

// applyRevisionAsHEAD 把清单中每个项目的 revision 替换为真实 sha：
// 普通项目取工作树当前 HEAD；镜像项目按清单 revision 在裸库解析（绝不使用 HEAD）。
// 默认并按上游语义生成 upstream=<生成时的 revision 表达式> 与
// dest-branch=<revision 表达式>（--suppress-* 时清空对应字段）。
// 用于 `repo manifest -r` 与 --snapshot。任一项目缺失或解析失败都返回错误。
func applyRevisionAsHEAD(m *manifest.Manifest, cfg *config.Config, opts *ManifestOptions, log logger.Logger) error {
	projectManager := project.NewManagerFromManifest(m, cfg)

	maxWorkers := opts.Jobs
	if maxWorkers <= 0 {
		maxWorkers = 8
	}
	sem := make(chan struct{}, maxWorkers)
	var wg sync.WaitGroup
	errs := make([]error, len(m.Projects))

	for i := range m.Projects {
		wg.Add(1)
		sem <- struct{}{}
		go func(idx int) {
			defer func() { <-sem; wg.Done() }()
			p := &m.Projects[idx]
			relPath := manifestProjectRelpath(p)

			// 必须按 name+path 定位：multi-tree 下同名项目可能对应多个工作树，
			// 只按 name 会全部取第一个工作树的 HEAD。
			proj, err := findManifestProject(projectManager, p, m.RepoDir)
			if err != nil {
				errs[idx] = err
				return
			}
			repoDir := manifestProjectRepoDir(proj)
			if repoDir == "" {
				errs[idx] = fmt.Errorf("project %q (path %q) has no worktree or gitdir", p.Name, relPath)
				return
			}

			// 普通项目锁定当前 HEAD；镜像项目没有工作树 HEAD 语义，
			// 必须按清单 revision（refs/tags/SHA/refs/remotes 等）在裸库解析。
			var commitHash string
			if proj.Mirror {
				// manifest 项目未显式写 revision 时继承 default，使用 manager
				// 解析后的有效 revision。
				revisionExpr := p.Revision
				if revisionExpr == "" {
					revisionExpr = proj.Revision
				}
				commitHash, err = manifestResolveMirrorRevision(proj, repoDir, revisionExpr)
				if err != nil {
					errs[idx] = fmt.Errorf("failed to resolve revision %q for mirror project %q (path %q): %w", revisionExpr, p.Name, relPath, err)
					return
				}
			} else {
				output, revErr := proj.GitRepo.Runner.RunInDir(repoDir, "rev-parse", "HEAD")
				if revErr != nil {
					errs[idx] = fmt.Errorf("failed to resolve HEAD for project %q (path %q): %w", p.Name, relPath, revErr)
					return
				}
				commitHash = strings.TrimSpace(string(output))
				if commitHash == "" {
					errs[idx] = fmt.Errorf("project %q (path %q) resolved an empty HEAD sha", p.Name, relPath)
					return
				}
			}

			// 生成快照时记录原 revision 表达式，作为 upstream/dest-branch 的默认值。
			origRevision := p.Revision
			p.Revision = commitHash

			if opts.SuppressUpstreamRevision {
				p.Upstream = ""
			} else if p.Upstream == "" && commitHash != origRevision {
				p.Upstream = origRevision
			}
			if opts.SuppressDestBranch {
				p.DestBranch = ""
			} else if p.DestBranch == "" && commitHash != origRevision {
				p.DestBranch = origRevision
			}
			if opts.NoCloneBundle {
				if p.CustomAttrs == nil {
					p.CustomAttrs = map[string]string{}
				}
				p.CustomAttrs["no-clone-bundle"] = "true"
			}

			log.Debug("项目 %s (path %s) 的 revision 已设为 %s", p.Name, relPath, commitHash)
		}(i)
	}
	wg.Wait()

	failures := make([]error, 0, len(errs))
	for _, err := range errs {
		if err != nil {
			failures = append(failures, err)
		}
	}
	if len(failures) > 0 {
		return fmt.Errorf("%d project(s) failed to lock to HEAD: %w", len(failures), errors.Join(failures...))
	}
	return nil
}

// manifestProjectRepoDir 返回读取项目 HEAD 的仓库目录：
// 普通项目使用工作树；镜像项目 Worktree 为空时使用 Gitdir（裸仓库）。
func manifestProjectRepoDir(p *project.Project) string {
	if p.Worktree != "" {
		return p.Worktree
	}
	if p.Gitdir != "" {
		return p.Gitdir
	}
	return p.Path
}

// manifestResolveMirrorRevision 在镜像裸库中把清单 revision 解析为提交 sha。
// 绝不回退 HEAD：revision 为空或无法解析时返回错误。
//
// 候选顺序兼顾 mirror clone（refs/heads/*）与标准 remote 布局（refs/remotes），
// 并支持 refs/tags/*、完整 ref 与裸 SHA。
func manifestResolveMirrorRevision(proj *project.Project, repoDir, revision string) (string, error) {
	rev := strings.TrimSpace(revision)
	if rev == "" {
		return "", fmt.Errorf("revision is empty")
	}
	for _, candidate := range manifestRevisionCandidates(proj, rev) {
		out, err := proj.GitRepo.Runner.RunInDir(repoDir, "rev-parse", "--verify", "--quiet", candidate+"^{commit}")
		if err != nil {
			continue
		}
		if sha := strings.TrimSpace(string(out)); sha != "" {
			return sha, nil
		}
	}
	return "", fmt.Errorf("revision %q not found in mirror repository", revision)
}

// manifestRevisionCandidates 生成镜像裸库中 revision 的可能 ref 形式。
func manifestRevisionCandidates(proj *project.Project, revision string) []string {
	if uploadIsCommitID(revision) {
		return []string{revision}
	}

	remote := "origin"
	if proj != nil && proj.RemoteName != "" {
		remote = proj.RemoteName
	}

	candidates := make([]string, 0, 4)
	switch {
	case strings.HasPrefix(revision, "refs/tags/"), strings.HasPrefix(revision, "refs/remotes/"):
		candidates = append(candidates, revision)
	case strings.HasPrefix(revision, "refs/heads/"):
		if remote != "" {
			candidates = append(candidates, "refs/remotes/"+remote+"/"+strings.TrimPrefix(revision, "refs/heads/"))
		}
		candidates = append(candidates, revision)
	case strings.HasPrefix(revision, "refs/"):
		candidates = append(candidates, revision)
	default:
		if remote != "" {
			if strings.HasPrefix(revision, remote+"/") {
				// 形如 origin/main 的短远端分支名。
				candidates = append(candidates, "refs/remotes/"+revision)
			}
			candidates = append(candidates, "refs/remotes/"+remote+"/"+revision)
		}
		candidates = append(candidates, "refs/heads/"+revision, "refs/tags/"+revision, revision)
	}
	return candidates
}

// manifestProjectRelpath 返回清单项目的工作区相对路径（空 path 回退 name）。
func manifestProjectRelpath(p *manifest.Project) string {
	if p.Path != "" {
		return p.Path
	}
	return p.Name
}

// findManifestProject 按 name+path 在项目管理器中定位清单项目自己的工作树。
// 同名多 path（multi-tree）必须按路径精确匹配；name 唯一时才允许退回该唯一项目。
func findManifestProject(mgr *project.Manager, mp *manifest.Project, repoDir string) (*project.Project, error) {
	relPath := filepath.Clean(manifestProjectRelpath(mp))
	wantWorktree := filepath.Clean(filepath.Join(repoDir, relPath))

	sameName := make([]*project.Project, 0, 1)
	for _, p := range mgr.Projects {
		if p.Name != mp.Name {
			continue
		}
		sameName = append(sameName, p)

		// 兼容两种布局：Path 可能是清单 relpath（新语义）或历史绝对/展开路径。
		// 镜像项目 Worktree 为空，此时用 Relpath 精确匹配同名多 path。
		if filepath.Clean(p.Relpath) == relPath ||
			filepath.Clean(p.Path) == relPath ||
			(p.Worktree != "" && filepath.Clean(p.Worktree) == wantWorktree) ||
			(p.Path != "" && filepath.Clean(p.Path) == wantWorktree) {
			return p, nil
		}
	}

	switch len(sameName) {
	case 0:
		return nil, fmt.Errorf("project %q (path %q) not found in working tree", mp.Name, relPath)
	case 1:
		// 镜像/自定义布局下工作树路径可能不同于清单 path；name 唯一时无歧义。
		return sameName[0], nil
	default:
		return nil, fmt.Errorf("project %q has %d worktrees but none matches path %q", mp.Name, len(sameName), relPath)
	}
}
