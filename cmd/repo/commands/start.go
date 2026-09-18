package commands

import (
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"

	"github.com/leopardxu/repo-go/internal/config"
	"github.com/leopardxu/repo-go/internal/git"
	"github.com/leopardxu/repo-go/internal/logger"
	"github.com/leopardxu/repo-go/internal/manifest"
	"github.com/leopardxu/repo-go/internal/progress"
	"github.com/leopardxu/repo-go/internal/project"
	"github.com/spf13/cobra"
)

// StartOptions 包含start命令的选项
type StartOptions struct {
	All              bool
	Rev              string
	Branch           string
	Jobs             int
	Verbose          bool
	Quiet            bool
	OuterManifest    bool
	NoOuterManifest  bool
	ThisManifestOnly bool
	HEAD             bool
	Config           *config.Config
	CommonManifestOptions
}

// startStats 用于统计分支创建结果
type startStats struct {
	mu      sync.Mutex
	total   int
	success int
	failed  int
}

// increment 增加统计计数
func (s *startStats) increment(success bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.total++
	if success {
		s.success++
	} else {
		s.failed++
	}
}

// StartCmd 返回start命令
func StartCmd() *cobra.Command {
	opts := &StartOptions{
		Jobs: runtime.NumCPU() * 2,
	}

	cmd := &cobra.Command{
		Use:   "start <branch_name> [<project>...]",
		Short: "Start a new branch for development",
		Long:  `Create a new branch for development based on the current manifest.`,
		Args:  cobra.MinimumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
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

			// 加载配置
			cfg, err := config.Load()
			if err != nil {
				log.Error("failed to load config: %v", err)
				return fmt.Errorf("failed to load config: %w", err)
			}
			opts.Config = cfg

			return runStart(opts, args, log)
		},
	}

	// 添加命令行选项
	cmd.Flags().BoolVar(&opts.All, "all", false, "start branch in all projects")
	cmd.Flags().StringVarP(&opts.Rev, "rev", "r", "", "start branch from the specified revision")
	cmd.Flags().StringVarP(&opts.Branch, "branch", "b", "", "specify an alternate branch name")
	cmd.Flags().IntVarP(&opts.Jobs, "jobs", "j", opts.Jobs, "number of jobs to run in parallel (default: based on number of CPU cores)")
	cmd.Flags().BoolVarP(&opts.Verbose, "verbose", "v", false, "show all output including debug logs")
	cmd.Flags().BoolVarP(&opts.Quiet, "quiet", "q", false, "only show errors")
	AddManifestFlags(cmd, &opts.CommonManifestOptions)
	cmd.Flags().BoolVar(&opts.HEAD, "HEAD", false, "abbreviation for --rev HEAD")

	return cmd
}

