package repo_sync

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
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

	msg := fmt.Sprintf("[%s] %s 在%s 阶段失败%s: %v",
		timeStr, e.ProjectName, e.Phase, retryInfo, e.Err)
	// GitCommandError.Error() 已携带 git 的 stderr/stdout；Output 通常与之相同，
	// 仅在错误文本尚未包含时才追加，避免 CI 日志中同一段诊断重复出现。
	if e.Output != "" {
		msg = appendDiagnosticOnce(msg, e.Output)
	}
	return msg
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

// summarizeSyncErrors 把同步过程中收集到的错误拆分为两类：
//   - real：项目自身的真实失败（需要向用户报告并计入失败数）；
//   - canceled：因 --fail-fast 或外部取消被中断的项目（不是项目失败，
//     不能与真实失败混在一起统计，否则进度行与最终汇总会互相矛盾）。
func summarizeSyncErrors(errs []error) (real []error, canceled int) {
	real = make([]error, 0, len(errs))
	for _, err := range errs {
		if err == nil {
			continue
		}
		if errors.Is(err, context.Canceled) {
			canceled++
			continue
		}
		real = append(real, err)
	}
	return real, canceled
}

// syncProgressStatus 返回进度行中单个项目的状态文案。
// 被取消的项目与真实失败必须区分显示：取消是 fail-fast/外部中断的结果，
// 不应让用户误以为大量项目同步失败。
func syncProgressStatus(err error) string {
	switch {
	case err == nil:
		return "完成"
	case errors.Is(err, context.Canceled):
		return "已取消"
	default:
		return "失败"
	}
}

// Options 包含同步引擎的选
// Options moved to options.go to avoid duplicate declarations

