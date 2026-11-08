package commands

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/leopardxu/repo-go/internal/git"
	"github.com/leopardxu/repo-go/internal/logger"
	"github.com/leopardxu/repo-go/internal/manifest"
	"github.com/leopardxu/repo-go/internal/project"
)

// TestParseSSHReviewURL 验证 Gerrit review URL 到 SSH 连接目标的解析。
// 覆盖阶段 0.4 的 host 解析逻辑（ssh://、user@host:port、https 回退、空值）。
func TestParseSSHReviewURL(t *testing.T) {
	tests := []struct {
		name      string
		reviewURL string
		wantHost  string
		wantPort  string
		wantUser  string
		wantNil   bool
	}{
		{
			name:      "ssh scheme with user and port and path",
			reviewURL: "ssh://gerrit-user@gerrit.example.com:29418/gerrit",
			wantHost:  "gerrit.example.com",
			wantPort:  "29418",
			wantUser:  "gerrit-user",
		},
		{
			name:      "ssh scheme host only default port",
			reviewURL: "ssh://gerrit.example.com/",
			wantHost:  "gerrit.example.com",
			wantPort:  "29418", // Gerrit 默认 SSH 端口
			wantUser:  "",
		},
		{
			name:      "scp-like user@host:port",
			reviewURL: "john@gerrit.example.com:2222",
			wantHost:  "gerrit.example.com",
			wantPort:  "2222",
			wantUser:  "john",
		},
		{
			name:      "scp-like user@host:path must not treat path as port",
			reviewURL: "john@gerrit.example.com:gerrit",
			wantHost:  "gerrit.example.com",
			wantPort:  "29418",
			wantUser:  "john",
		},
		{
			name:      "scp-like user@host:/path must not treat path as port",
			reviewURL: "john@gerrit.example.com:/srv/gerrit",
			wantHost:  "gerrit.example.com",
			wantPort:  "29418",
			wantUser:  "john",
		},
		{
			name:      "https scheme returns nil (not usable for ssh)",
			reviewURL: "https://gerrit.example.com/gerrit",
			wantNil:   true,
		},
		{
			name:      "empty returns nil",
			reviewURL: "",
			wantNil:   true,
		},
		{
			name:      "whitespace only returns nil",
			reviewURL: "   ",
			wantNil:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseSSHReviewURL(tt.reviewURL)
			if tt.wantNil {
				if got != nil {
					t.Errorf("parseSSHReviewURL(%q) = %+v, want nil", tt.reviewURL, got)
				}
				return
			}
			if got == nil {
				t.Fatalf("parseSSHReviewURL(%q) = nil, want non-nil", tt.reviewURL)
			}
			if got.host != tt.wantHost {
				t.Errorf("host = %q, want %q", got.host, tt.wantHost)
			}
			if got.port != tt.wantPort {
				t.Errorf("port = %q, want %q", got.port, tt.wantPort)
			}
			if got.user != tt.wantUser {
				t.Errorf("user = %q, want %q", got.user, tt.wantUser)
			}
		})
	}
}

// TestParseChangeNumber 验证从 change 标识解析数字 change number。
// 覆盖阶段 3.2 的 ref 分片回退逻辑。
func TestParseChangeNumber(t *testing.T) {
	tests := []struct {
		name     string
		changeID string
		want     int
	}{
		{"pure number", "12345", 12345},
		{"single digit", "7", 7},
		{"change-id (I+40hex) returns 0", "I1234567890abcdef1234567890abcdef12345678", 0},
		{"with non-digit returns 0", "123abc", 0},
		{"empty returns 0", "", 0},
		{"large number", "999999", 999999},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseChangeNumber(tt.changeID)
			if got != tt.want {
				t.Errorf("parseChangeNumber(%q) = %d, want %d", tt.changeID, got, tt.want)
			}
		})
	}
}

