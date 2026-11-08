package commands

import (
	"bytes"
	"strings"
	"testing"

	"github.com/leopardxu/repo-go/internal/git"
	"github.com/leopardxu/repo-go/internal/logger"
	"github.com/leopardxu/repo-go/internal/project"
)

// TestPrintPendingBranches 验证上游 "Pending Branches" 输出格式：
// 分组头、当前分支 * 标记、提交数与日期（核心渲染逻辑，不依赖 git）。
// Pending 属于结果 payload，-q 也必须输出。
func TestPrintPendingBranches(t *testing.T) {
	tests := []struct {
		name    string
		pending []pendingBranch
	}{
		{
			name: "空列表不输出",
		},
		{
			name: "单项目多分支分组输出",
			pending: []pendingBranch{
				{Project: "component/a", Branch: "topic1", Commits: 3, Date: "2026-08-01"},
				{Project: "component/a", Branch: "topic2", Current: true, Commits: 12, Date: "2026-08-02"},
			},
		},
		{
			name: "多项目分组切换",
			pending: []pendingBranch{
				{Project: "component/a", Branch: "topic1", Commits: 1, Date: "2026-08-01"},
				{Project: "component/b", Branch: "topic9", Commits: 7, Date: "2026-08-05"},
			},
		},
		{
			name: "quiet 模式仍输出 payload",
			pending: []pendingBranch{
				{Project: "component/a", Branch: "topic1", Commits: 3, Date: "2026-08-01"},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			opts := &PruneOptions{Quiet: true}
			printPendingBranches(&buf, tt.pending, opts)

			if len(tt.pending) == 0 {
				if buf.Len() != 0 {
					t.Fatalf("output = %q, want empty", buf.String())
				}
				return
			}
			out := buf.String()
			if !strings.Contains(out, "Pending Branches") {
				t.Errorf("output missing header: %q", out)
			}
			for _, pb := range tt.pending {
				if !strings.Contains(out, pb.Branch) {
					t.Errorf("output missing branch %q: %q", pb.Branch, out)
				}
				if !strings.Contains(out, "project "+pb.Project+"/") {
					t.Errorf("output missing project %q: %q", pb.Project, out)
				}
			}
			if strings.Contains(out, "*") && !hasCurrentBranch(tt.pending) {
				t.Errorf("unexpected current marker in %q", out)
			}
		})
	}
}

// hasCurrentBranch 判断待报告分支中是否存在当前分支（* 标记）。
func hasCurrentBranch(pending []pendingBranch) bool {
	for _, pb := range pending {
		if pb.Current {
			return true
		}
	}
	return false
}

