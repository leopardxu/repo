package project

import (
	"testing"

	"github.com/leopardxu/repo-go/internal/manifest"
)

// TestSplitGroupsByManifest 验证组拆分统一走 manifest.SplitGroups（manager
// 构造不再自带私有实现），空串返回 nil。
func TestSplitGroupsByManifest(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []string
	}{
		{"empty", "", nil},
		{"single", "cix", []string{"cix"}},
		{"multiple with spaces", "cix, dev ,test", []string{"cix", "dev", "test"}},
		{"space separated", "cix dev\ttest", []string{"cix", "dev", "test"}},
		{"leading/trailing spaces", "  cix  ", []string{"cix"}},
		{"only whitespace and commas", " , , ", nil},
		{"trailing comma", "cix,", []string{"cix"}},
		{"duplicate removed", "cix,cix,soc", []string{"cix", "soc"}},
		{"duplicate with spaces removed", "cix, cix , soc", []string{"cix", "soc"}},
		{"semicolon is not a separator (upstream _ParseList)", "cix;soc", []string{"cix;soc"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := manifest.SplitGroups(tt.in)
			if !equalStrSlices(got, tt.want) {
				t.Errorf("SplitGroups(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

// TestIsInGroup 验证单组判定：精确匹配、all 隐式、空白去除、不匹配。
func TestIsInGroup(t *testing.T) {
	p := NewProject("p", "p", "origin", "url", "main", []string{"cix", "dev"}, nil)
	tests := []struct {
		name  string
		group string
		want  bool
	}{
		{"empty matches", "", true},
		{"all matches implicitly", "all", true},
		{"exact match", "cix", true},
		{"whitespace trimmed", "  dev  ", true},
		{"no match", "other", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := p.IsInGroup(tt.group); got != tt.want {
				t.Errorf("IsInGroup(%q) = %v, want %v", tt.group, got, tt.want)
			}
		})
	}
}

// TestIsInAnyGroup 验证组过滤判定对齐上游 MatchesGroups：顺序求值、
// -排除、all/default 隐式、notdefault 抑制 default。
func TestIsInAnyGroup(t *testing.T) {
	p := NewProject("p", "p", "origin", "url", "main", []string{"cix", "dev"}, nil)
	notDefault := NewProject("nd", "nd", "origin", "url", "main", []string{"notdefault", "extra"}, nil)
	tests := []struct {
		name   string
		groups []string
		want   bool
	}{
		{"empty filter matches all", nil, true},
		{"exact match", []string{"cix"}, true},
		{"second group matches", []string{"dev"}, true},
		{"whitespace trimmed in requested", []string{"  cix "}, true},
		{"all matches implicitly", []string{"all"}, true},
		{"all among others", []string{"other", "all"}, true},
		{"default matches implicitly", []string{"default"}, true},
		{"no match", []string{"other"}, false},
		{"empty entries skipped", []string{"", "cix"}, true},
		// 上游顺序求值：命中即置真/置否，最终状态决定结果
		{"exclusion wins over inclusion (ordered)", []string{"cix", "-dev"}, false},
		{"inclusion after exclusion re-enables", []string{"-dev", "cix"}, true},
		{"exclusion of absent group is no-op", []string{"-missing"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := p.IsInAnyGroup(tt.groups); got != tt.want {
				t.Errorf("IsInAnyGroup(%v) = %v, want %v", tt.groups, got, tt.want)
			}
		})
	}

	// notdefault 组的项目不再隐式归属 default
	if notDefault.IsInAnyGroup([]string{"default"}) {
		t.Errorf("notdefault 项目在 [default] 过滤下应被排除")
	}
	if !notDefault.IsInAnyGroup([]string{"extra"}) {
		t.Errorf("notdefault 项目在 [extra] 过滤下应被保留")
	}
}

// TestNewManagerFromManifestTrimsGroups 是本次修复的核心回归测试：
// manifest 中 groups 含逗号后空白且目标组非首项时（如 "default, cix"），
// 过去 manager 端 IsInAnyGroup 因不去空白而丢弃，导致 sync 报 0 项目。
// 修复后 manager 构造即去空白，GetProjectsInAnyGroup 能正确命中。
func TestNewManagerFromManifestTrimsGroups(t *testing.T) {
	m := &manifest.Manifest{
		RepoDir: "/tmp/repo",
		Default: manifest.Default{Remote: "origin", Revision: "main"},
		Remotes: []manifest.Remote{{Name: "origin", Fetch: "https://example.com/"}},
		Projects: []manifest.Project{
			{Name: "proj-a", Path: "a", Groups: "default, cix"}, // cix 非首项、逗号后有空格
			{Name: "proj-b", Path: "b", Groups: "other"},
			{Name: "proj-c", Path: "c", Groups: " cix "}, // 首尾空格
		},
	}

	manager := NewManagerFromManifest(m, nil)

	// 过滤 cix 组：应同时命中 proj-a 与 proj-c，而非 0 个
	got := manager.GetProjectsInAnyGroup([]string{"cix"})
	if len(got) != 2 {
		names := projectNames(got)
		t.Fatalf("GetProjectsInAnyGroup([cix]) = %d projects %v, want 2", len(got), names)
	}

	// 单组 cix 同样应命中
	gotOne := manager.GetProjectsInGroup("cix")
	if len(gotOne) != 2 {
		t.Errorf("GetProjectsInGroup(cix) = %d, want 2", len(gotOne))
	}

	// other 组仅命中 proj-b
	gotOther := manager.GetProjectsInAnyGroup([]string{"other"})
	if len(gotOther) != 1 || gotOther[0].Name != "proj-b" {
		t.Errorf("GetProjectsInAnyGroup([other]) = %v, want [proj-b]", projectNames(gotOther))
	}
}

// projectNames 提取项目名切片，便于断言失败时定位。
func projectNames(ps []*Project) []string {
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		out = append(out, p.Name)
	}
	return out
}

// equalStrSlices 字符串切片判等辅助。
func equalStrSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