// TestBuildSSHQueryArgs 验证 Gerrit SSH 查询参数构造：
// host 仅用 user@host（不得把 :port 拼回 host，否则 ssh 视为非法主机名导致 exit 255），
// 端口通过 -p 单独传递，gerrit 子命令只出现一次。
func TestBuildSSHQueryArgs(t *testing.T) {
	tests := []struct {
		name     string
		target   *sshTarget
		changeID string
		want     []string
	}{
		{
			name:     "user host with port",
			target:   &sshTarget{host: "gitmirror.cixcomputing.com", port: "29418", user: "git"},
			changeID: "67571",
			want:     []string{"-p", "29418", "git@gitmirror.cixcomputing.com", "gerrit", "query", "--format=JSON", "--current-patch-set", "67571"},
		},
		{
			name:     "host only no port",
			target:   &sshTarget{host: "gerrit.example.com", port: "", user: ""},
			changeID: "42",
			want:     []string{"gerrit.example.com", "gerrit", "query", "--format=JSON", "--current-patch-set", "42"},
		},
		{
			name:     "user host default port",
			target:   &sshTarget{host: "gerrit.example.com", port: "29418", user: "gerrit-user"},
			changeID: "7",
			want:     []string{"-p", "29418", "gerrit-user@gerrit.example.com", "gerrit", "query", "--format=JSON", "--current-patch-set", "7"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildSSHQueryArgs(tt.target, tt.changeID)
			if len(got) != len(tt.want) {
				t.Fatalf("len = %d, want %d (got=%v)", len(got), len(tt.want), got)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("arg[%d] = %q, want %q", i, got[i], tt.want[i])
				}
			}
			// 回归保护：除 -p 外，任何参数不得包含 ':'（防止 :port 拼回 host）
			for _, a := range got {
				if a != "-p" && strings.Contains(a, ":") {
					t.Errorf("arg %q contains ':' (port leaked into host?)", a)
				}
			}
			// gerrit 子命令 token 应只出现一次（防止 fallback 路径 gerrit gerrit query 回归）
			gerritTokens := 0
			for _, a := range got {
				if a == "gerrit" {
					gerritTokens++
				}
			}
			if gerritTokens != 1 {
				t.Errorf("gerrit subcommand token should appear exactly once, got=%v", got)
			}
		})
	}
}

// TestFindCwdProject 验证 cwd 项目探测：精确匹配、祖先目录匹配、无匹配返回 nil。
// 路径两侧均经 filepath.Clean 处理，跨平台一致。
func TestFindCwdProject(t *testing.T) {
	mgr := project.NewManager("", "", "", nil)
	mgr.AddProject(&project.Project{Name: "cix_opensource/linux", Worktree: "/data/code/testrepo/linux"})
	mgr.AddProject(&project.Project{Name: "cix_opensource/other", Worktree: "/data/code/testrepo/other"})

	tests := []struct {
		name string
		cwd  string
		want string // 期望项目 Name；空串表示期望 nil
	}{
		{"exact match", "/data/code/testrepo/linux", "cix_opensource/linux"},
		{"subdir of project", "/data/code/testrepo/linux/drivers", "cix_opensource/linux"},
		{"repo root no match", "/data/code/testrepo", ""},
		{"unrelated path", "/tmp/somewhere", ""},
		{"empty cwd", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := findCwdProject(mgr, tt.cwd)
			if tt.want == "" {
				if got != nil {
					t.Errorf("findCwdProject(%q) = %q, want nil", tt.cwd, got.Name)
				}
				return
			}
			if got == nil {
				t.Fatalf("findCwdProject(%q) = nil, want %q", tt.cwd, tt.want)
			}
			if got.Name != tt.want {
				t.Errorf("name = %q, want %q", got.Name, tt.want)
			}
		})
	}
}