// TestPruneReportBranches 验证删除后存活分支分类：
//   - targets 中存活即删除失败（伪分支/权限等）；
//   - refused（未合并）存活为 pending；
//   - detached HEAD（currentBranch == ""）不产生当前分支报告；
//   - includeCurrent 时当前分支始终进入 Pending Branches（* 标记）。
func TestPruneReportBranches(t *testing.T) {
	tests := []struct {
		name          string
		targets       []string
		refused       []string
		survivors     map[string]bool
		currentBranch string
		includeCur    bool
		wantPending   []string
		wantFailed    []string
	}{
		{
			name:      "全部删除成功",
			targets:   []string{"a", "b"},
			survivors: map[string]bool{},
		},
		{
			name:        "未合并分支保留为 pending",
			refused:     []string{"a"},
			survivors:   map[string]bool{"a": true},
			wantPending: []string{"a"},
		},
		{
			name:       "已合并分支存活上报错误",
			targets:    []string{"a"},
			survivors:  map[string]bool{"a": true},
			wantFailed: []string{"a"},
		},
		{
			name:          "当前分支始终进入报告",
			survivors:     map[string]bool{"main": true},
			currentBranch: "main",
			includeCur:    true,
			wantPending:   []string{"main"},
		},
		{
			name:          "detached 时不报告当前分支",
			refused:       []string{"a"},
			survivors:     map[string]bool{"a": true},
			currentBranch: "",
			wantPending:   []string{"a"},
		},
		{
			name:          "targets 与 refused 混合分类",
			targets:       []string{"a"},
			refused:       []string{"b"},
			survivors:     map[string]bool{"a": true, "b": true, "main": true},
			currentBranch: "main",
			includeCur:    true,
			wantPending:   []string{"b", "main"},
			wantFailed:    []string{"a"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pending, failed := pruneReportBranches(tt.targets, tt.refused, tt.survivors, tt.currentBranch, tt.includeCur)
			if got, want := len(pending), len(tt.wantPending); got != want {
				t.Fatalf("pending = %v, want %v", pending, tt.wantPending)
			}
			for i := range tt.wantPending {
				if pending[i] != tt.wantPending[i] {
					t.Errorf("pending[%d] = %q, want %q", i, pending[i], tt.wantPending[i])
				}
			}
			if got, want := len(failed), len(tt.wantFailed); got != want {
				t.Fatalf("failed = %v, want %v", failed, tt.wantFailed)
			}
			for i := range tt.wantFailed {
				if failed[i] != tt.wantFailed[i] {
					t.Errorf("failed[%d] = %q, want %q", i, failed[i], tt.wantFailed[i])
				}
			}
		})
	}
}

// TestPruneDryRunPlan 验证 dry-run 计划：默认按已合并判定，
// --force 下未合并分支也纳入删除计划。
func TestPruneDryRunPlan(t *testing.T) {
	tests := []struct {
		name       string
		kill       []string
		force      bool
		merged     func(string) bool
		wantDelete []string
		wantKept   []string
	}{
		{
			name:       "仅计划删除已合并分支",
			kill:       []string{"a", "b"},
			merged:     func(b string) bool { return b == "a" },
			wantDelete: []string{"a"},
			wantKept:   []string{"b"},
		},
		{
			name:       "force 计划删除全部",
			kill:       []string{"a", "b"},
			force:      true,
			merged:     func(string) bool { return false },
			wantDelete: []string{"a", "b"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			deleted, kept := pruneDryRunPlan(tt.kill, tt.force, tt.merged)
			if len(deleted) != len(tt.wantDelete) || len(kept) != len(tt.wantKept) {
				t.Fatalf("deleted=%v kept=%v, want delete=%v kept=%v", deleted, kept, tt.wantDelete, tt.wantKept)
			}
			for i := range tt.wantDelete {
				if deleted[i] != tt.wantDelete[i] {
					t.Errorf("deleted[%d] = %q, want %q", i, deleted[i], tt.wantDelete[i])
				}
			}
			for i := range tt.wantKept {
				if kept[i] != tt.wantKept[i] {
					t.Errorf("kept[%d] = %q, want %q", i, kept[i], tt.wantKept[i])
				}
			}
		})
	}
}

// pruneSequenceRunner 在 mockGitRunner 之上支持对同一命令预设多次响应，
// 用于模拟"删除分支前后 for-each-ref 结果不同"。仅 RunInDir 走序列，
// 其余 Runner 方法仍委托 mockGitRunner。
type pruneSequenceRunner struct {
	*mockGitRunner
	sequences map[string][]mockGitResp
}

func newPruneSequenceRunner(sequences map[string][]mockGitResp) *pruneSequenceRunner {
	return &pruneSequenceRunner{
		mockGitRunner: &mockGitRunner{responses: make(map[string]mockGitResp)},
		sequences:     sequences,
	}
}

func (r *pruneSequenceRunner) RunInDir(dir string, args ...string) ([]byte, error) {
	key := strings.Join(args, " ")
	if queue := r.sequences[key]; len(queue) > 0 {
		resp := queue[0]
		r.sequences[key] = queue[1:]
		r.commands = append(r.commands, key)
		return resp.out, resp.err
	}
	return r.mockGitRunner.RunInDir(dir, args...)
}

