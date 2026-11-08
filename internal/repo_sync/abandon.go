package repo_sync

import (
	"fmt"
	"io"
	"strings"
	"sync"

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
//   - topic == ""：删除每个项目的全部本地分支，对齐上游 GetBranches() +
//     AbandonBranch()：当前分支先 detach 到 manifest revision 再删除，manifest
//     revision 分支本身也删除（不再跳过 "受保护" 分支，因此仅剩当前/manifest
//     分支时仍会正常处理并成功）。
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
				// --all：对齐上游 abandon.py 的 GetBranches()/AbandonBranch()：
				//   - 删除每个项目的全部本地分支（包括 manifest revision 分支）；
				//   - 当前分支由 project.DeleteBranch 先 detach 到 manifest revision
				//     再删除，因此当前分支必须排在最前：manifest 分支可能在其后
				//     被删除，若先删 manifest 分支，后续 detach 目标就不存在了。
				// 当前分支检测使用 project.GetCurrentBranch（Repository.CurrentBranch
				// 基于完整 symbolic-ref HEAD 并剥离 refs/heads/，不再使用存在同名
				// tag 歧义的 --short）。
				all, err := proj.ListLocalBranches()
				if err != nil {
					resultsChan <- AbandonResult{Project: proj, Branch: "", Success: false, Error: fmt.Errorf("列出分支failed: %w", err)}
					return
				}
				// 项目没有任何本地分支时不产生结果，由上层报
				// "no project has local branch(es)"（对齐上游 success 为空）。
				if len(all) == 0 {
					return
				}
				currentBranch, cerr := proj.GetCurrentBranch()
				if cerr != nil {
					resultsChan <- AbandonResult{Project: proj, Success: false, Error: fmt.Errorf("获取当前分支failed: %w", cerr)}
					return
				}
				currentBranch = abandonLocalBranchName(currentBranch)
				branches = make([]string, 0, len(all))
				for _, b := range all {
					if b == currentBranch {
						branches = append(branches, b)
					}
				}
				for _, b := range all {
					if b == currentBranch {
						continue
					}
					branches = append(branches, b)
				}
			}

			for _, branch := range branches {
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

// abandonLocalBranchName 把 GetCurrentBranch 的结果归一为本地分支短名：
// 完整 refs/heads/X 去掉前缀；detached sentinel、SHA、tag 原样返回。
// 这样无论 Repository.CurrentBranch 返回完整 ref 还是短名，都能与
// ListLocalBranches 的短名列表正确比较。
func abandonLocalBranchName(name string) string {
	return strings.TrimPrefix(strings.TrimSpace(name), "refs/heads/")
}

// PrintAbandonSummary 把放弃分支的汇总信息写到 w（结果 payload，调用方传 os.Stdout）。
// dryRun 为 true 时输出"将删除"而非"成功删除"；NotFound 结果不计入成功/失败。
// 即使 -q 也不得吞掉该输出（只有诊断日志受 -q 影响）。
func PrintAbandonSummary(w io.Writer, results []AbandonResult, dryRun bool) {
	total := 0
	success := 0
	failed := 0
	notFound := 0
	action := "成功删除分支"
	if dryRun {
		action = "将删除分支"
	}

	for _, r := range results {
		switch {
		case r.NotFound:
			// 分支不存在：静默跳过（对齐上游 AbandonBranch 返回 None）
			notFound++
		case r.Success:
			total++
			success++
			fmt.Fprintf(w, "[OK]    %s: %s %s\n", r.Project.Name, action, r.Branch)
		default:
			total++
			failed++
			fmt.Fprintf(w, "[FAIL]  %s: 删除分支 %s failed (%v)\n", r.Project.Name, r.Branch, r.Error)
		}
	}

	fmt.Fprintf(w, "\n共处理项 %d, 成功: %d, failed: %d, 未找到分支: %d\n", total, success, failed, notFound)
}