// TestParseChangeArg 验证 change 参数解析：
// 支持 project~branch~change 三元组（project/branch 可含 /）以及 /<patchset> 后缀。
func TestParseChangeArg(t *testing.T) {
	tests := []struct {
		name       string
		arg        string
		wantChange string
		wantPatch  string
		wantProj   string
		wantBranch string
		wantErr    bool
	}{
		{
			name:       "纯 Change-Id",
			arg:        "I0123456789abcdef0123456789abcdef01234567",
			wantChange: "I0123456789abcdef0123456789abcdef01234567",
		},
		{
			name:       "Change-Id 带 patchset",
			arg:        "I0123456789abcdef0123456789abcdef01234567/3",
			wantChange: "I0123456789abcdef0123456789abcdef01234567",
			wantPatch:  "3",
		},
		{
			name:       "数字 change 带 patchset",
			arg:        "67571/2",
			wantChange: "67571",
			wantPatch:  "2",
		},
		{
			name:       "三元组 project/branch 含斜杠",
			arg:        "platform/build~feature/x~I0123456789abcdef0123456789abcdef01234567/4",
			wantChange: "I0123456789abcdef0123456789abcdef01234567",
			wantPatch:  "4",
			wantProj:   "platform/build",
			wantBranch: "feature/x",
		},
		{
			name:       "三元组数字 change",
			arg:        "platform/build~master~67571",
			wantChange: "67571",
			wantProj:   "platform/build",
			wantBranch: "master",
		},
		{
			name:    "非法 change",
			arg:     "not-a-change",
			wantErr: true,
		},
		{
			name:    "非法 patchset",
			arg:     "I0123456789abcdef0123456789abcdef01234567/x",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseChangeArg(tt.arg)
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseChangeArg(%q) error = %v, wantErr %v", tt.arg, err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if got.ChangeID != tt.wantChange {
				t.Errorf("ChangeID = %q, want %q", got.ChangeID, tt.wantChange)
			}
			if got.PatchID != tt.wantPatch {
				t.Errorf("PatchID = %q, want %q", got.PatchID, tt.wantPatch)
			}
			if got.Project != tt.wantProj {
				t.Errorf("Project = %q, want %q", got.Project, tt.wantProj)
			}
			if got.Branch != tt.wantBranch {
				t.Errorf("Branch = %q, want %q", got.Branch, tt.wantBranch)
			}
		})
	}
}

// TestIsChangeArg 验证 change 参数识别（含三元组与 patchset 后缀）。
func TestIsChangeArg(t *testing.T) {
	changeID := "I0123456789abcdef0123456789abcdef01234567"
	tests := []struct {
		arg  string
		want bool
	}{
		{changeID, true},
		{changeID + "/2", true},
		{"67571", true},
		{"67571/9", true},
		{"platform/build~feature/x~" + changeID, true},
		{"platform/build~feature/x~" + changeID + "/5", true},
		{"platform/build", false},
		{"proj/a", false},
		{"--cherry-pick", false},
	}
	for _, tt := range tests {
		t.Run(tt.arg, func(t *testing.T) {
			if got := isChangeArg(tt.arg); got != tt.want {
				t.Errorf("isChangeArg(%q) = %v, want %v", tt.arg, got, tt.want)
			}
		})
	}
}

// TestSelectGerritChange 验证 Gerrit query 多结果处理：
// 能用 project/branch 唯一确定时返回该结果，否则返回歧义错误（不得取第一行）。
func TestSelectGerritChange(t *testing.T) {
	build := ChangeInfo{Project: "platform/build", Branch: "master", Number: 1}
	kernel := ChangeInfo{Project: "platform/kernel", Branch: "master", Number: 2}

	t.Run("单结果", func(t *testing.T) {
		got, err := selectGerritChange([]ChangeInfo{build}, "", "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got.Number != 1 {
			t.Errorf("Number = %d, want 1", got.Number)
		}
	})

	t.Run("project 过滤后唯一", func(t *testing.T) {
		got, err := selectGerritChange([]ChangeInfo{build, kernel}, "platform/kernel", "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got.Number != 2 {
			t.Errorf("Number = %d, want 2", got.Number)
		}
	})

	t.Run("project+branch 过滤后唯一", func(t *testing.T) {
		other := ChangeInfo{Project: "platform/build", Branch: "release", Number: 3}
		got, err := selectGerritChange([]ChangeInfo{build, other}, "platform/build", "release")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got.Number != 3 {
			t.Errorf("Number = %d, want 3", got.Number)
		}
	})

	t.Run("多结果无法唯一确定返回歧义错误", func(t *testing.T) {
		if _, err := selectGerritChange([]ChangeInfo{build, kernel}, "", ""); err == nil {
			t.Fatal("expected ambiguity error, got nil")
		}
	})

	t.Run("过滤后无匹配返回错误", func(t *testing.T) {
		if _, err := selectGerritChange([]ChangeInfo{build, kernel}, "vendor/other", ""); err == nil {
			t.Fatal("expected no-match error, got nil")
		}
	})
}

