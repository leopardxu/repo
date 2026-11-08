package commands

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/leopardxu/repo-go/internal/config"
	"github.com/leopardxu/repo-go/internal/git"
	"github.com/leopardxu/repo-go/internal/logger"
	"github.com/leopardxu/repo-go/internal/manifest"
	"github.com/leopardxu/repo-go/internal/project"
)

// TestUploadCmdReviewerAndReadyFlags 验证 upload flag 绑定：
// -r 绑定 ready；reviewers 仅 --re/--reviewers 且可重复；-t 绑定 topic-branch。
func TestUploadCmdReviewerAndReadyFlags(t *testing.T) {
	cmd := UploadCmd()
	flags := cmd.Flags()

	reviewers := flags.Lookup("reviewers")
	if reviewers == nil {
		t.Fatal("missing --reviewers flag")
	}
	if reviewers.Shorthand != "" {
		t.Errorf("--reviewers must be long-only, got shorthand %q", reviewers.Shorthand)
	}
	if flags.Lookup("re") == nil {
		t.Error("missing alias --re")
	}
	if flags.Lookup("reviewer") != nil {
		t.Error("--reviewer alias should be removed (upstream only has --re/--reviewers)")
	}

	ready := flags.Lookup("ready")
	if ready == nil {
		t.Fatal("missing --ready flag")
	}
	if ready.Shorthand != "r" {
		t.Errorf("--ready shorthand = %q, want %q", ready.Shorthand, "r")
	}

	topicBranch := flags.Lookup("topic-branch")
	if topicBranch == nil {
		t.Fatal("missing --topic-branch flag")
	}
	if topicBranch.Shorthand != "t" {
		t.Errorf("--topic-branch shorthand = %q, want %q", topicBranch.Shorthand, "t")
	}
	if topic := flags.Lookup("topic"); topic == nil {
		t.Fatal("missing --topic flag")
	} else if topic.Shorthand != "" {
		t.Errorf("--topic must be long-only, got shorthand %q", topic.Shorthand)
	}

	// -r 绑定 ready，而非 reviewers
	if err := cmd.ParseFlags([]string{"-r"}); err != nil {
		t.Fatalf("ParseFlags(-r) failed: %v", err)
	}
	if !flags.Changed("ready") {
		t.Error("-r did not bind to --ready")
	}
	if isReady, _ := flags.GetBool("ready"); !isReady {
		t.Error("-r did not enable --ready")
	}
	if got, _ := flags.GetStringArray("reviewers"); len(got) != 0 {
		t.Errorf("-r unexpectedly set reviewers: %v", got)
	}

	// --re 可重复
	cmd2 := UploadCmd()
	flags2 := cmd2.Flags()
	if err := cmd2.ParseFlags([]string{"--re", "alice@example.com", "--re", "bob@example.com"}); err != nil {
		t.Fatalf("ParseFlags(--re --re) failed: %v", err)
	}
	got, err := flags2.GetStringArray("reviewers")
	if err != nil {
		t.Fatalf("GetStringArray(reviewers) failed: %v", err)
	}
	if want := []string{"alice@example.com", "bob@example.com"}; !reflect.DeepEqual(got, want) {
		t.Errorf("reviewers = %#v, want %#v", got, want)
	}
	if flags2.Changed("ready") {
		t.Error("--re unexpectedly set --ready")
	}

	// 上游短选项对齐：-p/-w/-D/-y/-l
	shorthands := map[string]string{
		"private":     "p",
		"wip":         "w",
		"destination": "D",
		"yes":         "y",
		"label":       "l",
	}
	for name, want := range shorthands {
		f := flags.Lookup(name)
		if f == nil {
			t.Errorf("missing --%s flag", name)
			continue
		}
		if f.Shorthand != want {
			t.Errorf("--%s shorthand = %q, want %q", name, f.Shorthand, want)
		}
	}

	// -l 可重复
	cmd3 := UploadCmd()
	if err := cmd3.ParseFlags([]string{"-l", "CodeReview+1", "-l", "Verified+1"}); err != nil {
		t.Fatalf("ParseFlags(-l -l) failed: %v", err)
	}
	labels, err := cmd3.Flags().GetStringArray("label")
	if err != nil {
		t.Fatalf("GetStringArray(label) failed: %v", err)
	}
	if want := []string{"CodeReview+1", "Verified+1"}; !reflect.DeepEqual(labels, want) {
		t.Errorf("labels = %#v, want %#v", labels, want)
	}
}

