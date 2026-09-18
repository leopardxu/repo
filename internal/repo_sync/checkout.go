package repo_sync

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/leopardxu/repo-go/internal/logger"
	"github.com/leopardxu/repo-go/internal/progress"
	"github.com/leopardxu/repo-go/internal/project"
)

// 添加分支名称字段和统计信息
type checkoutStats struct {
	Success       int
	Failed        int
	BranchMissing int // 分支不存在的项目数（上游 CheckoutBranch 返回 None）
	mu            sync.Mutex
}

// SetBranchName 设置要检出的分支名称
func (e *Engine) SetBranchName(branchName string) {
	e.branchName = branchName
}

// GetCheckoutStats 获取检出操作的统计信息
func (e *Engine) GetCheckoutStats() (int, int) {
	return e.checkoutStats.Success, e.checkoutStats.Failed
}

// CheckoutBranch 检出指定分支
func (e *Engine) CheckoutBranch(projects []*project.Project) error {
	if e.logger == nil {
		e.logger = logger.NewDefaultLogger()
		if e.options.Verbose {
			e.logger.SetLevel(logger.LogLevelDebug)
		} else if e.options.Quiet {
			e.logger.SetLevel(logger.LogLevelError)
		}
	}

	if e.branchName == "" {
		return fmt.Errorf("branch name is not specified")
	}

	e.logger.Info("开始检出分支'%s' 到%d 个项目", e.branchName, len(projects))

	// 初始化统计信息
	e.checkoutStats = &checkoutStats{}

	// 只检出有工作树的项目
	var worktreeProjects []*project.Project
	for _, project := range projects {
		if project.Worktree != "" {
			worktreeProjects = append(worktreeProjects, project)
		}
	}

	// 创建进度条
	pm := progress.NewConsoleReporter()
	if !e.options.Quiet {
		pm.Start(len(worktreeProjects))
	}

	// 执行检出
	if len(worktreeProjects) == 0 {
		e.logger.Info("没有可检出的项目")
		return nil
	}

	if e.options.JobsCheckout == 1 {
		e.logger.Debug("使用单线程模式检出项")
		for _, project := range worktreeProjects {
			result := e.checkoutOneBranch(project)
			e.processCheckoutResult(result, pm)
		}
	} else {
		// 多线程检出
		e.logger.Debug("使用多线程模式检出项目，并发数 %d", e.options.JobsCheckout)

		// 创建工作组
		var wg sync.WaitGroup
		resultsChan := make(chan CheckoutResult, len(worktreeProjects))

		// 限制并发数
		semaphore := make(chan struct{}, e.options.JobsCheckout)

		for _, p := range worktreeProjects {
			wg.Add(1)
			go func(proj *project.Project) {
				defer wg.Done()

				// 获取信号量
				semaphore <- struct{}{}
				defer func() { <-semaphore }()

				// 执行检出
				result := e.checkoutOneBranch(proj)
				resultsChan <- result
			}(p)
		}

		// 等待所有检出完成
		go func() {
			wg.Wait()
			close(resultsChan)
		}()

		// 处理结果
		for result := range resultsChan {
			e.processCheckoutResult(result, pm)
		}
	}

	if !e.options.Quiet {
		pm.Finish()
	}

	e.logger.Info("检出分支'%s' 完成: %d 成功, %d failed", e.branchName, e.checkoutStats.Success, e.checkoutStats.Failed)

	// 上游语义：检出failed -> 报错；所有项目都没有该分支 -> "no project has branch"
	if e.checkoutStats.Failed > 0 {
		return fmt.Errorf("checkout failed for %d projects", e.checkoutStats.Failed)
	}
	if e.checkoutStats.Success == 0 && e.checkoutStats.BranchMissing > 0 {
		return fmt.Errorf("no project has branch %s", e.branchName)
	}

	return nil
}

// processCheckoutResult 处理检出结果
func (e *Engine) processCheckoutResult(result CheckoutResult, pm progress.Reporter) {
	e.checkoutStats.mu.Lock()
	defer e.checkoutStats.mu.Unlock()

	switch {
	case result.Success:
		e.checkoutStats.Success++
		if e.options.Verbose && !e.options.Quiet {
			e.logger.Debug("项目 %s 检出成功", result.Project.Name)
		}
	case result.BranchMissing:
		// 分支不存在：上游返回 None，不参与成功/失败统计，静默跳过
		e.checkoutStats.BranchMissing++
	default:
		e.checkoutStats.Failed++
		if !e.options.Quiet {
			e.logger.Error("项目 %s 检出failed", result.Project.Name)
		}
	}

	if !e.options.Quiet {
		pm.Update(1, result.Project.Name)
	}
}