func newPruneTestProject(runner git.Runner) *project.Project {
	return &project.Project{
		Name:       "p",
		Path:       "/repo/p",
		Worktree:   "/repo/p",
		RemoteName: "origin",
		Revision:   "main",
		RevisionID: "main",
		GitRepo:    git.NewRepository("/repo/p", runner),
	}
}

func pruneTestLogger() logger.Logger {
	log := logger.NewDefaultLogger()
	log.SetLevel(logger.LogLevelError)
	return log
}

// TestPruneCurrentBranchAtManifestRevision 验证当前分支删除守卫：
// manifest 修订版本按 refs/remotes/origin/main 解析（不是本地同名分支），
// HEAD 等于该提交且工作树干净时才 detach 并删除当前分支。
func TestPruneCurrentBranchAtManifestRevision(t *testing.T) {
	runner := newPruneSequenceRunner(map[string][]mockGitResp{
		"for-each-ref --format=%(refname) refs/heads/": {
			{out: []byte("refs/heads/main\nrefs/heads/topic\n")},
			{out: []byte("")},
		},
		"symbolic-ref HEAD": {{out: []byte("refs/heads/main\n")}},
		"rev-parse --verify --quiet refs/remotes/origin/main^0": {{out: []byte("abc1234\n")}},
		"rev-parse --verify HEAD":                               {{out: []byte("abc1234\n")}},
		"diff-index --quiet HEAD --":                            {{}},
		"checkout --detach abc1234":                             {{}},
		"checkout main":                                         {{err: pruneTestError("branch deleted")}},
		"branch -D main topic":                                  {{}},
	})
	proj := newPruneTestProject(runner)

	deleted, pending, result := pruneProjectBranches(proj, &PruneOptions{}, pruneTestLogger())

	if result.err != nil {
		t.Fatalf("pruneProjectBranches() error = %v", result.err)
	}
	if deleted != 2 || result.success != 2 {
		t.Fatalf("deleted = %d, success = %d, want 2", deleted, result.success)
	}
	if len(pending) != 0 {
		t.Fatalf("pending = %v, want none", pending)
	}
	if !runner.hasCommand("checkout --detach abc1234") {
		t.Errorf("expected detach to manifest revision, commands = %v", runner.commands)
	}
	if !runner.hasCommand("branch -D main topic") {
		t.Errorf("expected current branch included in delete, commands = %v", runner.commands)
	}
	// 关键回归：不得用 rev-parse <revision> 解析本地同名分支。
	if runner.hasCommand("rev-parse --verify main") || runner.hasCommand("rev-parse --verify --quiet main^0") {
		t.Errorf("manifest revision must not resolve local branch, commands = %v", runner.commands)
	}
}

// TestPruneCurrentBranchNotAtManifestRevision 验证 HEAD 与 manifest 修订版本不同时
// 不删除当前分支（作为 pending 保留），删除非当前分支也不 detach（工作树保持现状），
// 只在必要时恢复 HEAD。
func TestPruneCurrentBranchNotAtManifestRevision(t *testing.T) {
	runner := newPruneSequenceRunner(map[string][]mockGitResp{
		"for-each-ref --format=%(refname) refs/heads/": {
			{out: []byte("refs/heads/main\nrefs/heads/topic\n")},
			{out: []byte("refs/heads/main\n")},
		},
		"symbolic-ref HEAD": {{out: []byte("refs/heads/main\n")}},
		"rev-parse --verify --quiet refs/remotes/origin/main^0": {{out: []byte("abc1234\n")}},
		"rev-parse --verify HEAD":                               {{out: []byte("def5678\n")}},
		"branch -D topic":                                       {{}},
	})
	proj := newPruneTestProject(runner)

	deleted, pending, result := pruneProjectBranches(proj, &PruneOptions{}, pruneTestLogger())

	if result.err != nil {
		t.Fatalf("pruneProjectBranches() error = %v", result.err)
	}
	if deleted != 1 {
		t.Fatalf("deleted = %d, want 1", deleted)
	}
	// 非当前分支无需 detach，工作树不应被切换。
	if runner.hasCommand("checkout --detach abc1234") {
		t.Errorf("non-current branches must not require detach, commands = %v", runner.commands)
	}
	if !runner.hasCommand("branch -D topic") {
		t.Errorf("expected topic deleted, commands = %v", runner.commands)
	}
	if runner.hasCommand("checkout main") {
		t.Errorf("HEAD must not be touched when no detach happened, commands = %v", runner.commands)
	}
	if len(pending) != 1 || pending[0].Branch != "main" || !pending[0].Current {
		t.Fatalf("pending = %+v, want current branch main", pending)
	}
}

