package commands

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"runtime"

	"github.com/leopardxu/repo-go/internal/config"
	"github.com/leopardxu/repo-go/internal/logger"
	"github.com/leopardxu/repo-go/internal/manifest"
	"github.com/leopardxu/repo-go/internal/project"
	"github.com/spf13/cobra"
)

// UploadOptions 包含upload命令的选项
type UploadOptions struct {
	Branch          string
	CurrentBranch   bool
	NoCurrentBranch bool
	Draft           bool
	Force           bool
	DryRun          bool
	PushOption      []string
	Reviewers       string
	Ready           bool // -r/--ready：标记为 ready for review（清除 WIP）
	Topic           string
	NoVerify        bool
	IgnoreHooks     bool // 忽略hooks
	Replace         bool // 替换现有change
	Private         bool
	Wip             bool
	Jobs            int
	Hashtags        string
	HashtagBranch   bool
	Labels          string
	CC              string
	NoEmails        bool
	Destination     string
	Yes             bool
	NoCertChecks    bool
	Verbose         bool
	Quiet           bool
	CommonManifestOptions
	// 添加配置字段，避免重复加载
	Config *config.Config
}

// uploadStats 用于统计上传结果
type uploadStats struct {
	mu      sync.Mutex
	total   int
	success int
	failed  int
}

// increment 增加计数
func (s *uploadStats) increment(success bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.total++
	if success {
		s.success++
	} else {
		s.failed++
	}
}

// UploadCmd 返回upload命令
func UploadCmd() *cobra.Command {
	opts := &UploadOptions{}

	cmd := &cobra.Command{
		Use:   "upload [--re --cc] [<project>...]",
		Short: "Upload changes for code review",
		Long: `Upload changes to Gerrit code review system.

By default, changes are uploaded for review (ready for review).
The upload pushes to refs/for/<branch> for Gerrit review, not directly to the branch.

Use --wip to upload as Work In Progress, --draft for draft changes, or --private for private changes.
Specify reviewers with -r and CC with --cc.`,
		RunE: func(_ *cobra.Command, args []string) error {
			return runUpload(opts, args)
		},
	}

	// 添加命令行选项
	cmd.Flags().StringVarP(&opts.Branch, "branch", "b", "", "上传指定分支")
	cmd.Flags().StringVar(&opts.Branch, "br", "", "alias for --branch")
	cmd.Flags().BoolVarP(&opts.CurrentBranch, "current-branch", "c", false, "仅上传当前分支")
	cmd.Flags().BoolVar(&opts.NoCurrentBranch, "no-current-branch", false, "upload all git branches")
	cmd.Flags().BoolVarP(&opts.Draft, "draft", "d", false, "upload as WIP (deprecated alias of --wip)")
	cmd.Flags().BoolVarP(&opts.Force, "force", "f", false, "强制上传，即使没有变更")
	cmd.Flags().BoolVarP(&opts.DryRun, "dry-run", "n", false, "不实际上传，仅显示将要上传的内容")
	cmd.Flags().StringArrayVarP(&opts.PushOption, "push-option", "o", nil, "push option (may be given multiple times)")
	cmd.Flags().StringVar(&opts.Reviewers, "re", "", "request reviews from these people (comma-separated)")
	cmd.Flags().StringVar(&opts.Reviewers, "reviewers", "", "alias for --re")
	cmd.Flags().BoolVarP(&opts.Ready, "ready", "r", false, "mark change as ready for review (clears WIP)")
	cmd.Flags().StringVarP(&opts.Topic, "topic", "t", "", "变更的主题")
	cmd.Flags().BoolVar(&opts.NoVerify, "no-verify", false, "绕过上传前钩子")
	cmd.Flags().BoolVar(&opts.IgnoreHooks, "ignore-hooks", false, "忽略所有hooks")
	cmd.Flags().BoolVar(&opts.Replace, "replace", false, "替换现有的change")
	cmd.Flags().BoolVar(&opts.Private, "private", false, "上传为私有状态（覆盖默认的 WIP 状态）")
	cmd.Flags().BoolVar(&opts.Wip, "wip", false, "上传为进行中状态（WIP）")
	cmd.Flags().IntVarP(&opts.Jobs, "jobs", "j", runtime.NumCPU()*2, "并行运行的任务数量")
	cmd.Flags().StringVar(&opts.Hashtags, "hashtag", "", "添加标签（逗号分隔）到审查中")
	cmd.Flags().BoolVar(&opts.HashtagBranch, "hashtag-branch", false, "将本地分支名添加为标签")
	cmd.Flags().StringVar(&opts.Labels, "label", "", "上传时添加标签")
	cmd.Flags().StringVar(&opts.CC, "cc", "", "同时发送邮件给这些邮箱地址")
	cmd.Flags().StringVar(&opts.Destination, "destination", "", "提交到此目标分支进行审查")
	cmd.Flags().BoolVar(&opts.NoEmails, "no-emails", false, "上传时不发送邮件")
	cmd.Flags().BoolVar(&opts.Yes, "yes", false, "对所有安全提示回答是")
	cmd.Flags().BoolVar(&opts.NoCertChecks, "no-cert-checks", false, "禁用SSL证书验证（不安全）")
	cmd.Flags().BoolVarP(&opts.Verbose, "verbose", "v", false, "显示详细输出，包括调试信息")
	cmd.Flags().BoolVarP(&opts.Quiet, "quiet", "q", false, "仅显示错误信息")
	AddManifestFlags(cmd, &opts.CommonManifestOptions)

	return cmd
}

