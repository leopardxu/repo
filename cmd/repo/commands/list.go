package commands

import (
	"fmt"
	"path/filepath"
	"regexp"
	"sort"

	"github.com/leopardxu/repo-go/internal/config"
	"github.com/leopardxu/repo-go/internal/logger"
	"github.com/leopardxu/repo-go/internal/manifest"
	"github.com/leopardxu/repo-go/internal/project"
	"github.com/spf13/cobra"
)

// ListOptions 包含list命令的选项
type ListOptions struct {
	Path        bool
	Name        bool
	URL         bool
	FullName    bool
	FullPath    bool
	Groups      string
	MissingOK   bool
	PathPrefix  string
	Regex       string
	RelativeTo  string
	AllProjects bool
	Verbose     bool
	Quiet       bool
	Config      *config.Config
	CommonManifestOptions
}

// listStats 用于统计list命令的执行结果
type listStats struct {
	success int
}

// ListCmd 返回list命令
func ListCmd() *cobra.Command {
	opts := &ListOptions{}

	cmd := &cobra.Command{
		Use:   "list [-f] [<project>...]",
		Short: "List projects and their associated directories",
		Long: `List all projects; pass '.' to list the project for the cwd.

By default, only projects that currently exist in the checkout are shown. If you
want to list all projects (using the specified filter settings), use the --all
option. If you want to show all projects regardless of the manifest groups, then
also pass --groups all.

This is similar to running: repo forall -c 'echo "$REPO_PATH : $REPO_PROJECT"'`,
		RunE: func(_ *cobra.Command, args []string) error {
			return runList(opts, args)
		},
	}

	// 添加命令行选项
	cmd.Flags().BoolVarP(&opts.Path, "path-only", "p", false, "display only the path of the repository")
	cmd.Flags().BoolVar(&opts.Path, "path", false, "alias for --path-only")
	cmd.Flags().BoolVarP(&opts.Name, "name-only", "n", false, "display only the name of the repository")
	cmd.Flags().BoolVar(&opts.Name, "name", false, "alias for --name-only")
	cmd.Flags().BoolVarP(&opts.URL, "url", "u", false, "display the fetch url instead of name")
	cmd.Flags().BoolVar(&opts.FullName, "full-name", false, "show project name and directory")
	cmd.Flags().BoolVarP(&opts.FullPath, "fullpath", "f", false, "display the full work tree path instead of the relative path")
	cmd.Flags().StringVarP(&opts.Groups, "groups", "g", "", "filter projects by groups")
	cmd.Flags().BoolVar(&opts.MissingOK, "missing-ok", false, "don't exit with an error if a project doesn't exist")
	cmd.Flags().StringVar(&opts.PathPrefix, "path-prefix", "", "limit to projects with path prefix")
	cmd.Flags().StringVarP(&opts.Regex, "regex", "r", "", "filter the project list based on regex or wildcard matching of strings")
	cmd.Flags().StringVar(&opts.RelativeTo, "relative-to", "", "display paths relative to this one (default: top of repo client checkout)")
	cmd.Flags().BoolVarP(&opts.AllProjects, "all", "a", false, "show projects regardless of checkout state")
	cmd.Flags().BoolVarP(&opts.Verbose, "verbose", "v", false, "show all output")
	cmd.Flags().BoolVarP(&opts.Quiet, "quiet", "q", false, "only show errors")
	AddManifestFlags(cmd, &opts.CommonManifestOptions)

	return cmd
}

