package commands

import (
	"reflect"
	"strings"
	"testing"

	"github.com/leopardxu/repo-go/internal/config"
	"github.com/leopardxu/repo-go/internal/git"
	"github.com/leopardxu/repo-go/internal/project"
)

// TestInfoDiffArgs 验证 -d 使用三点比较 HEAD...<远端引用>。
func TestInfoDiffArgs(t *testing.T) {
	tests := []struct {
		name   string
		target string
		want   []string
	}{
		{
			name:   "远端跟踪引用",
			target: "refs/remotes/origin/main",
			want:   []string{"log", "--oneline", "--left-right", "HEAD...refs/remotes/origin/main", "--"},
		},
		{
			name:   "显式 target",
			target: "origin/main",
			want:   []string{"log", "--oneline", "--left-right", "HEAD...origin/main", "--"},
		},
		{
			name:   "空 target 表示无法比较",
			target: "",
			want:   nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := infoDiffArgs(tt.target)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("infoDiffArgs() = %v, want %v", got, tt.want)
			}
			if len(got) > 0 && !strings.Contains(strings.Join(got, " "), "HEAD...") {
				t.Errorf("infoDiffArgs() 未使用三点比较: %v", got)
			}
		})
	}
}

// TestInfoDiffTargetRef 验证 refs/remotes/<remote>/<branch> 的构造。
func TestInfoDiffTargetRef(t *testing.T) {
	tests := []struct {
		name       string
		remoteName string
		branch     string
		want       string
	}{
		{
			name:       "常规 remote 与分支",
			remoteName: "origin",
			branch:     "main",
			want:       "refs/remotes/origin/main",
		},
		{
			name:       "remote 为空回退 origin",
			remoteName: "",
			branch:     "main",
			want:       "refs/remotes/origin/main",
		},
		{
			name:       "自定义 remote",
			remoteName: "aosp",
			branch:     "android-14",
			want:       "refs/remotes/aosp/android-14",
		},
		{
			name:       "分支为空",
			remoteName: "origin",
			branch:     "",
			want:       "",
		},
		{
			name:       "refs/heads 前缀去掉后拼远端",
			remoteName: "origin",
			branch:     "refs/heads/dev",
			want:       "refs/remotes/origin/dev",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := infoDiffTargetRef(tt.remoteName, tt.branch); got != tt.want {
				t.Errorf("infoDiffTargetRef() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestInfoDiffTargetCandidates 验证比较目标顺序：SHA/tag 直接使用；分支名
// 不直接用本地分支，先远端 <remote>/<revision>，最后回退清单分支远端引用。
func TestInfoDiffTargetCandidates(t *testing.T) {
	tests := []struct {
		name           string
		remoteName     string
		manifestBranch string
		revision       string
		want           []string
	}{
		{
			name:           "分支名先远端 revision 再本地回退与清单分支",
			remoteName:     "origin",
			manifestBranch: "main",
			revision:       "dev",
			want:           []string{"refs/remotes/origin/dev", "refs/heads/dev", "refs/remotes/origin/main"},
		},
		{
			name:           "revision 与清单分支同名去重",
			remoteName:     "origin",
			manifestBranch: "main",
			revision:       "main",
			want:           []string{"refs/remotes/origin/main", "refs/heads/main"},
		},
		{
			name:           "SHA 直接使用，不再映射远端 revision",
			remoteName:     "origin",
			manifestBranch: "main",
			revision:       "0123456789abcdef0123456789abcdef01234567",
			want:           []string{"0123456789abcdef0123456789abcdef01234567", "refs/remotes/origin/main"},
		},
		{
			name:           "refs/tags 直接使用",
			remoteName:     "origin",
			manifestBranch: "main",
			revision:       "refs/tags/v1.0",
			want:           []string{"refs/tags/v1.0", "refs/remotes/origin/main"},
		},
		{
			name:           "refs/heads 先远端再本地",
			remoteName:     "origin",
			manifestBranch: "main",
			revision:       "refs/heads/dev",
			want:           []string{"refs/remotes/origin/dev", "refs/heads/dev", "refs/remotes/origin/main"},
		},
		{
			name:           "清单分支为空只保留 revision 候选",
			remoteName:     "origin",
			manifestBranch: "",
			revision:       "dev",
			want:           []string{"refs/remotes/origin/dev", "refs/heads/dev"},
		},
		{
			name:           "裸 tag 名按分支处理",
			remoteName:     "origin",
			manifestBranch: "main",
			revision:       "v1.0",
			want:           []string{"refs/remotes/origin/v1.0", "refs/heads/v1.0", "refs/remotes/origin/main"},
		},
		{
			name:           "均为空无候选",
			remoteName:     "origin",
			manifestBranch: "",
			revision:       "",
			want:           []string{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := infoDiffTargetCandidates(tt.remoteName, tt.manifestBranch, tt.revision)
			if len(got) != len(tt.want) || (len(got) > 0 && !reflect.DeepEqual(got, tt.want)) {
				t.Errorf("infoDiffTargetCandidates() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestInfoResolveDiffTarget 验证目标解析顺序：
// 本地 revision 优先，其次远端 <remote>/<revision>，最后清单分支。
func TestInfoResolveDiffTarget(t *testing.T) {
	repoDir := t.TempDir()
	sha := infoTestInitRepo(t, repoDir)
	runner := git.NewRunner()
	if out, err := runner.RunInDir(repoDir, "update-ref", "refs/remotes/origin/main", sha); err != nil {
		t.Fatalf("update-ref failed: %v: %s", err, out)
	}
	if out, err := runner.RunInDir(repoDir, "update-ref", "refs/remotes/origin/dev", sha); err != nil {
		t.Fatalf("update-ref failed: %v: %s", err, out)
	}
	proj := &project.Project{
		Name:       "foo",
		RemoteName: "origin",
		GitRepo:    git.NewRepository(repoDir, runner),
	}
	cfg := &config.Config{ManifestBranch: "main"}

	// 无项目 revision：回退清单分支的远端跟踪引用
	target, err := infoResolveDiffTarget(proj, cfg)
	if err != nil {
		t.Fatalf("infoResolveDiffTarget 失败: %v", err)
	}
	if want := "refs/remotes/origin/main"; target != want {
		t.Errorf("target = %q, want %q", target, want)
	}

	// 本地 revision 不存在时回退 refs/remotes/<remote>/<revision>
	proj.Revision = "dev"
	cfg.ManifestBranch = "does-not-exist"
	target, err = infoResolveDiffTarget(proj, cfg)
	if err != nil {
		t.Fatalf("回退远端 revision 失败: %v", err)
	}
	if want := "refs/remotes/origin/dev"; target != want {
		t.Errorf("回退 target = %q, want %q", target, want)
	}

	// 本地 revision 可解析时优先返回 revision 本身（SHA/tag/本地分支）
	proj.Revision = sha
	cfg.ManifestBranch = "main"
	target, err = infoResolveDiffTarget(proj, cfg)
	if err != nil {
		t.Fatalf("本地 revision 解析失败: %v", err)
	}
	if want := sha; target != want {
		t.Errorf("本地 revision target = %q, want %q", target, want)
	}

	// 裸 revision 只按分支名处理：存在同名 tag 也不能解析成 tag；远端 revision
	// 与本地回退都不存在时回退清单分支的远端引用。
	if out, err := runner.RunInDir(repoDir, "tag", "v-tag", sha); err != nil {
		t.Fatalf("git tag failed: %v: %s", err, out)
	}
	proj.Revision = "v-tag"
	cfg.ManifestBranch = "main"
	target, err = infoResolveDiffTarget(proj, cfg)
	if err != nil {
		t.Fatalf("裸 tag 场景解析失败: %v", err)
	}
	if want := "refs/remotes/origin/main"; target != want {
		t.Errorf("裸 tag 名不应按 tag 解析 target = %q, want %q", target, want)
	}

	// tags 仅支持显式 refs/tags/ 前缀。
	proj.Revision = "refs/tags/v-tag"
	target, err = infoResolveDiffTarget(proj, cfg)
	if err != nil {
		t.Fatalf("显式 tag 解析失败: %v", err)
	}
	if want := "refs/tags/v-tag"; target != want {
		t.Errorf("显式 tag target = %q, want %q", target, want)
	}

	// 所有候选都不存在时报错，由调用方标注无法比较远端
	proj.Revision = "missing"
	cfg.ManifestBranch = "does-not-exist"
	if _, err := infoResolveDiffTarget(proj, cfg); err == nil {
		t.Error("期望无法解析比较目标时报错")
	}
}

// TestInfoDiffRemoteCommitsWhenLocalBehind 验证本地落后远端时 -d 的
// Remote Commits 正确：比较目标必须解析为远端跟踪引用而非本地分支。
func TestInfoDiffRemoteCommitsWhenLocalBehind(t *testing.T) {
	repoDir := t.TempDir()
	localID := infoTestInitRepo(t, repoDir)
	runner := git.NewRunner()

	// 在本地创建第二个提交作为远端领先的提交，然后回退 HEAD 到第一个提交。
	if out, err := runner.RunInDir(repoDir,
		"-c", "user.name=repo-test",
		"-c", "user.email=repo-test@example.com",
		"commit", "--allow-empty", "-q", "-m", "remote-ahead",
	); err != nil {
		t.Fatalf("git commit failed: %v: %s", err, out)
	}
	remoteOut, err := runner.RunInDir(repoDir, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("git rev-parse remote failed: %v: %s", err, remoteOut)
	}
	remoteID := strings.TrimSpace(string(remoteOut))
	if out, err := runner.RunInDir(repoDir, "update-ref", "refs/remotes/origin/main", remoteID); err != nil {
		t.Fatalf("update-ref failed: %v: %s", err, out)
	}
	if out, err := runner.RunInDir(repoDir, "reset", "--hard", localID); err != nil {
		t.Fatalf("reset failed: %v: %s", err, out)
	}

	proj := &project.Project{
		Name:       "foo",
		RemoteName: "origin",
		Revision:   "main",
		GitRepo:    git.NewRepository(repoDir, runner),
	}
	cfg := &config.Config{ManifestBranch: "mobile"}
	target, err := infoResolveDiffTarget(proj, cfg)
	if err != nil {
		t.Fatalf("infoResolveDiffTarget failed: %v", err)
	}
	if want := "refs/remotes/origin/main"; target != want {
		t.Fatalf("target = %q, want %q", target, want)
	}

	out, err := proj.GitRepo.RunCommand(infoDiffArgs(target)...)
	if err != nil {
		t.Fatalf("git log failed: %v: %s", err, out)
	}
	local, remote := infoSplitDiffCommits(string(out))
	if len(local) != 0 {
		t.Errorf("本地不应有独有提交, got %v", local)
	}
	if len(remote) != 1 || !strings.Contains(remote[0], "remote-ahead") {
		t.Errorf("Remote Commits 应包含远端领先提交, got %v", remote)
	}
}

func infoTestInitRepo(t *testing.T, dir string) string {
	t.Helper()
	runner := git.NewRunner()
	if out, err := runner.RunInDir(dir, "init", "-q"); err != nil {
		t.Fatalf("git init %s failed: %v: %s", dir, err, out)
	}
	if out, err := runner.RunInDir(dir,
		"-c", "user.name=repo-test",
		"-c", "user.email=repo-test@example.com",
		"commit", "--allow-empty", "-q", "-m", "init",
	); err != nil {
		t.Fatalf("git commit %s failed: %v: %s", dir, err, out)
	}
	out, err := runner.RunInDir(dir, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("git rev-parse %s failed: %v: %s", dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

// TestInfoDiffUnavailable 验证无法比较远端时的输出标注。
func TestInfoDiffUnavailable(t *testing.T) {
	got := infoDiffUnavailable("refs/remotes/origin/main")
	if !strings.Contains(got, "无法比较远端") {
		t.Errorf("输出缺少“无法比较远端”: %q", got)
	}
	if !strings.Contains(got, "refs/remotes/origin/main") {
		t.Errorf("输出缺少原因: %q", got)
	}
	if strings.Contains(got, "Local Commits") {
		t.Errorf("不得伪造本地提交: %q", got)
	}
}

// TestInfoSplitDiffCommits 验证 --left-right 输出按 < / > 拆成 local/remote。
func TestInfoSplitDiffCommits(t *testing.T) {
	out := "<aaaa111 local one\n>bbbb222 remote one\n<cccc333 local two\n"
	local, remote := infoSplitDiffCommits(out)
	if want := []string{"aaaa111 local one", "cccc333 local two"}; !reflect.DeepEqual(local, want) {
		t.Errorf("local = %v, want %v", local, want)
	}
	if want := []string{"bbbb222 remote one"}; !reflect.DeepEqual(remote, want) {
		t.Errorf("remote = %v, want %v", remote, want)
	}
}

// TestInfoFormatDiff 验证 Local/Remote 两段输出。
func TestInfoFormatDiff(t *testing.T) {
	got := infoFormatDiff([]string{"a local"}, []string{"b remote"})
	for _, want := range []string{
		"Local Commits: 1", "a local",
		"Remote Commits: 1", "b remote",
		"----------------------------",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("输出缺少 %q:\n%s", want, got)
		}
	}
}
