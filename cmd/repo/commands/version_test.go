package commands

import "testing"

// TestVersionInfoFormat 验证 version 子命令的多行输出：
// 完整注入时输出缩进括注行；占位值（dev/none/unknown/空）省略对应字段。
func TestVersionInfoFormat(t *testing.T) {
	tests := []struct {
		name string
		info VersionInfo
		want string
	}{
		{
			name: "fully injected",
			info: VersionInfo{Version: "v2.1.0", Commit: "8046640", Date: "2026-08-31T00:00:00Z"},
			want: "repo version v2.1.0\n       (commit 8046640, built 2026-08-31T00:00:00Z)\n",
		},
		{
			name: "dev build placeholders omitted",
			info: VersionInfo{Version: "dev", Commit: "none", Date: "unknown"},
			want: "repo version dev\n",
		},
		{
			name: "empty fields omitted",
			info: VersionInfo{Version: "dev"},
			want: "repo version dev\n",
		},
		{
			name: "commit only",
			info: VersionInfo{Version: "v2.1.0", Commit: "abc1234"},
			want: "repo version v2.1.0\n       (commit abc1234)\n",
		},
		{
			name: "date only",
			info: VersionInfo{Version: "v2.1.0", Date: "2026-08-31"},
			want: "repo version v2.1.0\n       (built 2026-08-31)\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.info.Format(); got != tt.want {
				t.Errorf("Format() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestVersionInfoOneLine 验证 --version 旗标的单行输出与占位值省略，
// 并保证与 Format() 的版本行/附注同源一致
func TestVersionInfoOneLine(t *testing.T) {
	tests := []struct {
		name string
		info VersionInfo
		want string
	}{
		{
			name: "fully injected",
			info: VersionInfo{Version: "v2.1.0", Commit: "8046640", Date: "2026-08-31T00:00:00Z"},
			want: "repo version v2.1.0 (commit 8046640, built 2026-08-31T00:00:00Z)",
		},
		{
			name: "commit only",
			info: VersionInfo{Version: "v2.1.0", Commit: "abc1234"},
			want: "repo version v2.1.0 (commit abc1234)",
		},
		{
			name: "dev build placeholders omitted",
			info: VersionInfo{Version: "dev", Commit: "none", Date: "unknown"},
			want: "repo version dev",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.info.OneLine(); got != tt.want {
				t.Errorf("OneLine() = %q, want %q", got, tt.want)
			}
		})
	}
}
