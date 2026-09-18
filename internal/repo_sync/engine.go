package repo_sync

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/leopardxu/repo-go/internal/config"
	"github.com/leopardxu/repo-go/internal/git"
	"github.com/leopardxu/repo-go/internal/logger"
	"github.com/leopardxu/repo-go/internal/manifest"
	"github.com/leopardxu/repo-go/internal/progress"
	"github.com/leopardxu/repo-go/internal/project"
	"github.com/leopardxu/repo-go/internal/workerpool"
)

// SyncError 表示同步过程中的错误
type SyncError struct {
	ProjectName string
	Phase       string
	Err         error
	Output      string
	Timestamp   time.Time // 添加时间戳
	RetryCount  int       // 添加重试计数
}

// Error 实现 error 接口
func (e *SyncError) Error() string {
	timeStr := e.Timestamp.Format("2006-01-02 15:04:05")
	retryInfo := ""
	if e.RetryCount > 0 {
		retryInfo = fmt.Sprintf(" (重试次数: %d)", e.RetryCount)
	}

	if e.Output != "" {
		return fmt.Sprintf("[%s] %s 在%s 阶段failed%s: %v\n%s",
			timeStr, e.ProjectName, e.Phase, retryInfo, e.Err, e.Output)
	}
	return fmt.Sprintf("[%s] %s 在%s 阶段failed%s: %v",
		timeStr, e.ProjectName, e.Phase, retryInfo, e.Err)
}

// Unwrap 返回底层错误，支持 errors.Is/errors.As 穿透
func (e *SyncError) Unwrap() error {
	return e.Err
}

// NewMultiError 创建包含多个错误的错误对象
// 使用 errors.Join 保留所有错误详情
func NewMultiError(errs []error) error {
	if len(errs) == 0 {
		return nil
	}
	if len(errs) == 1 {
		return errs[0]
	}
	return errors.Join(errs...)
}

// Options 包含同步引擎的选
// Options moved to options.go to avoid duplicate declarations

// Engine 同步引擎
type Engine struct {
	projects        []*project.Project
	config          *config.Config
	options         *Options
	logger          logger.Logger
	progressReport  progress.Reporter
	workerPool      *workerpool.WorkerPool
	repoRoot        string
	errors          []error
	errorsMu        sync.Mutex
	manifestCache   []byte
	manifest        *manifest.Manifest
	ctx             context.Context  // 添加 ctx 字段
	branchName      string           // 要检出的分支名称
	checkoutStats   *checkoutStats   // 检出操作的统计信息（repo checkout 命令使用）
	commitHash      string           // 要cherry-pick的提交哈希
	cherryPickStats *cherryPickStats // cherry-pick操作的统计信息
	networkSem      chan struct{}    // 限制并发网络操作数（fetch/clone）
	checkoutSem     chan struct{}    // 限制并发检出操作数
	gitRunner       git.Runner       // 统一的 Git 命令执行器（替代直接 exec.Command）
}

// NewEngine 创建同步引擎
func NewEngine(options *Options, manifest *manifest.Manifest, log logger.Logger) *Engine {
	return NewEngineWithContext(context.Background(), options, manifest, log)
}

// NewEngineWithContext 创建支持 context 取消的同步引擎
func NewEngineWithContext(ctx context.Context, options *Options, manifest *manifest.Manifest, log logger.Logger) *Engine {
	if options.Jobs <= 0 {
		options.Jobs = runtime.NumCPU()
	}

	// 并发限制：网络操作与检出操作分别由独立信号量控制
	jobsNetwork := options.JobsNetwork
	if jobsNetwork <= 0 {
		jobsNetwork = options.Jobs
	}
	jobsCheckout := options.JobsCheckout
	if jobsCheckout <= 0 {
		jobsCheckout = options.Jobs
	}

	var progressReport progress.Reporter
	if !options.Quiet {
		progressReport = progress.NewConsoleReporter()
	}

	// 初始化目列表
	var projects []*project.Project
	// 目列表将在后续操作中填充

	// 获取仓库根目录
	var repoRoot string
	if manifest != nil && manifest.Topdir != "" {
		repoRoot = manifest.Topdir
	} else {
		// 如果manifest.Topdir为空，尝试从当前working directory推断
		cwd, err := os.Getwd()
		if err == nil {
			// 查找顶层仓库目录
			topDir := project.FindTopLevelRepoDir(cwd)
			if topDir != "" {
				repoRoot = topDir
			} else {
				repoRoot = cwd // 如果找不到顶层目录，使用当前目录
			}
		}
	}

	// 统一的 Git 命令执行器：
	// - 关闭 Runner 内部重试：重试统一由引擎层负责（fetch 走 RetryWithBackoff，
	//   clone/checkout 走各自的内联重试循环），否则两层叠加最坏会执行 4x4=16 次
	//   同一命令并累计约 38s 的固定睡眠
	// - 并发度对齐 worker 数：Runner 内部信号量固定为 5，若低于 --jobs 会成为
	//   隐形瓶颈，使 --jobs/--jobs-network/--jobs-checkout 大于 5 时失效；
	//   实际网络/检出并发仍由 networkSem/checkoutSem 精确控制
	gitRunner := git.NewRunner()
	gitRunner.SetMaxRetries(0)
	gitRunner.SetConcurrency(options.Jobs)

	return &Engine{
		projects:       projects,
		config:         options.Config, // 透传配置，供 smart-sync 重载清单等使用
		options:        options,
		manifest:       manifest,
		logger:         log,
		progressReport: progressReport,
		workerPool:     workerpool.New(options.Jobs),
		repoRoot:       repoRoot, // 设置仓库根目录
		ctx:            ctx,      // 使用传入的 context
		networkSem:     make(chan struct{}, jobsNetwork),
		checkoutSem:    make(chan struct{}, jobsCheckout),
		gitRunner:      gitRunner,
	}
}

// Sync 执行同步
func (e *Engine) Sync() error {
	// 创建带取消功能的上下文，以引擎的 ctx 作为父上下文
	// 这样外部取消可以正确传播到同步流程中
	parentCtx := e.ctx
	if parentCtx == nil {
		parentCtx = context.Background()
	}
	ctx, cancel := context.WithCancel(parentCtx)
	defer cancel() // 确保函数退出时取消上下文

	// 1.2 预处理：超级目 / 智能同步 / HyperSync
	if err := e.preSync(ctx); err != nil {
		return err
	}

	// HyperSync 可能将同步范围缩减为已变更目的子集
	syncProjects := e.projects
	if e.options.HyperSync {
		hyper, err := e.getHyperSyncProjects()
		if err != nil {
			e.logger.Warn("Failed to get HyperSync projects，将同步所有目: %v", err)
		} else if len(hyper) == 0 {
			e.logger.Info("HyperSync: No projects need updating")
			return nil
		} else {
			syncProjects = hyper
		}
	}

	totalProjects := len(syncProjects)
	if totalProjects == 0 {
		e.logger.Info("No projects to sync")
		return nil
	}

	// 记录开始时间，用于计算预估完成时间
	startTime := time.Now()

	if !e.options.Quiet {
		e.logger.Info("Syncing %d projects，并发数 %d", totalProjects, e.options.Jobs)
		if e.progressReport != nil {
			e.progressReport.Start(totalProjects)
		}
	}

	var count int32
	var successCount int32
	var failCount int32

	// 提交同步任务
	for _, p := range syncProjects {
		project := p // 创建副本避免闭包问题
		e.workerPool.Submit(func() (interface{}, error) {
			// 检查上下文是否已取消
			select {
			case <-ctx.Done():
				return nil, ctx.Err() // 如果上下文已取消，则不执行任务
			default:
				// 继续执行
			}

			err := e.syncProject(ctx, project)

			current := atomic.AddInt32(&count, 1)
			if err != nil {
				atomic.AddInt32(&failCount, 1)
			} else {
				atomic.AddInt32(&successCount, 1)
			}

			if !e.options.Quiet && e.progressReport != nil {
				status := "完成"
				if err != nil {
					status = "failed"
				}

				// 计算预估完成时间
				var etaStr string
				if current > 0 && current < int32(totalProjects) {
					elapsed := time.Since(startTime)
					estimatedTotal := elapsed * time.Duration(totalProjects) / time.Duration(current)
					estimatedRemaining := estimatedTotal - elapsed
					if estimatedRemaining > 0 {
						etaStr = fmt.Sprintf("，预计剩余时 %s", formatDuration(estimatedRemaining))
					}
				}

				progressMsg := fmt.Sprintf("%s: %s (进度: %d/%d, 成功: %d, failed: %d%s)",
					project.Name, status, current, totalProjects,
					successCount, failCount, etaStr)
				e.progressReport.Update(int(current), progressMsg)
			}

			if err != nil {
				e.errorsMu.Lock()
				e.errors = append(e.errors, err)
				e.errorsMu.Unlock()
				// --fail-fast：首个错误立即 cancel 派生 ctx，既阻止排队任务启动，
				// 也中断在途项目的重试退避等待（重试循环监听同一 ctx）
				if e.options.FailFast {
					cancel()
				}
			} else if !e.options.Quiet {
				e.logger.Debug("project %s sync complete", project.Name)
			}
			return nil, nil
		})
	}

	// 等待所有任务完
	e.workerPool.Wait()

	if !e.options.Quiet && e.progressReport != nil {
		e.progressReport.Finish()
	}

	// 1.8 --auto-gc：Sync complete后对每个目运行 git gc --auto
	if e.options.AutoGC {
		e.runAutoGC(syncProjects)
	}

	// 计算总耗时
	totalDuration := time.Since(startTime)

	// 汇总错
	// fail-fast 或外部取消发生后，在途项目会被 ctx 打断并返回 context.Canceled，
	// 这类错误不是项目自身的同步失败，不应计入最终汇总；
	// 若过滤后为空（纯取消、无真实失败），保留原始错误，避免把被中断的同步误报为成功。
	syncErrors := e.errors
	if ctx.Err() != nil {
		filtered := make([]error, 0, len(e.errors))
		for _, err := range e.errors {
			if errors.Is(err, context.Canceled) {
				continue
			}
			filtered = append(filtered, err)
		}
		if len(filtered) > 0 {
			syncErrors = filtered
		}
	}

	if len(syncErrors) > 0 {
		e.logger.Error("Sync complete，有 %d 个目failed，总耗时: %s",
			len(syncErrors), formatDuration(totalDuration))
		return NewMultiError(syncErrors)
	}

	e.logger.Info("All projects synced，总耗时: %s", formatDuration(totalDuration))
	return nil
}