// TestFindCwdProjectRelativeWorktree 验证相对 worktree 基于 TopDir 解析后再与 cwd 比较。
func TestFindCwdProjectRelativeWorktree(t *testing.T) {
	topDir := t.TempDir()
	mgr := project.NewManager("", "", "", nil)
	mgr.TopDir = topDir
	mgr.AddProject(&project.Project{Name: "component/rel", Relpath: "component/rel", Worktree: "component/rel"})

	got := findCwdProject(mgr, filepath.Join(topDir, "component", "rel", "src"))
	if got == nil || got.Name != "component/rel" {
		t.Fatalf("findCwdProject(relative worktree) = %+v, want component/rel", got)
	}
	if got := findCwdProject(mgr, filepath.Join(topDir, "other")); got != nil {
		t.Errorf("findCwdProject(other) = %+v, want nil", got)
	}
}

// TestParseGerritQueryOutput 验证多行 JSON 解析：
// stats 行被忽略；单结果直接返回；多结果可用 project/branch 消歧，否则报错。
func TestParseGerritQueryOutput(t *testing.T) {
	build := `{"project":"platform/build","branch":"master","id":"platform/build~master~I0123456789abcdef0123456789abcdef01234567","number":101,"currentPatchSet":{"number":3}}`
	kernel := `{"project":"platform/kernel","branch":"master","id":"platform/kernel~master~I0123456789abcdef0123456789abcdef01234567","number":202,"currentPatchSet":{"number":5}}`
	stats := `{"type":"stats","rowCount":1,"runTimeMilliseconds":10}`

	t.Run("单结果忽略 stats", func(t *testing.T) {
		got, err := parseGerritQueryOutput([]byte(build+"\n"+stats+"\n"), "", "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got.Number != 101 || got.CurrentPatchSet.Number != 3 {
			t.Errorf("got number=%d patchset=%d, want 101/3", got.Number, got.CurrentPatchSet.Number)
		}
	})

	t.Run("project 消歧", func(t *testing.T) {
		got, err := parseGerritQueryOutput([]byte(build+"\n"+kernel+"\n"+stats+"\n"), "platform/kernel", "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got.Number != 202 {
			t.Errorf("Number = %d, want 202", got.Number)
		}
	})

	t.Run("多结果无法唯一确定返回错误", func(t *testing.T) {
		if _, err := parseGerritQueryOutput([]byte(build+"\n"+kernel+"\n"+stats+"\n"), "", ""); err == nil {
			t.Fatal("expected ambiguity error, got nil")
		}
	})
}

// downloadTestRepo 创建一个带 main 分支的本地 git 仓库，返回项目与命令 helper。
func downloadTestRepo(t *testing.T) (*project.Project, func(args ...string) string) {
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
	logger.SetGlobalLogger(logger.NewDefaultLogger())
	return p, run
}

func downloadCommit(t *testing.T, run func(args ...string) string, message string) string {
	t.Helper()
	run("commit", "--allow-empty", "-q", "-m", message)
	return strings.TrimSpace(run("rev-parse", "HEAD"))
}

// writeDownloadFetchHead 写入 .git/FETCH_HEAD，模拟已 fetch 的 change 提交。
func writeDownloadFetchHead(t *testing.T, p *project.Project, sha string) {
	t.Helper()
	path := filepath.Join(p.GitRepo.Path, ".git", "FETCH_HEAD")
	content := sha + "\t\tbranch 'refs/changes/test' of example.com\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("写入 FETCH_HEAD failed: %v", err)
	}
}

// TestDownloadFlags 验证 -b 为字符串分支名、-f 为布尔 ff-only。
func TestDownloadFlags(t *testing.T) {
	flags := DownloadCmd().Flags()

	branch := flags.Lookup("branch")
	if branch == nil {
		t.Fatal("missing --branch flag")
	}
	if got := branch.Value.Type(); got != "string" {
		t.Errorf("--branch type = %q, want string", got)
	}
	if branch.Shorthand != "b" {
		t.Errorf("--branch shorthand = %q, want b", branch.Shorthand)
	}

	ffOnly := flags.Lookup("ff-only")
	if ffOnly == nil {
		t.Fatal("missing --ff-only flag")
	}
	if got := ffOnly.Value.Type(); got != "bool" {
		t.Errorf("--ff-only type = %q, want bool", got)
	}
	if ffOnly.Shorthand != "f" {
		t.Errorf("--ff-only shorthand = %q, want f", ffOnly.Shorthand)
	}
}