// TestPrunePendingCommitsUseManifestBaseline 验证无 upstream 的 pending 分支
// 提交数兜底基线为 manifest 修订提交而不是 HEAD。
func TestPrunePendingCommitsUseManifestBaseline(t *testing.T) {
	runner := newPruneSequenceRunner(map[string][]mockGitResp{
		"rev-parse --verify topic@{upstream}":        {{err: pruneTestError("no upstream")}},
		"rev-list --count abc1234..refs/heads/topic": {{out: []byte("5\n")}},
		"log -1 --format=%cs refs/heads/topic":       {{out: []byte("2026-01-01\n")}},
	})
	proj := newPruneTestProject(runner)

	pb := newPendingBranch(proj, "topic", false, "abc1234")

	if pb.Commits != 5 {
		t.Fatalf("Commits = %d, want 5 (baseline manifest revision)", pb.Commits)
	}
	if runner.hasCommand("rev-list --count HEAD..refs/heads/topic") {
		t.Errorf("pending commits must not use HEAD as baseline")
	}
	if !runner.hasCommand("rev-list --count abc1234..refs/heads/topic") {
		t.Errorf("rev-list must use full refs/heads/topic, commands = %v", runner.commands)
	}
	if !runner.hasCommand("log -1 --format=%cs refs/heads/topic") {
		t.Errorf("log must use full refs/heads/topic, commands = %v", runner.commands)
	}
}

// TestPruneWorktreeOccupiedBranchIsPending 验证被其他 worktree 占用的已合并分支
// 按 pending 处理：不参与删除、不计为错误。
func TestPruneWorktreeOccupiedBranchIsPending(t *testing.T) {
	worktrees := strings.Join([]string{
		"worktree /repo/p",
		"HEAD def5678",
		"branch refs/heads/main",
		"",
		"worktree /repo/other",
		"HEAD abc1234",
		"branch refs/heads/topic",
		"",
	}, "\n")
	runner := newPruneSequenceRunner(map[string][]mockGitResp{
		"for-each-ref --format=%(refname) refs/heads/": {
			{out: []byte("refs/heads/main\nrefs/heads/topic\nrefs/heads/foo\n")},
			{out: []byte("refs/heads/main\nrefs/heads/topic\n")},
		},
		"symbolic-ref HEAD": {{out: []byte("refs/heads/main\n")}},
		"rev-parse --verify --quiet refs/remotes/origin/main^0": {{out: []byte("abc1234\n")}},
		"rev-parse --verify HEAD":                               {{out: []byte("def5678\n")}},
		"worktree list --porcelain":                             {{out: []byte(worktrees)}},
		"branch -D foo":                                         {{}},
	})
	proj := newPruneTestProject(runner)

	_, pending, result := pruneProjectBranches(proj, &PruneOptions{}, pruneTestLogger())

	if result.err != nil {
		t.Fatalf("pruneProjectBranches() error = %v", result.err)
	}
	if result.failed != 0 {
		t.Fatalf("failed = %d, want 0 (worktree occupied is pending)", result.failed)
	}
	if runner.hasCommand("branch -D foo topic") || runner.hasCommand("branch -D topic foo") {
		t.Errorf("worktree occupied branch must not be passed to git branch -D, commands = %v", runner.commands)
	}
	found := false
	for _, pb := range pending {
		if pb.Branch == "topic" {
			found = true
		}
	}
	if !found {
		t.Fatalf("pending = %+v, want worktree occupied topic", pending)
	}
}