// preSync 执行同步前的可选预处理：超级目修订 ID 与智能同步清单重载。
// 任一特性仅在对应选启用时触发；未配置 manifest-server/superproject 时给出告警而非中断。
func (e *Engine) preSync(_ context.Context) error {
	// --use-superproject：用超级目覆盖各目的修订 ID
	if e.options.UseSuperproject {
		if _, err := e.updateProjectsRevisionID(); err != nil {
			return fmt.Errorf("use-superproject failed: %w", err)
		}
	}

	// --smart-sync / --smart-tag：从 manifest-server 获取已审批清单并重载
	if e.options.SmartSync || e.options.SmartTag != "" {
		if err := e.handleSmartSync(); err != nil {
			return fmt.Errorf("smart-sync failed: %w", err)
		}
		// handleSmartSync 重载了清单并刷新了 e.projects
	}

	// --force-remove-dirty 当前尚未在活动同步路径中实现（需带安全护栏的工作树移除，
	// 与 prune 同级风险），此处显式告警而非静默忽略。
	if e.options.ForceRemoveDirty {
		e.logger.Warn("--force-remove-dirty 尚未在同步路径实现，本次将按常规方式处理脏目")
	}
	return nil
}

// runAutoGC 对每个目执行 git gc --auto（仅在 --auto-gc 启用时调用）。
func (e *Engine) runAutoGC(projects []*project.Project) {
	for _, p := range projects {
		if _, err := p.GitRepo.RunCommand("gc", "--auto"); err != nil {
			e.logger.Debug("目 %s 的 git gc --auto failed: %v", p.Name, err)
		}
	}
}

// UpdateManifestRepo 更新 manifest 仓库并重新生成 .repo/manifest.xml
func (e *Engine) UpdateManifestRepo() error {
	if e.options.NoManifestUpdate {
		e.logger.Debug("跳过 manifest 仓库更新，因为设置了 --no-manifest-update 选")
		return nil
	}

	manifestProjectPath := filepath.Join(e.repoRoot, ".repo", "manifests")
	e.logger.Info("正在更新 manifest 仓库: %s", manifestProjectPath)

	// 检查 manifest 仓库是否存在
	if _, err := os.Stat(manifestProjectPath); os.IsNotExist(err) {
		e.logger.Warn("manifest 仓库不存在: %s", manifestProjectPath)
		return nil
	}

	// 使用 git -C 参数在指定目录执行命令，避免 os.Chdir 的线程安全问题
	gitRunner := git.NewRunner()

	// 获取更新前的 HEAD 提交哈希以检查是否有更新
	oldHeadOutput, err := gitRunner.RunInDir(manifestProjectPath, "rev-parse", "HEAD")
	oldHead := strings.TrimSpace(string(oldHeadOutput))
	if err != nil {
		e.logger.Debug("获取当前 HEAD failed: %v", err)
	}

	// 执行 fetch 操作
	e.logger.Debug("Fetching manifest repository updates...")
	_, err = gitRunner.RunInDir(manifestProjectPath, "fetch", "origin")
	if err != nil {
		e.logger.Debug("fetch origin failed: %v, trying fetch --all", err)
		// 尝试获取所有远程分支
		_, fetchErr := gitRunner.RunInDir(manifestProjectPath, "fetch", "--all")
		if fetchErr != nil {
			e.logger.Debug("fetch --all also failed: %v", fetchErr)
		}
	}

	// 确定要同步的目标引用（对齐上游 repo：使用 manifest_branch 配置，回退到当前分支）
	manifestBranch := ""
	if e.config != nil && e.config.ManifestBranch != "" {
		manifestBranch = e.config.ManifestBranch
	} else {
		// 回退：获取当前分支名
		branchOutput, branchErr := gitRunner.RunInDir(manifestProjectPath, "rev-parse", "--abbrev-ref", "HEAD")
		if branchErr != nil {
			return fmt.Errorf("failed to get current manifest branch: %w", branchErr)
		}
		manifestBranch = strings.TrimSpace(string(branchOutput))
	}

	// 对齐上游 repo：使用 git reset --hard 而非 merge，确保 manifest 仓库
	// 始终与远程一致，避免 merge conflict 导致同步卡住。
	// 上游 manifestProject.Sync_NetworkHalf + Sync_LocalHalf 的核心行为就是
	// fetch 后 reset --hard 到远程分支。
	remoteRef := "origin/" + manifestBranch
	e.logger.Debug("Resetting manifest to %s...", remoteRef)
	_, err = gitRunner.RunInDir(manifestProjectPath, "reset", "--hard", remoteRef)
	if err != nil {
		// 如果指定分支不存在，尝试不指定分支（使用当前跟踪的远程分支）
		e.logger.Debug("reset --hard %s failed, trying HEAD", remoteRef)
		_, err = gitRunner.RunInDir(manifestProjectPath, "reset", "--hard", "FETCH_HEAD")
		if err != nil {
			return fmt.Errorf("failed to update manifest repository: %w", err)
		}
	}

	// 检查是否有更新
	newHeadOutput, err := gitRunner.RunInDir(manifestProjectPath, "rev-parse", "HEAD")
	newHead := strings.TrimSpace(string(newHeadOutput))
	if err != nil {
		e.logger.Debug("获取更新后的 HEAD failed: %v", err)
	}

	e.logger.Debug("manifest repository updated")

	// 如果 manifest 仓库有更新，则重新生成 .repo/manifest.xml
	if oldHead != newHead {
		e.logger.Debug("manifest updated, regenerating .repo/manifest.xml")
		if err := e.regenerateManifestXML(); err != nil {
			e.logger.Debug("regenerate manifest.xml failed: %v", err)
			// 不返回错误，继续执行同步
		}
	} else {
		e.logger.Debug("manifest unchanged, skipping regeneration")
	}

	return nil
}

// regenerateManifestXML 重新生成 .repo/manifest.xml 文件
func (e *Engine) regenerateManifestXML() error {
	// 获取配置信息
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	// 确定主 manifest 文件路径
	manifestPath := filepath.Join(e.repoRoot, ".repo", "manifests", cfg.ManifestName)
	e.logger.Debug("解析主 manifest 文件: %s", manifestPath)

	// 解析主 manifest 文件
	parser := manifest.NewParser()

	// 对齐上游 repo：manifest.xml 始终保留完整目列表（不过滤组），
	// 组过滤由 sync 命令在 GetProjects 时动态执行。
	mainManifest, err := parser.ParseFromFile(manifestPath, nil)
	if err != nil {
		return fmt.Errorf("failed to parse main manifest file: %w", err)
	}

	// 处理 include 标签
	if len(mainManifest.Includes) > 0 {
		e.logger.Info("处理 %d 个 include 标签", len(mainManifest.Includes))
		merger := manifest.NewMerger(parser, filepath.Join(e.repoRoot, ".repo", "manifests"))

		// 创建包含所有清单的切片，从主清单开始
		allManifests := []*manifest.Manifest{mainManifest}

		// 处理每个 include
		for _, include := range mainManifest.Includes {
			includePath := filepath.Join(e.repoRoot, ".repo", "manifests", include.Name)
			e.logger.Debug("加载包含的 manifest: %s", include.Name)

			includeManifest, err := parser.ParseFromFile(includePath, nil)
			if err != nil {
				e.logger.Warn("解析包含的 manifest %s failed: %v", include.Name, err)
				continue
			}

			allManifests = append(allManifests, includeManifest)
		}

		// 合并所有清单
		mergedManifest, err := merger.Merge(allManifests)
		if err != nil {
			return fmt.Errorf("failed to merge manifests: %w", err)
		}

		mainManifest = mergedManifest
	}

	// 目已在 ParseFromFile 阶段按组过滤（shouldIncludeProject -> MatchesGroupFilter），
	// 合并仅累加已过滤的子清单目，无需再次过滤。

	// 生成合并后的 manifest XML
	mergedData, err := mainManifest.ToXML()
	if err != nil {
		return fmt.Errorf("failed to generate merged manifest XML: %w", err)
	}

	// Save to .repo/manifest.xml
	// Remove existing manifest.xml if it is a symlink (legacy from old repo-go versions).
	outputPath := filepath.Join(e.repoRoot, ".repo", "manifest.xml")
	if fi, err := os.Lstat(outputPath); err == nil && (fi.Mode()&os.ModeSymlink != 0) {
		os.Remove(outputPath)
	}
	e.logger.Debug("saving merged manifest to: %s", outputPath)
	if err := os.WriteFile(outputPath, []byte(mergedData), 0644); err != nil {
		return fmt.Errorf("failed to save manifest file: %w", err)
	}

	e.logger.Info("成功重新生成 .repo/manifest.xml，包含 %d 个目", len(mainManifest.Projects))
	return nil
}