// TestDownloadFFOnlyKeepsBranch 验证 -f 使用 merge --no-stat --ff-only，
// 前进当前分支且 HEAD 不 detach。
func TestDownloadFFOnlyKeepsBranch(t *testing.T) {
	p, run := downloadTestRepo(t)
	shaA := downloadCommit(t, run, "A")
	shaB := downloadCommit(t, run, "B")
	run("reset", "--hard", shaA)
	writeDownloadFetchHead(t, p, shaB)

	if err := downloadApplyChange(p, &DownloadOptions{FFOnly: true}, "B"); err != nil {
		t.Fatalf("downloadApplyChange(ff-only) failed: %v", err)
	}

	head := strings.TrimSpace(run("rev-parse", "HEAD"))
	if head != shaB {
		t.Errorf("HEAD = %s, want %s (fast-forwarded to FETCH_HEAD)", head, shaB)
	}
	branch := strings.TrimSpace(run("symbolic-ref", "--short", "HEAD"))
	if branch != "main" {
		t.Errorf("HEAD attached to %q, want main (must not detach)", branch)
	}
}

// TestDownloadFFOnlyWithNewBranch 验证 -f 与 -b <branch> 组合：
// 先创建分支，再 fast-forward 合并，HEAD 附着在新分支上。
func TestDownloadFFOnlyWithNewBranch(t *testing.T) {
	p, run := downloadTestRepo(t)
	shaA := downloadCommit(t, run, "A")
	shaB := downloadCommit(t, run, "B")
	run("reset", "--hard", shaA)
	writeDownloadFetchHead(t, p, shaB)

	if err := downloadApplyChange(p, &DownloadOptions{FFOnly: true, Branch: "review"}, "B"); err != nil {
		t.Fatalf("downloadApplyChange(ff-only -b) failed: %v", err)
	}
	if branch := strings.TrimSpace(run("symbolic-ref", "--short", "HEAD")); branch != "review" {
		t.Errorf("HEAD attached to %q, want review", branch)
	}
	if head := strings.TrimSpace(run("rev-parse", "HEAD")); head != shaB {
		t.Errorf("HEAD = %s, want %s", head, shaB)
	}
}

// TestDownloadBranchCheckout 验证默认模式下 -b <branch> 在 FETCH_HEAD 上创建分支。
func TestDownloadBranchCheckout(t *testing.T) {
	p, run := downloadTestRepo(t)
	shaA := downloadCommit(t, run, "A")
	shaB := downloadCommit(t, run, "B")
	run("reset", "--hard", shaA)
	writeDownloadFetchHead(t, p, shaB)

	if err := downloadApplyChange(p, &DownloadOptions{Branch: "review"}, "B"); err != nil {
		t.Fatalf("downloadApplyChange(-b) failed: %v", err)
	}
	if branch := strings.TrimSpace(run("symbolic-ref", "--short", "HEAD")); branch != "review" {
		t.Errorf("HEAD attached to %q, want review", branch)
	}
	if head := strings.TrimSpace(run("rev-parse", "HEAD")); head != shaB {
		t.Errorf("HEAD = %s, want %s", head, shaB)
	}
}

// TestDownloadCherryPickWithBranch 验证 -c 与 -b 组合：先切到新分支再 cherry-pick。
func TestDownloadCherryPickWithBranch(t *testing.T) {
	p, run := downloadTestRepo(t)
	shaA := downloadCommit(t, run, "A")

	changeFile := filepath.Join(p.GitRepo.Path, "change.txt")
	if err := os.WriteFile(changeFile, []byte("change\n"), 0o644); err != nil {
		t.Fatalf("写入文件failed: %v", err)
	}
	run("add", "change.txt")
	run("commit", "-q", "-m", "Change B")
	shaB := strings.TrimSpace(run("rev-parse", "HEAD"))
	run("reset", "--hard", shaA)
	writeDownloadFetchHead(t, p, shaB)

	if err := downloadApplyChange(p, &DownloadOptions{CherryPick: true, Branch: "review"}, "B"); err != nil {
		t.Fatalf("downloadApplyChange(cherry-pick -b) failed: %v", err)
	}
	if branch := strings.TrimSpace(run("symbolic-ref", "--short", "HEAD")); branch != "review" {
		t.Errorf("HEAD attached to %q, want review", branch)
	}
	if subject := strings.TrimSpace(run("log", "-1", "--format=%s")); subject != "Change B" {
		t.Errorf("HEAD subject = %q, want Change B", subject)
	}
	if _, err := os.Stat(changeFile); err != nil {
		t.Errorf("cherry-picked file missing: %v", err)
	}
}