// Engine 同步引擎
type Engine struct {
	projects          []*project.Project
	config            *config.Config
	options           *Options
	logger            logger.Logger
	progressReport    progress.Reporter
	workerPool        *workerpool.WorkerPool
	repoRoot          string
	errors            []error
	errorsMu          sync.Mutex
	manifestCache     []byte
	manifest          *manifest.Manifest
	ctx               context.Context  // 添加 ctx 字段
	branchName        string           // 要检出的分支名称
	checkoutStats     *checkoutStats   // 检出操作的统计信息（repo checkout 命令使用）
	commitHash        string           // 要cherry-pick的提交哈希
	cherryPickStats   *cherryPickStats // cherry-pick操作的统计信息
	networkSem        chan struct{}    // 限制并发网络操作数（fetch/clone）
	checkoutSem       chan struct{}    // 限制并发检出操作数
	gitRunner         git.Runner       // 统一的 Git 命令执行器（替代直接 exec.Command）
	projectSubset     map[string]bool  // SetProjectSubset 指定的项目子集（smart-sync 重载后据此过滤）
	smartSyncImplicit bool             // 智能同步由清单 <default sync-smartsync> 隐式启用（允许告警回退）
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

	// 配置在构造函数中解析/加载一次，worker 只读，避免并发写入 e.config 的数据竞态
	resolvedConfig := options.Config
	if resolvedConfig == nil {
		if loaded, loadErr := config.Load(); loadErr == nil {
			resolvedConfig = loaded
			options.Config = loaded
		}
	}

	return &Engine{
		projects:       projects,
		config:         resolvedConfig, // 透传配置，供 smart-sync 重载清单等使用
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

	// 0. --default-remote：清单未给出项目 remote 名时用它兜底
	// （在 preSync/smart-sync 重载清单前先应用一次，重载后再次应用）。
	e.applyDefaultRemoteToProjects()

	// 1.2 预处理：超级目 / 智能同步 / HyperSync
	if err := e.preSync(ctx); err != nil {
		return err
	}

	// HyperSync 可能将同步范围缩减为已变更项目的子集。
	// 未配置 manifest-server 时（runner 常无条件传 --hyper-sync），
	// 必须安静地按全量项目同步，而不是打印 "cannot connect" 告警。
	syncProjects := e.projects
	switch {
	case !e.options.HyperSync:
		// 未请求 HyperSync，全量同步。
	case e.options.LocalOnly:
		// --local-only 禁止任何网络操作：不向清单服务器发 HTTP，退化为全量本地同步。
		e.logger.Info("--local-only 禁用网络：跳过 HyperSync 查询，按全量项目执行本地同步")
	case !e.hyperSyncAvailable():
		e.logger.Debug("HyperSync 已跳过：未配置 manifest-server，按全量项目同步")
	default:
		e.logger.Info("Using manifest server for HyperSync")
		hyper, err := e.getHyperSyncProjects()
		if err != nil {
			e.logger.Warn("Failed to get HyperSync projects，将同步所有项目: %v", err)
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
	var canceledCount int32

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
			// fail-fast/外部取消打断的项目不是项目自身的同步失败，单独计数，
			// 否则进度行会显示大量 "失败" 而最终汇总只报个别真实失败。
			canceled := err != nil && errors.Is(err, context.Canceled)
			switch {
			case err == nil:
				atomic.AddInt32(&successCount, 1)
			case canceled:
				atomic.AddInt32(&canceledCount, 1)
			default:
				atomic.AddInt32(&failCount, 1)
			}

			if !e.options.Quiet && e.progressReport != nil {
				status := syncProgressStatus(err)

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

				progressMsg := fmt.Sprintf("%s: %s (进度: %d/%d, 成功: %d, 失败: %d, 已取消: %d%s)",
					project.Name, status, current, totalProjects,
					atomic.LoadInt32(&successCount), atomic.LoadInt32(&failCount),
					atomic.LoadInt32(&canceledCount), etaStr)
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

	// 1.7 更新 .repo/project.list 并清理清单中已不存在的项目检出（对齐上游
	// UpdateProjectList）。fail-fast/外部取消时跳过：上游在 fail-fast 下同样
	// 不执行本地清理，避免被中断的同步顺带删除工作树。
	if ctx.Err() == nil {
		if cleanupErrs := e.updateProjectList(); len(cleanupErrs) > 0 {
			e.errorsMu.Lock()
			e.errors = append(e.errors, cleanupErrs...)
			e.errorsMu.Unlock()
		}
	}

	// 1.8 --auto-gc：Sync complete后对每个目运行 git gc --auto
	if e.options.AutoGC {
		e.runAutoGC(syncProjects)
	}

	// 计算总耗时
	totalDuration := time.Since(startTime)

	// 汇总错误：fail-fast 或外部取消发生后，在途项目会被 ctx 打断并返回
	// context.Canceled，这类错误不是项目自身的同步失败，必须与真实失败分开统计，
	// 否则进度条（"失败"）与最终汇总（只剩真实失败）会互相矛盾。
	realErrors, canceledErrs := summarizeSyncErrors(e.errors)
	completed := int(atomic.LoadInt32(&count))
	skipped := totalProjects - completed
	if skipped < 0 {
		skipped = 0
	}

	if len(realErrors) > 0 {
		if canceledErrs > 0 || skipped > 0 {
			e.logger.Error("Sync complete，有 %d 个项目失败，%d 个项目已取消，%d 个项目未执行，总耗时: %s",
				len(realErrors), canceledErrs, skipped, formatDuration(totalDuration))
		} else {
			e.logger.Error("Sync complete，有 %d 个项目失败，总耗时: %s",
				len(realErrors), formatDuration(totalDuration))
		}
		return NewMultiError(realErrors)
	}

	// 没有项目自身的失败，但同步被取消/中断（排队任务未执行）：不能静默返回成功。
	if ctx.Err() != nil || canceledErrs > 0 || skipped > 0 {
		e.logger.Warn("Sync 已取消，%d 个项目已取消，%d 个项目未执行，总耗时: %s",
			canceledErrs, skipped, formatDuration(totalDuration))
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fmt.Errorf("sync canceled: %w", ctxErr)
		}
		return fmt.Errorf("sync canceled: %d canceled, %d not synced", canceledErrs, skipped)
	}

	e.logger.Info("All projects synced，总耗时: %s", formatDuration(totalDuration))
	return nil
}

// preSync 执行同步前的可选预处理：超级目修订 ID 与智能同步清单重载。
// 任一特性仅在对应选启用时触发；未配置 manifest-server/superproject 时给出告警而非中断。
func (e *Engine) preSync(_ context.Context) error {
	// --smart-sync / --smart-tag：从 manifest-server 获取已审批清单并重载。
	// 清单 <default sync-smartsync="true"> 可隐式启用智能同步；隐式启用失败
	// （无服务器/网络错误）时告警并回退常规同步，显式请求则必须报错。
	implicitSmartSync := false
	if e.options.SmartSync || e.options.SmartTag != "" {
		implicitSmartSync = e.smartSyncImplicit
	} else if !e.options.LocalOnly && defaultSyncSmartSyncEnabled(e.manifest) {
		e.options.SmartSync = true
		e.smartSyncImplicit = true
		implicitSmartSync = true
	}
	if e.options.SmartSync || e.options.SmartTag != "" {
		if err := e.handleSmartSync(); err != nil {
			if implicitSmartSync {
				e.logger.Warn("smart sync 失败，回退到常规同步: %v", err)
				e.options.SmartSync = false
				e.smartSyncImplicit = false
			} else {
				return fmt.Errorf("smart-sync failed: %w", err)
			}
		}
		// handleSmartSync 重载了清单并刷新了 e.projects
	}

	// --use-superproject：用超级项目覆盖各项目的修订 ID。
	// 清单未定义 <superproject> 时按上游语义告警并跳过，不中断同步。
	if e.options.UseSuperproject {
		if e.manifest == nil || e.manifest.Superproject == nil {
			e.logger.Warn("清单中未定义 <superproject>，跳过 --use-superproject")
		} else if e.options.LocalOnly {
			// --local-only 禁止网络：不 fetch 超级项目，复用已有的
			// .repo/superproject-manifest.xml（上游 _ReloadManifest 语义）。
			if ok, err := e.applySuperprojectRevisionsFromFile(); err != nil {
				e.logger.Warn("本地复用超级项目清单 failed，跳过 --use-superproject: %v", err)
			} else if !ok {
				e.logger.Warn("--local-only 下未找到 .repo/superproject-manifest.xml，跳过 --use-superproject")
			}
		} else if _, err := e.updateProjectsRevisionID(); err != nil {
			return fmt.Errorf("use-superproject failed: %w", err)
		}
	}

	// --force-remove-dirty 的语义在 Sync 末尾 updateProjectList 中实现：
	// 仅影响清单中已不存在的项目检出，脏工作树是否强制删除由它决定。
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

	// 使用 git -C 参数在指定目录执行命令，避免 os.Chdir 的线程安全问题。
	// 优先复用引擎的 Runner（测试可注入零重试 Runner，也保持统一配置）。
	gitRunner := e.gitRunner
	if gitRunner == nil {
		gitRunner = git.NewRunner()
	}

	// 获取更新前的 HEAD 提交哈希以检查是否有更新
	oldHeadOutput, err := gitRunner.RunInDir(manifestProjectPath, "rev-parse", "HEAD")
	oldHead := strings.TrimSpace(string(oldHeadOutput))
	if err != nil {
		e.logger.Debug("获取当前 HEAD failed: %v", err)
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

	// -b 指定完整提交 ID（40 位 SHA-1 或 64 位 SHA-256）时，直接获取该对象
	// 并 reset 到它，绝不能回退到默认分支。
	if isCommitHash(manifestBranch) {
		if _, shaFetchErr := gitRunner.RunInDir(manifestProjectPath, "fetch", "origin", manifestBranch); shaFetchErr != nil {
			// 服务端可能禁止按 SHA fetch：退化为获取全部引用，只要本地已有
			// 该对象即可继续（“或 upstream ref”路径）。
			if _, fetchOriginErr := gitRunner.RunInDir(manifestProjectPath, "fetch", "origin"); fetchOriginErr != nil {
				if _, fetchAllErr := gitRunner.RunInDir(manifestProjectPath, "fetch", "--all"); fetchAllErr != nil {
					return fmt.Errorf("failed to fetch manifest repository: %w",
						errors.Join(shaFetchErr, fetchOriginErr, fetchAllErr))
				}
			}
			if _, objErr := gitRunner.RunInDir(manifestProjectPath, "cat-file", "-e", manifestBranch+"^{commit}"); objErr != nil {
				return fmt.Errorf("manifest revision %s not found in remote: %w", manifestBranch, shaFetchErr)
			}
		}

		e.logger.Debug("Resetting manifest to commit %s...", manifestBranch)
		if _, resetErr := gitRunner.RunInDir(manifestProjectPath, "reset", "--hard", manifestBranch); resetErr != nil {
			return fmt.Errorf("failed to reset manifest repository to %s: %w", manifestBranch, resetErr)
		}
		return e.finishManifestUpdate(manifestProjectPath, gitRunner, oldHead)
	}

	// 执行 fetch 操作：origin 与 --all 均失败必须返回错误，runSync 的致命
	// 处理才能生效，避免带着过期清单继续同步。
	e.logger.Debug("Fetching manifest repository updates...")
	if _, err = gitRunner.RunInDir(manifestProjectPath, "fetch", "origin"); err != nil {
		e.logger.Debug("fetch origin failed: %v, trying fetch --all", err)
		// 尝试获取所有远程分支
		if _, fetchErr := gitRunner.RunInDir(manifestProjectPath, "fetch", "--all"); fetchErr != nil {
			return fmt.Errorf("failed to fetch manifest repository: %w",
				errors.Join(err, fmt.Errorf("fetch --all: %w", fetchErr)))
		}
	}

	// 对齐上游 repo：使用 git reset --hard 而非 merge，确保 manifest 仓库
	// 始终与远程一致，避免 merge conflict 导致同步卡住。
	// 上游 manifestProject.Sync_NetworkHalf + Sync_LocalHalf 的核心行为就是
	// fetch 后 reset --hard 到远程分支。
	remoteRef := "origin/" + manifestBranch
	e.logger.Debug("Resetting manifest to %s...", remoteRef)
	if _, resetErr := gitRunner.RunInDir(manifestProjectPath, "reset", "--hard", remoteRef); resetErr != nil {
		// 仅当远端分支确实不存在时才允许回退到 FETCH_HEAD；分支存在却
		// reset 失败（如工作树被锁）必须报错，不能静默换目标。
		if _, verifyErr := gitRunner.RunInDir(manifestProjectPath, "rev-parse", "--verify", "refs/remotes/"+remoteRef); verifyErr == nil {
			return fmt.Errorf("failed to reset manifest repository to %s: %w", remoteRef, resetErr)
		}
		if _, fetchHeadErr := gitRunner.RunInDir(manifestProjectPath, "rev-parse", "--verify", "FETCH_HEAD"); fetchHeadErr != nil {
			return fmt.Errorf("remote branch %s not found and FETCH_HEAD is unavailable: %w", remoteRef, resetErr)
		}
		e.logger.Debug("remote branch %s does not exist, falling back to FETCH_HEAD", remoteRef)
		if _, fallbackErr := gitRunner.RunInDir(manifestProjectPath, "reset", "--hard", "FETCH_HEAD"); fallbackErr != nil {
			return fmt.Errorf("failed to update manifest repository: %w", fallbackErr)
		}
	}

	return e.finishManifestUpdate(manifestProjectPath, gitRunner, oldHead)
}

// finishManifestUpdate 在 manifest 仓库 HEAD 变化后重新生成 .repo/manifest.xml。
// 重新生成失败只记录日志（与原有行为一致），不阻断后续同步。
func (e *Engine) finishManifestUpdate(manifestProjectPath string, gitRunner git.Runner, oldHead string) error {
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

	// ParseFromFile 已在解析阶段递归处理 include 并把子清单项目合并进 Projects、
	// remote/notice 等，这里直接使用已合并的清单，禁止再次 Parse+Merge 覆盖。
	// 生成扁平清单前清空 Includes，避免输出仍引用子清单的 include 元素。
	mainManifest.Includes = nil

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

// fetchRetries 返回 fetch/clone 的重试次数，取自 --retry-fetches。
// 上游 repo 的选项默认值为 0（不额外重试）；0 是合法且必须可表达的配置，
// 因此这里不做哨兵值替换。
func (e *Engine) fetchRetries() int {
	if e.options == nil || e.options.RetryFetches < 0 {
		return 0
	}
	return e.options.RetryFetches
}

// minimumFetchRetries 是 fetch/clone 的最小重试次数（不含首次执行）。
// 上游 repo 的 project._Fetch 会执行 `retry_fetches = max(retry_fetches, 2)`，
// 即 --retry-fetches 为 0 时仍会尝试 2 次；repo-go 对齐该语义，避免单次
// transient SSH/网络抖动（connection reset、connection closed）直接判失败。
const minimumFetchRetries = 1

// effectiveFetchRetries 返回 fetch/clone 实际使用的重试次数：
// 取其配置值与上游最小值的较大者。
func (e *Engine) effectiveFetchRetries() int {
	if retries := e.fetchRetries(); retries > minimumFetchRetries {
		return retries
	}
	return minimumFetchRetries
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

	// 检查是否为镜像模式：项目布局（p.Mirror）优先，兼容配置驱动
	isMirror := p.Mirror
	if !isMirror && e.options.Config != nil && e.options.Config.Mirror {
		isMirror = true
		if !e.options.Quiet {
			e.logger.Debug("目 %s 使用镜像模式", p.Name)
		}
	}

	if !exists {
		// --local-only 明确禁止网络操作：缺失项目不能 clone，按上游返回同款错误，
		// 提示用户先执行网络同步。
		if e.options.LocalOnly {
			return &SyncError{
				ProjectName: p.Name,
				Phase:       "local_sync",
				Err: fmt.Errorf(
					"Cannot checkout %s due to missing network sync; Run `repo sync -n %s` first.",
					p.Name, p.Name),
				Timestamp: time.Now(),
			}
		}
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

			// 处理 submodule（如果启用）：失败必须计入项目失败，不能只打日志。
			if err := e.updateSubmodules(ctx, p); err != nil {
				return &SyncError{
					ProjectName: p.Name,
					Phase:       "submodule_update",
					Err:         err,
					Timestamp:   time.Now(),
				}
			}
		} else {
			e.logger.Info("镜像模式，跳过处理目 %s 的链接文件和复制文件", p.Name)
		}
	}

	return nil
}

// isAbsoluteRemoteURL 判断是否为已完整的远程地址：带 scheme（含 scp 风式）
// 或绝对文件路径。其余值（"."、".."、"platform"、"foo/bar" 等）都必须按
// 清单仓库 URL 做 urljoin，绝不能原样交给 git。
func isAbsoluteRemoteURL(raw string) bool {
	raw = strings.TrimSpace(raw)
	if strings.Contains(raw, "://") {
		return true
	}
	// scheme（如 ssh:host/path）或 scp 风式 [user@]host:path：冒号位于首个 '/' 之前。
	if i := strings.Index(raw, ":"); i > 0 && !strings.Contains(raw[:i], "/") {
		return true
	}
	return filepath.IsAbs(raw)
}

// resolveURLAgainstManifest 以清单仓库 URL 为基准，把 fetch/pushurl 基址按
// urljoin 语义解析为完整地址（非点前缀相对值如 "platform" 同样解析）。
// 无法解析时返回错误，绝不把相对值原样交给 git。
func resolveURLAgainstManifest(manifestURL, raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("URL 为空")
	}
	if manifestURL != "" {
		resolver := &config.Config{ManifestURL: manifestURL}
		if resolved := resolver.ResolveRelativeURL(raw); resolved != "" {
			return resolved, nil
		}
	}
	// 无 scheme 的相对本地清单路径：config.ResolveRelativeURL 以“缺少可用
	// baseline”返回空，这里按清单文件所在目录做本地 urljoin。
	if manifestURL != "" && !strings.Contains(manifestURL, "://") && !isAbsoluteRemoteURL(manifestURL) {
		return filepath.Join(filepath.Dir(filepath.FromSlash(manifestURL)), filepath.FromSlash(raw)), nil
	}
	return "", fmt.Errorf("无法基于清单 URL %q 解析相对地址 %q", manifestURL, raw)
}

// projectManifestBase 返回项目 URL 解析使用的清单仓库 URL（urljoin 基址）。
func (e *Engine) projectManifestBase(p *project.Project) string {
	if p != nil && p.ManifestURL != "" {
		return p.ManifestURL
	}
	return e.manifestRepositoryURL()
}

// resolveRelativeURL 将相对 fetch/pushurl 解析为完整项目 URL：
// 按 urljoin 语义（config.ResolveRelativeURL）基于清单仓库 URL 解析，
// 解析结果只拼接一次 "/<项目名>"。无法解析时返回错误，绝不静默保留相对路径。
func (e *Engine) resolveRelativeURL(p *project.Project, rawURL, kind string) (string, error) {
	rawURL = strings.TrimSpace(rawURL)
	name := ""
	if p != nil {
		name = p.Name
	}
	resolved, err := resolveURLAgainstManifest(e.projectManifestBase(p), rawURL)
	if err != nil {
		return "", fmt.Errorf("cannot resolve relative %s %q for project %s: %w", kind, rawURL, name, err)
	}
	// 解析后仍与原始相对值完全相同，说明清单基址未生效。
	if resolved == rawURL && !isAbsoluteRemoteURL(resolved) {
		return "", fmt.Errorf("cannot resolve relative %s %q for project %s", kind, rawURL, name)
	}
	resolved = strings.TrimSuffix(resolved, "/")
	if name != "" {
		resolved += "/" + strings.TrimSuffix(name, "/")
	}
	if e.options != nil && e.options.Verbose && e.logger != nil {
		e.logger.Debug("resolved relative %s %s to %s", kind, rawURL, resolved)
	}
	return resolved, nil
}

// resolveRemoteURL 解析项目的远程 URL。
// 绝对 URL（含 scp 风式与绝对路径）原样返回；其余相对 fetch（含非点前缀，
// 如 "platform"）走 urljoin 解析，并只拼接一次 "/<项目名>"。解析结果为
// 完整地址，再次调用会直接透传（幂等）。
func (e *Engine) resolveRemoteURL(p *project.Project) (string, error) {
	remoteURL := strings.TrimSpace(p.RemoteURL)
	// 清单构建的项目可能已有 "解析后基址/项目名" 的相对 RemoteURL
	// （manager 无 manifest URL 时无法解析）；优先用原始 FetchBase 重新解析，
	// 避免把项目名再拼一次。
	if !isAbsoluteRemoteURL(remoteURL) {
		if fetchBase := strings.TrimSpace(p.FetchBase); fetchBase != "" {
			if isAbsoluteRemoteURL(fetchBase) {
				return strings.TrimSuffix(fetchBase, "/") + "/" + strings.TrimSuffix(p.Name, "/"), nil
			}
			remoteURL = fetchBase
		}
	}
	if remoteURL == "" {
		remoteURL = ".."
	}
	if isAbsoluteRemoteURL(remoteURL) {
		return remoteURL, nil
	}
	return e.resolveRelativeURL(p, remoteURL, "fetch")
}

// manifestURLFromRepoDir 读取指定 .repo 元数据目录下 config.json 的 manifest_url。
// 该值是 urljoin 的基址（清单仓库 URL），不能截断成父目录。
func manifestURLFromRepoDir(repoDir string) string {
	if repoDir == "" {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(repoDir, "config.json"))
	if err != nil {
		return ""
	}
	var configData struct {
		ManifestURL string `json:"manifest_url"`
	}
	if err := json.Unmarshal(data, &configData); err != nil {
		return ""
	}
	return strings.TrimSpace(configData.ManifestURL)
}

// readManifestURLFromConfigFile 从 <repoRoot>/.repo/config.json 读取 manifest_url。
func (e *Engine) readManifestURLFromConfigFile() string {
	repoRoot := e.repoRoot
	if repoRoot == "" {
		repoRoot = "."
	}
	manifestURL := manifestURLFromRepoDir(filepath.Join(repoRoot, ".repo"))
	if manifestURL != "" && e.options != nil && e.options.Verbose && e.logger != nil {
		e.logger.Debug("read ManifestURL from config.json: %s", manifestURL)
	}
	return manifestURL
}

// manifestRepositoryURL 返回清单仓库 URL（urljoin 基址）。
// 优先使用已加载配置，其次回退到磁盘上的 .repo/config.json；
// 该函数可能在并发 worker 中调用，只读不写 e.config（竞态）。
func (e *Engine) manifestRepositoryURL() string {
	if e.config != nil && e.config.ManifestURL != "" {
		if e.options != nil && e.options.Verbose && e.logger != nil {
			e.logger.Debug("using loaded config ManifestURL: %s", e.config.ManifestURL)
		}
		return e.config.ManifestURL
	}
	if cfg, err := config.Load(); err == nil && cfg != nil && cfg.ManifestURL != "" {
		if e.options != nil && e.options.Verbose && e.logger != nil {
			e.logger.Debug("loaded config from file, ManifestURL: %s", cfg.ManifestURL)
		}
		return cfg.ManifestURL
	} else if err != nil && e.logger != nil {
		e.logger.Debug("failed to load config from file: %v", err)
	}
	return e.readManifestURLFromConfigFile()
}

// fetchProject 执行单个目的网络同
// ctx 取消时 RetryWithBackoff 会立即中断退避等待并返回取消错误。
func (e *Engine) fetchProject(ctx context.Context, p *project.Project) error {
	// 输出详细日志，显示实际使用的远程 URL
	if e.options.Verbose {
		e.logger.Debug("正在failed to get project %s，原始远URL: %s", p.Name, p.RemoteURL)
	}

	// 1.8 --optimized-fetch：仅当 manifest revision 本身形如提交 ID 且本地
	// 已包含该对象时跳过网络获取。分支/标签的远端可能已前进，必须正常 fetch
	// （即使超级项目覆盖写入了 RevisionID，也不能把分支当成不可变提交）。
	if e.options.OptimizedFetch && looksLikeCommitID(p.Revision) {
		if _, err := e.runGitInDir(projectRepoDir(p), "cat-file", "-e", p.Revision); err == nil {
			e.logger.Debug("目 %s 已包含修订 %s，跳过 fetch (optimized-fetch)", p.Name, p.Revision)
			return nil
		}
	}

	// 解析远程URL
	remoteURL, err := e.resolveRemoteURL(p)
	if err != nil {
		return &SyncError{
			ProjectName: p.Name,
			Phase:       "resolve_remote_url",
			Err:         err,
			Timestamp:   time.Now(),
		}
	}
	// Updating projectRemoteURL 为解析后URL
	p.RemoteURL = remoteURL
	if err := e.ensureRemoteExists(p, remoteURL); err != nil {
		return &SyncError{
			ProjectName: p.Name,
			Phase:       "ensure_remote",
			Err:         err,
			Timestamp:   time.Now(),
		}
	}

	// 执行 fetch 命令
	args := []string{"-C", projectRepoDir(p), "fetch"}

	// 检查是否为镜像模式：项目布局（p.Mirror）优先，兼容配置驱动
	isMirror := p.Mirror || (e.options.Config != nil && e.options.Config.Mirror)
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

	// Retry fetch using RetryWithBackoff; 上游 _Fetch 至少尝试 2 次，
	// 即使 --retry-fetches 为 0（transient 网络错误仍会重试一次）。
	maxRetries := e.effectiveFetchRetries()
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
	remoteURL, err := e.resolveRemoteURL(p)
	if err != nil {
		return &SyncError{
			ProjectName: p.Name,
			Phase:       "resolve_remote_url",
			Err:         err,
			Timestamp:   time.Now(),
		}
	}
	// Updating projectRemoteURL 为解析后URL
	p.RemoteURL = remoteURL

	// 确保RemoteName有值
	if p.RemoteName == "" {
		p.RemoteName = "origin"
		if e.options.Verbose {
			e.logger.Debug("目 %s 未指定远程名称，使用默认值 'origin'", p.Name)
		}
	}

	// 创建父目录（镜像项目的目标是 Gitdir=<topdir>/<name>.git）
	cloneDest := projectRepoDir(p)
	if cloneDest == "" {
		return &SyncError{
			ProjectName: p.Name,
			Phase:       "clone",
			Err:         fmt.Errorf("project %s has no clone destination", p.Name),
			Timestamp:   time.Now(),
		}
	}
	if err := os.MkdirAll(filepath.Dir(cloneDest), 0755); err != nil {
		return &SyncError{
			ProjectName: p.Name,
			Phase:       "mkdir",
			Err:         err,
			Timestamp:   time.Now(),
		}
	}

	// 检查是否为镜像模式：项目布局（p.Mirror）优先，兼容配置驱动
	isMirror := p.Mirror || (e.options.Config != nil && e.options.Config.Mirror)

	// 嵌套项目（如 A 的 worktree 位于 B 的 worktree 之下）不能直接
	// `git clone` 到 worktree：并发时子项目会先 MkdirAll 出祖先目录，
	// 或用父项目已检出的内容占用目标路径，导致 git 报
	// “目标路径已存在且不是空目录”。这类项目先克隆到 .repo/tmp，
	// 成功后再合并到最终 worktree。
	nested := e.hasNestedProjectPath(cloneDest)

	// 构建 clone 命令；嵌套项目每次尝试都指向独立的临时目录，
	// 在循环内按临时路径构造，避免重复记录分支日志。
	var args []string
	if !nested {
		args = e.buildCloneArgs(p, remoteURL, isMirror)
	}

	// 1.6 限制并发网络操作数（--jobs-network）；clone 是网络密集型操作
	e.networkSem <- struct{}{}
	defer func() { <-e.networkSem }()

	// 上游 clone 走 _Fetch，重试次数同样取 max(--retry-fetches, 2) 的尝试预算。
	maxRetries := e.effectiveFetchRetries()
	// 记录本次运行前的目标目录状态：只有“本次运行前不存在/为空”的目录
	// 才能在重试时安全清理；含用户数据的目录必须显式 --force-sync。
	targetState := inspectCloneTarget(cloneDest)
	// 运行前已存在且非空的目录：无 --force-sync 时直接拒绝，避免触碰用户数据。
	// 嵌套项目路径下的目录可能由其他项目创建，不属于用户数据，放行合并。
	if !nested && targetState.existed && !targetState.empty && !e.options.ForceSync && !isBareGitRepo(cloneDest) {
		return &SyncError{
			ProjectName: p.Name,
			Phase:       "clone",
			Err: fmt.Errorf(
				"destination %s already exists; refusing to remove and re-clone without --force-sync",
				cloneDest),
			Timestamp: time.Now(),
		}
	}
	var lastErr error
	var stderr bytes.Buffer

	// --force-sync 下运行前已存在的非空目录视为损坏/残留：必须在第一次
	// clone（retryCount==0）前清理，否则 --retry-fetches 0 时没有任何重试
	// 机会执行清理，force-sync 无法恢复。有效裸仓库与删除安全校验仍由
	// cleanupIncompleteCloneState 负责。嵌套路径不能删除：目录里可能已有
	// 已克隆的子项目工作树。
	cleanupBeforeFirstClone := !nested && e.options.ForceSync && targetState.existed && !targetState.empty
	if cleanupBeforeFirstClone {
		if cleanupErr := e.cleanupIncompleteCloneState(p, targetState); cleanupErr != nil {
			return &SyncError{
				ProjectName: p.Name,
				Phase:       "clone",
				Err:         cleanupErr,
				Timestamp:   time.Now(),
				RetryCount:  0,
			}
		}
	}

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

			// 清理上次克隆残留：本次运行前为空/不存在的目录可直接清理，
			// 含用户数据的目录仅在 --force-sync 下允许。嵌套项目直接克隆
			// 到临时目录，失败只清理临时目录，不能触碰最终 worktree。
			if !nested {
				if cleanupErr := e.cleanupIncompleteCloneState(p, targetState); cleanupErr != nil {
					return &SyncError{
						ProjectName: p.Name,
						Phase:       "clone",
						Err:         cleanupErr,
						Timestamp:   time.Now(),
						RetryCount:  retryCount,
					}
				}
			}
		}

		// 嵌套项目克隆到 .repo/tmp 下的临时目录，成功后合并到最终路径，
		// 避免与父/子项目并发创建目录产生“目标已存在且非空”冲突。
		tempDest := ""
		cloneArgs := args
		if nested {
			created, createErr := e.newCloneTempDir()
			if createErr != nil {
				return &SyncError{
					ProjectName: p.Name,
					Phase:       "clone",
					Err:         fmt.Errorf("failed to create temporary clone directory: %w", createErr),
					Timestamp:   time.Now(),
					RetryCount:  retryCount,
				}
			}
			tempDest = created
			cloneArgs = e.buildCloneArgsForDest(p, remoteURL, isMirror, tempDest)
		}
		cleanupTemp := func() {
			if tempDest != "" {
				_ = os.RemoveAll(tempDest)
				tempDest = ""
			}
		}

		// 执行 clone 命令
		_, stderrStr, runErr := e.runGitWithStderr(cloneArgs)
		stderr.WriteString(stderrStr)
		lastErr = runErr

		if lastErr == nil {
			if nested {
				if finErr := e.relocateNestedClone(cloneDest, tempDest); finErr != nil {
					cleanupTemp()
					return &SyncError{
						ProjectName: p.Name,
						Phase:       "clone",
						Err:         finErr,
						Timestamp:   time.Now(),
						RetryCount:  retryCount,
					}
				}
				cleanupTemp()
			}
			// 成功克隆，跳出重试循环
			break
		}

		cleanupTemp()

		// ctx 在 clone 执行期间被取消（fail-fast/外部取消）：立即返回，
		// 并保留 ctx 语义供 errors.Is 检测。
		if ctxErr := ctx.Err(); ctxErr != nil {
			return &SyncError{
				ProjectName: p.Name,
				Phase:       "clone",
				Err:         fmt.Errorf("clone canceled (last error: %w): %w", lastErr, ctxErr),
				Output:      stderr.String(),
				Timestamp:   time.Now(),
				RetryCount:  retryCount,
			}
		}

		// git clone 在 checkout 阶段失败时会留下可用的 .git（对象已下载）。
		// 若目录是本次运行新建/原本为空，尝试恢复工作树而不是直接放弃。
		// 嵌套项目在临时目录内克隆，失败后临时目录已清理，不做原地恢复。
		if !nested && !isMirror && e.canReuseCloneTarget(targetState) && e.recoverIncompleteCloneCheckout(p) {
			e.logger.Warn("clone for project %s failed during checkout; recovered existing repository", p.Name)
			lastErr = nil
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

		// 处理 submodule（如果启用）：失败必须计入项目失败，不能只打日志。
		if err := e.updateSubmodules(ctx, p); err != nil {
			return &SyncError{
				ProjectName: p.Name,
				Phase:       "submodule_update",
				Err:         err,
				Timestamp:   time.Now(),
			}
		}
	} else if isMirror {
		e.logger.Info("镜像模式，跳过处理目 %s 的链接文件和复制文件", p.Name)
	} else {
		e.logger.Info("NetworkOnly模式，跳过处理目 %s 的链接文件和复制文件", p.Name)
	}

	return nil
}

// cloneTargetState 记录一次 clone 开始前的目标目录状态。
// 仅用于判断重试时能否安全清理残留，不会在运行中更新。
type cloneTargetState struct {
	existed bool
	empty   bool
}

// inspectCloneTarget 探测目标目录在本次运行前的状态。
func inspectCloneTarget(worktree string) cloneTargetState {
	entries, err := os.ReadDir(worktree)
	if err != nil {
		if os.IsNotExist(err) {
			return cloneTargetState{existed: false}
		}
		// 无法读取时保守视为非空已存在目录，避免误删。
		return cloneTargetState{existed: true, empty: false}
	}
	return cloneTargetState{existed: true, empty: len(entries) == 0}
}

// canReuseCloneTarget 判断目标目录是否由本次运行创建/原本为空，
// 因而其内容可视为克隆残留（而非用户数据）。--force-sync 时允许回收任意目录。
func (e *Engine) canReuseCloneTarget(state cloneTargetState) bool {
	return e.options.ForceSync || !state.existed || state.empty
}

// cleanupIncompleteClone 在克隆重试前处理已存在的目标目录。
// 直接调用时以当前目录状态判定（空目录可清理，非空目录需 --force-sync）；
// cloneProject 使用 cleanupIncompleteCloneState 传入真实的运行前状态，
// 以便清理本次克隆写入的残留。
// 完整裸仓库不是克隆残留，永不删除；删除前经 IsSafeToDelete 容器校验。
func (e *Engine) cleanupIncompleteClone(p *project.Project) error {
	return e.cleanupIncompleteCloneState(p, inspectCloneTarget(projectRepoDir(p)))
}

// cleanupIncompleteCloneState 依据运行前状态清理克隆目标。
//   - 本次运行前不存在/为空的目录：可直接删除重试（不丢用户数据）；
//   - 本次运行前已存在且非空：必须显式 --force-sync，否则拒绝；
//   - 有效裸仓库永不删除。
func (e *Engine) cleanupIncompleteCloneState(p *project.Project, state cloneTargetState) error {
	dest := projectRepoDir(p)
	if _, statErr := os.Stat(dest); statErr != nil {
		if os.IsNotExist(statErr) {
			return nil
		}
		return fmt.Errorf("failed to inspect clone destination %s: %w", dest, statErr)
	}

	if !e.canReuseCloneTarget(state) {
		return fmt.Errorf(
			"destination %s already exists; refusing to remove and re-clone without --force-sync",
			dest)
	}

	if isBareGitRepo(dest) {
		e.logger.Warn(
			"target %s already exists and is a valid bare repository, refusing to remove and re-clone",
			dest)
		return fmt.Errorf(
			"destination %s already exists and is a valid bare repository, refusing to delete",
			dest)
	}

	// 容器安全检查：filepath.Rel + 分隔符感知，禁止删除 repo 根目录
	// 本身及其兄弟目录（HasPrefix 误判）。
	if safeErr := IsSafeToDelete(dest, e.repoRoot); safeErr != nil {
		return fmt.Errorf("refusing to remove %s: %w", dest, safeErr)
	}

	e.logger.Info("removing incomplete clone directory: %s", dest)
	if err := os.RemoveAll(dest); err != nil {
		return fmt.Errorf("failed to remove incomplete clone directory %s: %w", dest, err)
	}
	return nil
}

// recoverIncompleteCloneCheckout 尝试修复 clone 在 checkout 阶段失败后留下的
// 可用仓库（对象已下载、HEAD 已设置）。成功时工作树被 reset 到 HEAD。
// 返回 false 表示无法安全恢复（例如 .git 缺失或 HEAD 不可解析）。
func (e *Engine) recoverIncompleteCloneCheckout(p *project.Project) bool {
	gitDir := filepath.Join(projectRepoDir(p), ".git")
	if _, err := os.Stat(gitDir); err != nil {
		if isBareGitRepo(p.Worktree) {
			gitDir = p.Worktree
		} else {
			return false
		}
	}
	if !isGitRepoDir(gitDir) {
		return false
	}

	if _, err := e.runGitInDir(projectRepoDir(p), "rev-parse", "--verify", "HEAD"); err != nil {
		// HEAD 尚未建立（clone 在早期失败），无法安全恢复。
		return false
	}

	if _, err := e.runGitInDir(projectRepoDir(p), "reset", "--hard", "HEAD"); err != nil {
		e.logger.Debug("failed to recover checkout for project %s: %v", p.Name, err)
		return false
	}
	return true
}

// buildCloneArgs 构造 git clone 参数（不含执行），便于对参数构造做单元测试。
// isMirror 由调用方根据配置判定并用于跳过分支参数。
func (e *Engine) buildCloneArgs(p *project.Project, remoteURL string, isMirror bool) []string {
	return e.buildCloneArgsForDest(p, remoteURL, isMirror, projectRepoDir(p))
}

// buildCloneArgsForDest 与 buildCloneArgs 相同，但允许指定目标目录。
// 嵌套项目先克隆到临时目录，再通过 relocateNestedClone 合并到最终 worktree。
func (e *Engine) buildCloneArgsForDest(p *project.Project, remoteURL string, isMirror bool, dest string) []string {
	args := []string{"clone"}

	// --no-clone-bundle：repo-go 不实现 /clone.bundle 下载，保持 no-op，
	// 绝不能把该选项透传给 git clone（git 会因未知选项直接失败）。

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

	// --network-only 只获取对象、不落工作树：非镜像克隆追加 --no-checkout，
	// 克隆后的 checkout/linkfile/copyfile/submodule 也全部跳过。
	// 镜像克隆本身产物就是裸仓库，无需该参数。
	if e.options.NetworkOnly && !isMirror {
		args = append(args, "--no-checkout")
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
	args = append(args, remoteURL, dest)

	return args
}

// hasNestedProjectPath 判断 dest 与任一其他项目的工作树是否存在祖先/后代关系。
// 嵌套项目并发 clone 时会互相创建目标路径（如子项目先 MkdirAll 出父目录），
// 因此必须走“临时目录 clone + 合并”路径，不能直接 clone 到 worktree。
func (e *Engine) hasNestedProjectPath(dest string) bool {
	if dest == "" {
		return false
	}
	destAbs, err := filepath.Abs(dest)
	if err != nil {
		destAbs = filepath.Clean(dest)
	}
	for _, q := range e.projects {
		if q == nil || q.Mirror {
			continue
		}
		qDir := projectRepoDir(q)
		if qDir == "" {
			continue
		}
		qAbs, absErr := filepath.Abs(qDir)
		if absErr != nil {
			continue
		}
		if qAbs == destAbs {
			continue
		}
		if pathIsWithin(destAbs, qAbs) || pathIsWithin(qAbs, destAbs) {
			return true
		}
	}
	return false
}

// pathIsWithin 判断 child 是否位于 parent 之下（不包含相等路径）。
func pathIsWithin(parent, child string) bool {
	rel, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	return rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// newCloneTempDir 在 .repo/tmp 下创建本次 clone 的临时目录。
// 与最终 worktree 位于同一文件系统，保证成功后可用 rename 原子落位。
// 临时目录先以 0700 隔离，落位后由 relocateNestedClone 调整为 0755，
// 与普通 git clone 创建的工作树权限一致。
func (e *Engine) newCloneTempDir() (string, error) {
	root := os.TempDir()
	if e.repoRoot != "" {
		root = filepath.Join(e.repoRoot, ".repo", "tmp")
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		return "", fmt.Errorf("failed to create clone temp root %s: %w", root, err)
	}
	if err := os.Chmod(root, 0700); err != nil {
		return "", fmt.Errorf("failed to secure clone temp root %s: %w", root, err)
	}
	return os.MkdirTemp(root, "clone-")
}

// relocateNestedClone 把临时 clone 目录落位到最终 worktree。
// 目标不存在/为空时直接 rename；目标已由父/子项目创建时递归合并，
// 保留已有项目内容并写入本次 clone 的 .git 与工作树文件。
func (e *Engine) relocateNestedClone(dest, tempDest string) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0755); err != nil {
		return fmt.Errorf("failed to create parent of %s: %w", dest, err)
	}
	// MkdirTemp 默认 0700；落位后应表现为普通 git clone 的 0755 目录。
	if err := os.Chmod(tempDest, 0755); err != nil {
		return fmt.Errorf("failed to set permissions on temporary clone %s: %w", tempDest, err)
	}

	info, err := os.Stat(dest)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.Rename(tempDest, dest); err != nil {
			return fmt.Errorf("failed to move cloned project into %s: %w", dest, err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("failed to inspect clone destination %s: %w", dest, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("clone destination %s exists and is not a directory", dest)
	}

	entries, err := os.ReadDir(dest)
	if err != nil {
		return fmt.Errorf("failed to read clone destination %s: %w", dest, err)
	}
	if len(entries) == 0 {
		if err := os.Remove(dest); err != nil {
			return fmt.Errorf("failed to remove empty clone destination %s: %w", dest, err)
		}
		if err := os.Rename(tempDest, dest); err != nil {
			return fmt.Errorf("failed to move cloned project into %s: %w", dest, err)
		}
		return nil
	}

	// 目标非空：只允许合并“由其他嵌套项目创建”的目录，不能覆盖已有 git 仓库。
	if isBareGitRepo(dest) {
		return fmt.Errorf("clone destination %s already contains a bare git repository", dest)
	}
	if _, statErr := os.Stat(filepath.Join(dest, ".git")); statErr == nil {
		return fmt.Errorf("clone destination %s already contains a git repository", dest)
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return fmt.Errorf("failed to inspect %s/.git: %w", dest, statErr)
	}

	e.logger.Debug("merging nested clone for %s into existing directory", dest)
	if err := mergeCloneTree(tempDest, dest); err != nil {
		return fmt.Errorf("failed to merge nested clone into %s: %w", dest, err)
	}
	return nil
}

// mergeCloneTree 把 src 目录内容递归合并进 dst：
//   - 目标不存在：直接 rename（同文件系统，O(1)）；
//   - 两侧都是目录：递归合并；
//   - 其余冲突：用源文件替换目标文件，目标为目录时拒绝，避免静默破坏。
func mergeCloneTree(src, dst string) error {
	entries, err := os.ReadDir(src)
	if err != nil {
		return fmt.Errorf("failed to read %s: %w", src, err)
	}
	for _, entry := range entries {
		from := filepath.Join(src, entry.Name())
		to := filepath.Join(dst, entry.Name())

		toInfo, statErr := os.Lstat(to)
		if errors.Is(statErr, os.ErrNotExist) {
			if err := os.Rename(from, to); err != nil {
				return fmt.Errorf("failed to move %s to %s: %w", from, to, err)
			}
			continue
		}
		if statErr != nil {
			return fmt.Errorf("failed to inspect %s: %w", to, statErr)
		}

		fromInfo, infoErr := entry.Info()
		if infoErr != nil {
			return fmt.Errorf("failed to inspect %s: %w", from, infoErr)
		}
		if fromInfo.IsDir() && toInfo.IsDir() {
			if err := mergeCloneTree(from, to); err != nil {
				return err
			}
			continue
		}
		if toInfo.IsDir() {
			return fmt.Errorf("cannot merge %s: destination is a directory", from)
		}
		if err := os.Rename(from, to); err != nil {
			return fmt.Errorf("failed to move %s to %s: %w", from, to, err)
		}
	}
	return nil
}

// checkoutProject 将工作树对齐到 manifest revision,语义对齐上游 Sync_LocalHalf:
//  1. 先把 revision 解析为具体提交 SHA(GetRevisionId 语义),解析失败报
//     "revision X in Y not found";绝不把分支名直接传给 git checkout——
//     工作树内存在同名文件时 git 会报引用/路径歧义(如 toolchain/gcc 项目)
//  2. HEAD 已指向 revid:默认不执行任何 git 命令(上游:No changes);
//     但显式 --detach 且当前仍附着分支时,仍需 checkout --detach
//  3. --detach 或游离 HEAD:checkout [--force] --detach <revid> --(上游 _Checkout);
//     --force 仅在 --force-sync/--force-overwrite 时追加
//     3.5 进入分支语义前先补齐空工作树（sync -n 遗留的 index/工作树为空），
//     否则 merge --ff-only 会因“tracked 文件被删除”失败；只填充当前 HEAD，
//     不移动分支，已有 tracked 内容时拒绝 reset。
//  4. 在 revision 分支上(或当前分支跟踪该 revision):merge --ff-only 快进
//     (上游 _FastForward);失败返回错误(项目计为失败,提示人工处理),不自动 rebase/reset-hard
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

	// 解析当前 HEAD 状态，供"已就位"与 detach 判定共用
	headSha := e.gitHeadSha(p)
	headBranch := e.gitHeadBranch(p)

	// 空工作树补齐必须发生在进入 switch 之前：`repo sync -n` 留下空 index/
	// 工作树，其后远端前进时 HEAD 仍指向旧提交，直接 merge --ff-only 会因
	// 所有 tracked 文件“被删除”而失败。以当前 HEAD 补齐（不移动分支），
	// 保留后续 ff/detach 语义；已有 tracked 内容的目录不会被 reset。
	if headSha != "" {
		if err := e.ensureWorktreePopulated(ctx, p, headSha); err != nil {
			return &SyncError{
				ProjectName: p.Name,
				Phase:       "checkout",
				Err:         err,
				Timestamp:   time.Now(),
			}
		}
	}

	// 已就位：默认不执行任何 checkout；但 --detach 明确要求游离 HEAD 时，
	// 若当前仍附着在分支上，仍需 checkout --detach 把 HEAD 分离。
	if headSha == revid {
		if e.options.Detach && headBranch != "" {
			e.checkoutSem <- struct{}{}
			defer func() { <-e.checkoutSem }()
			return e.checkoutRevision(ctx, p, revid, true)
		}
		return nil
	}

	// Limit concurrent checkout operations (--jobs-checkout)
	e.checkoutSem <- struct{}{}
	defer func() { <-e.checkoutSem }()

	branch := revisionBranchName(p.Revision)

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

// ensureWorktreePopulated 修复“HEAD 已指向目标提交但工作树/index 为空”的状态。
// 典型来源：--network-only 的 git clone --no-checkout，或 clone 在 checkout
// 阶段失败后重试复用残留仓库。
//
// 安全约束：
//  1. 裸仓库（含镜像）没有工作树，直接返回 nil——在裸仓库执行 reset --hard
//     会失败，且绝不能借“补检出”之名改写引用；
//  2. 只有确认工作树确实没有 HEAD tree 内容（ls-tree 路径全部 Lstat 不存在，
//     且 status 无工作树列改动）才执行 `git reset --hard <revid>`，
//     避免 index 丢失但文件仍在时覆盖用户改动。
func (e *Engine) ensureWorktreePopulated(ctx context.Context, p *project.Project, revid string) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	repoDir := projectRepoDir(p)
	if repoDir == "" {
		return nil
	}

	// 裸仓库/镜像项目：没有工作树，不能 reset --hard。
	if p.Mirror || isBareGitRepo(repoDir) {
		return nil
	}

	// 再确认 git 认为当前目录是工作树（兼容 submodule/.git 文件等布局）。
	insideWorkTree, err := e.runGitInDir(repoDir, "rev-parse", "--is-inside-work-tree")
	if err != nil || strings.TrimSpace(string(insideWorkTree)) != "true" {
		return nil
	}

	// index 非空说明工作树已有内容，不做任何处理。
	indexFiles, err := e.runGitInDir(repoDir, "ls-files", "-z")
	if err != nil {
		return nil
	}
	if len(indexFiles) > 0 {
		return nil
	}

	// HEAD tree 为空（如空仓库）时无需检出。
	treeFiles, err := e.runGitInDir(repoDir, "ls-tree", "-r", "--name-only", "-z", "HEAD")
	if err != nil || len(bytes.TrimSpace(treeFiles)) == 0 {
		return nil
	}

	// 工作树中只要有任一 HEAD tree 路径存在，就说明内容尚在，
	// reset --hard 可能覆盖用户改动，必须放弃。
	hasContent, err := worktreeHasTrackedContent(repoDir, treeFiles)
	if err != nil {
		return err
	}
	if hasContent {
		return nil
	}

	// 进一步确认 status 没有工作树列（Y）改动。index 为空时出现
	// “D  path”（X=D、Y=空格）是正常的，不能据此阻止补检出。
	if statusOut, statusErr := e.runGitInDir(repoDir, "status", "--porcelain", "--untracked-files=no"); statusErr == nil {
		if statusHasWorktreeChanges(statusOut) {
			return nil
		}
	}

	if _, err := e.runGitInDir(repoDir, "reset", "--hard", revid); err != nil {
		return fmt.Errorf("failed to populate empty worktree for %s: %w", p.Name, err)
	}
	return nil
}

// worktreeHasTrackedContent 判断 HEAD tree 中的路径是否至少有一个存在于工作树。
// 返回 false 表示工作树确实没有 HEAD tree 内容（可安全补检出）。
// 无法确认（Lstat 非 IsNotExist 错误）时保守返回 true。
func worktreeHasTrackedContent(repoDir string, treeFiles []byte) (bool, error) {
	paths := bytes.Split(bytes.TrimRight(treeFiles, "\x00"), []byte{0})
	for _, rel := range paths {
		if len(rel) == 0 {
			continue
		}
		if _, statErr := os.Lstat(filepath.Join(repoDir, string(rel))); statErr == nil {
			return true, nil
		} else if !os.IsNotExist(statErr) {
			return true, nil
		}
	}
	return false, nil
}

// statusHasWorktreeChanges 判断 porcelain status 输出是否存在工作树列（Y）改动。
// 输出格式为 "XY <path>"：X=index、Y=worktree；只检查 Y 列，
// 避免 index 为空时的 “D ” 记录阻止补检出。
func statusHasWorktreeChanges(statusOut []byte) bool {
	for _, line := range strings.Split(string(statusOut), "\n") {
		if len(line) < 2 {
			continue
		}
		if y := line[1]; y != ' ' && y != '?' {
			return true
		}
	}
	return false
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
	// 超级项目覆盖：RevisionID 为完整 SHA 时优先使用它（对齐上游
	// GetRevisionId 中 revisionId 覆盖 revisionExpr 的语义），使
	// --use-superproject 写入的提交真正生效。
	if revisionID := strings.TrimSpace(p.RevisionID); isFullCommitID(revisionID) {
		return revisionID, nil
	}

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
		out, err := e.runGitInDir(projectRepoDir(p), "rev-parse", "--verify", "--quiet", ref+"^0")
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
	out, err := e.runGitInDir(projectRepoDir(p), "rev-parse", "--verify", "--quiet", "HEAD")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// gitHeadBranch 返回 HEAD 的分支引用全名(如 refs/heads/master);游离 HEAD 时为空。
func (e *Engine) gitHeadBranch(p *project.Project) string {
	out, err := e.runGitInDir(projectRepoDir(p), "symbolic-ref", "-q", "HEAD")
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
	out, err := e.runGitInDir(projectRepoDir(p), "config", "--get", key)
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
	_, err := e.runGitInDir(projectRepoDir(p), "rev-parse", "--verify", "--quiet", "refs/heads/"+branch)
	return err == nil
}

// fastForwardTo 将当前分支快进到 revid(上游 _FastForward)。
// --ff-only 失败时返回带 stderr 的错误(项目计为失败)，绝不返回 nil 冒充成功；
// 与上游一致不做 rebase/reset --hard，避免破坏用户本地提交。
func (e *Engine) fastForwardTo(p *project.Project, revid string) error {
	args := []string{"-C", projectRepoDir(p), "merge", "--no-stat", "--ff-only", revid}
	_, stderrStr, err := e.runGitWithStderr(args)
	if err != nil {
		return &SyncError{
			ProjectName: p.Name,
			Phase:       "checkout",
			Err:         fmt.Errorf("merge --ff-only %s failed: %w", revid, err),
			Output:      stderrStr,
			Timestamp:   time.Now(),
		}
	}
	return nil
}

// checkoutRevision 执行 git checkout,带重试。
// 仅当显式 --force-sync/--force-overwrite 时才追加 --force；普通重试绝不在
// 最后一轮偷偷强制覆盖脏工作树，重试耗尽即返回错误。
// target 为提交 SHA 或本地分支名;结尾的 "--" 防止同名文件触发路径歧义(上游同款)。
// ctx 取消时重试退避等待会被立即中断。
func (e *Engine) checkoutRevision(ctx context.Context, p *project.Project, target string, detach bool) error {
	args := []string{"-C", projectRepoDir(p), "checkout"}
	if e.options.ForceSync || e.options.ForceOverwrite {
		args = append(args, "--force")
	}
	if detach {
		args = append(args, "--detach")
	}
	args = append(args, target, "--")

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
		}

		_, stderrStr, runErr := e.runGitWithStderr(args)
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

// looksLikeCommitID 判断 revision 是否为不可变提交 ID（40 位 SHA-1 或
// 64 位 SHA-256，小写十六进制）。与 isCommitHash 共用同一判定：只有完整
// 提交 ID 才视为不可变对象；7-40 位十六进制字符串仍可能是合法分支名
// （如 "deadbeef"），必须按分支解析（refs/remotes → refs/heads）。
func looksLikeCommitID(rev string) bool {
	return isCommitHash(rev)
}

// isCommitHash 判断字符串是否为完整的提交 ID：40 位 SHA-1 或 64 位 SHA-256。
// 只接受小写十六进制（git 输出的对象名形式）；大写或短 SHA 均按分支/引用处理。
// 用于识别 -b 指定的不可变提交（不得回退到默认分支）以及超级项目 RevisionID。
func isCommitHash(rev string) bool {
	rev = strings.TrimSpace(rev)
	if len(rev) != 40 && len(rev) != 64 {
		return false
	}
	for _, c := range rev {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// isFullCommitID 判断字符串是否为完整提交 ID（40 位 SHA-1 或 64 位 SHA-256）。
// 用于识别超级项目覆盖写入的 RevisionID（仅完整 SHA 可信，
// Project.RevisionID 在普通构造时可能保存分支名）。
func isFullCommitID(rev string) bool {
	return isCommitHash(rev)
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

// projectRepoDir 返回项目实际的 git 仓库目录：
//   - 镜像项目没有工作树，仓库目录为 Gitdir（= Objdir = <topdir>/<name>.git）；
//   - 普通项目为工作树目录。
func projectRepoDir(p *project.Project) string {
	if p == nil {
		return ""
	}
	if p.Mirror {
		if p.Gitdir != "" {
			return p.Gitdir
		}
		return p.Objdir
	}
	return p.Worktree
}

// projectExists 检查目目录是否存
// 兼容两种布局：
//   - 标准工作树：worktree/.git 目录
//   - 裸仓库/镜像：仓库目录本身即 git 目录（HEAD/objects/refs 在顶层，无 .git 子目录）
func (e *Engine) projectExists(p *project.Project) (bool, error) {
	repoDir := projectRepoDir(p)
	if p.Mirror {
		return isBareGitRepo(repoDir), nil
	}
	gitDir := filepath.Join(repoDir, ".git")
	_, err := os.Stat(gitDir)
	if os.IsNotExist(err) {
		// 无 .git 子目录：检查是否为裸仓库（mirror 模式克隆产物）
		if isBareGitRepo(repoDir) {
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
	output, err := e.runGitInDir(projectRepoDir(p), "remote")
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
		if _, err := e.runGitInDir(projectRepoDir(p), "remote", "remove", "origin"); err != nil {
			e.logger.Warn("failed to remove 'origin' remote for project %s: %v", p.Name, err)
		}
	}

	if !remoteExists {
		// Remote does not exist, add it
		if _, err := e.runGitInDir(projectRepoDir(p), "remote", "add", p.RemoteName, remoteURL); err != nil {
			return fmt.Errorf("failed to add remote: %w", err)
		}
		if e.options.Verbose {
			e.logger.Debug("added remote '%s' for project %s: %s", p.RemoteName, p.Name, remoteURL)
		}
	} else {
		// Remote exists, check if URL needs updating
		urlOutput, err := e.runGitInDir(projectRepoDir(p), "remote", "get-url", p.RemoteName)
		if err != nil {
			return fmt.Errorf("failed to get remote URL: %w", err)
		}
		currentURL := strings.TrimSpace(string(urlOutput))
		if currentURL != remoteURL {
			if _, err := e.runGitInDir(projectRepoDir(p), "remote", "set-url", p.RemoteName, remoteURL); err != nil {
				return fmt.Errorf("failed to update remote URL: %w", err)
			}
			if e.options.Verbose {
				e.logger.Debug("updated remote '%s' URL for project %s: %s", p.RemoteName, p.Name, remoteURL)
			}
		}
	}

	// pushurl 只取清单 remote 的 pushurl 基址，无条件拼接 "/<项目名>"
	// （对齐上游 GitConfig.Remote.Save）；review 单独写入 remote.<name>.review，
	// 绝不冒用为 pushurl。字段消失时必须 unset-all，避免残留旧地址。
	pushURL, pushErr := e.projectPushURL(p)
	if pushErr != nil {
		return pushErr
	}
	repoDir := projectRepoDir(p)
	if err := e.setOrUnsetGitConfig(repoDir,
		fmt.Sprintf("remote.%s.pushurl", p.RemoteName), pushURL); err != nil {
		return err
	}
	if e.options.Verbose && pushURL != "" {
		e.logger.Debug("set pushurl for project %s remote %s: %s", p.Name, p.RemoteName, pushURL)
	}

	reviewURL := e.projectReviewURL(p)
	if err := e.setOrUnsetGitConfig(repoDir,
		fmt.Sprintf("remote.%s.review", p.RemoteName), reviewURL); err != nil {
		return err
	}
	if e.options.Verbose && reviewURL != "" {
		e.logger.Debug("set review for project %s remote %s: %s", p.Name, p.RemoteName, reviewURL)
	}

	// mirror 用 set（替换）而不是 --add，避免重复执行后同一 key 累积多值。
	isMirror := p.Mirror || (e.options.Config != nil && e.options.Config.Mirror)
	if isMirror {
		if _, err := e.runGitInDir(repoDir,
			"config", "--replace-all", fmt.Sprintf("remote.%s.mirror", p.RemoteName), "true"); err != nil {
			return fmt.Errorf("failed to set mirror config: %w", err)
		}
		if e.options.Verbose {
			e.logger.Debug("set mirror mode for project %s remote %s", p.Name, p.RemoteName)
		}
	}

	return nil
}

// setOrUnsetGitConfig 写入或清除单个 git config key（对齐上游 Remote.Save：
// value 为 None 时删除）。--unset-all 清理多值残留；key 本就不存在（exit 5）
// 视为成功。
func (e *Engine) setOrUnsetGitConfig(repoDir, key, value string) error {
	if value != "" {
		if _, err := e.runGitInDir(repoDir, "config", "--replace-all", key, value); err != nil {
			return fmt.Errorf("failed to set %s: %w", key, err)
		}
		return nil
	}
	if _, err := e.runGitInDir(repoDir, "config", "--unset-all", key); err != nil {
		var gitErr *git.GitCommandError
		if errors.As(err, &gitErr) && gitErr.ExitCode == 5 {
			return nil
		}
		return fmt.Errorf("failed to unset %s: %w", key, err)
	}
	return nil
}

// projectPushURL 返回应写入 remote.<name>.pushurl 的完整 URL：
// 只取清单 remote 的显式 pushurl；基址无条件拼接 "/<项目名>"
// （对齐上游 pushUrl + "/" + projectname）。未配置时返回空字符串（调用方 unset）。
func (e *Engine) projectPushURL(p *project.Project) (string, error) {
	if e.manifest == nil || p == nil {
		return "", nil
	}
	for _, r := range e.manifest.Remotes {
		if r.Name == p.RemoteName || (r.Alias != "" && r.Alias == p.RemoteName) {
			raw := strings.TrimSpace(r.PushURL)
			if raw == "" {
				return "", nil
			}
			base := raw
			if !isAbsoluteRemoteURL(raw) {
				resolved, err := resolveURLAgainstManifest(e.projectManifestBase(p), raw)
				if err != nil {
					return "", fmt.Errorf("cannot resolve pushurl %q for project %s: %w", raw, p.Name, err)
				}
				base = resolved
			}
			base = strings.TrimSuffix(base, "/")
			if p.Name != "" {
				base += "/" + strings.TrimSuffix(p.Name, "/")
			}
			return base, nil
		}
	}
	return "", nil
}

// projectReviewURL 返回应写入 remote.<name>.review 的原值（对齐上游
// GitConfig.Remote.Save：review 不拼接项目名，也不做 URL 规范化）。
func (e *Engine) projectReviewURL(p *project.Project) string {
	if e.manifest == nil || p == nil {
		return ""
	}
	for _, r := range e.manifest.Remotes {
		if r.Name == p.RemoteName || (r.Alias != "" && r.Alias == p.RemoteName) {
			return strings.TrimSpace(r.Review)
		}
	}
	return ""
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
	output, err := e.runGitInDir(projectRepoDir(p), "lfs", "ls-files")
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
			if _, err := e.runGitInDir(projectRepoDir(p), "lfs", "pull"); err != nil {
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
	projects, err := project.NewManagerFromManifest(e.manifest, e.config).GetProjectsInGroups(e.manifestGroups())
	if err != nil {
		return nil, fmt.Errorf("failed to get projects: %w", err)
	}

	e.projects = projects
	e.applyDefaultRemoteToProjects()

	return e.projects, nil
}

// reloadManifestFromCache 重新加载manifest
func (e *Engine) reloadManifestFromCache() error {
	if len(e.manifestCache) == 0 {
		return fmt.Errorf("manifest cache is empty")
	}

	// 解析缓存的manifest数据
	parser := manifest.NewParser()
	newManifest, err := parser.ParseFromBytes(e.manifestCache, e.manifestGroups())
	if err != nil {
		return fmt.Errorf("failed to parse manifest from cache: %w", err)
	}

	// 更新引擎中的manifest
	e.manifest = newManifest

	// 重新failed to get project列表
	projects, err := project.NewManagerFromManifest(e.manifest, e.config).GetProjectsInGroups(e.manifestGroups())
	if err != nil {
		return fmt.Errorf("failed to get projects from cached manifest: %w", err)
	}

	// smart-sync 重载清单后，仅当命令行显式请求了项目子集时过滤：
	// 新清单可能包含更多项目，无参数同步应保留全部已审批项目。
	if len(e.projectSubset) > 0 {
		filtered := make([]*project.Project, 0, len(projects))
		for _, p := range projects {
			if p != nil && e.projectSubset[p.Name] {
				filtered = append(filtered, p)
			}
		}
		projects = filtered
	}
	e.projects = projects
	e.applyDefaultRemoteToProjects()

	return nil
}

// manifestGroups 返回解析清单时使用的组过滤：
// 命令行/调用方显式给出 Groups 时原样使用；为空时回退
// config.EffectiveGroups()（未配置组时为 "default"，对齐上游默认组语义）。
func (e *Engine) manifestGroups() []string {
	// 非 nil（含显式空切片）表示调用方已给出过滤语义：空切片=不过滤。
	if e.options != nil && e.options.Groups != nil {
		return e.options.Groups
	}
	if e.config != nil {
		return manifest.SplitGroups(e.config.EffectiveGroups())
	}
	return nil
}

// applySuperprojectRevisionsFromFile 在 --local-only 下复用已有的
// .repo/superproject-manifest.xml：按 path/name 把其中的 revision 写回
// project.RevisionID，避免任何网络 fetch。文件不存在时返回 (false, nil)。
func (e *Engine) applySuperprojectRevisionsFromFile() (bool, error) {
	repoDir := resolveRepoMetadataDir(e.manifest)
	manifestPath := filepath.Join(repoDir, "superproject-manifest.xml")
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("读取超级项目清单 %s failed: %w", manifestPath, err)
	}

	var parsed struct {
		Projects []struct {
			Name     string `xml:"name,attr"`
			Path     string `xml:"path,attr"`
			Revision string `xml:"revision,attr"`
		} `xml:"project"`
	}
	if err := xml.Unmarshal(data, &parsed); err != nil {
		return false, fmt.Errorf("解析超级项目清单 %s failed: %w", manifestPath, err)
	}

	byPath := make(map[string]string, len(parsed.Projects))
	byName := make(map[string]string, len(parsed.Projects))
	for _, p := range parsed.Projects {
		revision := strings.TrimSpace(p.Revision)
		if revision == "" {
			continue
		}
		if p.Path != "" {
			byPath[p.Path] = revision
		}
		if p.Name != "" {
			byName[p.Name] = revision
		}
	}

	for _, p := range e.projects {
		if p == nil {
			continue
		}
		revision := byPath[p.Path]
		if revision == "" {
			revision = byPath[p.Relpath]
		}
		if revision == "" {
			revision = byName[p.Name]
		}
		if revision != "" {
			p.RevisionID = revision
		}
	}
	return true, nil
}

// applyDefaultRemoteToProjects 对缺少 remote 名的项目应用 --default-remote。
func (e *Engine) applyDefaultRemoteToProjects() {
	for _, p := range e.projects {
		e.applyDefaultRemote(p)
	}
}

// applyDefaultRemote 用 --default-remote 兜底缺失的项目 remote 名：
// 仅在项目未解析出 remote 名时生效，并从清单查询该 remote 的 fetch 基址
// 补齐 RemoteURL/FetchBase，使 fetch/clone 真正使用该 remote。
func (e *Engine) applyDefaultRemote(p *project.Project) {
	if p == nil || e.options == nil {
		return
	}
	name := strings.TrimSpace(e.options.DefaultRemote)
	if name == "" || p.RemoteName != "" {
		return
	}
	p.RemoteName = name
	if p.Remote == "" {
		p.Remote = name
	}
	if p.RemoteURL != "" || p.FetchBase != "" || e.manifest == nil {
		return
	}
	base, err := e.manifest.GetRemoteURL(name)
	if err != nil {
		return
	}
	base = strings.TrimSpace(base)
	if base == "" {
		return
	}
	p.FetchBase = base
	if isAbsoluteRemoteURL(base) {
		p.RemoteURL = strings.TrimSuffix(base, "/") + "/" + strings.TrimSuffix(p.Name, "/")
	} else {
		// 相对基址留给 resolveRemoteURL 按清单 URL 解析并拼接项目名。
		p.RemoteURL = base
	}
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

// SetProjects 设置要同步的项目列表。
// 注意：这里不再记录项目名称集合。smart-sync 从服务器重载清单后，
// 只有命令行显式传入项目参数时（SetProjectSubset）才保留子集；
// 无参数同步必须使用重载后的全部已审批项目，避免新增项目被旧名称过滤。
func (e *Engine) SetProjects(projects []*project.Project) {
	e.projects = projects
}

// SetProjectSubset 记录命令行显式请求的项目名称子集。
// reloadManifestFromCache 在 smart-sync 重载清单后据此过滤，
// 保证只同步用户请求的项目；未调用时保留重载后的全部项目。
func (e *Engine) SetProjectSubset(names []string) {
	if len(names) == 0 {
		e.projectSubset = nil
		return
	}
	subset := make(map[string]bool, len(names))
	for _, name := range names {
		if name != "" {
			subset[name] = true
		}
	}
	e.projectSubset = subset
}

// updateSubmodules Updating project的 submodule
func (e *Engine) updateSubmodules(ctx context.Context, p *project.Project) error {
	// --local-only 禁止网络操作：submodule update 会访问远端，必须跳过。
	if e.options.LocalOnly {
		if e.options.Verbose {
			e.logger.Debug("--local-only：跳过项目 %s 的 submodule 更新", p.Name)
		}
		return nil
	}

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
	args := []string{"-C", projectRepoDir(p), "submodule", "update", "--init", "--recursive"}

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