// formatDuration 格式化持续时间为人类可读格式
func formatDuration(d time.Duration) string {
	d = d.Round(time.Second)
	h := d / time.Hour
	d -= h * time.Hour
	m := d / time.Minute
	d -= m * time.Minute
	s := d / time.Second

	if h > 0 {
		return fmt.Sprintf("%d小时%d分钟%d秒", h, m, s)
	} else if m > 0 {
		return fmt.Sprintf("%d分钟%d秒", m, s)
	}
	return fmt.Sprintf("%d秒", s)
}

// fetchRetries 返回 fetch/clone 的重试次数，取自 --retry-fetches，默认 3。
func (e *Engine) fetchRetries() int {
	if e.options.RetryFetches > 0 {
		return e.options.RetryFetches
	}
	return 3
}

// syncProject 同步单个目
// ctx 为 Sync 派生的可取消上下文：fail-fast 或外部取消会中断其内部
// fetch/clone/checkout 的重试退避等待（对齐上游 repo 失败即停的语义）。
func (e *Engine) syncProject(ctx context.Context, p *project.Project) error {
	// 检查目目录是否存在
	exists, err := e.projectExists(p)
	if err != nil {
		return fmt.Errorf("checking project %s failed: %w", p.Name, err)
	}

	// 检查是否为镜像模式
	isMirror := false
	if e.options.Config != nil && e.options.Config.Mirror {
		isMirror = true
		if !e.options.Quiet {
			e.logger.Debug("目 %s 使用镜像模式", p.Name)
		}
	}

	if !exists {
		// Cloning project
		if !e.options.Quiet {
			e.logger.Info("Cloning project: %s", p.Name)
		}
		return e.cloneProject(ctx, p)
	}

	// Updating project
	if !e.options.NetworkOnly && !e.options.LocalOnly {
		if !e.options.Quiet {
			e.logger.Info("Updating project: %s", p.Name)
		}
	}

	if !e.options.LocalOnly {
		// 执行网络操作
		if err := e.fetchProject(ctx, p); err != nil {
			return err
		}
	}

	if !e.options.NetworkOnly {
		// 执行本地操作（镜像模式下不需要检出特定分支）
		if !isMirror {
			if err := e.checkoutProject(ctx, p); err != nil {
				return err
			}
		}

		// 更新成功后处理linkfile copyfile（仅在非NetworkOnly模式且非镜像模式下）
		if !isMirror {
			e.logger.Info("目 %s 更新完成，开始处理 linkfile 和 copyfile", p.Name)
			if err := e.processLinkAndCopyFiles(p); err != nil {
				e.logger.Error("目 %s 更新后处理 linkfile 和 copyfile failed: %v", p.Name, err)
				return &SyncError{
					ProjectName: p.Name,
					Phase:       "link_copy_files_after_update",
					Err:         err,
					Timestamp:   time.Now(),
				}
			}
			e.logger.Info("成功处理目 %s 更新后的链接文件和复制文件", p.Name)

			// 处理 submodule（如果启用）
			if err := e.updateSubmodules(ctx, p); err != nil {
				e.logger.Error("目 %s 更新 submodule failed: %v", p.Name, err)
				// submodule 更新failed不阻断整个同步流程，只记录错误
				if !e.options.Quiet {
					e.logger.Warn("跳过目 %s 的 submodule 更新", p.Name)
				}
			}
		} else {
			e.logger.Info("镜像模式，跳过处理目 %s 的链接文件和复制文件", p.Name)
		}
	}

	return nil
}

// trimLastPathSegment safely removes the last path segment from a URL,
// preserving protocol and hostname components.
func trimLastPathSegment(url string) string {
	url = strings.TrimSuffix(url, "/")
	hasProtocol := strings.Contains(url, "://")
	parts := strings.Split(url, "/")
	if len(parts) <= 3 && hasProtocol {
		// URL is protocol://host or protocol://host/, keep as-is
		return url
	}
	return strings.Join(parts[:len(parts)-1], "/")
}

// resolveBaseURLFromManifest resolves the base URL from the manifest's remote definitions.
// Returns the remote fetch URL and the resolved remote name.
func (e *Engine) resolveBaseURLFromManifest(p *project.Project) (baseURL, remoteName string) {
	if e.manifest == nil {
		return "", ""
	}

	remoteName = p.RemoteName
	if remoteName == "" {
		if e.options != nil && e.options.DefaultRemote != "" {
			remoteName = e.options.DefaultRemote
			if e.options.Verbose && e.logger != nil {
				e.logger.Debug("project %s has no remote, using default remote from CLI: %s", p.Name, remoteName)
			}
		} else if e.manifest.Default.Remote != "" {
			remoteName = e.manifest.Default.Remote
			if e.options.Verbose && e.logger != nil {
				e.logger.Debug("project %s has no remote, using manifest default: %s", p.Name, remoteName)
			}
		}
	}

	if remoteName != "" {
		var err error
		baseURL, err = e.manifest.GetRemoteURL(remoteName)
		if err == nil && baseURL != "" {
			if e.options.Verbose && e.logger != nil {
				e.logger.Debug("resolved remote %s URL from manifest: %s", remoteName, baseURL)
			}
		} else if e.logger != nil {
			e.logger.Debug("failed to get remote %s URL from manifest: %v", remoteName, err)
		}
	}

	return baseURL, remoteName
}

// resolveBaseURLFromConfig resolves a base URL from the configuration file
// when the manifest does not provide one. Falls back to reading .repo/config.json directly.
func (e *Engine) resolveBaseURLFromConfig() string {
	var manifestURL string

	if e.config != nil && e.config.ManifestURL != "" {
		manifestURL = e.config.ManifestURL
		if e.options.Verbose && e.logger != nil {
			e.logger.Debug("using loaded config ManifestURL: %s", manifestURL)
		}
	} else {
		cfg, err := config.Load()
		if err == nil && cfg != nil {
			e.config = cfg
			manifestURL = cfg.ManifestURL
			if e.options.Verbose && e.logger != nil {
				e.logger.Debug("loaded config from file, ManifestURL: %s", manifestURL)
			}
		} else {
			if e.logger != nil {
				e.logger.Debug("failed to load config from file: %v", err)
			}
			// Fallback: read .repo/config.json directly
			manifestURL = e.readManifestURLFromConfigFile()
		}
	}

	if manifestURL == "" {
		if e.logger != nil {
			e.logger.Error("cannot resolve relative URL: failed to get ManifestURL")
		}
		return ""
	}

	baseURL := trimLastPathSegment(manifestURL)
	if baseURL == "" {
		if e.logger != nil {
			e.logger.Error("failed to extract baseURL from ManifestURL: %s", manifestURL)
		}
		return ""
	}

	if e.options.Verbose && e.logger != nil {
		e.logger.Debug("extracted baseURL from config: %s", baseURL)
	}
	return baseURL
}

// readManifestURLFromConfigFile reads the manifest_url field from .repo/config.json directly.
func (e *Engine) readManifestURLFromConfigFile() string {
	configPath := filepath.Join(".repo", "config.json")
	if _, err := os.Stat(configPath); err != nil {
		if e.logger != nil {
			e.logger.Debug("config.json does not exist: %v", err)
		}
		return ""
	}

	data, err := os.ReadFile(configPath)
	if err != nil {
		if e.logger != nil {
			e.logger.Debug("failed to read config.json: %v", err)
		}
		return ""
	}

	var configData struct {
		ManifestURL string `json:"manifest_url"`
	}
	if err := json.Unmarshal(data, &configData); err != nil {
		if e.logger != nil {
			e.logger.Debug("failed to parse config.json: %v", err)
		}
		return ""
	}

	if configData.ManifestURL != "" && e.options.Verbose && e.logger != nil {
		e.logger.Debug("read ManifestURL from config.json: %s", configData.ManifestURL)
	}
	return configData.ManifestURL
}

