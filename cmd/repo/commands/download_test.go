package commands

import (
	"strings"
	"testing"

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