// TestUploadCurrentOnly 验证 -c/--current-branch 与 --no-current-branch 的优先级：
// --no-current-branch 显式覆盖 -c（真正生效）。
func TestUploadCurrentOnly(t *testing.T) {
	tests := []struct {
		name string
		opts *UploadOptions
		want bool
	}{
		{name: "默认上传所有分支", opts: &UploadOptions{}, want: false},
		{name: "-c 仅当前分支", opts: &UploadOptions{CurrentBranch: true}, want: true},
		{name: "--no-current-branch 单独使用", opts: &UploadOptions{NoCurrentBranch: true}, want: false},
		{name: "--no-current-branch 覆盖 -c", opts: &UploadOptions{CurrentBranch: true, NoCurrentBranch: true}, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := uploadCurrentOnly(tt.opts); got != tt.want {
				t.Errorf("uploadCurrentOnly() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestBuildUploadGerritOptions 验证 push option 构造：
// --ready 显式追加 ready（而非仅去掉 wip），reviewers/labels 可重复，
// -t/--topic-branch 使用本地分支名（--topic 显式优先）。
func TestBuildUploadGerritOptions(t *testing.T) {
	tests := []struct {
		name    string
		opts    *UploadOptions
		want    []string
		wantErr bool
	}{
		{
			name: "默认无选项",
			opts: &UploadOptions{},
			want: []string{},
		},
		{
			name: "ready 追加 ready 且不追加 wip",
			opts: &UploadOptions{Ready: true},
			want: []string{"ready"},
		},
		{
			name: "wip",
			opts: &UploadOptions{Wip: true},
			want: []string{"wip"},
		},
		{
			name: "draft 等价 wip",
			opts: &UploadOptions{Draft: true},
			want: []string{"wip"},
		},
		{
			name: "ready 覆盖 wip",
			opts: &UploadOptions{Wip: true, Ready: true},
			want: []string{"ready"},
		},
		{
			name: "reviewers 可重复且支持逗号拆分",
			opts: &UploadOptions{Reviewers: []string{"alice@example.com", "bob@example.com,carol@example.com"}},
			want: []string{"r=alice@example.com", "r=bob@example.com", "r=carol@example.com"},
		},
		{
			name: "topic-branch 使用分支名",
			opts: &UploadOptions{TopicBranch: true},
			want: []string{"topic=feature/x"},
		},
		{
			name: "显式 topic 优先于 topic-branch",
			opts: &UploadOptions{Topic: "t1", TopicBranch: true},
			want: []string{"topic=t1"},
		},
		{
			name: "topic/hashtag/label/cc/no-emails",
			opts: &UploadOptions{
				Topic:      "t1",
				Hashtags:   "h1,h2",
				Labels:     []string{"CodeReview+1", "Verified+1,Owed"},
				CC:         "carol@example.com",
				NoEmails:   true,
				PushOption: []string{"custom=1"},
			},
			want: []string{
				"topic=t1",
				"hashtag=h1",
				"hashtag=h2",
				"label=CodeReview+1",
				"label=Verified+1",
				"label=Owed",
				"cc=carol@example.com",
				"notify=NONE",
				"custom=1",
			},
		},
		{
			name:    "wip 与 private 互斥",
			opts:    &UploadOptions{Wip: true, Private: true},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := buildUploadGerritOptions(tt.opts, "feature/x")
			if (err != nil) != tt.wantErr {
				t.Fatalf("error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("options = %#v, want %#v", got, tt.want)
			}
		})
	}
}

// uploadTestRepo 创建一个本地 git 仓库（分支 main），返回项目与 helper。
func uploadTestRepo(t *testing.T) (*project.Project, func(args ...string) string) {
	t.Helper()
	dir := t.TempDir()
	runner := git.NewRunner()
	run := func(args ...string) string {
		t.Helper()
		out, err := runner.RunInDir(dir, args...)
		if err != nil {
			t.Fatalf("git %v failed: %v: %s", args, err, out)
		}
		return string(out)
	}
	run("init", "-q")
	run("symbolic-ref", "HEAD", "refs/heads/main")
	run("config", "user.name", "repo-test")
	run("config", "user.email", "repo-test@example.com")
	p := &project.Project{
		Name:       "proj",
		RemoteName: "origin",
		GitRepo:    git.NewRepository(dir, runner),
	}
	return p, run
}

func uploadCommit(t *testing.T, run func(args ...string) string, message string) string {
	t.Helper()
	run("commit", "--allow-empty", "-q", "-m", message)
	return trimUploadTestSHA(run("rev-parse", "HEAD"))
}

func trimUploadTestSHA(value string) string {
	return strings.TrimSpace(value)
}

// TestUploadMapFetchSpec 验证 refspec 映射（含精确匹配与通配符）。
func TestUploadMapFetchSpec(t *testing.T) {
	tests := []struct {
		name string
		spec string
		ref  string
		want string
		ok   bool
	}{
		{"通配符映射", "+refs/heads/*:refs/remotes/origin/*", "refs/heads/main", "refs/remotes/origin/main", true},
		{"精确映射", "refs/heads/main:refs/remotes/custom/main", "refs/heads/main", "refs/remotes/custom/main", true},
		{"不匹配", "+refs/heads/*:refs/remotes/origin/*", "refs/tags/v1", "", false},
		{"非法 spec", "refs/heads/main", "refs/heads/main", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := uploadMapFetchSpec(tt.spec, tt.ref)
			if ok != tt.ok || got != tt.want {
				t.Errorf("uploadMapFetchSpec(%q, %q) = (%q, %v), want (%q, %v)",
					tt.spec, tt.ref, got, ok, tt.want, tt.ok)
			}
		})
	}
}

// TestUploadBranchBaseMapping 验证基线按 branch.<name>.remote 的 fetch refspec 映射：
// branch.main.merge=refs/heads/main 应变为 refs/remotes/origin/main，而不是本地同名分支。
func TestUploadBranchBaseMapping(t *testing.T) {
	p, run := uploadTestRepo(t)
	shaA := uploadCommit(t, run, "A")
	run("update-ref", "refs/remotes/origin/main", shaA)
	run("config", "branch.main.remote", "origin")
	run("config", "branch.main.merge", "refs/heads/main")

	base := uploadResolveBranchBase(p, "main", "origin")
	if base.Remote != "origin" || base.Merge != "refs/heads/main" || base.Ref != "refs/remotes/origin/main" {
		t.Fatalf("base = %+v, want remote=origin merge=refs/heads/main ref=refs/remotes/origin/main", base)
	}
	if dest := uploadBranchBaseDest(base); dest != "main" {
		t.Errorf("uploadBranchBaseDest() = %q, want main", dest)
	}

	// 与基线相同：无变更且无错误
	hasChanges, err := uploadBranchHasChanges(p, "origin", "main")
	if err != nil {
		t.Fatalf("uploadBranchHasChanges() error = %v", err)
	}
	if hasChanges {
		t.Error("uploadBranchHasChanges() = true, want false for unchanged branch")
	}

	// 新增一个提交后应检测到变更，并列出该提交
	shaB := uploadCommit(t, run, "B")
	hasChanges, err = uploadBranchHasChanges(p, "origin", "main")
	if err != nil {
		t.Fatalf("uploadBranchHasChanges() error = %v", err)
	}
	if !hasChanges {
		t.Error("uploadBranchHasChanges() = false, want true after new commit")
	}
	commits := uploadBranchCommits(p, "origin", "main")
	if len(commits) != 1 || commits[0] != shaB {
		t.Errorf("uploadBranchCommits() = %v, want [%s]", commits, shaB)
	}
}

// TestUploadBranchCustomFetchSpec 验证自定义 refspec 也参与映射
// （refs/heads/* -> refs/remotes/upstream/*）。
func TestUploadBranchCustomFetchSpec(t *testing.T) {
	p, run := uploadTestRepo(t)
	shaA := uploadCommit(t, run, "A")
	run("config", "remote.origin.fetch", "+refs/heads/*:refs/remotes/upstream/*")
	run("update-ref", "refs/remotes/upstream/main", shaA)
	run("config", "branch.main.remote", "origin")
	run("config", "branch.main.merge", "refs/heads/main")

	base := uploadResolveBranchBase(p, "main", "origin")
	if base.Ref != "refs/remotes/upstream/main" {
		t.Fatalf("base.Ref = %q, want refs/remotes/upstream/main", base.Ref)
	}
	if dest := uploadBranchBaseDest(base); dest != "main" {
		t.Errorf("uploadBranchBaseDest() = %q, want main", dest)
	}

	hasChanges, err := uploadBranchHasChanges(p, "origin", "main")
	if err != nil {
		t.Fatalf("uploadBranchHasChanges() error = %v", err)
	}
	if hasChanges {
		t.Error("uploadBranchHasChanges() = true, want false")
	}
}

// TestUploadBranchMissingBaseNotSilent 验证配置了基线但本地基线 ref 不存在时
// 返回错误，而不是静默当作“无变更”。
func TestUploadBranchMissingBaseNotSilent(t *testing.T) {
	p, run := uploadTestRepo(t)
	uploadCommit(t, run, "A")
	run("config", "branch.main.remote", "origin")
	run("config", "branch.main.merge", "refs/heads/main")

	hasChanges, err := uploadBranchHasChanges(p, "origin", "main")
	if err == nil {
		t.Fatalf("uploadBranchHasChanges() = %v, nil; want error for missing baseline ref", hasChanges)
	}
	if hasChanges {
		t.Error("uploadBranchHasChanges() = true with error, want false")
	}
}

// TestUploadBranchNamesUnambiguous 验证存在同名 tag 时分支枚举/当前分支仍返回
// 真实短分支名（而不是 %(refname:short) 的 heads/foo），且 -b 对不存在的分支
// 返回空列表而非错误。
func TestUploadBranchNamesUnambiguous(t *testing.T) {
	p, run := uploadTestRepo(t)
	uploadCommit(t, run, "A")
	run("branch", "foo")
	run("tag", "foo")
	for _, b := range []string{"main", "foo"} {
		run("config", "branch."+b+".remote", "origin")
		run("config", "branch."+b+".merge", "refs/heads/main")
	}

	branches, err := uploadBranchesForProject(p, &UploadOptions{})
	if err != nil {
		t.Fatalf("uploadBranchesForProject() failed: %v", err)
	}
	if want := []string{"foo", "main"}; !reflect.DeepEqual(branches, want) {
		t.Errorf("branches = %#v, want %#v", branches, want)
	}

	current, err := uploadCurrentBranchName(p)
	if err != nil {
		t.Fatalf("uploadCurrentBranchName() failed: %v", err)
	}
	if current != "main" {
		t.Errorf("current branch = %q, want main", current)
	}

	selected, err := uploadBranchesForProject(p, &UploadOptions{Branch: "foo"})
	if err != nil || !reflect.DeepEqual(selected, []string{"foo"}) {
		t.Errorf("selected = %#v, err = %v; want [foo]", selected, err)
	}

	missing, err := uploadBranchesForProject(p, &UploadOptions{Branch: "missing"})
	if err != nil {
		t.Fatalf("missing branch should be skipped, got error: %v", err)
	}
	if len(missing) != 0 {
		t.Errorf("missing branch = %#v, want empty", missing)
	}
}

// TestUploadRemoteURLs 验证分支 remote 的 review/push 目标解析：
// 项目自身 remote 用 Project 字段；其他 remote 严格按 manifest 定义解析，
// review 只规范化+拼名（不 urljoin），push 仅对点前缀相对基址 urljoin；
// 非项目 remote 解析不到时返回空串，绝不回退项目级字段。
func TestUploadRemoteURLs(t *testing.T) {
	mf := &manifest.Manifest{
		Remotes: []manifest.Remote{
			{Name: "origin", Fetch: "https://example.com", Review: "https://review.example.com/Gerrit"},
			{Name: "upstream", Alias: "up", Fetch: "../base", PushURL: "ssh://push.example.com/base", Review: "ssh://review.example.com:29418/"},
			{Name: "rel", Fetch: "../base"},
			{Name: "plain", Fetch: "platform"},
			{Name: "reviewonly", Review: "persistent-https://review.example.com/Gerrit"},
		},
	}
	cfg := &config.Config{ManifestURL: "https://manifest.example.com/manifests"}
	p := &project.Project{
		Name:       "platform/build",
		RemoteName: "origin",
		ReviewURL:  "https://review.example.com/platform/build",
		PushURL:    "https://example.com/platform/build",
	}

	tests := []struct {
		name       string
		remote     string
		wantReview string
		wantPush   string
	}{
		{
			name:       "项目 remote 直接用 Project 字段",
			remote:     "origin",
			wantReview: "https://review.example.com/platform/build",
			wantPush:   "https://example.com/platform/build",
		},
		{
			name:       "其他 remote 由 manifest review 规范化并拼接项目名",
			remote:     "upstream",
			wantReview: "ssh://review.example.com:29418/platform/build",
			wantPush:   "ssh://push.example.com/base/platform/build",
		},
		{
			name:       "remote alias 同样生效",
			remote:     "up",
			wantReview: "ssh://review.example.com:29418/platform/build",
			wantPush:   "ssh://push.example.com/base/platform/build",
		},
		{
			name:       "点前缀相对 fetch 按清单 URL 解析后拼项目名",
			remote:     "rel",
			wantReview: "", // 非项目 remote 无 review：不回退项目级字段
			wantPush:   "https://manifest.example.com/base/platform/build",
		},
		{
			name:       "非点前缀相对 fetch 不做 urljoin",
			remote:     "plain",
			wantReview: "",
			wantPush:   "platform/platform/build",
		},
		{
			name:       "review 仅规范化（去 persistent- 与 /Gerrit）且不 urljoin",
			remote:     "reviewonly",
			wantReview: "https://review.example.com/platform/build",
			wantPush:   "",
		},
		{
			name:       "未知 remote 不得回退项目级字段",
			remote:     "missing",
			wantReview: "",
			wantPush:   "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts := &UploadOptions{Manifest: mf, Config: cfg}
			review, push := uploadRemoteURLs(opts, p, tt.remote)
			if review != tt.wantReview {
				t.Errorf("reviewURL = %q, want %q", review, tt.wantReview)
			}
			if push != tt.wantPush {
				t.Errorf("pushURL = %q, want %q", push, tt.wantPush)
			}
		})
	}
}

// TestUploadRemoteURLsProjectRemoteWithoutManifest 验证清单未定义项目自身 remote
// 时仍可使用 manager 解析出的项目字段（仅限项目自身 remote）。
func TestUploadRemoteURLsProjectRemoteWithoutManifest(t *testing.T) {
	p := &project.Project{
		Name:       "proj",
		RemoteName: "origin",
		ReviewURL:  "https://review.example.com/proj",
		PushURL:    "https://example.com/proj",
	}
	review, push := uploadRemoteURLs(&UploadOptions{}, p, "origin")
	if review != p.ReviewURL || push != p.PushURL {
		t.Errorf("uploadRemoteURLs(own remote) = (%q, %q), want (%q, %q)", review, push, p.ReviewURL, p.PushURL)
	}

	// 非项目 remote 在无清单定义时不得使用项目字段。
	review, push = uploadRemoteURLs(&UploadOptions{}, p, "upstream")
	if review != "" || push != "" {
		t.Errorf("uploadRemoteURLs(non-project remote) = (%q, %q), want empty", review, push)
	}
}

// TestUploadReceivePackOption 验证仅 ssh:// review 端点追加 gerrit receive-pack。
func TestUploadReceivePackOption(t *testing.T) {
	tests := []struct {
		name       string
		pushTarget string
		want       []string
	}{
		{name: "ssh 端点", pushTarget: "ssh://review.example.com:29418", want: []string{"--receive-pack=gerrit receive-pack"}},
		{name: "https 端点", pushTarget: "https://review.example.com/gerrit"},
		{name: "remote 名", pushTarget: "origin"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := uploadReceivePackOption(tt.pushTarget)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("uploadReceivePackOption(%q) = %#v, want %#v", tt.pushTarget, got, tt.want)
			}
		})
	}
}

// TestUploadProjectMissingBranchSkipped 验证 -b 指定的分支不存在时项目被跳过，
// 不报错也不统计上传。
func TestUploadProjectMissingBranchSkipped(t *testing.T) {
	p, run := uploadTestRepo(t)
	uploadCommit(t, run, "A")

	log := logger.NewDefaultLogger()
	log.SetLevel(logger.LogLevelError)
	stats := &uploadStats{}
	if err := uploadProject(&UploadOptions{Branch: "missing"}, p, t.TempDir(), "", log, stats); err != nil {
		t.Fatalf("uploadProject() = %v, want nil (missing branch skipped)", err)
	}
	if stats.total != 0 {
		t.Errorf("stats.total = %d, want 0", stats.total)
	}
}

// uploadHookTestRepo 准备一个含变更的项目和 repo 级 pre-upload hook。
func uploadHookTestRepo(t *testing.T, hookBody string) (*project.Project, string) {
	t.Helper()
	p, run := uploadTestRepo(t)
	shaA := uploadCommit(t, run, "A")
	run("update-ref", "refs/remotes/origin/main", shaA)
	run("config", "branch.main.remote", "origin")
	run("config", "branch.main.merge", "refs/heads/main")
	uploadCommit(t, run, "B")
	// hook 的 cmd.Dir 由 Worktree 决定。
	p.Worktree = p.GitRepo.Path
	// dry-run 不会真正 push，但 review 校验仍需通过。
	p.ReviewURL = "ssh://review.example.com:29418"

	repoRoot := t.TempDir()
	hookDir := filepath.Join(repoRoot, ".repo", "hooks")
	if err := os.MkdirAll(hookDir, 0o755); err != nil {
		t.Fatalf("创建 hook 目录failed: %v", err)
	}
	hookPath := filepath.Join(hookDir, "pre-upload")
	if err := os.WriteFile(hookPath, []byte(hookBody), 0o755); err != nil {
		t.Fatalf("写入 hook failed: %v", err)
	}
	return p, repoRoot
}

// TestUploadIgnoreHooks 验证 --ignore-hooks 运行 hook 但失败仅告警并继续，
// --no-verify 完全跳过 hook。
func TestUploadIgnoreHooks(t *testing.T) {
	p, repoRoot := uploadHookTestRepo(t, "#!/bin/sh\nexit 3\n")

	log := logger.NewDefaultLogger()
	log.SetLevel(logger.LogLevelError)

	if err := uploadOneBranch(&UploadOptions{DryRun: true}, p, "main", "origin", repoRoot, "", log); err == nil {
		t.Fatal("hook failure should abort upload without --ignore-hooks/--no-verify")
	}
	if err := uploadOneBranch(&UploadOptions{DryRun: true, IgnoreHooks: true}, p, "main", "origin", repoRoot, "", log); err != nil {
		t.Fatalf("--ignore-hooks should continue after hook failure: %v", err)
	}
	if err := uploadOneBranch(&UploadOptions{DryRun: true, NoVerify: true}, p, "main", "origin", repoRoot, "", log); err != nil {
		t.Fatalf("--no-verify should skip hook: %v", err)
	}
}

// TestUploadDryRunBranchModeDoesNotCheckout 验证 --dry-run 不会切换工作区：
// hook 看到的仍是原分支（若发生 checkout 会看到 -b 指定的分支）。
func TestUploadDryRunBranchModeDoesNotCheckout(t *testing.T) {
	outFile := filepath.Join(t.TempDir(), "head.txt")
	p, repoRoot := uploadHookTestRepo(t, "#!/bin/sh\ngit symbolic-ref --short HEAD > \"$UPLOAD_TEST_HEAD_FILE\"\n")
	t.Setenv("UPLOAD_TEST_HEAD_FILE", outFile)

	// 创建带变更的 review 分支（相对 origin/main 有 1 个提交）。
	run := func(args ...string) string {
		out, err := p.GitRepo.Runner.RunInDir(p.GitRepo.Path, args...)
		if err != nil {
			t.Fatalf("git %v failed: %v: %s", args, err, out)
		}
		return string(out)
	}
	run("branch", "review")
	run("config", "branch.review.remote", "origin")
	run("config", "branch.review.merge", "refs/heads/main")

	log := logger.NewDefaultLogger()
	log.SetLevel(logger.LogLevelError)
	opts := &UploadOptions{Branch: "review", DryRun: true}
	if err := uploadOneBranch(opts, p, "review", "origin", repoRoot, "", log); err != nil {
		t.Fatalf("uploadOneBranch(--dry-run -b) failed: %v", err)
	}

	data, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatalf("读取 hook 输出failed: %v", err)
	}
	if got := strings.TrimSpace(string(data)); got != "main" {
		t.Errorf("hook saw HEAD %q, want main (--dry-run must not checkout review)", got)
	}
}

