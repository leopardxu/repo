package commands

import (
	"fmt"
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

// manifestStats 用于统计manifest命令的执行结果
type manifestStats struct {
	mu      sync.Mutex
	success int
	failed  int
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
	manifestObj, err := parser.ParseFromFile(manifestName, manifest.SplitGroups(cfg.Groups))
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

	// 如果指定了输出文
	if opts.OutputFile != "" {
		// 确保输出目录存在
		outputDir := filepath.Dir(opts.OutputFile)
		log.Debug("确保输出目录存在: %s", outputDir)
		if err := os.MkdirAll(outputDir, 0755); err != nil {
			log.Error("创建输出目录failed: %v", err)
			return fmt.Errorf("failed to create output directory: %w", err)
		}

		// 写入输出文件
		log.Debug("正在写入清单到文 %s", opts.OutputFile)
		if err := manifestObj.WriteToFile(opts.OutputFile); err != nil {
			log.Error("写入清单到文件失 %v", err)
			return fmt.Errorf("failed to write manifest to file: %w", err)
		}

		log.Info("清单已写入到文件: %s", opts.OutputFile)
	} else {
		// 否则，输出到标准输出
		log.Debug("正在准备输出清单到标准输出")
		// --format 优先；--json 是 --format=json 的隐藏别名
		format := opts.Format
		if format == "" {
			if opts.JSONOutput {
				format = "json"
			} else {
				format = "xml"
			}
		}
		// 非法格式值直接报错，而非静默回退 XML（对齐上游 "invalid format" 语义）
		if format != "xml" && format != "json" {
			log.Error("无效的输出格式: %s", format)
			return fmt.Errorf("invalid format: %s (must be xml or json)", format)
		}
		if format == "json" {
			jsonData, err := manifestObj.ToJSON()
			if err != nil {
				log.Error("转换清单到JSONfailed: %v", err)
				return fmt.Errorf("failed to convert manifest to JSON: %w", err)
			}
			fmt.Println(jsonData)
		} else {
			xml, err := manifestObj.ToXML()
			if err != nil {
				log.Error("转换清单到XMLfailed: %v", err)
				return fmt.Errorf("failed to convert manifest to XML: %w", err)
			}
			fmt.Println(xml)
		}
		log.Debug("清单输出完成")
	}

	return nil
}

// createSnapshotManifest 创建快照清单
func createSnapshotManifest(m *manifest.Manifest, cfg *config.Config, opts *ManifestOptions, log logger.Logger) (*manifest.Manifest, error) {
	// 创建快照清单的副本
	snapshotManifest := *m

	log.Info("开始创建清单快照")

	// 创建项目管理器
	log.Debug("正在创建项目管理器...")
	projectManager := project.NewManagerFromManifest(&snapshotManifest, cfg)

	// 并发处理项目更新
	type projectUpdate struct {
		index int
		proj  *project.Project
		err   error
	}

	// 设置并发控制
	maxWorkers := opts.Jobs
	if maxWorkers <= 0 {
		maxWorkers = 8
	}
	log.Debug("设置并发数为: %d", maxWorkers)

	// 创建统计对象
	stats := &manifestStats{}

	// 使用WaitGroup确保所有goroutine完成
	var wg sync.WaitGroup
	sem := make(chan struct{}, maxWorkers)
	results := make(chan projectUpdate, len(snapshotManifest.Projects))

	log.Info("开始处理 %d 个项目...", len(snapshotManifest.Projects))

	for i, p := range snapshotManifest.Projects {
		wg.Add(1)
		sem <- struct{}{}
		go func(idx int, projName string) {
			defer func() {
				<-sem
				wg.Done()
			}()
			update := projectUpdate{index: idx}

			// failed to get project对象
			log.Debug("正在failed to get project: %s", projName)
			update.proj = projectManager.GetProject(projName)
			if update.proj == nil {
				log.Warn("项目 %s 在工作区中未找到，跳过", projName)

				// 更新统计信息
				stats.mu.Lock()
				stats.failed++
				stats.mu.Unlock()

				results <- update
				return
			}

			// 获取当前HEAD提交哈希
			log.Debug("正在failed to get project %s 的HEAD提交哈希", projName)
			output, err := update.proj.GitRepo.Runner.RunInDir(update.proj.Path, "rev-parse", "HEAD")
			if err != nil {
				log.Warn("failed to get project %s 的HEAD提交哈希failed: %v", projName, err)
				update.err = err

				// 更新统计信息
				stats.mu.Lock()
				stats.failed++
				stats.mu.Unlock()

				results <- update
				return
			}

			// 获取提交哈希（去除末尾的换行符）
			commitHash := strings.TrimSpace(string(output))
			log.Debug("项目 %s 的HEAD提交哈希: %s", projName, commitHash)

			// 根据选项更新修订版本
			if opts.RevisionAsHEAD {
				// 写入真实 sha（而非字面量 "HEAD"），对齐上游 repo manifest -r
				log.Debug("将项目 %s 的修订版本设置为 HEAD sha: %s", projName, commitHash)
				snapshotManifest.Projects[update.index].Revision = commitHash
			} else {
				log.Debug("将项目 %s 的修订版本设置为提交哈希: %s", projName, commitHash)
				snapshotManifest.Projects[update.index].Revision = commitHash
			}

			// 处理 SuppressUpstreamRevision：清空结构体字段（而非 CustomAttrs）
			if opts.SuppressUpstreamRevision {
				if snapshotManifest.Projects[update.index].Upstream != "" {
					log.Debug("从项目 %s 中清空 upstream 字段", projName)
					snapshotManifest.Projects[update.index].Upstream = ""
				}
			}

			// 处理 SuppressDestBranch：清空结构体字段
			if opts.SuppressDestBranch {
				if snapshotManifest.Projects[update.index].DestBranch != "" {
					log.Debug("从项目 %s 中清空 dest-branch 字段", projName)
					snapshotManifest.Projects[update.index].DestBranch = ""
				}
			}

			// 处理NoCloneBundle选项
			if opts.NoCloneBundle {
				// 添加no-clone-bundle属性
				snapshotManifest.Projects[update.index].CustomAttrs["no-clone-bundle"] = "true"
				log.Debug("为项目 %s 添加no-clone-bundle属性", projName)
			}

			log.Info("已更新项目 %s 的修订版本为 %s", projName, snapshotManifest.Projects[update.index].Revision)

			// 更新统计信息
			stats.mu.Lock()
			stats.success++
			stats.mu.Unlock()

			results <- update
		}(i, p.Name)
	}

	// 等待所有goroutine完成
	log.Debug("等待所有项目处理完成...")
	wg.Wait()
	close(results)

	// 处理Platform选项
	if opts.Platform {
		// 在平台模式下，可能需要添加一些特定的属性或修改
		snapshotManifest.CustomAttrs["platform"] = "true"
		log.Info("已应用平台模式到清单")
	}

	// 输出统计信息
	log.Info("清单快照创建完成: %d 个项目成功, %d 个项目failed", stats.success, stats.failed)

	return &snapshotManifest, nil
}

// applyRevisionAsHEAD 把清单中每个项目的 revision 替换为该工作树当前 HEAD 的真实 sha，
// 并按 --suppress-upstream-revision / --suppress-dest-branch 清空对应字段。
// 用于 `repo manifest -r`（非 snapshot）路径。直接修改传入的清单。
func applyRevisionAsHEAD(m *manifest.Manifest, cfg *config.Config, opts *ManifestOptions, log logger.Logger) error {
	projectManager := project.NewManagerFromManifest(m, cfg)

	maxWorkers := opts.Jobs
	if maxWorkers <= 0 {
		maxWorkers = 8
	}
	sem := make(chan struct{}, maxWorkers)
	var wg sync.WaitGroup
	var mu sync.Mutex
	failed := 0

	for i := range m.Projects {
		wg.Add(1)
		sem <- struct{}{}
		go func(idx int) {
			defer func() { <-sem; wg.Done() }()
			p := &m.Projects[idx]

			proj := projectManager.GetProject(p.Name)
			if proj == nil {
				log.Warn("项目 %s 在工作区中未找到，跳过 -r", p.Name)
				mu.Lock()
				failed++
				mu.Unlock()
				return
			}

			output, err := proj.GitRepo.Runner.RunInDir(proj.Path, "rev-parse", "HEAD")
			if err != nil {
				log.Warn("failed to get project %s 的 HEAD sha failed: %v", p.Name, err)
				mu.Lock()
				failed++
				mu.Unlock()
				return
			}
			commitHash := strings.TrimSpace(string(output))
			log.Debug("项目 %s 的 HEAD sha: %s", p.Name, commitHash)

			p.Revision = commitHash
			if opts.SuppressUpstreamRevision {
				p.Upstream = ""
			}
			if opts.SuppressDestBranch {
				p.DestBranch = ""
			}
		}(i)
	}
	wg.Wait()

	if failed > 0 {
		log.Warn("-r 模式: %d 个项目未能获取 HEAD sha，其 revision 保持不变", failed)
	}
	return nil
}