// CheckoutResult 表示检出操作的结果
type CheckoutResult struct {
	Success       bool
	BranchMissing bool // 项目中不存在该本地分支（上游 CheckoutBranch 返回 None）
	Project       *project.Project
}

// checkoutOneBranch 检出单个项目的指定分支。
// 对齐上游 project.py CheckoutBranch：仅检出本地已存在分支，绝不创建分支，
// 分支不存在返回 BranchMissing（上游返回 None，交由上层汇总为 "no project has branch"）。
func (e *Engine) checkoutOneBranch(project *project.Project) CheckoutResult {
	if !e.options.Quiet {
		e.logger.Info("检出项%s 的分%s", project.Name, e.branchName)
	}

	// 仅检查本地分支（repo checkout 用于切到 repo start 创建的分支）
	localOut, err := project.GitRepo.RunCommand("branch", "--list", e.branchName)
	if err != nil {
		e.logger.Error("项目 %s 查询分支 %s failed: %v", project.Name, e.branchName, err)
		return CheckoutResult{Success: false, Project: project}
	}
	if strings.TrimSpace(string(localOut)) == "" {
		e.logger.Debug("项目 %s 无本地分支 %s，跳过", project.Name, e.branchName)
		return CheckoutResult{Success: false, BranchMissing: true, Project: project}
	}

	// --detach 为 repo-go 扩展选项：检出分支后分离 HEAD（git checkout --detach <branch>）
	checkoutArgs := []string{"checkout"}
	if e.options.Detach {
		checkoutArgs = append(checkoutArgs, "--detach")
	}
	checkoutArgs = append(checkoutArgs, e.branchName)
	if _, err := project.GitRepo.RunCommand(checkoutArgs...); err != nil {
		e.logger.Error("项目 %s 检出分支 %s failed: %v", project.Name, e.branchName, err)
		return CheckoutResult{Success: false, Project: project}
	}

	// 检出成功后复制钩子脚本到项目
	repoHooksDir := filepath.Join(e.repoRoot, ".repo", "hooks")
	projectGitDir := filepath.Join(project.Worktree, ".git")

	if err := copyHooksToProject(repoHooksDir, projectGitDir); err != nil {
		e.logger.Warn("failed to copy hook scripts to project%s: %v", project.Name, err)
		// 不因为钩子复制failed而导致整个检出失
	}

	return CheckoutResult{Success: true, Project: project}
}

// copyHooksToProject copies hooks from .repo/hooks into the project's .git/hooks directory.
func copyHooksToProject(repoHooksDir, projectGitDir string) error {
	hooks, err := os.ReadDir(repoHooksDir)
	if err != nil {
		// If .repo/hooks directory does not exist, skip silently
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("failed to read .repo/hooks directory: %w", err)
	}

	projectHooksDir := filepath.Join(projectGitDir, "hooks")
	if err := os.MkdirAll(projectHooksDir, 0755); err != nil {
		return fmt.Errorf("failed to create project hooks directory %s: %w", projectHooksDir, err)
	}

	for _, hookEntry := range hooks {
		if hookEntry.IsDir() {
			continue
		}

		hookName := hookEntry.Name()

		// Security: reject path traversal in hook filenames
		if !filepath.IsLocal(hookName) {
			fmt.Printf("warning: skipping hook with unsafe filename: %s\n", hookName)
			continue
		}

		srcPath := filepath.Join(repoHooksDir, hookName)
		destPath := filepath.Join(projectHooksDir, hookName)

		if err := copyFileWithMode(srcPath, destPath, 0755); err != nil {
			fmt.Printf("warning: failed to copy hook %s: %v\n", hookName, err)
			continue
		}
	}
	return nil
}

// copyFileWithMode copies a single file from src to dst with the given permission mode.
// The source file is closed immediately after reading, preventing file descriptor leaks
// when called in a loop.
func copyFileWithMode(src, dst string, mode os.FileMode) error {
	srcFile, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("failed to open source file %s: %w", src, err)
	}
	defer srcFile.Close()

	destFile, err := os.OpenFile(dst, os.O_RDWR|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return fmt.Errorf("failed to create destination file %s: %w", dst, err)
	}
	defer destFile.Close()

	if _, err := io.Copy(destFile, srcFile); err != nil {
		return fmt.Errorf("failed to copy %s to %s: %w", src, dst, err)
	}

	// Ensure executable permission is set
	if err := os.Chmod(dst, mode); err != nil {
		return fmt.Errorf("failed to set permissions on %s: %w", dst, err)
	}

	return nil
}
