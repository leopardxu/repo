package commands

import (
	"fmt"
	"os"
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

	// 加载清单
	log.Debug("正在解析清单文件...")
	parser := manifest.NewParser()
	manifestObj, err := parser.ParseFromFile(cfg.ManifestName, manifest.SplitGroups(cfg.Groups))
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
	var groupsArg []string
	if opts.Groups != "" {
		groupsArg = manifest.SplitGroups(opts.Groups)
		log.Debug("按组过滤项目: %v", groupsArg)
	}

	if opts.Regex != "" {
		// -r：对全部项目按模式过滤（flag 值 + 全部位置参数均为模式，对齐上游
		// list.py "repo list -r str1 [str2...]" 与 command.py FindProjects：
		// 忽略大小写，匹配项目名或相对路径，任一模式命中即保留）
		all, aerr := manager.GetProjectsInGroups(groupsArg)
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
		projects, err = manager.GetProjectsInGroups(groupsArg)
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

	// --all：默认仅列出已 checkout（工作树存在）的项目；--all 列出全部（对齐上游 repo list）
	if !opts.AllProjects {
		filtered := make([]*project.Project, 0, len(projects))
		for _, p := range projects {
			if p.Worktree == "" {
				continue
			}
			if _, statErr := os.Stat(p.Worktree); statErr == nil {
				filtered = append(filtered, p)
			}
		}
		projects = filtered
	}

	stats := &listStats{}

	// 上游 repo list 对输出行排序后打印：按 path（默认输出首列）排序保证确定性
	sort.Slice(projects, func(i, j int) bool { return projects[i].Path < projects[j].Path })

	// 不并发以避免输出乱序
	for _, p := range projects {
		var output string
		path := p.Path
		if opts.RelativeTo != "" {
			relPath, err := filepath.Rel(opts.RelativeTo, p.Path)
			if err == nil {
				path = relPath
			} else {
				log.Debug("failed to compute relative path: %v", err)
			}
		}

		switch {
		case opts.Path:
			output = path
		case opts.URL:
			// --url 输出解析后的远程仓库 URL：相对 fetch 路径基于 manifest 服务器解析
			// （上游 GetRemoteUrl），为空时回退 remote 名称
			output = manager.ResolveRemoteURL(p.RemoteURL)
			if output == "" {
				output = p.RemoteName
			}
		case opts.FullName:
			output = fmt.Sprintf("%s : %s", p.Name, path)
		case opts.FullPath:
			absPath, err := filepath.Abs(p.Path)
			if err == nil {
				output = absPath
			} else {
				log.Debug("计算绝对路径failed: %v", err)
				output = p.Path
			}
		default:
			// 默认输出项目目录与名称，与原生git-repo保持一致
			output = fmt.Sprintf("%s : %s", path, p.Name)
		}

		fmt.Println(output)
		stats.success++
	}

	log.Debug("列出完成，共处理 %d 个项目", stats.success)

	return nil
}