// TestDownloadRevertAlwaysNoEdit 验证 --revert 始终传 --no-edit：
// 即便 --quiet，也不能因为没有 --no-edit 而调用编辑器（GIT_EDITOR=false 会失败）。
func TestDownloadRevertAlwaysNoEdit(t *testing.T) {
	p, run := downloadTestRepo(t)
	file := filepath.Join(p.GitRepo.Path, "f.txt")
	if err := os.WriteFile(file, []byte("a\n"), 0o644); err != nil {
		t.Fatalf("写入文件failed: %v", err)
	}
	run("add", "f.txt")
	run("commit", "-q", "-m", "A")
	if err := os.WriteFile(file, []byte("b\n"), 0o644); err != nil {
		t.Fatalf("写入文件failed: %v", err)
	}
	run("add", "f.txt")
	run("commit", "-q", "-m", "B")
	shaB := strings.TrimSpace(run("rev-parse", "HEAD"))
	writeDownloadFetchHead(t, p, shaB)

	t.Setenv("GIT_EDITOR", "false")
	if err := downloadApplyChange(p, &DownloadOptions{Revert: true, Quiet: true}, "B"); err != nil {
		t.Fatalf("downloadApplyChange(-r -q) failed: %v", err)
	}
	if head := strings.TrimSpace(run("rev-parse", "HEAD")); head == shaB {
		t.Error("HEAD still points at reverted commit, want a revert commit")
	}
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("读取文件failed: %v", err)
	}
	if string(data) != "a\n" {
		t.Errorf("file content = %q, want %q", string(data), "a\n")
	}
}

// TestDownloadNewBranchTrackingConfig 验证 -b 新建分支后写入
// branch.<name>.remote/merge（对齐上游 StartBranch），使后续 upload 可用。
func TestDownloadNewBranchTrackingConfig(t *testing.T) {
	tests := []struct {
		name     string
		revision string
		useSHA   bool
		want     string
	}{
		{name: "短分支名补 refs/heads/", revision: "main", want: "refs/heads/main"},
		{name: "完整 ref 保持", revision: "refs/heads/dev", want: "refs/heads/dev"},
		{name: "commit id 保持", useSHA: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, run := downloadTestRepo(t)
			shaA := downloadCommit(t, run, "A")
			shaB := downloadCommit(t, run, "B")
			run("reset", "--hard", shaA)
			writeDownloadFetchHead(t, p, shaB)

			wantMerge := tt.want
			if tt.useSHA {
				p.Revision = shaA
				wantMerge = shaA
			} else {
				p.Revision = tt.revision
			}

			if err := downloadApplyChange(p, &DownloadOptions{Branch: "review"}, "B"); err != nil {
				t.Fatalf("downloadApplyChange(-b) failed: %v", err)
			}
			if got := strings.TrimSpace(run("config", "--get", "branch.review.remote")); got != "origin" {
				t.Errorf("branch.review.remote = %q, want origin", got)
			}
			if got := strings.TrimSpace(run("config", "--get", "branch.review.merge")); got != wantMerge {
				t.Errorf("branch.review.merge = %q, want %q", got, wantMerge)
			}
		})
	}
}

// TestDownloadRevisionBase 验证清单 revision 映射为本地基线 ref：
// 分支名映射到远端跟踪分支，tags/remotes/SHA 原样返回。
func TestDownloadRevisionBase(t *testing.T) {
	tests := []struct {
		name     string
		revision string
		fetch    string
		want     string
	}{
		{name: "空 revision", revision: "", want: ""},
		{name: "短分支名映射远端跟踪", revision: "main", want: "refs/remotes/origin/main"},
		{name: "refs/heads 映射远端跟踪", revision: "refs/heads/dev", want: "refs/remotes/origin/dev"},
		{name: "自定义 refspec", revision: "main", fetch: "+refs/heads/*:refs/remotes/upstream/*", want: "refs/remotes/upstream/main"},
		{name: "refs/tags 原样", revision: "refs/tags/v1", want: "refs/tags/v1"},
		{name: "refs/remotes 原样", revision: "refs/remotes/up/dev", want: "refs/remotes/up/dev"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, run := downloadTestRepo(t)
			downloadCommit(t, run, "A")
			if tt.fetch != "" {
				run("config", "remote.origin.fetch", tt.fetch)
			}
			p.Revision = tt.revision
			if got := downloadRevisionBase(p); got != tt.want {
				t.Errorf("downloadRevisionBase() = %q, want %q", got, tt.want)
			}
		})
	}

	t.Run("commit id 原样", func(t *testing.T) {
		p, run := downloadTestRepo(t)
		sha := downloadCommit(t, run, "A")
		p.Revision = sha
		if got := downloadRevisionBase(p); got != sha {
			t.Errorf("downloadRevisionBase() = %q, want %q", got, sha)
		}
	})
}