// TestPruneRestoreHeadOnDeleteFailure 验证当前分支删除失败时把 HEAD 恢复到原分支，
// 不会留下 detached HEAD。
func TestPruneRestoreHeadOnDeleteFailure(t *testing.T) {
	runner := newPruneSequenceRunner(map[string][]mockGitResp{
		"for-each-ref --format=%(refname) refs/heads/": {
			{out: []byte("refs/heads/main\nrefs/heads/topic\n")},
			{out: []byte("refs/heads/main\nrefs/heads/topic\n")},
		},
		"symbolic-ref HEAD": {{out: []byte("refs/heads/main\n")}},
		"rev-parse --verify --quiet refs/remotes/origin/main^0": {{out: []byte("abc1234\n")}},
		"rev-parse --verify HEAD":                               {{out: []byte("abc1234\n")}},
		"diff-index --quiet HEAD --":                            {{}},
		"checkout --detach abc1234":                             {{}},
		"checkout main":                                         {{}},
		"branch -D main topic":                                  {{err: pruneTestError("cannot delete")}},
	})
	proj := newPruneTestProject(runner)

	_, _, result := pruneProjectBranches(proj, &PruneOptions{}, pruneTestLogger())

	if result.err == nil {
		t.Fatal("pruneProjectBranches() error = nil, want deletion failure")
	}
	if !runner.hasCommand("checkout main") {
		t.Errorf("expected HEAD restored to original branch, commands = %v", runner.commands)
	}
}

// TestPruneUnambiguousBranchNameWithTag 验证存在同名 tag 时分支名仍按
// refs/heads/ 剥离后的短名传递给 git branch -d（而不是 %(refname:short) 的
// 歧义形式 "heads/foo"），current branch 也从完整 symbolic-ref 剥离前缀。
func TestPruneUnambiguousBranchNameWithTag(t *testing.T) {
	runner := newPruneSequenceRunner(map[string][]mockGitResp{
		"for-each-ref --format=%(refname) refs/heads/": {
			// foo 同时是 tag 名，for-each-ref 仍只返回 refs/heads/foo。
			{out: []byte("refs/heads/main\nrefs/heads/foo\n")},
			{out: []byte("refs/heads/main\n")},
		},
		"symbolic-ref HEAD": {{out: []byte("refs/heads/main\n")}},
		"rev-parse --verify --quiet refs/remotes/origin/main^0": {{out: []byte("abc1234\n")}},
		"rev-parse --verify HEAD":                               {{out: []byte("def5678\n")}},
		"branch -D foo":                                         {{}},
	})
	proj := newPruneTestProject(runner)

	deleted, pending, result := pruneProjectBranches(proj, &PruneOptions{}, pruneTestLogger())

	if result.err != nil {
		t.Fatalf("pruneProjectBranches() error = %v", result.err)
	}
	if deleted != 1 || result.success != 1 {
		t.Fatalf("deleted = %d, success = %d, want 1", deleted, result.success)
	}
	if !runner.hasCommand("branch -D foo") {
		t.Errorf("expected branch -D foo, commands = %v", runner.commands)
	}
	if runner.hasCommand("branch -D heads/foo") {
		t.Errorf("ambiguous %%(refname:short) name must not be used, commands = %v", runner.commands)
	}
	if len(pending) != 1 || pending[0].Branch != "main" || !pending[0].Current {
		t.Fatalf("pending = %+v, want current branch main (prefix stripped)", pending)
	}
}

