package manifest

import "testing"

// TestGetRemoteReview 验证从 manifest 的 remote 列表取 review URL，
// 包括 name 匹配、alias 匹配、未配置 review、remote 不存在等场景。
// 覆盖阶段 0.4 新增的 GetRemoteReview 方法。
func TestGetRemoteReview(t *testing.T) {
	m := &Manifest{
		Remotes: []Remote{
			{Name: "aosp", Review: "https://android.googlesource.com"},
			{Name: "origin", Alias: "upstream", Review: "ssh://gerrit.example.com:29418/gerrit"},
			{Name: "bare", Fetch: "https://example.com/bare"},
		},
	}

	tests := []struct {
		name       string
		remote     string
		wantReview string
		wantErr    bool
	}{
		{"by name", "aosp", "https://android.googlesource.com", false},
		{"by alias", "upstream", "ssh://gerrit.example.com:29418/gerrit", false},
		{"by name (with alias set)", "origin", "ssh://gerrit.example.com:29418/gerrit", false},
		{"remote without review returns empty", "bare", "", false},
		{"nonexistent remote returns error", "ghost", "", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := m.GetRemoteReview(tt.remote)
			if (err != nil) != tt.wantErr {
				t.Errorf("GetRemoteReview(%q) error = %v, wantErr %v", tt.remote, err, tt.wantErr)
				return
			}
			if got != tt.wantReview {
				t.Errorf("GetRemoteReview(%q) = %q, want %q", tt.remote, got, tt.wantReview)
			}
		})
	}
}

// TestGetRemotePushURL 验证从 manifest remote 取推送地址：pushurl 优先，否则回退 fetch。
// 覆盖 name/alias 匹配、pushurl 缺省回退 fetch、remote 不存在等场景。
func TestGetRemotePushURL(t *testing.T) {
	m := &Manifest{
		Remotes: []Remote{
			{Name: "aosp", Fetch: "https://android.googlesource.com"},
			{Name: "origin", Alias: "upstream", Fetch: "ssh://git@gerrit.example.com/", PushURL: "ssh://git@gerrit.example.com:29418/origin"},
			{Name: "bare", Fetch: "https://example.com/bare"},
		},
	}

	tests := []struct {
		name    string
		remote  string
		wantURL string
		wantErr bool
	}{
		{"by name returns fetch when no pushurl", "aosp", "https://android.googlesource.com", false},
		{"by alias returns pushurl when set", "upstream", "ssh://git@gerrit.example.com:29418/origin", false},
		{"by name (with alias set) returns pushurl", "origin", "ssh://git@gerrit.example.com:29418/origin", false},
		{"remote without pushurl returns fetch", "bare", "https://example.com/bare", false},
		{"nonexistent remote returns error", "ghost", "", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := m.GetRemotePushURL(tt.remote)
			if (err != nil) != tt.wantErr {
				t.Errorf("GetRemotePushURL(%q) error = %v, wantErr %v", tt.remote, err, tt.wantErr)
				return
			}
			if got != tt.wantURL {
				t.Errorf("GetRemotePushURL(%q) = %q, want %q", tt.remote, got, tt.wantURL)
			}
		})
	}
}