// resolveRelativeRemoteURL resolves a relative remote URL (.., ../, ./) against a base URL.
func resolveRelativeRemoteURL(remoteURL, baseURL, projectName string) string {
	baseURL = strings.TrimSuffix(baseURL, "/")

	if remoteURL == ".." {
		baseURL = trimLastPathSegment(baseURL)
		return baseURL + "/" + projectName
	}

	if strings.HasPrefix(remoteURL, "../") {
		count := 0
		tempURL := remoteURL
		for strings.HasPrefix(tempURL, "../") {
			count++
			tempURL = tempURL[3:]
		}

		tempBaseURL := baseURL
		for i := 0; i < count; i++ {
			tempBaseURL = trimLastPathSegment(tempBaseURL)
		}

		if tempURL == "" {
			return tempBaseURL + "/" + projectName
		}
		if strings.HasSuffix(tempURL, "/") {
			return tempBaseURL + "/" + tempURL + projectName
		}
		return tempBaseURL + "/" + tempURL + "/" + projectName
	}

	if strings.HasPrefix(remoteURL, "./") {
		baseURL = trimLastPathSegment(baseURL)
		relPath := strings.TrimPrefix(remoteURL, "./")
		if relPath == "" {
			return baseURL + "/" + projectName
		}
		if strings.HasSuffix(relPath, "/") {
			return baseURL + "/" + relPath + projectName
		}
		return baseURL + "/" + relPath + "/" + projectName
	}

	return remoteURL
}

// resolveRemoteURL resolves the project's remote URL, handling relative paths
// (.., ../, ./) by resolving against the manifest's remote URL or the config's ManifestURL.
func (e *Engine) resolveRemoteURL(p *project.Project) string {
	remoteURL := p.RemoteURL
	if remoteURL == "" {
		remoteURL = ".."
	}

	// Only process relative URLs
	if remoteURL != ".." && !strings.HasPrefix(remoteURL, "../") && !strings.HasPrefix(remoteURL, "./") {
		return remoteURL
	}

	// Step 1: Try to get base URL from manifest
	baseURL, _ := e.resolveBaseURLFromManifest(p)

	// Step 2: If manifest did not provide a valid protocol URL, fall back to config
	if !strings.HasPrefix(baseURL, "ssh://") &&
		!strings.HasPrefix(baseURL, "http://") &&
		!strings.HasPrefix(baseURL, "https://") {
		baseURL = e.resolveBaseURLFromConfig()
	}

	// Step 3: Resolve the relative URL against the base URL
	if baseURL != "" {
		remoteURL = resolveRelativeRemoteURL(remoteURL, baseURL, p.Name)
		if e.options != nil && e.options.Verbose && e.logger != nil {
			e.logger.Debug("resolved relative URL %s to %s", p.RemoteURL, remoteURL)
		}
	}

	return remoteURL
}

// fetchProject 执行单个目的网络同
// ctx 取消时 RetryWithBackoff 会立即中断退避等待并返回取消错误。
func (e *Engine) fetchProject(ctx context.Context, p *project.Project) error {
	// 输出详细日志，显示实际使用的远程 URL
	if e.options.Verbose {
		e.logger.Debug("正在failed to get project %s，原始远URL: %s", p.Name, p.RemoteURL)
	}

	// 1.8 --optimized-fetch：若本地已包含目标修订则跳过网络获取
	if e.options.OptimizedFetch && p.RevisionID != "" {
		if _, err := e.runGitInDir(p.Worktree, "cat-file", "-e", p.RevisionID); err == nil {
			e.logger.Debug("目 %s 已包含修订 %s，跳过 fetch (optimized-fetch)", p.Name, p.RevisionID)
			return nil
		}
	}

	// 解析远程URL
	remoteURL := e.resolveRemoteURL(p)
	// Updating projectRemoteURL 为解析后URL
	p.RemoteURL = remoteURL

	// 执行 Git 操作
	// 检查远程仓库是否存
	if err := e.ensureRemoteExists(p, remoteURL); err != nil {
		return &SyncError{
			ProjectName: p.Name,
			Phase:       "ensure_remote",
			Err:         err,
			Timestamp:   time.Now(),
		}
	}

	// 执行 fetch 命令
	args := []string{"-C", p.Worktree, "fetch"}

	// 检查是否为镜像模式
	isMirror := e.options.Config != nil && e.options.Config.Mirror
	// 1.5 --prune 或镜像模式下添加 --prune，删除远程已不存在的引用
	if e.options.Prune || isMirror {
		args = append(args, "--prune")
		if !e.options.Quiet && e.options.Verbose {
			e.logger.Debug("目 %s 添加 --prune 参数", p.Name)
		}
	}
	e.logger.Debug("目镜像模式 %s ", isMirror)

	// 对齐上游 Sync_LocalHalf：--tags/--no-tags 由 CLI 显式值或项目级
	// sync-tags（继承 default，缺省 true）决定；--no-tags 优先级最高
	fetchTags := p.SyncTags
	if e.options.Tags {
		fetchTags = true
	}
	if e.options.NoTags {
		fetchTags = false
	}
	if fetchTags {
		args = append(args, "--tags")
	} else {
		args = append(args, "--no-tags")
	}
	if e.options.Quiet {
		args = append(args, "--quiet")
	}

	// 使用远程名称
	args = append(args, p.RemoteName)

	// 显式 refspec(对齐上游 Sync_NetworkHalf):命令行给出 refspec 后
	// remote.<name>.fetch 配置不再参与,因此通配 refspec 必须显式补上,
	// 否则会静默退化为只取 revision 一个分支。
	// 追加精确 revision refspec 的目的:远端缺少 manifest 指定分支时
	// fetch 阶段即报错(git: couldn't find remote ref),错误更早更清晰,
	// 而不是拖到 checkout 阶段才失败。
	if !isMirror {
		if !e.options.CurrentBranch {
			args = append(args,
				fmt.Sprintf("+refs/heads/*:refs/remotes/%s/*", p.RemoteName))
		}
		if spec := revisionFetchRefspec(p); spec != "" {
			args = append(args, spec)
		}
	}

	// Limit concurrent network operations (--jobs-network)
	e.networkSem <- struct{}{}
	defer func() { <-e.networkSem }()

	// Retry fetch using RetryWithBackoff with --retry-fetches (default 3)
	maxRetries := e.fetchRetries()
	var stderrBuf bytes.Buffer

	opts := RetryOptions{
		MaxRetries:  maxRetries,
		BaseDelay:   2 * time.Second,
		MaxDelay:    30 * time.Second,
		ShouldRetry: IsRetryableGitError,
	}

	retryErr := RetryWithBackoff(ctx, opts, func(attempt int) error {
		if attempt > 0 {
			e.logger.Info("retrying fetch for project %s (attempt %d)", p.Name, attempt)
			stderrBuf.Reset()
		}

		_, stderrStr, runErr := e.runGitWithStderr(args)
		stderrBuf.WriteString(stderrStr)
		if runErr != nil {
			return fmt.Errorf("fetch failed: %w", runErr)
		}
		return nil
	})

	if retryErr != nil {
		return &SyncError{
			ProjectName: p.Name,
			Phase:       "fetch",
			Err:         retryErr,
			Output:      stderrBuf.String(),
			Timestamp:   time.Now(),
			RetryCount:  maxRetries,
		}
	}

	// Pull LFS files if enabled
	if e.options.GitLFS {
		if err := e.pullLFS(ctx, p); err != nil {
			return &SyncError{
				ProjectName: p.Name,
				Phase:       "lfs_pull",
				Err:         err,
			}
		}
	}

	return nil
}