// runList 执行list命令
func runList(opts *ListOptions, args []string) error {
	// -f/--fullpath 与 -n/--name-only 互斥（对齐上游 list.py）
	if opts.FullPath && opts.Name {
		return fmt.Errorf("cannot combine -f/--fullpath and -n/--name-only")
	}
	// 初始化日志记录器
	log := logger.NewDefaultLogger()
	if opts.Verbose {
		log.SetLevel(logger.LogLevelDebug)
	} else if opts.Quiet {
		log.SetLevel(logger.LogLevelError)
	} else {
		log.SetLevel(logger.LogLevelInfo)
	}

	log.Debug("开始列出项目")

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
	log.Debug("正在加载配置...")
	cfg, err := config.Load()
	if err != nil {
		log.Error("failed to load config: %v", err)
		return fmt.Errorf("failed to load config: %w", err)
	}
	opts.Config = cfg

	// 加载清单（显式 --groups 优先，否则 cfg.Groups；只解析一次）
	log.Debug("正在解析清单文件...")
	parser := manifest.NewParser()
	manifestObj, err := parser.ParseFromFile(cfg.ManifestName, readCmdEffectiveGroups(opts.Groups, cfg))
	if err != nil {
		log.Error("failed to parse manifest file: %v", err)
		return fmt.Errorf("failed to parse manifest: %w", err)
	}

	// 创建项目管理器
	log.Debug("正在创建项目管理器...")
	manager := project.NewManagerFromManifest(manifestObj, cfg)

	// 获取要处理的项目
	log.Debug("正在failed to get project列表...")
	var projects []*project.Project

	if opts.Regex != "" {
		// -r：对全部项目按模式过滤（flag 值 + 全部位置参数均为模式，对齐上游
		// list.py "repo list -r str1 [str2...]" 与 command.py FindProjects：
		// 忽略大小写，匹配项目名或相对路径，任一模式命中即保留）
		all, aerr := manager.GetProjectsInGroups(nil)
		if aerr != nil {
			log.Error("failed to get projectfailed: %v", aerr)
			return fmt.Errorf("failed to get projects: %w", aerr)
		}
		patterns := append([]string{opts.Regex}, args...)
		regexps := make([]*regexp.Regexp, 0, len(patterns))
		for _, pat := range patterns {
			re, cerr := regexp.Compile(`(?i)` + pat)
			if cerr != nil {
				log.Error("无效的正则表达式: %v", cerr)
				return fmt.Errorf("invalid regex pattern: %w", cerr)
			}
			regexps = append(regexps, re)
		}
		projects = make([]*project.Project, 0, len(all))
		for _, pr := range all {
			for _, re := range regexps {
				if re.MatchString(pr.Name) || re.MatchString(pr.Path) {
					projects = append(projects, pr)
					break
				}
			}
		}
		log.Debug("模式过滤后剩余 %d 个项目", len(projects))
	} else if len(args) == 0 {
		log.Debug("获取所有项目")
		projects, err = manager.GetProjectsInGroups(nil)
		if err != nil {
			log.Error("failed to get projectfailed: %v", err)
			return fmt.Errorf("failed to get projects: %w", err)
		}
	} else {
		// 位置参数：按上游 GetProjects 语义解析（名字 + 路径回溯，"." 表示当前项目；
		// --missing-ok 时跳过未命中的参数）
		projects = make([]*project.Project, 0, len(args))
		for _, arg := range args {
			matched, gerr := manager.GetProjectsByArgs([]string{arg}, originalDir)
			if gerr != nil {
				if opts.MissingOK {
					log.Warn("跳过未找到的项目: %v", gerr)
					continue
				}
				log.Error("failed to get projectfailed: %v", gerr)
				return fmt.Errorf("failed to get projects: %w", gerr)
			}
			projects = append(projects, matched...)
		}
	}

	log.Debug("找到 %d 个项目", len(projects))

	// --all：默认仅列出已初始化仓库的项目（普通项目工作树 / 镜像裸库）；
	// --all 列出全部（对齐上游 repo list）
	if !opts.AllProjects {
		filtered := make([]*project.Project, 0, len(projects))
		for _, p := range projects {
			if readCmdProjectExists(p) {
				filtered = append(filtered, p)
			}
		}
		projects = filtered
	}

	stats := &listStats{}

	// 上游 repo list 先收集输出行，再整体排序后打印，保证确定性
	lines := make([]string, 0, len(projects))
	for _, p := range projects {
		// 展示一律使用 Relpath：镜像项目的 Path 可能是绝对裸库路径，
		// 不得把 .git 目录暴露给用户（Path 已改为 relpath 后保持一致）。
		relPath := readCmdProjectRelpath(p)
		displayPath := relPath
		if opts.RelativeTo != "" && !opts.FullPath {
			// 项目绝对路径 = TopDir + 清单 Path，再与 relative-to 求相对路径。
			relTo, rerr := listRelativePath(manager.TopDir, opts.RelativeTo, relPath)
			if rerr == nil {
				displayPath = relTo
			} else {
				log.Debug("failed to compute relative path: %v", rerr)
			}
		}
		if opts.FullPath {
			displayPath = listProjectAbsPath(manager.TopDir, relPath)
		}

		// --url 输出解析后的远程仓库 URL：基址（相对 fetch 基于 manifest
		// 服务器解析）无条件拼接项目名，项目名不重复拼接。
		remoteURL := manager.ResolveProjectURL(p)
		if remoteURL == "" {
			remoteURL = p.RemoteName
		}

		lines = append(lines, listProjectOutput(p, displayPath, remoteURL, opts))
	}

	sort.Strings(lines)
	for _, line := range lines {
		fmt.Println(line)
	}
	stats.success = len(lines)

	log.Debug("列出完成，共处理 %d 个项目", stats.success)

	return nil
}

// listProjectOutput 按上游 list.py Execute 的优先级生成一行输出：
//   - -n 仅名称（与 -p 同用时仍输出 "path : name"）；
//   - -p 仅路径（与 -f/--relative-to 组合时使用对应展示路径）；
//   - 默认及 -p/-n 同用时输出 "path : name"。
func listProjectOutput(p *project.Project, displayPath, remoteURL string, opts *ListOptions) string {
	switch {
	case opts.Name && opts.Path:
		return fmt.Sprintf("%s : %s", displayPath, p.Name)
	case opts.Name:
		return p.Name
	case opts.Path:
		return displayPath
	case opts.URL:
		return remoteURL
	case opts.FullName:
		return fmt.Sprintf("%s : %s", p.Name, displayPath)
	default:
		// 默认输出项目目录与名称，与原生 git-repo 保持一致
		return fmt.Sprintf("%s : %s", displayPath, p.Name)
	}
}

// listProjectAbsPath 返回项目绝对路径：清单中的 Path 为相对路径时，以 repo 顶层
// 目录（TopDir）为基准拼接（对齐上游 worktree = topdir + relpath）。
func listProjectAbsPath(topDir, projectPath string) string {
	p := projectPath
	if !filepath.IsAbs(p) {
		p = filepath.Join(topDir, p)
	}
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	return filepath.Clean(p)
}

// listRelativePath 计算 --relative-to 的展示路径。relativeTo 允许相对或绝对路径，
// 统一切成绝对路径后与项目绝对路径求 Rel。
func listRelativePath(topDir, relativeTo, projectPath string) (string, error) {
	base := relativeTo
	if !filepath.IsAbs(base) {
		if abs, err := filepath.Abs(base); err == nil {
			base = abs
		}
	}
	return filepath.Rel(base, listProjectAbsPath(topDir, projectPath))
}