// TestPruneDetachFailureKeepsOtherDeletions 验证 detach 失败只影响当前分支，
// 不会因 detach 失败放弃其他可删的非当前分支。
func TestPruneDetachFailureKeepsOtherDeletions(t *testing.T) {
	runner := newPruneSequenceRunner(map[string][]mockGitResp{
		"for-each-ref --format=%(refname) refs/heads/": {
			{out: []byte("refs/heads/main\nrefs/heads/topic\n")},
			{out: []byte("refs/heads/main\n")},
		},
		"symbolic-ref HEAD": {{out: []byte("refs/heads/main\n")}},
		"rev-parse --verify --quiet refs/remotes/origin/main^0": {{out: []byte("abc1234\n")}},
		"rev-parse --verify HEAD":                               {{out: []byte("abc1234\n")}},
		"diff-index --quiet HEAD --":                            {{}},
		"checkout --detach abc1234":                             {{err: pruneTestError("dirty worktree")}},
		"branch -D topic":                                       {{}},
	})
	proj := newPruneTestProject(runner)

	deleted, _, result := pruneProjectBranches(proj, &PruneOptions{}, pruneTestLogger())

	if result.err == nil {
		t.Fatal("pruneProjectBranches() error = nil, want detach failure for current branch")
	}
	if deleted != 1 {
		t.Fatalf("deleted = %d, want 1 (non-current branch still pruned)", deleted)
	}
	if !runner.hasCommand("branch -D topic") {
		t.Errorf("expected non-current branch deleted despite detach failure, commands = %v", runner.commands)
	}
	if runner.hasCommand("branch -D main topic") || runner.hasCommand("branch -D main") {
		t.Errorf("current branch must not be deleted when detach fails, commands = %v", runner.commands)
	}
}

// TestPruneDirtyWorktreeDeletesNonCurrent 验证工作树脏时仍可删除已合并的非当前分支，
// 且不会 checkout/detach；当前分支因工作树脏保留为 pending。
func TestPruneDirtyWorktreeDeletesNonCurrent(t *testing.T) {
	runner := newPruneSequenceRunner(map[string][]mockGitResp{
		"for-each-ref --format=%(refname) refs/heads/": {
			{out: []byte("refs/heads/main\nrefs/heads/topic\n")},
			{out: []byte("refs/heads/main\n")},
		},
		"symbolic-ref HEAD": {{out: []byte("refs/heads/main\n")}},
		"rev-parse --verify --quiet refs/remotes/origin/main^0": {{out: []byte("abc1234\n")}},
		"rev-parse --verify HEAD":                               {{out: []byte("abc1234\n")}},
		"diff-index --quiet HEAD --":                            {{err: pruneTestError("dirty")}},
		"branch -D topic":                                       {{}},
	})
	proj := newPruneTestProject(runner)

	deleted, pending, result := pruneProjectBranches(proj, &PruneOptions{}, pruneTestLogger())

	if result.err != nil {
		t.Fatalf("pruneProjectBranches() error = %v", result.err)
	}
	if deleted != 1 || result.success != 1 {
		t.Fatalf("deleted = %d, success = %d, want 1", deleted, result.success)
	}
	if runner.hasCommand("checkout --detach abc1234") || runner.hasCommand("checkout main") {
		t.Errorf("dirty worktree prune must not checkout, commands = %v", runner.commands)
	}
	if !runner.hasCommand("branch -D topic") {
		t.Errorf("expected merged non-current branch deleted in dirty worktree, commands = %v", runner.commands)
	}
	if len(pending) != 1 || pending[0].Branch != "main" || !pending[0].Current {
		t.Fatalf("pending = %+v, want dirty current branch main", pending)
	}
}