// runUpload 执行upload命令
func runUpload(opts *UploadOptions, args []string) error {
	// 创建日志记录
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

	log.Info("开始上传代码变更进行审核")

	// -c/--current-branch：repo-go 默认仅推送当前分支 HEAD，故该 flag 为已满足的默认行为。
	if opts.CurrentBranch {
		log.Debug("--current-branch：仅上传当前分支（repo-go 默认即此行为）")
	}
	// --yes：自动确认；repo-go upload 无交互式确认提示，故该 flag 为已满足的默认行为。
	if opts.Yes {
		log.Debug("--yes：自动确认（repo-go upload 无交互式确认提示）")
	}

	// 加载配置
	if opts.Config == nil {
		opts.Config, err = config.Load()
		if err != nil {
			log.Error("failed to load config: %v", err)
			return fmt.Errorf("failed to load config: %w", err)
		}
	}

	// 加载清单
	parser := manifest.NewParser()
	manifest, err := parser.ParseFromFile(opts.Config.ManifestName, manifest.SplitGroups(opts.Config.Groups))
	if err != nil {
		log.Error("failed to parse manifest: %v", err)
		return fmt.Errorf("failed to parse manifest: %w", err)
	}

	// 创建项目管理
	manager := project.NewManagerFromManifest(manifest, opts.Config)

	// 获取要处理的项目
	var projects []*project.Project
	if len(args) == 0 {
		// 如果没有指定项目，则处理所有项目
		log.Debug("未指定项目，将处理所有项目")
		projects, err = manager.GetProjectsInGroups(nil)
		if err != nil {
			log.Error("获取所有项目failed: %v", err)
			return fmt.Errorf("获取所有项目failed: %w", err)
		}
	} else {
		// 否则，只处理指定的项
		log.Debug("将处理指定的项目: %v", args)
		projects, err = manager.GetProjectsByNames(args)
		if err != nil {
			log.Error("获取指定项目failed: %v", err)
			return fmt.Errorf("获取指定项目failed: %w", err)
		}
	}

	log.Info("共有 %d 个项目需要处理", len(projects))

	// 创建manifest项目名到项目的映射，用于访问dest-branch等信息
	type ManifestProjectInfo struct {
		DestBranch string
		Upstream   string
	}
	manifestProjectInfo := make(map[string]ManifestProjectInfo)
	for _, proj := range manifest.Projects {
		manifestProjectInfo[proj.Name] = ManifestProjectInfo{
			DestBranch: proj.DestBranch,
			Upstream:   proj.Upstream,
		}
	}

	// 创建统计对象
	stats := &uploadStats{}

	// 创建错误通道和工作通道
	errChan := make(chan error, len(projects))
	// 规范化并发数：-j <= 0 会导致 makechan panic，回退到默认 8（与其他命令一致）
	if opts.Jobs <= 0 {
		opts.Jobs = 8
	}
	sem := make(chan struct{}, opts.Jobs)
	var wg sync.WaitGroup

	log.Info("开始并行处理项目，并发 %d", opts.Jobs)

	// 并发上传每个项目
	for _, p := range projects {
		p := p
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()

			log.Debug("处理项目: %s", p.Name)

			// 确定目标分支（dest-branch）
			destBranch := opts.Destination
			if destBranch == "" && opts.Branch == "" {
				// 如果没有指定，从 manifest 获取
				if projInfo, exists := manifestProjectInfo[p.Name]; exists && projInfo.DestBranch != "" {
					destBranch = projInfo.DestBranch
				} else if manifest.Default.DestBranch != "" {
					// 否则使用 default 的 dest-branch
					destBranch = manifest.Default.DestBranch
				}
			}

			log.Debug("项目 %s 的目标分支: %s", p.Name, destBranch)

			// failed to get project的远程名称
			remoteName := p.RemoteName
			if remoteName == "" {
				remoteName = "origin" // 默认值
			}
			log.Debug("项目 %s 的远程名称: %s", p.Name, remoteName)

			// -b/--branch：切换到指定本地分支后再上传，上传完成后恢复原 HEAD
			var origHead string
			if opts.Branch != "" {
				// 记录原 HEAD（用于恢复）
				if headOut, err := p.GitRepo.RunCommand("rev-parse", "HEAD"); err == nil {
					origHead = strings.TrimSpace(string(headOut))
				}
				log.Debug("项目 %s 切换到分支 %s 以便上传", p.Name, opts.Branch)
				if _, err := p.GitRepo.RunCommand("checkout", opts.Branch); err != nil {
					errMsg := fmt.Sprintf("项目 %s 切换到分支 %s failed: %v", p.Name, opts.Branch, err)
					log.Error("%s", errMsg)
					errChan <- errors.New(errMsg)
					stats.increment(false)
					return
				}
			}

			// 上传完成后恢复原 HEAD（即使出错也恢复）
			if origHead != "" {
				defer func() {
					log.Debug("项目 %s 恢复到原 HEAD %s", p.Name, origHead)
					if _, err := p.GitRepo.RunCommand("checkout", origHead); err != nil {
						log.Warn("项目 %s 恢复原 HEAD %s failed: %v", p.Name, origHead, err)
					}
				}()
			}

			// --no-verify / --ignore-hooks：跳过 .repo/hooks（repo 级钩子）
			// 注意：这与 git push --no-verify 不同；此处不再把 --no-verify 传给 git push。
			if opts.NoVerify || opts.IgnoreHooks {
				log.Debug("项目 %s 跳过 .repo/hooks（--no-verify/--ignore-hooks）", p.Name)
			} else {
				// 未跳过时，尝试运行 .repo/hooks/pre-upload（若存在）
				if err := runUploadHook(p, log); err != nil {
					errMsg := fmt.Sprintf("项目 %s 的 upload hook failed: %v", p.Name, err)
					log.Error("%s", errMsg)
					errChan <- errors.New(errMsg)
					stats.increment(false)
					return
				}
			}

			// 检查是否有更改
			hasChanges, err := p.GitRepo.HasChangesToPush(remoteName)
			if err != nil {
				errMsg := fmt.Sprintf("检查项目 %s 是否有变更failed: %v", p.Name, err)
				log.Error("%s", errMsg)
				log.Debug("请确保项目已正确配置远程仓库，并且当前在有效的分支上")
				errChan <- errors.New(errMsg)
				stats.increment(false)
				return
			}

			if !hasChanges && !opts.Force {
				log.Info("跳过项目 %s (没有变更需要上传)", p.Name)
				stats.increment(true) // 视为成功，因为这是预期行
				return
			}

			// 获取当前分支
			currentBranch, err := p.GitRepo.CurrentBranch()
			if err != nil {
				errMsg := fmt.Sprintf("failed to get project %s 的当前分支failed: %v", p.Name, err)
				log.Error("%s", errMsg)
				errChan <- errors.New(errMsg)
				stats.increment(false)
				return
			}
			// 分离 HEAD 状态无法上传（对齐上游 "not currently on a branch"）
			if strings.Contains(currentBranch, "detached") || currentBranch == "HEAD" {
				errMsg := fmt.Sprintf("项目 %s 不在分支上（分离 HEAD），无法上传审查", p.Name)
				log.Error("%s", errMsg)
				errChan <- errors.New(errMsg)
				stats.increment(false)
				return
			}
			// 确定目标分支（用于 Gerrit 审查）。对齐上游 UploadForReview 的回退链：
			// --destination > manifest 项目 dest-branch > default dest-branch >
			// 当前分支的跟踪 merge 目标（branch.<name>.merge，repo start 时写入）> revision。
			// 绝不回退到当前本地分支名（那是 topic 分支，refs/for/<topic> 会被 Gerrit 拒绝）。
			targetBranch := destBranch
			if targetBranch == "" {
				if mergeOut, merr := p.GitRepo.RunCommand("config", fmt.Sprintf("branch.%s.merge", currentBranch)); merr == nil {
					targetBranch = strings.TrimPrefix(strings.TrimSpace(string(mergeOut)), "refs/heads/")
				}
			}
			if targetBranch == "" {
				targetBranch = strings.TrimPrefix(p.Revision, "refs/heads/")
			}
			if targetBranch == "" {
				errMsg := fmt.Sprintf("项目 %s 无法确定目标分支（当前分支 %s 未跟踪远程分支）", p.Name, currentBranch)
				log.Error("%s", errMsg)
				errChan <- errors.New(errMsg)
				stats.increment(false)
				return
			}
			log.Debug("项目 %s 的 Gerrit 目标分支: %s", p.Name, targetBranch)

			// 构建 Gerrit 推送引用: refs/for/<branch>
			gerritRef := fmt.Sprintf("refs/for/%s", targetBranch)

			// 构建推送命令参数
			pushArgs := []string{"push"}

			// --no-cert-checks：禁用 HTTPS 证书验证（通过 git -c http.sslVerify=false 实现，不安全）
			if opts.NoCertChecks {
				log.Debug("项目 %s：--no-cert-checks 已禁用 HTTPS 证书验证", p.Name)
				pushArgs = append([]string{"-c", "http.sslVerify=false"}, pushArgs...)
			}

			// 构建 Gerrit push options（使用 % 分隔符附加到 refspec）
			var gerritOptions []string

			// --draft 是 --wip 的废弃别名（等价于 %wip）；--wip/--draft 与 --private 互斥（同时设置生成非法 refspec）。
			wip := opts.Wip || opts.Draft
			// --ready：标记为 ready for review，清除 WIP 状态（对齐上游 repo upload -r/--ready）
			if opts.Ready {
				wip = false
			}
			if wip && opts.Private {
				errMsg := fmt.Sprintf("项目 %s: --wip/--draft 与 --private 互斥", p.Name)
				log.Error("%s", errMsg)
				errChan <- errors.New(errMsg)
				stats.increment(false)
				return
			}

			// WIP/Draft 状态（draft 等价 wip，均映射到 %wip）
			if wip {
				gerritOptions = append(gerritOptions, "wip")
			}

			// Private 状态
			if opts.Private {
				gerritOptions = append(gerritOptions, "private")
			}

			// --replace：让 Gerrit 替换既有 change（%replace）
			if opts.Replace {
				gerritOptions = append(gerritOptions, "replace")
			}

			// Topic
			if opts.Topic != "" {
				gerritOptions = append(gerritOptions, "topic="+opts.Topic)
			}

			// Hashtags
			if opts.Hashtags != "" {
				for _, tag := range strings.Split(opts.Hashtags, ",") {
					gerritOptions = append(gerritOptions, "hashtag="+strings.TrimSpace(tag))
				}
			}

			// --hashtag-branch：把本地分支名作为一个 hashtag 加入审查
			if opts.HashtagBranch && currentBranch != "" {
				gerritOptions = append(gerritOptions, "hashtag="+currentBranch)
			}

			// Labels
			if opts.Labels != "" {
				for _, label := range strings.Split(opts.Labels, ",") {
					gerritOptions = append(gerritOptions, "label="+strings.TrimSpace(label))
				}
			}

			// Reviewers
			if opts.Reviewers != "" {
				for _, reviewer := range strings.Split(opts.Reviewers, ",") {
					gerritOptions = append(gerritOptions, "r="+strings.TrimSpace(reviewer))
				}
			}

			// CC
			if opts.CC != "" {
				for _, cc := range strings.Split(opts.CC, ",") {
					gerritOptions = append(gerritOptions, "cc="+strings.TrimSpace(cc))
				}
			}

			// --no-emails：Gerrit 推送时不发送邮件通知（%notify=NONE）
			if opts.NoEmails {
				gerritOptions = append(gerritOptions, "notify=NONE")
			}

			// 自定义 push-option（可重复）
			gerritOptions = append(gerritOptions, opts.PushOption...)

			// 构建完整的 refspec，将选项附加到 refs 后面
			// 格式: HEAD:refs/for/<branch>%option1%option2%option3
			refspec := fmt.Sprintf("HEAD:%s", gerritRef)
			if len(gerritOptions) > 0 {
				refspec = refspec + "%" + strings.Join(gerritOptions, ",")
			}

			// 添加远程和引用
			// 格式: git push <remote> HEAD:refs/for/<branch>%wip%topic=xxx
			pushArgs = append(pushArgs, remoteName, refspec)

			log.Info("正在上传项目 %s 的变更到 Gerrit 审查系统 (%s -> %s)", p.Name, remoteName, gerritRef)
			if opts.Wip {
				log.Info("将创建 WIP (进行中) 状态的审查")
			}

			// 如果是模拟运行，不实际上传
			if opts.DryRun {
				log.Info("模拟运行: 将上传项目 %s 的变更，命令: git %s", p.Name, strings.Join(pushArgs, " "))
				stats.increment(true)
				return
			}

			// 执行上传命令
			outputBytes, err := p.GitRepo.RunCommand(pushArgs...)
			if err != nil {
				errMsg := fmt.Sprintf("上传项目 %s 的变更failed: %v\n%s", p.Name, err, string(outputBytes))
				log.Error("%s", errMsg)
				errChan <- errors.New(errMsg)
				stats.increment(false)
				return
			}

			log.Info("成功上传项目 %s 的变更", p.Name)
			output := strings.TrimSpace(string(outputBytes))
			if output != "" {
				log.Info("上传输出:\n%s", output)
			}
			stats.increment(true)
		}()
	}

	// 等待所有goroutine完成
	log.Debug("等待所有上传任务完成")
	wg.Wait()
	close(errChan)

	// 收集错误
	var errs []error
	for err := range errChan {
		errs = append(errs, err)
	}

	// 输出统计信息
	log.Info("上传完成，总计: %d, 成功: %d, failed: %d", stats.total, stats.success, stats.failed)

	if len(errs) > 0 {
		return errors.Join(errs...)
	}

	log.Info("所有项目上传成功完成")
	return nil
}

