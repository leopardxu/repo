package commands

import (
	"fmt"
	"os"
	"sort"
	"sync"

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

	manifest1Path := args[0]
	var manifest2Path string
	if len(args) == 2 {
		manifest2Path = args[1]
	} else {
		// 未指定第二个清单时，使用当前合并后的清单 .repo/manifest.xml（对齐上游 repo diffmanifests）
		manifest2Path = ".repo/manifest.xml"
	}
	// 校验清单文件存在：缺文件时报错而非静默回退默认清单（否则会出现"自己比自己"）
	for _, path := range []string{manifest1Path, manifest2Path} {
		if _, serr := os.Stat(path); serr != nil {
			log.Error("清单文件不存在: %s", path)
			return fmt.Errorf("manifest file not found: %s", path)
		}
	}

	log.Debug("正在比较清单文件 %s 和 %s", manifest1Path, manifest2Path)

	// 创建清单解析器
	parser := manifest.NewParser()

	// 解析第一个清单文件
	log.Debug("解析第一个清单文 %s", manifest1Path)
	manifest1, err := parser.ParseFromFile(manifest1Path, nil)
	if err != nil {
		log.Error("解析第一个清单文件失 %v", err)
		return fmt.Errorf("failed to parse first manifest: %w", err)
	}

	// 解析第二个清单文
	log.Debug("解析第二个清单文 %s", manifest2Path)
	manifest2, err := parser.ParseFromFile(manifest2Path, nil)
	if err != nil {
		log.Error("解析第二个清单文件失 %v", err)
		return fmt.Errorf("failed to parse second manifest: %w", err)
	}

	// 比较清单
	log.Debug("开始比较清单项目")
	diffs := compareManifests(manifest1, manifest2, opts, log)

	// 显示差异
	if len(diffs) == 0 {
		log.Info("清单文件之间没有发现差异")
	} else {
		log.Info("发现 %d 处差", len(diffs))
		for _, diff := range diffs {
			log.Info("%s", diff)
		}
	}

	return nil
}

// compareProjectsConcurrently 并发比较两个项目集合并返回差异列
func compareProjectsConcurrently(projects1, projects2 map[string]manifest.Project, opts *DiffManifestsOptions, log logger.Logger) []string {
	type diffResult struct {
		Diff string
	}

	var wg sync.WaitGroup
	results := make(chan diffResult, len(projects1)+len(projects2))
	maxConcurrency := 16 // 控制最大并发数
	sem := make(chan struct{}, maxConcurrency)
	diffs := []string{}

	log.Debug("开始并发比较 %d 个项目和 %d 个项目", len(projects1), len(projects2))

	// 检查项目1中存在但项目2中不存在的项目
	for name := range projects1 {
		wg.Add(1)
		sem <- struct{}{}
		go func(name string) {
			defer wg.Done()
			defer func() { <-sem }()
			if _, exists := projects2[name]; !exists {
				log.Debug("项目已移 %s", name)
				if opts.Raw {
					results <- diffResult{Diff: fmt.Sprintf("R %s %s", pathOrName(projects1[name]), projects1[name].Revision)}
				} else {
					results <- diffResult{Diff: fmt.Sprintf("Project removed: %s", name)}
				}
			}
		}(name)
	}

	// 检查项中存在但项目1中不存在的项目，或者比较两者的差异
	for name, p2 := range projects2 {
		wg.Add(1)
		sem <- struct{}{}
		go func(name string, p2 manifest.Project) {
			defer wg.Done()
			defer func() { <-sem }()
			p1, exists := projects1[name]
			if !exists {
				log.Debug("项目已添 %s", name)
				if opts.Raw {
					results <- diffResult{Diff: fmt.Sprintf("A %s %s", pathOrName(p2), p2.Revision)}
				} else {
					results <- diffResult{Diff: fmt.Sprintf("Project added: %s", name)}
				}
				return
			}

			// 比较项目属
			if (opts.Path || opts.All) && p1.Path != p2.Path {
				log.Debug("项目 %s 路径已更改 %s -> %s", name, p1.Path, p2.Path)
				if opts.Raw {
					results <- diffResult{Diff: fmt.Sprintf("C %s %s %s", pathOrName(p1), p1.Path, p2.Path)}
				} else {
					results <- diffResult{Diff: fmt.Sprintf("Project %s path: %s -> %s", name, p1.Path, p2.Path)}
				}
			}
			if (opts.Revision || opts.All) && p1.Revision != p2.Revision {
				log.Debug("项目 %s 版本已更改 %s -> %s", name, p1.Revision, p2.Revision)
				if opts.Raw {
					results <- diffResult{Diff: fmt.Sprintf("C %s %s %s", pathOrName(p1), p1.Revision, p2.Revision)}
				} else {
					results <- diffResult{Diff: fmt.Sprintf("Project %s revision: %s -> %s", name, p1.Revision, p2.Revision)}
				}
			}
			if (opts.Groups || opts.All) && p1.Groups != p2.Groups {
				log.Debug("项目 %s 组已更改: %s -> %s", name, p1.Groups, p2.Groups)
				if opts.Raw {
					results <- diffResult{Diff: fmt.Sprintf("C %s %s %s", pathOrName(p1), p1.Groups, p2.Groups)}
				} else {
					results <- diffResult{Diff: fmt.Sprintf("Project %s groups: %s -> %s", name, p1.Groups, p2.Groups)}
				}
			}
		}(name, p2)
	}

	// 等待所有比较完
	go func() {
		wg.Wait()
		close(results)
	}()

	// 收集结果
	for res := range results {
		if res.Diff != "" {
			diffs = append(diffs, res.Diff)
		}
	}

	// 排序保证输出顺序确定（并发比较导致结果顺序随机），便于脚本解析与可复现
	sort.Strings(diffs)

	log.Debug("比较完成，发现 %d 处差异", len(diffs))
	return diffs
}

// pathOrName 返回项目的 path，为空时回退到 name（raw diff 用 path 标识项目，对齐上游约定）。
func pathOrName(p manifest.Project) string {
	if p.Path != "" {
		return p.Path
	}
	return p.Name
}

// compareManifests 比较两个清单对象并返回差异列
func compareManifests(manifest1, manifest2 *manifest.Manifest, opts *DiffManifestsOptions, log logger.Logger) []string {
	log.Debug("准备比较清单，转换为项目映射")
	projects1 := make(map[string]manifest.Project)
	for _, p := range manifest1.Projects {
		projects1[p.Name] = p
	}

	projects2 := make(map[string]manifest.Project)
	for _, p := range manifest2.Projects {
		projects2[p.Name] = p
	}

	log.Debug("第一个清单包含 %d 个项目，第二个清单包含 %d 个项目", len(projects1), len(projects2))
	return compareProjectsConcurrently(projects1, projects2, opts, log)
}