// TestPruneRefArgsAvoidTagAmbiguity 验证 merge-base 使用完整 refs/heads/<branch>，
// 而 @{upstream} 仍使用分支短名（git 的 upstream 语法不接受 refs/heads/ 前缀）。
func TestPruneRefArgsAvoidTagAmbiguity(t *testing.T) {
	runner := newPruneSequenceRunner(map[string][]mockGitResp{
		"rev-parse --verify topic@{upstream}": {{out: []byte("def5678\n")}},
	})
	proj := newPruneTestProject(runner)

	if !branchMerged(proj, "topic", "abc1234") {
		t.Fatal("branchMerged() = false, want true (mock merge-base succeeds)")
	}
	if !runner.hasCommand("merge-base --is-ancestor refs/heads/topic def5678") {
		t.Errorf("merge-base must use full refs/heads/topic, commands = %v", runner.commands)
	}

	runner.commands = nil
	runner.responses["rev-parse --verify topic@{upstream}"] = mockGitResp{out: []byte("def5678\n")}
	_ = newPendingBranch(proj, "topic", false, "abc1234")
	if !runner.hasCommand("rev-list --count def5678..refs/heads/topic") {
		t.Errorf("rev-list must use full refs/heads/topic, commands = %v", runner.commands)
	}
	if !runner.hasCommand("log -1 --format=%cs refs/heads/topic") {
		t.Errorf("log must use full refs/heads/topic, commands = %v", runner.commands)
	}
}

// TestPruneRejectMirrorProjects 验证显式指定镜像项目时返回清晰错误。
func TestPruneRejectMirrorProjects(t *testing.T) {
	mirror := &project.Project{Name: "mirror", Mirror: true}
	normal := &project.Project{Name: "normal"}

	if err := pruneRejectMirrorProjects([]*project.Project{normal}); err != nil {
		t.Fatalf("pruneRejectMirrorProjects(normal) error = %v", err)
	}
	err := pruneRejectMirrorProjects([]*project.Project{normal, mirror})
	if err == nil {
		t.Fatal("pruneRejectMirrorProjects(mirror) = nil, want error")
	}
	if !strings.Contains(err.Error(), "mirror") || !strings.Contains(err.Error(), "mirror project") {
		t.Errorf("error = %q, want clear mirror unsupported message", err)
	}
}

// TestPruneForceFlagHelp 验证 -f 帮助与实际行为一致：--force 跳过已合并校验，
// 强制删除未合并分支，而不是"有本地改动时强制清理"。
func TestPruneForceFlagHelp(t *testing.T) {
	flag := PruneCmd().Flags().Lookup("force")
	if flag == nil {
		t.Fatal("prune --force flag not found")
	}
	if !strings.Contains(flag.Usage, "not merged") || !strings.Contains(flag.Usage, "merge check") {
		t.Errorf("force usage = %q, want not-merged/merge-check description", flag.Usage)
	}
}

// TestPruneManifestRevisionFailure 验证 manifest revision 无法解析时报错，
// 绝不静默回退 base=HEAD（否则会以工作区 HEAD 为基准误删未合并分支）。
func TestPruneManifestRevisionFailure(t *testing.T) {
	runner := newPruneSequenceRunner(map[string][]mockGitResp{
		"for-each-ref --format=%(refname) refs/heads/": {
			{out: []byte("refs/heads/main\nrefs/heads/topic\n")},
		},
		"symbolic-ref HEAD": {{out: []byte("refs/heads/main\n")}},
		// refs/remotes/origin/main^0 未配置响应 -> 空输出，解析失败。
	})
	proj := newPruneTestProject(runner)

	_, _, result := pruneProjectBranches(proj, &PruneOptions{}, pruneTestLogger())

	if result.err == nil || !strings.Contains(result.err.Error(), "not found") {
		t.Fatalf("error = %v, want manifest revision not found", result.err)
	}
	if runner.hasCommand("rev-parse --verify HEAD") {
		t.Errorf("must not fall back to HEAD for merge baseline, commands = %v", runner.commands)
	}
	if runner.hasCommand("branch -D topic") || runner.hasCommand("branch -D main topic") {
		t.Errorf("must not delete branches when revision is unresolved, commands = %v", runner.commands)
	}
}