// TestUploadCheckoutRestoreUsesRealBranch 验证 -b 切换后恢复到真实分支名：
// 原分支与 tag 同名时也必须回到分支（不能 detach 到 tag）。
func TestUploadCheckoutRestoreUsesRealBranch(t *testing.T) {
	p, run := uploadTestRepo(t)
	uploadCommit(t, run, "A")
	run("branch", "review")
	// 原分支 main 与同名 tag 共存。
	run("tag", "main")

	log := logger.NewDefaultLogger()
	log.SetLevel(logger.LogLevelError)
	restore, err := checkoutUploadBranch(p, "review", log)
	if err != nil {
		t.Fatalf("checkoutUploadBranch() failed: %v", err)
	}
	if branch := strings.TrimSpace(run("symbolic-ref", "--short", "HEAD")); branch != "review" {
		t.Fatalf("HEAD = %q, want review", branch)
	}
	restore()

	ref := strings.TrimSpace(run("symbolic-ref", "HEAD"))
	if ref != "refs/heads/main" {
		t.Errorf("restored HEAD = %q, want refs/heads/main (real branch, not tag)", ref)
	}
}

// TestUploadLocalBranchExistsUsesFullRef 验证分支存在性检查使用完整 ref，
// 同名 tag 不会造成误判。
func TestUploadLocalBranchExistsUsesFullRef(t *testing.T) {
	p, run := uploadTestRepo(t)
	uploadCommit(t, run, "A")
	run("branch", "foo")
	run("tag", "foo")

	if !uploadLocalBranchExists(p, "foo") {
		t.Error("uploadLocalBranchExists(foo) = false, want true")
	}
	if uploadLocalBranchExists(p, "refs/heads/missing") {
		t.Error("uploadLocalBranchExists(refs/heads/missing) = true, want false")
	}
}