// runStart 执行start命令
func runStart(opts *StartOptions, args []string, log logger.Logger) error {
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

	// 创建统计对象
	stats := &startStats{}

	if opts.HEAD {
		opts.Rev = "HEAD"
	}

	// --branch/-b 与 --HEAD 是 repo-go 自造选项，上游 repo 无；仅保留兼容，告警用户
	if opts.Branch != "" {
		log.Warn("--branch/-b 为 repo-go 扩展选项（上游 repo 无），仅为兼容保留")
	}
	if opts.HEAD {
		log.Warn("--HEAD 为 repo-go 扩展选项（上游 repo 无），仅为兼容保留")
	}

	// 获取分支名称
	branchName := args[0]
	if opts.Branch != "" {
		branchName = opts.Branch
	}

	// 验证分支名格式（与原生git-repo保持一致）
	if err := git.CheckRefFormat(fmt.Sprintf("heads/%s", branchName)); err != nil {
		log.Error("分支名格式验证failed: %v", err)
		return fmt.Errorf("'%s' is not a valid branch name", branchName)
	}

	// failed to get project列表
	projectNames := args[1:]

	log.Debug("开始创建分支'%s'", branchName)

	// 加载清单
	log.Debug("正在加载清单文件: %s", opts.Config.ManifestName)
	parser := manifest.NewParser()
	manifest, err := parser.ParseFromFile(opts.Config.ManifestName, manifest.SplitGroups(opts.Config.Groups))
	if err != nil {
		log.Error("failed to parse manifest: %v", err)
		return fmt.Errorf("failed to parse manifest: %w", err)
	}
	log.Debug("成功加载清单，包含 %d 个项目", len(manifest.Projects))

	// 创建项目管理器
	log.Debug("正在初始化项目管理器...")
	manager := project.NewManagerFromManifest(manifest, opts.Config)

	// 获取要处理的项目
	var projects []*project.Project
	if opts.All {
		// --all：在所有项目中创建分支
		log.Debug("获取所有项目 (--all)")
		projects, err = manager.GetProjectsInGroups(nil)
		if err != nil {
			log.Error("failed to get projectfailed: %v", err)
			return fmt.Errorf("failed to get projects: %w", err)
		}
		log.Debug("共获取到 %d 个项目", len(projects))
	} else if len(projectNames) == 0 {
		// 未指定项目且未 --all：仅当前目录所在项目（对齐上游 repo start 的 "." 语义）。
		// 解析失败直接报错，绝不回退到全部项目--那会在全部仓库里批量建分支。
		log.Debug("未指定项目，解析当前目录所在项目")
		projects, err = manager.GetProjectsByArgs([]string{"."}, originalDir)
		if err != nil {
			log.Error("解析当前目录所在项目failed: %v", err)
			return fmt.Errorf("failed to get projects: %w", err)
		}
		log.Debug("共获取到 %d 个项目", len(projects))
	} else {
		// 指定了项目名或路径：按上游语义解析（名字精确匹配 + 路径回溯，"." 表示当前项目）
		log.Debug("解析项目参数: %v", projectNames)
		projects, err = manager.GetProjectsByArgs(projectNames, originalDir)
		if err != nil {
			log.Error("解析项目参数failed: %v", err)
			return fmt.Errorf("failed to get projects: %w", err)
		}
		log.Debug("共获取到 %d 个项目", len(projects))
	}

	// 使用goroutine池并发创建分支
	// 规范化并发数：-j <= 0 会导致 makechan panic，回退到默认 8（与其他命令一致）
	if opts.Jobs <= 0 {
		opts.Jobs = 8
	}
	log.Debug("开始创建分支，并行任务数 %d...", opts.Jobs)

	// 创建manifest项目名到项目的映射，用于访问upstream等信息
	type ManifestProject struct {
		Upstream   string
		DestBranch string
	}
	manifestProjectInfo := make(map[string]ManifestProject)
	for _, proj := range manifest.Projects {
		manifestProjectInfo[proj.Name] = ManifestProject{
			Upstream:   proj.Upstream,
			DestBranch: proj.DestBranch,
		}
	}

	// 创建进度显示器
	prog := progress.NewProgress(fmt.Sprintf("Starting %s", branchName), len(projects), opts.Quiet)

	var wg sync.WaitGroup
	errChan := make(chan error, len(projects))
	resultChan := make(chan string, len(projects))
	sem := make(chan struct{}, opts.Jobs) // 使用信号量控制并发数

	for _, p := range projects {
		p := p // 创建副本避免闭包问题
		wg.Add(1)
		sem <- struct{}{} // 获取信号

		go func() {
			defer wg.Done()
			defer func() { <-sem }() // 释放信号

			log.Debug("在项目 %s 中创建分支 '%s'...", p.Name, branchName)

			// 确定分支起点：--rev 优先（上游 --rev/--head 仅改变起点，不影响跟踪关系），
			// 否则从 manifest 修订版本开始（上游 repo start 从 manifest revision 起分支）
			startPoint := opts.Rev
			if startPoint == "" {
				startPoint = p.Revision
			}

			// 确定跟踪 merge 目标（branch_merge，对齐上游 start.py/project.py StartBranch）：
			// revisionExpr 不可变（SHA1/tag/changes）时用 dest_branch 或 default revision，
			// 否则跟踪 revisionExpr 本身；上游不使用项目的 upstream 属性。
			mergeBase := p.Revision
			projInfo, hasProjInfo := manifestProjectInfo[p.Name]
			if git.IsImmutable(p.Revision) {
				switch {
				case hasProjInfo && projInfo.DestBranch != "":
					mergeBase = projInfo.DestBranch
				case manifest.Default.Revision != "":
					mergeBase = manifest.Default.Revision
				}
			}
			if !strings.HasPrefix(mergeBase, "refs/") && !git.IsImmutable(mergeBase) {
				mergeBase = "refs/heads/" + mergeBase
			}

			log.Debug("项目 %s 分支起点: %s, 跟踪 merge: %s", p.Name, startPoint, mergeBase)

			// 创建分支。若分支已存在则切换到它（上游 repo start 重复运行会切到已存在分支而非failed）。
			existingOut, err := p.GitRepo.RunCommand("branch", "--list", branchName)
			if err != nil {
				log.Error("项目 %s 查询分支failed: %v", p.Name, err)
				errChan <- fmt.Errorf("project %s: %w", p.Name, err)
				stats.increment(false)
				prog.Update(p.Name + " - failed")
				return
			}
			if strings.TrimSpace(string(existingOut)) != "" {
				log.Debug("项目 %s 分支 '%s' 已存在，将切换到该分支", p.Name, branchName)
			} else if err := p.GitRepo.CreateBranch(branchName, startPoint); err != nil {
				log.Error("项目 %s 创建分支failed: %v", p.Name, err)
				errChan <- fmt.Errorf("project %s: %w", p.Name, err)
				stats.increment(false)
				prog.Update(p.Name + " - failed")
				return
			}

			// 切换到分支（对齐上游 repo start = git checkout -b；失败视为项目失败）
			if _, err := p.GitRepo.RunCommand("checkout", branchName); err != nil {
				log.Error("项目 %s 切换到分支 '%s' failed: %v", p.Name, branchName, err)
				errChan <- fmt.Errorf("project %s: checkout %s: %w", p.Name, branchName, err)
				stats.increment(false)
				prog.Update(p.Name + " - failed")
				return
			}

			// 设置跟踪关系（对齐上游 branch.Save()：直接写 config，不要求远程跟踪分支已存在）
			remoteName := p.RemoteName
			if remoteName == "" {
				remoteName = "origin"
			}
			if _, err := p.GitRepo.RunCommand("config", fmt.Sprintf("branch.%s.remote", branchName), remoteName); err != nil {
				log.Error("项目 %s 设置 branch.%s.remote failed: %v", p.Name, branchName, err)
				errChan <- fmt.Errorf("project %s: set tracking remote: %w", p.Name, err)
				stats.increment(false)
				prog.Update(p.Name + " - failed")
				return
			}
			if _, err := p.GitRepo.RunCommand("config", fmt.Sprintf("branch.%s.merge", branchName), mergeBase); err != nil {
				log.Error("项目 %s 设置 branch.%s.merge failed: %v", p.Name, branchName, err)
				errChan <- fmt.Errorf("project %s: set tracking merge: %w", p.Name, err)
				stats.increment(false)
				prog.Update(p.Name + " - failed")
				return
			}

			resultChan <- fmt.Sprintf("项目 %s: 分支 '%s' 创建成功", p.Name, branchName)
			stats.increment(true)
			prog.Update(p.Name)
			log.Debug("项目 %s 分支创建完成", p.Name)
		}()
	}

	// 启动一goroutine 来关闭结果通道
	go func() {
		wg.Wait()
		prog.Finish("") // 完成进度
		close(errChan)
		close(resultChan)
	}()

	// 处理错误
	var errs []error
	for err := range errChan {
		errs = append(errs, err)
	}

	// 输出结果
	for result := range resultChan {
		log.Info("%s", result)
	}

	// 显示统计信息
	log.Info("分支创建操作完成，总计: %d，成功 %d，failed %d", stats.total, stats.success, stats.failed)

	// 如果有错误，返回汇总错误
	if len(errs) > 0 {
		return fmt.Errorf("%d projects failed: %w", len(errs), errors.Join(errs...))
	}

	return nil
}