// TestPruneRestoreFailureCountsFailed 验证 HEAD 恢复失败计入 failed 统计，
// 且返回错误（不能只记录 err 却漏报失败数）。
func TestPruneRestoreFailureCountsFailed(t *testing.T) {
	runner := newPruneSequenceRunner(map[string][]mockGitResp{
		"for-each-ref --format=%(refname) refs/heads/": {
			{out: []byte("refs/heads/main\nrefs/heads/topic\n")},
			{out: []byte("")},
		},
		"symbolic-ref HEAD": {{out: []byte("refs/heads/main\n")}},
		"rev-parse --verify --quiet refs/remotes/origin/main^0": {{out: []byte("abc1234\n")}},
		"rev-parse --verify HEAD":                               {{out: []byte("abc1234\n")}},
		"diff-index --quiet HEAD --":                            {{}},
		// 首次 checkout --detach 用于删除当前分支，必须成功；
		// 第二次（pruneRestoreHead）失败，用于验证失败计数。
		"checkout --detach abc1234": {
			{},
			{err: pruneTestError("cannot restore")},
		},
		"branch -D main topic": {{}},
		"checkout main":        {{err: pruneTestError("cannot restore")}},
	})
	proj := newPruneTestProject(runner)

	_, _, result := pruneProjectBranches(proj, &PruneOptions{}, pruneTestLogger())

	if result.err == nil || !strings.Contains(result.err.Error(), "restore HEAD") {
		t.Fatalf("error = %v, want restore HEAD failure", result.err)
	}
	if result.failed != 1 {
		t.Fatalf("failed = %d, want 1 (restore failure counted)", result.failed)
	}
	if result.success != 2 {
		t.Fatalf("success = %d, want 2", result.success)
	}
}

// TestPruneStdoutPayload 验证结果 payload（统计 / dry-run 计划）写入给定 writer，
// 而不是依赖 logger。
func TestPruneStdoutPayload(t *testing.T) {
	t.Run("统计", func(t *testing.T) {
		var buf bytes.Buffer
		prunePrintSummary(&buf, &pruneStats{total: 3, success: 2, failed: 1}, 4, &PruneOptions{})
		out := buf.String()
		for _, want := range []string{"共检查分支 3", "成功删除 2", "failed 1"} {
			if !strings.Contains(out, want) {
				t.Errorf("stdout %q missing %q", out, want)
			}
		}
	})

	t.Run("dry-run 统计", func(t *testing.T) {
		var buf bytes.Buffer
		prunePrintSummary(&buf, &pruneStats{success: 2}, 4, &PruneOptions{DryRun: true})
		out := buf.String()
		if !strings.Contains(out, "计划删除 2") || !strings.Contains(out, "报告 4") {
			t.Errorf("stdout = %q, want dry-run plan counts", out)
		}
	})

	t.Run("dry-run 计划", func(t *testing.T) {
		var buf bytes.Buffer
		prunePrintDryRunPlan(&buf, "p", []string{"a", "b"})
		out := buf.String()
		for _, want := range []string{"p", "分支 a", "分支 b"} {
			if !strings.Contains(out, want) {
				t.Errorf("stdout %q missing %q", out, want)
			}
		}
	})
}

// hasCommand 判断 mock 记录中是否出现过指定命令行
func (r *pruneSequenceRunner) hasCommand(cmd string) bool {
	for _, c := range r.commands {
		if c == cmd {
			return true
		}
	}
	return false
}

// pruneTestError 返回固定错误，便于在 mock 响应中模拟命令失败。
func pruneTestError(msg string) error { return &pruneTestErr{msg: msg} }

type pruneTestErr struct{ msg string }

func (e *pruneTestErr) Error() string { return e.msg }
