package repo_sync

import (
	"fmt"
	"strings"
	"sync"

	"github.com/leopardxu/repo-go/internal/logger"
	"github.com/leopardxu/repo-go/internal/project"
)

// AbandonResult 表示放弃分支操作的结果
type AbandonResult struct {
	Project  *project.Project
	Branch   string
	Success  bool
	NotFound bool // 项目中不存在该分支（上游 AbandonBranch 返回 None，不计成功/失败）
	Error    error
}

// AbandonTopics 支持批量放弃多个项目的本地topic分支，并发执行，输出简洁明了的结果
//
// 语义（对齐上游 abandon.py / project.py AbandonBranch）：
//   - topic != ""：删除每个项目中指定的分支；分支不存在记 NotFound（不计失败），
//     由上层在"所有项目都没有该分支"时报 "no project has local branch(es)"；
//   - topic == ""：删除每个项目的所有本地分支，跳过当前分支与 manifest revision 分支
//     （当前分支不可删除，记为failed）。
//
// 当 e.options.DryRun 为 true 时，只打印将要删除的分支，不实际删除。
func (e *Engine) AbandonTopics(projects []*project.Project, topic string) []AbandonResult {
	var wg sync.WaitGroup
	jobs := e.options.JobsCheckout
	if jobs < 1 {
		jobs = 1
	}
	semaphore := make(chan struct{}, jobs)
	resultsChan := make(chan AbandonResult, len(projects)*4)

	for _, p := range projects {
		wg.Add(1)
		go func(proj *project.Project) {
			defer wg.Done()
			semaphore <- struct{}{}
			defer func() { <-semaphore }()

			// 确定要删除的分支列表
			var branches []string
			if topic != "" {
				// 删除指定分支；不存在记 NotFound（对齐上游 AbandonBranch 返回 None）
				exists, err := proj.HasBranch(topic)
				if err != nil {
					resultsChan <- AbandonResult{Project: proj, Branch: topic, Success: false, Error: fmt.Errorf("列出分支failed: %w", err)}
					return
				}
				if !exists {
					resultsChan <- AbandonResult{Project: proj, Branch: topic, NotFound: true}
					return
				}
				branches = []string{topic}
			} else {
				// --all：枚举所有本地分支，跳过当前分支与 manifest revision 分支
				currentBranch, _ := proj.GetCurrentBranch()
				manifestRev := proj.Revision
				manifestRev = strings.TrimPrefix(manifestRev, "refs/heads/")
				all, err := proj.ListLocalBranches()
				if err != nil {
					resultsChan <- AbandonResult{Project: proj, Branch: "", Success: false, Error: fmt.Errorf("列出分支failed: %w", err)}
					return
				}
				for _, b := range all {
					if b == currentBranch {
						continue // 当前分支不可删除
					}
					if b == manifestRev {
						continue // manifest revision 分支不可删除（保护同步基准）
					}
					branches = append(branches, b)
				}
				if len(branches) == 0 {
					resultsChan <- AbandonResult{Project: proj, Branch: "", Success: true, Error: nil}
					return
				}
			}

			for _, branch := range branches {
				// 分离HEAD状态：无需删除
				if strings.HasPrefix(branch, "HEAD detached at ") {
					resultsChan <- AbandonResult{Project: proj, Branch: branch, Success: true, Error: nil}
					continue
				}

				// dry-run：只打印，不删除
				if e.options.DryRun {
					if !e.options.Quiet && e.logger != nil {
						e.logger.Info("[dry-run] 将删除项目 %s 的分支 %s", proj.Name, branch)
					}
					resultsChan <- AbandonResult{Project: proj, Branch: branch, Success: true, Error: nil}
					continue
				}

				// 放弃本地分支
				if !e.options.Quiet && e.logger != nil {
					e.logger.Debug("正在删除项目 %s 的分支 %s", proj.Name, branch)
				}

				err := proj.DeleteBranch(branch)
				if err != nil {
					if e.logger != nil {
						e.logger.Error("删除项目 %s 的分支 %s failed: %v", proj.Name, branch, err)
					}
					resultsChan <- AbandonResult{Project: proj, Branch: branch, Success: false, Error: err}
					continue
				}

				if !e.options.Quiet && e.logger != nil {
					e.logger.Debug("成功删除项目 %s 的分支 %s", proj.Name, branch)
				}
				resultsChan <- AbandonResult{Project: proj, Branch: branch, Success: true, Error: nil}
			}
		}(p)
	}

	go func() {
		wg.Wait()
		close(resultsChan)
	}()

	var results []AbandonResult
	for r := range resultsChan {
		results = append(results, r)
	}
	return results
}

// PrintAbandonSummary 输出放弃分支的汇总信息。
// dryRun 为 true 时输出"将删除"而非"成功删除"；NotFound 结果不计入成功/失败。
func PrintAbandonSummary(results []AbandonResult, dryRun bool, log logger.Logger) {
	total := 0
	success := 0
	failed := 0
	notFound := 0
	action := "成功删除分支"
	if dryRun {
		action = "将删除分支"
	}

	// 按项目名称排序输出结果
	for _, r := range results {
		switch {
		case r.NotFound:
			// 分支不存在：静默跳过（对齐上游 AbandonBranch 返回 None）
			notFound++
		case r.Success:
			total++
			success++
			if log != nil {
				log.Info("[OK]    %s: %s %s", r.Project.Name, action, r.Branch)
			} else {
				fmt.Printf("[OK]    %s: %s %s\n", r.Project.Name, action, r.Branch)
			}
		default:
			total++
			failed++
			if log != nil {
				log.Error("[FAIL]  %s: 删除分支 %s failed (%v)", r.Project.Name, r.Branch, r.Error)
			} else {
				fmt.Printf("[FAIL]  %s: %s (%v)\n", r.Project.Name, r.Branch, r.Error)
			}
		}
	}

	// 输出汇总信息
	if log != nil {
		log.Info("\n共处理项 %d, 成功: %d, failed: %d, 未找到分支: %d", total, success, failed, notFound)
	} else {
		fmt.Printf("\n共处理项 %d, 成功: %d, failed: %d, 未找到分支: %d\n", total, success, failed, notFound)
	}
}