// cloneProject 克隆单个目
// ctx 取消时（fail-fast/外部取消）重试退避等待会被立即中断。
func (e *Engine) cloneProject(ctx context.Context, p *project.Project) error {
	// 解析远程URL
	remoteURL := e.resolveRemoteURL(p)
	// Updating projectRemoteURL 为解析后URL
	p.RemoteURL = remoteURL

	// 确保RemoteName有值
	if p.RemoteName == "" {
		p.RemoteName = "origin"
		if e.options.Verbose {
			e.logger.Debug("目 %s 未指定远程名称，使用默认值 'origin'", p.Name)
		}
	}

	// 创建父目录
	if err := os.MkdirAll(filepath.Dir(p.Worktree), 0755); err != nil {
		return &SyncError{
			ProjectName: p.Name,
			Phase:       "mkdir",
			Err:         err,
			Timestamp:   time.Now(),
		}
	}

	// 检查是否为镜像模式
	isMirror := e.options.Config != nil && e.options.Config.Mirror

	// 构建 clone 命令
	args := e.buildCloneArgs(p, remoteURL, isMirror)

	// 1.6 限制并发网络操作数（--jobs-network）；clone 是网络密集型操作
	e.networkSem <- struct{}{}
	defer func() { <-e.networkSem }()

	// 1.4 添加重试机制：使用 --retry-fetches，默认 3
	maxRetries := e.fetchRetries()
	var lastErr error
	var stderr bytes.Buffer

	for retryCount := 0; retryCount <= maxRetries; retryCount++ {
		if retryCount > 0 {
			retryDelay := time.Duration(retryCount) * 3 * time.Second
			e.logger.Info("retrying clone for project %s (attempt %d, waiting %v)",
				p.Name, retryCount, retryDelay)

			// Context-aware sleep: cancel if the sync context is cancelled
			select {
			case <-ctx.Done():
				return &SyncError{
					ProjectName: p.Name,
					Phase:       "clone",
					Err:         ctx.Err(),
					Timestamp:   time.Now(),
				}
			case <-time.After(retryDelay):
			}

			stderr.Reset()

			// 检查目标目录是否已存在但不完整，如果存在则删除
			if _, err := os.Stat(p.Worktree); err == nil {
				// 安全护栏：目标目录已是有效的裸仓库（HEAD/objects/refs 齐全）时，
				// 说明它是已存在的完整镜像而非本次克隆残留，绝不删除，直接终止重试。
				// 注意：非镜像模式下克隆失败残留的是含 .git 子目录的工作树（顶层无 HEAD），
				// 不受此护栏影响，仍可正常清理重克隆。
				if isBareGitRepo(p.Worktree) {
					e.logger.Warn(
						"target %s already exists and is a valid bare repository, refusing to remove and re-clone",
						p.Worktree)
					return &SyncError{
						ProjectName: p.Name,
						Phase:       "clone",
						Err: fmt.Errorf(
							"destination %s already exists and is a valid bare repository, refusing to delete",
							p.Worktree),
						Timestamp:  time.Now(),
						RetryCount: retryCount,
					}
				}

				// 在镜像模式下，检查目路径是否在.repo目录下
				if isMirror {
					// 对于镜像模式，目路径是根据远程URL确定的，可能不在.repo目录下
					// 我们只需要确保路径是安全的，不删除系统目录
					absWorktree, absErr := filepath.Abs(p.Worktree)
					if absErr != nil {
						return &SyncError{
							ProjectName: p.Name,
							Phase:       "clone",
							Err:         fmt.Errorf("无法获取working directory绝对路径: %w", absErr),
						}
					}

					// 获取当前working directory
					currentDir, dirErr := os.Getwd()
					if dirErr != nil {
						return &SyncError{
							ProjectName: p.Name,
							Phase:       "clone",
							Err:         fmt.Errorf("failed to get current directory: %w", dirErr),
						}
					}

					// 绝对禁止删除系统关键目录
					// 检查是否为危险路径
					if p.Worktree == "." || p.Worktree == ".." ||
						strings.HasPrefix(p.Worktree, "../") || strings.HasPrefix(p.Worktree, "..\\") ||
						absWorktree == "/" || absWorktree == "\\" ||
						filepath.VolumeName(absWorktree) == absWorktree { // Windows根目录
						// 路径绝对不安全，deletion refused
						return &SyncError{
							ProjectName: p.Name,
							Phase:       "clone",
							Err:         fmt.Errorf("working directory %s is a system critical directory, deletion is forbidden", p.Worktree),
						}
					}

					// 检查路径是否在.repo目录下或当前目录下
					// 只有在这两个目录下的路径才允许删除
					// 特殊处理：如果路径是.repo目录本身或其子目录，则允许删除
					inRepoDir := strings.HasPrefix(absWorktree, e.repoRoot)
					inCurrentDir := strings.HasPrefix(absWorktree, currentDir)
					isRepoSubDir := absWorktree == e.repoRoot || strings.HasPrefix(absWorktree, e.repoRoot+string(filepath.Separator))

					if !inRepoDir && !inCurrentDir && !isRepoSubDir {
						// 路径is not within allowed range，deletion refused
						return &SyncError{
							ProjectName: p.Name,
							Phase:       "clone",
							Err:         fmt.Errorf("working directory %s is not within allowed range（.repo目录或当前目录），deletion refused", p.Worktree),
						}
					}
				} else {
					// 非镜像模式下，保持原有的安全检查
					// 安全检查：确保要删除的目录在repo根目录下
					repoRoot, repoErr := config.GetRepoRoot()
					if repoErr != nil {
						return &SyncError{
							ProjectName: p.Name,
							Phase:       "clone",
							Err:         fmt.Errorf("failed to get repo root: %w", repoErr),
						}
					}

					// 检查working directory是否在repo根目录下
					absWorktree, absErr := filepath.Abs(p.Worktree)
					if absErr != nil {
						return &SyncError{
							ProjectName: p.Name,
							Phase:       "clone",
							Err:         fmt.Errorf("无法获取working directory绝对路径: %w", absErr),
						}
					}

					relPath, relErr := filepath.Rel(repoRoot, absWorktree)
					if relErr != nil || strings.HasPrefix(relPath, "..") {
						// 目录is not under repo root，deletion refused
						return &SyncError{
							ProjectName: p.Name,
							Phase:       "clone",
							Err:         fmt.Errorf("working directory %s is not under repo root，deletion refused", p.Worktree),
						}
					}
				}

				e.logger.Info("removing incomplete clone directory: %s", p.Worktree)
				if err := os.RemoveAll(p.Worktree); err != nil {
					e.logger.Warn("failed to remove incomplete clone directory %s: %v", p.Worktree, err)
					// Continue anyway: the clone command may handle the existing directory
				}
			}
		}

		// 执行 clone 命令
		_, stderrStr, runErr := e.runGitWithStderr(args)
		stderr.WriteString(stderrStr)
		lastErr = runErr

		if lastErr == nil {
			// 成功克隆，跳出重试循环
			break
		}

		// 如果已经达到最大重试次数，则返回错误
		if retryCount == maxRetries {
			return &SyncError{
				ProjectName: p.Name,
				Phase:       "clone",
				Err:         lastErr,
				Output:      stderr.String(),
				Timestamp:   time.Now(),
				RetryCount:  retryCount,
			}
		}
	}

	// 克隆成功后，设置远程仓库
	if err := e.setupRemote(p, remoteURL); err != nil {
		return &SyncError{
			ProjectName: p.Name,
			Phase:       "setup_remote",
			Err:         err,
		}
	}

	// 克隆成功后，确保检出正确的分支（仅在非镜像模式下）
	if !e.options.NetworkOnly && p.Revision != "" && !isMirror {
		if !e.options.Quiet && e.options.Verbose {
			e.logger.Info("确保目 %s 检出到正确的版本: %s", p.Name, p.Revision)
		}

		// 执行检出操作
		if err := e.checkoutProject(ctx, p); err != nil {
			return &SyncError{
				ProjectName: p.Name,
				Phase:       "post_clone_checkout",
				Err:         err,
				Timestamp:   time.Now(),
			}
		}
	}

	// 如果启用LFS，执行LFS 拉取（仅在非镜像模式下）
	if e.options.GitLFS && !isMirror {
		if err := e.pullLFS(ctx, p); err != nil {
			return &SyncError{
				ProjectName: p.Name,
				Phase:       "lfs_pull",
				Err:         err,
			}
		}
	}

	// 记录克隆完成日志
	e.logger.Debug("目 %s 克隆完成", p.Name)

	// 处理 linkfile 和 copyfile（仅在非NetworkOnly模式且非镜像模式下）
	if !e.options.NetworkOnly && !isMirror {
		e.logger.Info("开始处理目 %s 的链接文件和复制文件", p.Name)
		if err := e.processLinkAndCopyFiles(p); err != nil {
			e.logger.Error("目 %s 处理 linkfile 和 copyfile failed: %v", p.Name, err)
			return &SyncError{
				ProjectName: p.Name,
				Phase:       "link_copy_files",
				Err:         err,
				Timestamp:   time.Now(),
			}
		}
		e.logger.Info("成功处理目 %s 的链接文件和复制文件", p.Name)

		// 处理 submodule（如果启用）
		if err := e.updateSubmodules(ctx, p); err != nil {
			e.logger.Error("目 %s 更新 submodule failed: %v", p.Name, err)
			// submodule 更新failed不阻断整个同步流程，只记录错误
			if !e.options.Quiet {
				e.logger.Warn("跳过目 %s 的 submodule 更新", p.Name)
			}
		}
	} else if isMirror {
		e.logger.Info("镜像模式，跳过处理目 %s 的链接文件和复制文件", p.Name)
	} else {
		e.logger.Info("NetworkOnly模式，跳过处理目 %s 的链接文件和复制文件", p.Name)
	}

	return nil
}