// TestDownloadStartBranchUsesManifestBase 验证 -b 无显式 revision 时以清单
// revision 映射的远端跟踪 ref 为基线，而不是当前 HEAD。
func TestDownloadStartBranchUsesManifestBase(t *testing.T) {
	p, run := downloadTestRepo(t)
	shaA := downloadCommit(t, run, "A")
	run("update-ref", "refs/remotes/origin/main", shaA)
	run("config", "remote.origin.fetch", "+refs/heads/*:refs/remotes/origin/*")
	downloadCommit(t, run, "B") // 当前 HEAD 领先基线

	p.Revision = "main"
	if err := downloadStartBranch(p, "review", ""); err != nil {
		t.Fatalf("downloadStartBranch() failed: %v", err)
	}
	if head := strings.TrimSpace(run("rev-parse", "HEAD")); head != shaA {
		t.Errorf("HEAD = %q, want manifest tracking base %q (not current HEAD)", head, shaA)
	}
	if merge := strings.TrimSpace(run("config", "--get", "branch.review.merge")); merge != "refs/heads/main" {
		t.Errorf("branch.review.merge = %q, want refs/heads/main", merge)
	}
}

// TestDownloadGerritQueryURL 验证 Gerrit 查询地址优先级：
// 项目 PushURL > 项目 ReviewURL > default remote pushurl > default remote review >
// 项目 RemoteURL（仅兜底）。
func TestDownloadGerritQueryURL(t *testing.T) {
	mf := &manifest.Manifest{
		Default: manifest.Default{Remote: "origin"},
		Remotes: []manifest.Remote{
			{Name: "origin", Fetch: "ssh://fetch.example.com/base", PushURL: "ssh://push.example.com/base", Review: "ssh://review.example.com/base"},
		},
	}
	reviewOnlyManifest := &manifest.Manifest{
		Default: manifest.Default{Remote: "origin"},
		Remotes: []manifest.Remote{{Name: "origin", Review: "ssh://review.example.com/base"}},
	}

	tests := []struct {
		name string
		mf   *manifest.Manifest
		proj *project.Project
		want string
	}{
		{
			name: "项目 PushURL 优先",
			mf:   mf,
			proj: &project.Project{PushURL: "ssh://push.example.com/proj", ReviewURL: "ssh://review.example.com/proj", RemoteURL: "https://fetch.example.com/proj"},
			want: "ssh://push.example.com/proj",
		},
		{
			name: "无 PushURL 用项目 ReviewURL",
			mf:   mf,
			proj: &project.Project{ReviewURL: "ssh://review.example.com/proj", RemoteURL: "https://fetch.example.com/proj"},
			want: "ssh://review.example.com/proj",
		},
		{
			name: "仅项目 RemoteURL 时兜底",
			mf:   nil,
			proj: &project.Project{RemoteURL: "https://fetch.example.com/proj"},
			want: "https://fetch.example.com/proj",
		},
		{
			name: "无上下文项目用 default pushurl",
			mf:   mf,
			proj: nil,
			want: "ssh://push.example.com/base",
		},
		{
			name: "default 仅 review",
			mf:   reviewOnlyManifest,
			proj: nil,
			want: "ssh://review.example.com/base",
		},
		{
			name: "无项目无清单",
			mf:   nil,
			proj: nil,
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := downloadGerritQueryURL(tt.mf, tt.proj); got != tt.want {
				t.Errorf("downloadGerritQueryURL() = %q, want %q", got, tt.want)
			}
		})
	}
}