// runUploadHook 在上传前运行 .repo/hooks/pre-upload（若存在）。
// 对齐上游契约：把待上传 commit 的 SHA 列表作为位置参数传入，并导出 REPO_* 环境变量。
// failed则阻止该项目上传；hook 不存在则视为通过。
func runUploadHook(p *project.Project, log logger.Logger) error {
	// 定位 .repo/hooks/pre-upload（相对于 repo 根）
	hookPath := filepath.Join(".repo", "hooks", "pre-upload")
	if _, err := os.Stat(hookPath); err != nil {
		// hook 不存在，跳过
		return nil
	}
	log.Debug("运行 upload hook: %s (项目 %s)", hookPath, p.Name)

	// 计算待上传的 commit 列表（manifest revision..HEAD），作为位置参数传给 hook
	commits := []string{}
	if p.Revision != "" {
		out, err := p.GitRepo.RunCommand("rev-list", p.Revision+"..HEAD")
		if err == nil {
			for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
				if line = strings.TrimSpace(line); line != "" {
					commits = append(commits, line)
				}
			}
		} else {
			log.Debug("项目 %s 计算 rev-list failed，hook 将不接收 commit 参数: %v", p.Name, err)
		}
	}

	// 构建 REPO_* 环境变量（与 forall 约定一致）
	env := os.Environ()
	outPwd := p.Worktree
	if abs, err := filepath.Abs(p.Worktree); err == nil {
		outPwd = abs
	}
	remoteName := p.RemoteName
	if remoteName == "" {
		remoteName = "origin"
	}
	env = append(env,
		"REPO_PATH="+p.Relpath,
		"REPO_PROJECT="+p.Name,
		"REPO_REMOTE="+remoteName,
		"REPO_LREV="+p.Revision,
		"REPO_RREV="+p.Revision,
		"REPO_OUT_PWD="+outPwd,
	)

	cmd := exec.Command(hookPath, commits...)
	cmd.Dir = p.Worktree
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = env
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("pre-upload hook failed: %w", err)
	}
	return nil
}