// buildCloneArgs 构造 git clone 参数（不含执行），便于对参数构造做单元测试。
// isMirror 由调用方根据配置判定并用于跳过分支参数。
func (e *Engine) buildCloneArgs(p *project.Project, remoteURL string, isMirror bool) []string {
	args := []string{"clone"}

	// 1.5 --no-clone-bundle：禁用 HTTP/HTTPS 上的 /clone.bundle 加速
	if e.options.NoCloneBundle {
		args = append(args, "--no-clone-bundle")
	}

	// 添加 --origin 参数，指定远程名称
	args = append(args, "--origin", p.RemoteName)
	if e.options.Verbose {
		e.logger.Debug("目 %s 克隆时指定远程名称: %s", p.Name, p.RemoteName)
	}

	if isMirror {
		args = append(args, "--mirror")
		if !e.options.Quiet {
			e.logger.Info("目 %s 将以镜像模式克隆", p.Name)
		}
	}

	// 添加 --reference 参数，指定本地参考仓库路径
	// 对齐上游 repo：Reference 可以是单个仓库，也可以是存放多个参考仓库的目录，
	// 后者按项目名拼路径（DIR/<name>.git / DIR/<name>）。
	if e.options.Reference != "" {
		refRepo := resolveReferenceRepo(e.options.Reference, p.Name)
		if refRepo != "" {
			args = append(args, "--reference", refRepo)
			if !e.options.Quiet {
				e.logger.Info("目 %s 将使用参考仓库: %s", p.Name, refRepo)
			}
			if e.options.Dissociate {
				// 克隆完成后把借用的对象复制进新仓库并删除 alternates，
				// 避免新仓库长期依赖参考仓库路径。
				args = append(args, "--dissociate")
			}
		} else {
			e.logger.Warn(
				"参考路径 %s 下未找到项目 %s 对应的参考仓库，将不使用参考仓库",
				e.options.Reference, p.Name)
		}
	}

	// 添加 LFS 支持
	if e.options.GitLFS {
		// 确保 git-lfs 已安装
		if _, err := exec.LookPath("git-lfs"); err == nil {
			args = append(args, "--filter=blob:limit=0")
		}
	}

	// 目级 clone-depth（对齐上游 repo manifest project clone-depth 属性）
	// 全局 --depth 优先于目级 clone-depth
	if e.options.Depth > 0 && !isMirror {
		args = append(args, fmt.Sprintf("--depth=%d", e.options.Depth))
	} else if p.CloneDepth > 0 && !isMirror {
		args = append(args, fmt.Sprintf("--depth=%d", p.CloneDepth))
	}

	if e.options.Quiet {
		args = append(args, "--quiet")
	}

	// 添加分支参数，确保克隆时指定正确的分支
	// 注意：镜像模式下不需要指定分支，因为镜像会克隆所有分支
	if p.Revision != "" && !isMirror {
		// 处理 revision 格式，移除可能的 refs/heads/ 或 refs/tags/ 前缀
		revision := p.Revision
		if strings.HasPrefix(revision, "refs/heads/") {
			revision = strings.TrimPrefix(revision, "refs/heads/")
		} else if strings.HasPrefix(revision, "refs/tags/") {
			revision = strings.TrimPrefix(revision, "refs/tags/")
		}

		// 只有当 revision 不是提交 ID 时才使用 --branch 参数
		// 对于提交 ID，我们将在克隆后执行检出操作
		if !looksLikeCommitID(revision) {
			args = append(args, "--branch", revision)
			if !e.options.Quiet {
				e.logger.Info("Cloning project %s 时指定分支: %s", p.Name, revision)
			}
		} else if !e.options.Quiet {
			e.logger.Info("目 %s 的 revision 看起来像提交 ID: %s，将在克隆后检出", p.Name, revision)
		}

	}

	// 添加远程URL和目标目录
	args = append(args, remoteURL, p.Worktree)

	return args
}

// checkoutProject 将工作树对齐到 manifest revision,语义对齐上游 Sync_LocalHalf:
//  1. 先把 revision 解析为具体提交 SHA(GetRevisionId 语义),解析失败报
//     "revision X in Y not found";绝不把分支名直接传给 git checkout——
//     工作树内存在同名文件时 git 会报引用/路径歧义(如 toolchain/gcc 项目)
//  2. HEAD 已指向 revid:不执行任何 git 命令直接返回(上游:No changes)
//  3. --detach 或游离 HEAD:checkout --force --detach <revid> --(上游 _Checkout)
//  4. 在 revision 分支上(或当前分支跟踪该 revision):merge --ff-only 快进
//     (上游 _FastForward);分叉时保留本地状态仅告警,不自动 rebase/reset-hard
//  5. 其他:本地存在同名分支则切换过去再快进,否则 detach 到 revid
//
// ctx 取消时（fail-fast/外部取消）重试退避等待会被立即中断。
func (e *Engine) checkoutProject(ctx context.Context, p *project.Project) error {
	revid, err := e.resolveRevisionID(p)
	if err != nil {
		return &SyncError{
			ProjectName: p.Name,
			Phase:       "checkout",
			Err:         err,
			Timestamp:   time.Now(),
		}
	}

	// 已就位:与上游一致直接返回,不执行任何 checkout
	if e.gitHeadSha(p) == revid {
		return nil
	}

	// Limit concurrent checkout operations (--jobs-checkout)
	e.checkoutSem <- struct{}{}
	defer func() { <-e.checkoutSem }()

	branch := revisionBranchName(p.Revision)
	headBranch := e.gitHeadBranch(p)

	switch {
	case e.options.Detach || headBranch == "":
		return e.checkoutRevision(ctx, p, revid, true)
	case headBranch == "refs/heads/"+branch || e.branchTracksRevision(p, headBranch):
		return e.fastForwardTo(p, revid)
	default:
		// 在其他分支上:本地存在 revision 同名分支则切换过去再快进,
		// 否则 detach 到 revid(上游此时会重写当前分支的跟踪配置并
		// rebase/reset-hard,repo-go 保守处理为切换/detach)
		if branch != "" && e.localBranchExists(p, branch) {
			if err := e.checkoutRevision(ctx, p, branch, false); err != nil {
				return err
			}
			if e.gitHeadSha(p) == revid {
				return nil
			}
			return e.fastForwardTo(p, revid)
		}
		return e.checkoutRevision(ctx, p, revid, true)
	}
}

// resolveRevisionID 将 manifest revision 解析为具体提交 SHA(对齐上游 GetRevisionId):
//   - 提交 ID:直接解析验证
//   - 分支名(或 refs/heads/<branch>):优先 refs/remotes/<remote>/<branch>
//     (fetch 之后的跟踪引用),回退 refs/heads/<branch>(clone 后远端被更名、
//     跟踪引用尚未建立的场景)
//   - 其他 refs/ 前缀(如 refs/tags/<tag>):直接解析
//
// 全部失败时返回上游同款错误文本 "revision <rev> in <name> not found"。
// 注意:与上游一致,不带前缀的 revision 只按分支解析,不回退到同名 tag。
func (e *Engine) resolveRevisionID(p *project.Project) (string, error) {
	rev := p.Revision
	if rev == "" {
		return "", fmt.Errorf("revision in %s not found", p.Name)
	}

	candidates := []string{}
	switch {
	case looksLikeCommitID(rev):
		candidates = append(candidates, rev)
	case strings.HasPrefix(rev, "refs/heads/"):
		branch := strings.TrimPrefix(rev, "refs/heads/")
		candidates = append(candidates, remoteTrackingRef(p.RemoteName, branch), "refs/heads/"+branch)
	case strings.HasPrefix(rev, "refs/"):
		candidates = append(candidates, rev)
	default:
		candidates = append(candidates, remoteTrackingRef(p.RemoteName, rev), "refs/heads/"+rev)
	}

	for _, ref := range candidates {
		out, err := e.runGitInDir(p.Worktree, "rev-parse", "--verify", "--quiet", ref+"^0")
		if err != nil {
			continue
		}
		if sha := strings.TrimSpace(string(out)); sha != "" {
			return sha, nil
		}
	}
	return "", fmt.Errorf("revision %s in %s not found", rev, p.Name)
}

// revisionBranchName 返回 revision 的分支名;SHA 或非 heads 引用时为空。
func revisionBranchName(rev string) string {
	if strings.HasPrefix(rev, "refs/heads/") {
		return strings.TrimPrefix(rev, "refs/heads/")
	}
	if strings.HasPrefix(rev, "refs/") || looksLikeCommitID(rev) {
		return ""
	}
	return rev
}

// remoteTrackingRef 返回分支对应的远端跟踪引用名。
func remoteTrackingRef(remoteName, branch string) string {
	if remoteName == "" || branch == "" {
		return ""
	}
	return fmt.Sprintf("refs/remotes/%s/%s", remoteName, branch)
}