// TestUploadSelectProjectsWithBranch 验证 -b 的项目过滤：只保留本地确实存在
// 该分支的项目，其余跳过。
func TestUploadSelectProjectsWithBranch(t *testing.T) {
	withBranch, runWith := uploadTestRepo(t)
	uploadCommit(t, runWith, "A")
	runWith("branch", "target")

	withoutBranch, runWithout := uploadTestRepo(t)
	uploadCommit(t, runWithout, "A")

	log := logger.NewDefaultLogger()
	log.SetLevel(logger.LogLevelError)
	got := uploadSelectProjectsWithBranch([]*project.Project{withoutBranch, withBranch}, "target", log)
	if len(got) != 1 || got[0] != withBranch {
		t.Errorf("selected = %#v, want only the project containing target", got)
	}

	if none := uploadSelectProjectsWithBranch([]*project.Project{withoutBranch}, "target", log); len(none) != 0 {
		t.Errorf("selected = %#v, want empty", none)
	}
}

// uploadBareReviewRepo 创建一个裸仓库作为 review push 目标。
func uploadBareReviewRepo(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "review.git")
	if out, err := git.NewRunner().RunInDir(filepath.Dir(dir), "init", "-q", "--bare", dir); err != nil {
		t.Fatalf("创建裸仓库failed: %v: %s", err, out)
	}
	return dir
}