// gitHeadSha 返回 HEAD 指向的提交 SHA; unborn 分支或仓库异常时为空。
func (e *Engine) gitHeadSha(p *project.Project) string {
	out, err := e.runGitInDir(p.Worktree, "rev-parse", "--verify", "--quiet", "HEAD")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// gitHeadBranch 返回 HEAD 的分支引用全名(如 refs/heads/master);游离 HEAD 时为空。
func (e *Engine) gitHeadBranch(p *project.Project) string {
	out, err := e.runGitInDir(p.Worktree, "symbolic-ref", "-q", "HEAD")
	if err != nil {
		return ""
	}
	ref := strings.TrimSpace(string(out))
	if !strings.HasPrefix(ref, "refs/heads/") {
		return ""
	}
	return ref
}

// branchTracksRevision 判断 headBranch 分支的跟踪配置是否指向 revision
// (对齐上游 branch.LocalMerge 判定):branch.<name>.remote 与项目远端一致
// 且 branch.<name>.merge 等于 revision 分支。
func (e *Engine) branchTracksRevision(p *project.Project, headBranch string) bool {
	branch := revisionBranchName(p.Revision)
	if branch == "" {
		return false
	}
	name := strings.TrimPrefix(headBranch, "refs/heads/")
	if name == headBranch || name == "" {
		return false
	}
	merge := e.gitConfigGet(p, "branch."+name+".merge")
	if merge == "" {
		return false
	}
	if merge != "refs/heads/"+branch && merge != p.Revision {
		return false
	}
	return e.gitConfigGet(p, "branch."+name+".remote") == p.RemoteName
}

// gitConfigGet 读取 worktree 级 git 配置,缺失或读取失败时为空。
func (e *Engine) gitConfigGet(p *project.Project, key string) string {
	out, err := e.runGitInDir(p.Worktree, "config", "--get", key)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// localBranchExists 判断本地分支 refs/heads/<branch> 是否存在。
func (e *Engine) localBranchExists(p *project.Project, branch string) bool {
	if branch == "" {
		return false
	}
	_, err := e.runGitInDir(p.Worktree, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch)
	return err == nil
}

// fastForwardTo 将当前分支快进到 revid(上游 _FastForward)。
// 无法快进(本地与上游分叉)时不失败:与上游"全部本地提交均为本人且无
// 上游增益"分支等价,保留本地状态仅记录告警;上游的分叉 rebase/reset-hard
// 分支 repo-go 不自动执行,避免破坏用户提交。
func (e *Engine) fastForwardTo(p *project.Project, revid string) error {
	args := []string{"-C", p.Worktree, "merge", "--no-stat", "--ff-only", revid}
	_, stderrStr, err := e.runGitWithStderr(args)
	if err != nil {
		e.logger.Warn("project %s 无法快进到 %s,保留本地状态: %v%s",
			p.Name, revid, err, stderrStr)
	}
	return nil
}

// checkoutRevision 执行 git checkout,带重试;最后一次重试追加 --force。
// target 为提交 SHA 或本地分支名;结尾的 "--" 防止同名文件触发路径歧义(上游同款)。
// ctx 取消时重试退避等待会被立即中断。
func (e *Engine) checkoutRevision(ctx context.Context, p *project.Project, target string, detach bool) error {
	buildArgs := func(force bool) []string {
		args := []string{"-C", p.Worktree, "checkout"}
		if e.options.ForceOverwrite || force {
			args = append(args, "--force")
		}
		if detach {
			args = append(args, "--detach")
		}
		return append(args, target, "--")
	}

	// Checkout retries (non-network, use lower limit)
	maxRetries := 2
	var lastErr error
	var stderrBuf bytes.Buffer

	for retryCount := 0; retryCount <= maxRetries; retryCount++ {
		// Check context cancellation between retries
		if retryCount > 0 {
			select {
			case <-ctx.Done():
				return &SyncError{
					ProjectName: p.Name,
					Phase:       "checkout",
					Err:         ctx.Err(),
					Timestamp:   time.Now(),
				}
			case <-time.After(time.Duration(retryCount) * time.Second):
			}

			e.logger.Info("retrying checkout for project %s revision %s (attempt %d)",
				p.Name, target, retryCount)
			stderrBuf.Reset()

			// On last attempt, try force checkout
			if retryCount == maxRetries {
				e.logger.Info("attempting force checkout for project %s", p.Name)
			}
		}

		_, stderrStr, runErr := e.runGitWithStderr(buildArgs(retryCount == maxRetries))
		stderrBuf.WriteString(stderrStr)
		lastErr = runErr

		if lastErr == nil {
			return nil
		}

		if retryCount == maxRetries {
			return &SyncError{
				ProjectName: p.Name,
				Phase:       "checkout",
				Err:         lastErr,
				Output:      stderrBuf.String(),
				Timestamp:   time.Now(),
				RetryCount:  retryCount,
			}
		}
	}
	return nil
}

// looksLikeCommitID 判断 revision 是否形如提交 ID(7-40 位十六进制)。
// clone 的 --branch 参数与 fetch/checkout 的 revision 解析共用此判定。
func looksLikeCommitID(rev string) bool {
	if len(rev) < 7 || len(rev) > 40 {
		return false
	}
	for _, c := range rev {
		// 德摩根变换：非十六进制字符即失败（数字/小写/大写三段均不命中）
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
			return false
		}
	}
	return true
}

// revisionFetchRefspec 构造 fetch 阶段的精确 revision refspec(对齐上游
// Sync_NetworkHalf):
//   - 分支名或 refs/heads/<branch>:+refs/heads/<branch>:refs/remotes/<remote>/<branch>
//   - 其他 refs/ 前缀(如 refs/tags/<tag>):+<rev>:<rev>
//   - 提交 ID:空(上游依赖通配 refspec 获取对象)
//
// 精确 refspec 使远端缺少 manifest 指定引用时在 fetch 阶段即报错。
func revisionFetchRefspec(p *project.Project) string {
	rev := p.Revision
	if rev == "" || looksLikeCommitID(rev) {
		return ""
	}
	if branch := revisionBranchName(rev); branch != "" {
		if tracking := remoteTrackingRef(p.RemoteName, branch); tracking != "" {
			return fmt.Sprintf("+refs/heads/%s:%s", branch, tracking)
		}
		return ""
	}
	return fmt.Sprintf("+%s:%s", rev, rev)
}

// projectExists 检查目目录是否存
// 兼容两种布局：
//   - 标准工作树：worktree/.git 目录
//   - 裸仓库/镜像：worktree 本身即 git 目录（HEAD/objects/refs 在顶层，无 .git 子目录）
func (e *Engine) projectExists(p *project.Project) (bool, error) {
	gitDir := filepath.Join(p.Worktree, ".git")
	_, err := os.Stat(gitDir)
	if os.IsNotExist(err) {
		// 无 .git 子目录：检查是否为裸仓库（mirror 模式克隆产物）
		if isBareGitRepo(p.Worktree) {
			return true, nil
		}
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// resolveReferenceRepo 为项目 name 在参考路径 reference 下定位参考仓库。
// 对齐上游 repo 语义：
//   - reference 本身是仓库（裸仓库或含 .git 的工作树）时原样返回，兼容直接传单个仓库路径；
//   - 否则视为目录，依次探测 DIR/<name>.git、DIR/<name>（上游 GitDir 映射规则）；
//   - 都不存在时返回空字符串，由调用方降级为不使用参考仓库。
func resolveReferenceRepo(reference, name string) string {
	if reference == "" || name == "" {
		return ""
	}
	if isGitRepoDir(reference) || isGitRepoDir(filepath.Join(reference, ".git")) {
		return reference
	}
	candidates := []string{
		filepath.Join(reference, name+".git"),
		filepath.Join(reference, name),
	}
	for _, c := range candidates {
		if isGitRepoDir(c) || isGitRepoDir(filepath.Join(c, ".git")) {
			return c
		}
	}
	return ""
}

// isGitRepoDir 判断目录是否具备 git 仓库（裸仓库）的必要布局。
func isGitRepoDir(dir string) bool {
	head := filepath.Join(dir, "HEAD")
	if _, err := os.Stat(head); err != nil {
		return false
	}
	for _, sub := range [...]string{"objects", "refs"} {
		if _, err := os.Stat(filepath.Join(dir, sub)); err != nil {
			return false
		}
	}
	return true
}

// isBareGitRepo 判断目录是否为有效的裸 git 仓库。
// git 标准布局要求 HEAD 文件与 objects/refs 目录同时存在；
// 仅 HEAD 不存在即可快速排除不完整克隆。
func isBareGitRepo(dir string) bool {
	headPath := filepath.Join(dir, "HEAD")
	if _, err := os.Stat(headPath); err != nil {
		return false
	}
	for _, sub := range [...]string{"objects", "refs"} {
		if _, err := os.Stat(filepath.Join(dir, sub)); err != nil {
			return false
		}
	}
	return true
}

// setupRemote ensures the project's git remote exists with the correct URL and mirror config.
// It handles both fresh clone (add remote) and existing repo (verify/update URL) scenarios.
func (e *Engine) setupRemote(p *project.Project, remoteURL string) error {
	output, err := e.runGitInDir(p.Worktree, "remote")
	if err != nil {
		return fmt.Errorf("failed to list remotes: %w", err)
	}

	if p.RemoteName == "" {
		p.RemoteName = "origin"
	}

	remotes := strings.Split(strings.TrimSpace(string(output)), "\n")
	remoteExists := false
	originExists := false
	for _, r := range remotes {
		if r == p.RemoteName {
			remoteExists = true
		}
		if r == "origin" {
			originExists = true
		}
	}

	// Remove conflicting 'origin' remote if the project uses a different remote name
	if originExists && p.RemoteName != "origin" {
		if e.options.Verbose {
			e.logger.Debug("project %s has default remote 'origin' but project specifies '%s', removing 'origin'",
				p.Name, p.RemoteName)
		}
		if _, err := e.runGitInDir(p.Worktree, "remote", "remove", "origin"); err != nil {
			e.logger.Warn("failed to remove 'origin' remote for project %s: %v", p.Name, err)
		}
	}

	if !remoteExists {
		// Remote does not exist, add it
		if _, err := e.runGitInDir(p.Worktree, "remote", "add", p.RemoteName, remoteURL); err != nil {
			return fmt.Errorf("failed to add remote: %w", err)
		}
		if e.options.Verbose {
			e.logger.Debug("added remote '%s' for project %s: %s", p.RemoteName, p.Name, remoteURL)
		}
	} else {
		// Remote exists, check if URL needs updating
		urlOutput, err := e.runGitInDir(p.Worktree, "remote", "get-url", p.RemoteName)
		if err != nil {
			return fmt.Errorf("failed to get remote URL: %w", err)
		}
		currentURL := strings.TrimSpace(string(urlOutput))
		if currentURL != remoteURL {
			if _, err := e.runGitInDir(p.Worktree, "remote", "set-url", p.RemoteName, remoteURL); err != nil {
				return fmt.Errorf("failed to update remote URL: %w", err)
			}
			if e.options.Verbose {
				e.logger.Debug("updated remote '%s' URL for project %s: %s", p.RemoteName, p.Name, remoteURL)
			}
		}
	}

	// Set mirror mode if applicable
	if e.options.Config != nil && e.options.Config.Mirror {
		if _, err := e.runGitInDir(
			p.Worktree,
			"config", "--add",
			fmt.Sprintf("remote.%s.mirror", p.RemoteName),
			"true",
		); err != nil {
			return fmt.Errorf("failed to set mirror config: %w", err)
		}
		if e.options.Verbose {
			e.logger.Debug("set mirror mode for project %s remote %s", p.Name, p.RemoteName)
		}
	}

	return nil
}

// ensureRemoteExists is an alias for setupRemote (unified remote management).
func (e *Engine) ensureRemoteExists(p *project.Project, remoteURL string) error {
	return e.setupRemote(p, remoteURL)
}

// pullLFS 拉取 LFS 文件
func (e *Engine) pullLFS(ctx context.Context, p *project.Project) error {
	// 检查是否安装了 git-lfs
	if _, err := exec.LookPath("git-lfs"); err != nil {
		// git-lfs 未安装，跳过
		return nil
	}

	// 检查仓库是否使LFS
	output, err := e.runGitInDir(p.Worktree, "lfs", "ls-files")
	if err != nil {
		// 可能不是 LFS 仓库，跳
		return nil
	}

	// 如果LFS 文件，执行拉
	if len(output) > 0 {
		// Runner 内部重试已关闭（重试由引擎层统一负责），
		// lfs pull 为网络操作，需要引擎层的重试与取消感知
		opts := RetryOptions{
			MaxRetries:  e.fetchRetries(),
			BaseDelay:   2 * time.Second,
			MaxDelay:    30 * time.Second,
			ShouldRetry: IsRetryableGitError,
		}
		if err := RetryWithBackoff(ctx, opts, func(attempt int) error {
			if attempt > 0 {
				e.logger.Info("retrying lfs pull for project %s (attempt %d)", p.Name, attempt)
			}
			if _, err := e.runGitInDir(p.Worktree, "lfs", "pull"); err != nil {
				return fmt.Errorf("LFS pull failed: %w", err)
			}
			return nil
		}); err != nil {
			return err
		}
	}

	return nil
}

// Cleanup 清理资源并释放内存
func (e *Engine) Cleanup() {
	// 停止工作池
	if e.workerPool != nil {
		e.workerPool.Stop()
	}

	// 清空错误列表
	e.errorsMu.Lock()
	e.errors = nil
	e.errorsMu.Unlock()

	// 清空目列表
	e.projects = nil

	// 清空缓存
	e.manifestCache = nil

	// 记录清理完成
	e.logger.Debug("同步引擎资源已清理完成")
}

// getProjects failed to get project列表
func (e *Engine) getProjects() ([]*project.Project, error) {
	// 如果已经有目列表，直接返回
	if len(e.projects) > 0 {
		return e.projects, nil
	}

	// failed to get project列表 - 修复参数类型
	projects, err := project.NewManagerFromManifest(e.manifest, e.config).GetProjectsInGroups(e.options.Groups)
	if err != nil {
		return nil, fmt.Errorf("failed to get projects: %w", err)
	}

	e.projects = projects

	return e.projects, nil
}

// reloadManifestFromCache 重新加载manifest
func (e *Engine) reloadManifestFromCache() error {
	if len(e.manifestCache) == 0 {
		return fmt.Errorf("manifest cache is empty")
	}

	// 解析缓存的manifest数据
	parser := manifest.NewParser()
	newManifest, err := parser.ParseFromBytes(e.manifestCache, e.options.Groups)
	if err != nil {
		return fmt.Errorf("failed to parse manifest from cache: %w", err)
	}

	// 更新引擎中的manifest
	e.manifest = newManifest

	// 重新failed to get project列表
	projects, err := project.NewManagerFromManifest(e.manifest, e.config).GetProjectsInGroups(e.options.Groups)
	if err != nil {
		return fmt.Errorf("failed to get projects from cached manifest: %w", err)
	}
	e.projects = projects

	return nil
}

// updateProjectsRevisionID 方法
func (e *Engine) updateProjectsRevisionID() (string, error) {
	// 创建超级目
	sp, err := NewSuperproject(e.manifest, e.options.Quiet)
	if err != nil {
		return "", fmt.Errorf("创建超级目failed: %w", err)
	}

	// Updating project的修订ID
	manifestPath, err := sp.UpdateProjectsRevisionID(e.projects)
	if err != nil {
		return "", fmt.Errorf("updating project revision ID failed: %w", err)
	}

	return manifestPath, nil
}

// runGit 执行 Git 命令（通过 Runner 接口），返回 stdout 和 error。
// 使用 Runner 提供的超时、重试和并发控制，替代直接 exec.Command。
func (e *Engine) runGit(args ...string) ([]byte, error) {
	if e.gitRunner == nil {
		e.gitRunner = git.NewRunner()
	}
	return e.gitRunner.Run(args...)
}

// runGitInDir 在指定目录执行 Git 命令，返回 stdout 和 error。
func (e *Engine) runGitInDir(dir string, args ...string) ([]byte, error) {
	if e.gitRunner == nil {
		e.gitRunner = git.NewRunner()
	}
	return e.gitRunner.RunInDir(dir, args...)
}

// runGitWithStderr 执行 Git 命令并捕获 stderr 输出。
// 返回 stdout、stderr 和 error，用于重试循环中需要分析 stderr 内容的场景。
func (e *Engine) runGitWithStderr(args []string) ([]byte, string, error) {
	if e.gitRunner == nil {
		e.gitRunner = git.NewRunner()
	}
	output, err := e.gitRunner.Run(args...)
	if err != nil {
		var gitErr *git.GitCommandError
		if errors.As(err, &gitErr) {
			return output, gitErr.Stderr, err
		}
		return output, "", err
	}
	return output, "", nil
}

// runGitWithStderrInDir 在指定目录执行 Git 命令并捕获 stderr 输出。
func (e *Engine) runGitWithStderrInDir(dir string, args []string) ([]byte, string, error) {
	if e.gitRunner == nil {
		e.gitRunner = git.NewRunner()
	}
	output, err := e.gitRunner.RunInDir(dir, args...)
	if err != nil {
		var gitErr *git.GitCommandError
		if errors.As(err, &gitErr) {
			return output, gitErr.Stderr, err
		}
		return output, "", err
	}
	return output, "", nil
}

// Run 执行同步操作
func (e *Engine) Run() error {
	// 初始化目列
	projects, err := e.getProjects()
	if err != nil {
		return fmt.Errorf("failed to get project列表failed: %w", err)
	}
	e.projects = projects

	// 执行同步操作
	return e.Sync()
}

// SetProjects 设置要同步的目列表
func (e *Engine) SetProjects(projects []*project.Project) {
	e.projects = projects
}

// updateSubmodules Updating project的 submodule
func (e *Engine) updateSubmodules(ctx context.Context, p *project.Project) error {
	// 检查是否启用 submodule 功能
	// 目级 sync-s="true" 也会启用 submodule 更新（对齐上游 repo）
	if !e.shouldUpdateSubmodules() && !p.SyncS {
		if e.options.Verbose {
			e.logger.Debug("目 %s: submodule 功能未启用，跳过", p.Name)
		}
		return nil
	}

	// 检查目是否包含 .gitmodules 文件
	gitmodulesPath := filepath.Join(p.Worktree, ".gitmodules")
	if _, err := os.Stat(gitmodulesPath); os.IsNotExist(err) {
		// 没有 .gitmodules 文件，跳过
		if e.options.Verbose {
			e.logger.Debug("目 %s 不包含 submodule", p.Name)
		}
		return nil
	}

	if !e.options.Quiet {
		e.logger.Info("正在Updating project %s 的 submodule...", p.Name)
	}

	// 执行 git submodule update --init --recursive
	args := []string{"-C", p.Worktree, "submodule", "update", "--init", "--recursive"}

	// 如果启用了 Quiet 模式，添加 --quiet 参数
	if e.options.Quiet {
		args = append(args, "--quiet")
	}

	// Runner 内部重试已关闭（重试由引擎层统一负责），
	// submodule update 为网络操作，需要引擎层的重试与取消感知
	opts := RetryOptions{
		MaxRetries:  e.fetchRetries(),
		BaseDelay:   2 * time.Second,
		MaxDelay:    30 * time.Second,
		ShouldRetry: IsRetryableGitError,
	}
	if err := RetryWithBackoff(ctx, opts, func(attempt int) error {
		if attempt > 0 {
			e.logger.Info("retrying submodule update for project %s (attempt %d)", p.Name, attempt)
		}
		_, stderrStr, err := e.runGitWithStderr(args)
		if err != nil {
			// submodule 更新failed
			errorMsg := stderrStr
			if errorMsg == "" {
				errorMsg = err.Error()
			}
			return fmt.Errorf("git submodule update failed: %s", errorMsg)
		}
		return nil
	}); err != nil {
		return err
	}

	if !e.options.Quiet {
		e.logger.Info("目 %s 的 submodule 更新成功", p.Name)
	}

	return nil
}

// shouldUpdateSubmodules 判断是否应该更新 submodule
func (e *Engine) shouldUpdateSubmodules() bool {
	// 优先检查命令行参数
	if e.options.FetchSubmodules {
		return true
	}

	// 检查配置文件
	if e.options.Config != nil && e.options.Config.Submodules {
		return true
	}

	return false
}