// uploadTestLogger 返回仅输出错误的 logger。
func uploadTestLogger() logger.Logger {
	log := logger.NewDefaultLogger()
	log.SetLevel(logger.LogLevelError)
	return log
}

// TestUploadBuildPushArgs 验证 push 参数恒含 --no-follow-tags、ssh 端点追加
// receive-pack、--quiet 追加 --no-progress。
func TestUploadBuildPushArgs(t *testing.T) {
	refspec := "refs/heads/main:refs/for/main"
	tests := []struct {
		name   string
		opts   *UploadOptions
		target string
		want   []string
	}{
		{
			name:   "ssh review 端点",
			opts:   &UploadOptions{},
			target: "ssh://review.example.com:29418/proj",
			want: []string{
				"push",
				"--receive-pack=gerrit receive-pack",
				"--no-follow-tags",
				"ssh://review.example.com:29418/proj",
				refspec,
			},
		},
		{
			name:   "quiet 追加 no-progress",
			opts:   &UploadOptions{Quiet: true},
			target: "https://review.example.com/proj",
			want: []string{
				"push",
				"--no-progress",
				"--no-follow-tags",
				"https://review.example.com/proj",
				refspec,
			},
		},
		{
			name:   "非 ssh 不追加 receive-pack",
			opts:   &UploadOptions{NoCertChecks: true},
			target: "/tmp/review.git",
			want: []string{
				"-c", "http.sslVerify=false",
				"push",
				"--no-follow-tags",
				"/tmp/review.git",
				refspec,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := uploadBuildPushArgs(tt.opts, tt.target, refspec)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("uploadBuildPushArgs() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

// TestUploadRequiresReviewURL 验证 review URL 缺失时直接报错，
// 绝不把 refs/for 推到普通 push/fetch 目标。
func TestUploadRequiresReviewURL(t *testing.T) {
	p, run := uploadTestRepo(t)
	shaA := uploadCommit(t, run, "A")
	run("update-ref", "refs/remotes/origin/main", shaA)
	run("config", "branch.main.remote", "origin")
	run("config", "branch.main.merge", "refs/heads/main")
	uploadCommit(t, run, "B")

	// push URL 存在但没有 review URL：不得退化为向 push 目标推送。
	reviewRepo := uploadBareReviewRepo(t)
	p.PushURL = reviewRepo
	p.ReviewURL = ""

	err := uploadOneBranch(&UploadOptions{}, p, "main", "origin", t.TempDir(), "", uploadTestLogger())
	if err == nil {
		t.Fatal("uploadOneBranch() = nil, want review url error")
	}
	if !strings.Contains(err.Error(), "remote origin has no review url") {
		t.Errorf("error = %v, want contains %q", err, "remote origin has no review url")
	}
	out, refErr := p.GitRepo.Runner.RunInDir(reviewRepo, "for-each-ref", "refs/for")
	if refErr != nil {
		t.Fatalf("for-each-ref failed: %v: %s", refErr, out)
	}
	if got := strings.TrimSpace(string(out)); got != "" {
		t.Errorf("review repo received refs %q, want none (must not push without review url)", got)
	}
}

// TestUploadPublishedRefSkipsRepeat 验证上传成功后写入 refs/published/<branch>，
// 且 tip 未变化时重复 upload 不再产生变更/枚举结果。
func TestUploadPublishedRefSkipsRepeat(t *testing.T) {
	p, run := uploadTestRepo(t)
	shaA := uploadCommit(t, run, "A")
	run("update-ref", "refs/remotes/origin/main", shaA)
	run("config", "branch.main.remote", "origin")
	run("config", "branch.main.merge", "refs/heads/main")
	shaB := uploadCommit(t, run, "B")

	reviewRepo := uploadBareReviewRepo(t)
	p.ReviewURL = reviewRepo

	log := uploadTestLogger()
	if err := uploadOneBranch(&UploadOptions{}, p, "main", "origin", t.TempDir(), "", log); err != nil {
		t.Fatalf("第一次 uploadOneBranch() failed: %v", err)
	}

	published := strings.TrimSpace(string(run("rev-parse", "--verify", "refs/published/main")))
	if published != shaB {
		t.Errorf("refs/published/main = %q, want %q", published, shaB)
	}
	pushed := strings.TrimSpace(string(uploadMustRunInDir(t, reviewRepo, "rev-parse", "--verify", "refs/for/main")))
	if pushed != shaB {
		t.Errorf("review refs/for/main = %q, want %q", pushed, shaB)
	}

	// 第二次上传：变更判定与分支枚举都应跳过已发布分支。
	hasChanges, err := uploadBranchHasChanges(p, "origin", "main")
	if err != nil {
		t.Fatalf("uploadBranchHasChanges() error = %v", err)
	}
	if hasChanges {
		t.Error("uploadBranchHasChanges() = true, want false after published")
	}
	branches, err := uploadBranchesForProject(p, &UploadOptions{})
	if err != nil {
		t.Fatalf("uploadBranchesForProject() error = %v", err)
	}
	if len(branches) != 0 {
		t.Errorf("branches = %#v, want empty for published tip", branches)
	}
}

// uploadMustRunInDir 在指定目录执行 git 命令并返回输出。
func uploadMustRunInDir(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := git.NewRunner().RunInDir(dir, args...)
	if err != nil {
		t.Fatalf("git %v in %s failed: %v: %s", args, dir, err, out)
	}
	return string(out)
}

// uploadPrepareFeatureBranch 准备 origin/main 基线与带一个提交的 feature 分支，
// 结束时工作区回到 main。
func uploadPrepareFeatureBranch(t *testing.T) (*project.Project, func(args ...string) string, string) {
	t.Helper()
	p, run := uploadTestRepo(t)
	shaA := uploadCommit(t, run, "A")
	run("update-ref", "refs/remotes/origin/main", shaA)
	run("config", "branch.main.remote", "origin")
	run("config", "branch.main.merge", "refs/heads/main")
	run("branch", "feature")
	run("config", "branch.feature.remote", "origin")
	run("config", "branch.feature.merge", "refs/heads/main")
	run("checkout", "-q", "feature")
	shaB := uploadCommit(t, run, "B")
	run("checkout", "-q", "main")
	p.Worktree = p.GitRepo.Path
	return p, run, shaB
}

// TestUploadBranchWithoutHookDoesNotCheckout 验证无 pre-upload hook 时 -b 不切换
// 工作区（push 直接使用 refspec）。
func TestUploadBranchWithoutHookDoesNotCheckout(t *testing.T) {
	p, run, _ := uploadPrepareFeatureBranch(t)
	p.ReviewURL = uploadBareReviewRepo(t)

	if err := uploadOneBranch(&UploadOptions{Branch: "feature"}, p, "feature", "origin", t.TempDir(), "", uploadTestLogger()); err != nil {
		t.Fatalf("uploadOneBranch(-b, no hook) failed: %v", err)
	}
	if branch := strings.TrimSpace(run("symbolic-ref", "--short", "HEAD")); branch != "main" {
		t.Errorf("HEAD = %q, want main (no hook means no checkout)", branch)
	}
}

// TestUploadHookCheckoutSeesBranch 验证存在 hook 时 -b 会 checkout 目标分支供
// hook 使用，并在上传后恢复原分支。
func TestUploadHookCheckoutSeesBranch(t *testing.T) {
	p, run, _ := uploadPrepareFeatureBranch(t)
	p.ReviewURL = uploadBareReviewRepo(t)

	repoRoot := t.TempDir()
	hookDir := filepath.Join(repoRoot, ".repo", "hooks")
	if err := os.MkdirAll(hookDir, 0o755); err != nil {
		t.Fatalf("创建 hook 目录failed: %v", err)
	}
	hookPath := filepath.Join(hookDir, "pre-upload")
	if err := os.WriteFile(hookPath, []byte("#!/bin/sh\ngit symbolic-ref --short HEAD > \"$UPLOAD_TEST_HEAD_FILE\"\n"), 0o755); err != nil {
		t.Fatalf("写入 hook failed: %v", err)
	}
	outFile := filepath.Join(t.TempDir(), "head.txt")
	t.Setenv("UPLOAD_TEST_HEAD_FILE", outFile)

	if err := uploadOneBranch(&UploadOptions{Branch: "feature"}, p, "feature", "origin", repoRoot, "", uploadTestLogger()); err != nil {
		t.Fatalf("uploadOneBranch(-b, hook) failed: %v", err)
	}
	data, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatalf("读取 hook 输出failed: %v", err)
	}
	if got := strings.TrimSpace(string(data)); got != "feature" {
		t.Errorf("hook saw HEAD %q, want feature", got)
	}
	if branch := strings.TrimSpace(run("symbolic-ref", "--short", "HEAD")); branch != "main" {
		t.Errorf("HEAD = %q after upload, want main (must restore)", branch)
	}
}

// TestUploadCheckoutFailureFallsBackToRefPush 验证 checkout 失败时仅告警并继续
// 按 ref 推送（untracked 文件冲突导致 checkout 失败）。
func TestUploadCheckoutFailureFallsBackToRefPush(t *testing.T) {
	p, run, _ := uploadPrepareFeatureBranch(t)
	reviewRepo := uploadBareReviewRepo(t)
	p.ReviewURL = reviewRepo

	// 在 feature 分支上新增文件并提交，然后回 main 放置同名 untracked 文件，
	// 使 checkout feature 因 “untracked working tree files would be overwritten” 失败。
	run("checkout", "-q", "feature")
	conflict := filepath.Join(p.GitRepo.Path, "conflict.txt")
	if err := os.WriteFile(conflict, []byte("feature\n"), 0o644); err != nil {
		t.Fatalf("写入文件failed: %v", err)
	}
	run("add", "conflict.txt")
	run("commit", "-q", "-m", "C")
	shaC := strings.TrimSpace(run("rev-parse", "HEAD"))
	run("checkout", "-q", "main")
	if err := os.WriteFile(conflict, []byte("main\n"), 0o644); err != nil {
		t.Fatalf("写入 untracked 文件failed: %v", err)
	}

	repoRoot := t.TempDir()
	hookDir := filepath.Join(repoRoot, ".repo", "hooks")
	if err := os.MkdirAll(hookDir, 0o755); err != nil {
		t.Fatalf("创建 hook 目录failed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(hookDir, "pre-upload"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("写入 hook failed: %v", err)
	}

	log := uploadTestLogger()
	if err := uploadOneBranch(&UploadOptions{Branch: "feature"}, p, "feature", "origin", repoRoot, "", log); err != nil {
		t.Fatalf("checkout 失败应降级继续按 ref 上传，实际: %v", err)
	}
	pushed := strings.TrimSpace(uploadMustRunInDir(t, reviewRepo, "rev-parse", "--verify", "refs/for/main"))
	if pushed != shaC {
		t.Errorf("review refs/for/main = %q, want %q", pushed, shaC)
	}
	if branch := strings.TrimSpace(run("symbolic-ref", "--short", "HEAD")); branch != "main" {
		t.Errorf("HEAD = %q, want main", branch)
	}
}

// TestUploadForceOverridesPublishedAtTip 验证 --force 在三条分支枚举路径
// （默认枚举 / -b / -c）都覆盖“已发布且 tip 相同”的提前跳过，
// 否则 --ready 无法为已发布的 WIP change 清除 WIP。
func TestUploadForceOverridesPublishedAtTip(t *testing.T) {
	p, run := uploadTestRepo(t)
	shaA := uploadCommit(t, run, "A")
	run("update-ref", "refs/remotes/origin/main", shaA)
	run("config", "branch.main.remote", "origin")
	run("config", "branch.main.merge", "refs/heads/main")
	shaB := uploadCommit(t, run, "B")
	run("update-ref", "refs/published/main", shaB)

	tests := []struct {
		name string
		opts *UploadOptions
		want []string
	}{
		{name: "默认枚举跳过已发布", opts: &UploadOptions{}, want: []string{}},
		{name: "默认枚举 force 覆盖", opts: &UploadOptions{Force: true}, want: []string{"main"}},
		{name: "-b 跳过已发布", opts: &UploadOptions{Branch: "main"}, want: []string{}},
		{name: "-b force 覆盖", opts: &UploadOptions{Branch: "main", Force: true}, want: []string{"main"}},
		{name: "-c 跳过已发布", opts: &UploadOptions{CurrentBranch: true}, want: []string{}},
		{name: "-c force 覆盖", opts: &UploadOptions{CurrentBranch: true, Force: true}, want: []string{"main"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := uploadBranchesForProject(p, tt.opts)
			if err != nil {
				t.Fatalf("uploadBranchesForProject() error = %v", err)
			}
			if len(got) != len(tt.want) || (len(got) > 0 && !reflect.DeepEqual(got, tt.want)) {
				t.Errorf("branches = %#v, want %#v", got, tt.want)
			}
		})
	}
}

// TestUploadConfirmPlan 验证 -y/--yes 行为：多项目/多分支未 -y 时输出计划并
// 拒绝自动上传；单项目单分支保持自动；--dry-run 不需要确认。
func TestUploadConfirmPlan(t *testing.T) {
	projA := &project.Project{Name: "a", Relpath: "a"}
	projB := &project.Project{Name: "b", Relpath: "b"}
	single := []uploadProjectPlan{{project: projA, branches: []string{"main"}}}
	multi := []uploadProjectPlan{
		{project: projA, branches: []string{"main"}},
		{project: projB, branches: []string{"dev"}},
	}

	tests := []struct {
		name       string
		opts       *UploadOptions
		plans      []uploadProjectPlan
		wantErr    bool
		wantPlan   bool
		wantBranch string
	}{
		{name: "单项目单分支自动", opts: &UploadOptions{Destination: "main"}, plans: single},
		{name: "多项目未 -y 拒绝", opts: &UploadOptions{Destination: "main"}, plans: multi, wantErr: true, wantPlan: true, wantBranch: "dev"},
		{name: "多项目 -y 允许", opts: &UploadOptions{Destination: "main", Yes: true}, plans: multi},
		{name: "多项目 dry-run 不拒绝", opts: &UploadOptions{Destination: "main", DryRun: true}, plans: multi},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			err := uploadConfirmPlan(tt.opts, tt.plans, "", &buf)
			if tt.wantErr != (err != nil) {
				t.Fatalf("uploadConfirmPlan() error = %v, wantErr %v", err, tt.wantErr)
			}
			output := buf.String()
			if tt.wantPlan {
				if !strings.Contains(output, "project a/") || !strings.Contains(output, "project b/") {
					t.Errorf("plan output missing projects: %q", output)
				}
				if !strings.Contains(output, "branch "+tt.wantBranch) {
					t.Errorf("plan output missing branch %q: %q", tt.wantBranch, output)
				}
			} else if output != "" {
				t.Errorf("plan output = %q, want empty", output)
			}
		})
	}
}

// TestUploadFindGerritURLs 验证从 git push stderr 提取 Gerrit review URL。
func TestUploadFindGerritURLs(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []string
	}{
		{
			name: "单 URL",
			in:   "remote: https://review.example.com/c/platform/build/+/12345\nTo repo\n",
			want: []string{"https://review.example.com/c/platform/build/+/12345"},
		},
		{
			name: "多 URL 与 http",
			in:   "remote: http://review.example.com/c/a/+/1\nremote: https://review.example.com/c/b/+/2\n",
			want: []string{"http://review.example.com/c/a/+/1", "https://review.example.com/c/b/+/2"},
		},
		{name: "无 URL", in: "To repo\n * [new reference] main -> refs/for/main\n", want: []string{}},
		{name: "非 Gerrit URL", in: "see https://example.com/docs\n", want: []string{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := uploadFindGerritURLs(tt.in)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("uploadFindGerritURLs() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

// TestUploadWriteReviewURLs 验证 review URL 作为 payload 逐行写入。
func TestUploadWriteReviewURLs(t *testing.T) {
	var buf bytes.Buffer
	uploadWriteReviewURLs(&buf, []byte("remote: https://review.example.com/c/a/+/1\nTo repo\nremote: https://review.example.com/c/a/+/2\n"))
	want := "https://review.example.com/c/a/+/1\nhttps://review.example.com/c/a/+/2\n"
	if got := buf.String(); got != want {
		t.Errorf("uploadWriteReviewURLs() = %q, want %q", got, want)
	}
}

// TestUploadDestBranchMismatch 验证 dest-branch 一致性检查的判据。
func TestUploadDestBranchMismatch(t *testing.T) {
	p, run := uploadTestRepo(t)
	uploadCommit(t, run, "A")

	tests := []struct {
		name       string
		merge      string
		revision   string
		destBranch string
		want       bool
	}{
		{name: "项目无 dest-branch 不校验", merge: "refs/heads/topic", revision: "refs/heads/main", destBranch: "", want: false},
		{name: "merge 等于 revision", merge: "refs/heads/main", revision: "refs/heads/main", destBranch: "master", want: false},
		{name: "merge 等于 dest", merge: "refs/heads/master", revision: "refs/heads/main", destBranch: "master", want: false},
		{name: "短名 revision 也能匹配", merge: "refs/heads/main", revision: "main", destBranch: "master", want: false},
		{name: "均不一致", merge: "refs/heads/topic", revision: "refs/heads/main", destBranch: "master", want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			run("config", "branch.main.merge", tt.merge)
			p.Revision = tt.revision
			p.DestBranch = tt.destBranch
			if got := uploadDestBranchMismatch(p, "main"); got != tt.want {
				t.Errorf("uploadDestBranchMismatch() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestUploadDestBranchMismatchSkipsUpload 验证不一致时警告并跳过，不推送 change。
func TestUploadDestBranchMismatchSkipsUpload(t *testing.T) {
	p, run := uploadTestRepo(t)
	shaA := uploadCommit(t, run, "A")
	run("update-ref", "refs/remotes/origin/main", shaA)
	run("update-ref", "refs/remotes/origin/topic", shaA)
	run("config", "branch.main.remote", "origin")
	run("config", "branch.main.merge", "refs/heads/topic")
	uploadCommit(t, run, "B")

	reviewRepo := uploadBareReviewRepo(t)
	p.ReviewURL = reviewRepo
	p.DestBranch = "master"
	p.Revision = "refs/heads/main"

	if err := uploadOneBranch(&UploadOptions{}, p, "main", "origin", t.TempDir(), "", uploadTestLogger()); err != nil {
		t.Fatalf("不一致应跳过而非报错，实际: %v", err)
	}
	out, refErr := p.GitRepo.Runner.RunInDir(reviewRepo, "for-each-ref", "refs/for")
	if refErr != nil {
		t.Fatalf("for-each-ref failed: %v: %s", refErr, out)
	}
	if got := strings.TrimSpace(string(out)); got != "" {
		t.Errorf("review repo received refs %q, want none (mismatch must skip)", got)
	}
	if _, err := uploadProbe(p, "rev-parse", "--verify", "--quiet", "refs/published/main"); err == nil {
		t.Error("refs/published/main should not be written when upload is skipped")
	}
}

// TestUploadDryRunWritesStdout 验证 dry-run 计划写 stdout，且 -q 不吞 payload。
func TestUploadDryRunWritesStdout(t *testing.T) {
	p, run := uploadTestRepo(t)
	shaA := uploadCommit(t, run, "A")
	run("update-ref", "refs/remotes/origin/main", shaA)
	run("config", "branch.main.remote", "origin")
	run("config", "branch.main.merge", "refs/heads/main")
	uploadCommit(t, run, "B")
	p.ReviewURL = uploadBareReviewRepo(t)

	output, err := uploadCaptureStdout(t, func() error {
		return uploadOneBranch(&UploadOptions{DryRun: true, Quiet: true}, p, "main", "origin", t.TempDir(), "", uploadTestLogger())
	})
	if err != nil {
		t.Fatalf("uploadOneBranch(dry-run) failed: %v", err)
	}
	if !strings.Contains(output, "Would upload") || !strings.Contains(output, "refs/for/main") {
		t.Errorf("dry-run stdout = %q, want upload plan", output)
	}
}

// TestUploadSuccessWritesReviewURLToStdout 验证成功 push 后从 git stderr 提取
// Gerrit review URL 写到 stdout（结果 payload）。通过裸仓库 pre-receive hook
// 产生 "remote: <url>" 的真实 stderr。
func TestUploadSuccessWritesReviewURLToStdout(t *testing.T) {
	p, run := uploadTestRepo(t)
	shaA := uploadCommit(t, run, "A")
	run("update-ref", "refs/remotes/origin/main", shaA)
	run("config", "branch.main.remote", "origin")
	run("config", "branch.main.merge", "refs/heads/main")
	uploadCommit(t, run, "B")

	reviewRepo := uploadBareReviewRepo(t)
	const reviewURL = "https://review.example.com/c/proj/+/42"
	hook := "#!/bin/sh\necho \"" + reviewURL + "\"\n"
	if err := os.WriteFile(filepath.Join(reviewRepo, "hooks", "pre-receive"), []byte(hook), 0o755); err != nil {
		t.Fatalf("写入 pre-receive hook failed: %v", err)
	}
	p.ReviewURL = reviewRepo

	output, err := uploadCaptureStdout(t, func() error {
		return uploadOneBranch(&UploadOptions{}, p, "main", "origin", t.TempDir(), "", uploadTestLogger())
	})
	if err != nil {
		t.Fatalf("uploadOneBranch() failed: %v", err)
	}
	if !strings.Contains(output, reviewURL) {
		t.Errorf("stdout = %q, want review URL %q", output, reviewURL)
	}
}

// uploadCaptureStdout 捕获 fn 执行期间写入 os.Stdout 的内容。
func uploadCaptureStdout(t *testing.T, fn func() error) (string, error) {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe failed: %v", err)
	}
	os.Stdout = w
	runErr := fn()
	os.Stdout = old
	if closeErr := w.Close(); closeErr != nil {
		t.Fatalf("关闭 stdout 管道 failed: %v", closeErr)
	}
	data, readErr := io.ReadAll(r)
	if readErr != nil {
		t.Fatalf("读取 stdout 管道 failed: %v", readErr)
	}
	return string(data), runErr
}

// TestUploadNonProjectRemoteWithoutReviewFails 验证分支 remote 不是项目自身
// remote 且无 review 时直接报错，绝不回退项目级 ReviewURL 上传。
func TestUploadNonProjectRemoteWithoutReviewFails(t *testing.T) {
	p, run := uploadTestRepo(t)
	shaA := uploadCommit(t, run, "A")
	run("update-ref", "refs/remotes/origin/main", shaA)
	run("config", "branch.main.remote", "upstream")
	run("config", "branch.main.merge", "refs/heads/main")
	uploadCommit(t, run, "B")

	reviewRepo := uploadBareReviewRepo(t)
	p.ReviewURL = reviewRepo // 项目级字段不得用于非项目 remote
	mf := &manifest.Manifest{
		Remotes: []manifest.Remote{{Name: "upstream", Fetch: reviewRepo}},
	}

	err := uploadOneBranch(&UploadOptions{Manifest: mf}, p, "main", "origin", t.TempDir(), "", uploadTestLogger())
	if err == nil {
		t.Fatal("uploadOneBranch() = nil, want review url error for non-project remote")
	}
	if !strings.Contains(err.Error(), "remote upstream has no review url") {
		t.Errorf("error = %v, want contains %q", err, "remote upstream has no review url")
	}
	out, refErr := p.GitRepo.Runner.RunInDir(reviewRepo, "for-each-ref", "refs/for")
	if refErr != nil {
		t.Fatalf("for-each-ref failed: %v: %s", refErr, out)
	}
	if got := strings.TrimSpace(string(out)); got != "" {
		t.Errorf("review repo received refs %q, want none", got)
	}
}

// TestUploadEffectiveGroups 验证无显式 -g 时使用 cfg.EffectiveGroups()，
// 显式 -g 优先。
func TestUploadEffectiveGroups(t *testing.T) {
	tests := []struct {
		name string
		opts *UploadOptions
		want string
	}{
		{name: "nil opts 回退 default", opts: nil, want: "default"},
		{name: "显式 -g 优先", opts: &UploadOptions{CommonManifestOptions: CommonManifestOptions{Groups: "all"}, Config: &config.Config{Groups: "default"}}, want: "all"},
		{name: "配置有组时沿用", opts: &UploadOptions{Config: &config.Config{Groups: "g1,g2"}}, want: "g1,g2"},
		{name: "配置无组回退 default", opts: &UploadOptions{Config: &config.Config{}}, want: "default"},
		{name: "无配置回退 default", opts: &UploadOptions{}, want: "default"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := uploadEffectiveGroups(tt.opts); got != tt.want {
				t.Errorf("uploadEffectiveGroups() = %q, want %q", got, tt.want)
			}
		})
	}
}
