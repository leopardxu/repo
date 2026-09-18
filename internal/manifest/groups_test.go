package manifest

import "testing"

// TestEffectiveGroups 验证项目的有效组集合：显式组去空白去重、追加隐式
// all/name:/path: 组（对齐上游 _ParseProject：{"all","name:<name>","path:<relpath>"}
// 并入显式组；default 不在此处展开——归属判定在过滤求值时动态进行，
// notdefault 才会剥夺 default 归属）。
func TestEffectiveGroups(t *testing.T) {
	tests := []struct {
		name string
		p    Project
		want []string
	}{
		{
			name: "no groups -> implicit only",
			p:    Project{Name: "p", Path: "p"},
			want: []string{"all", "name:p", "path:p"},
		},
		{
			name: "single explicit group",
			p:    Project{Name: "p", Path: "p", Groups: "cix"},
			want: []string{"cix", "all", "name:p", "path:p"},
		},
		{
			name: "multiple groups with whitespace trimmed",
			p:    Project{Name: "p", Path: "p", Groups: "cix, dev ,test"},
			want: []string{"cix", "dev", "test", "all", "name:p", "path:p"},
		},
		{
			name: "path defaults to name when empty",
			p:    Project{Name: "p", Groups: "cix"},
			want: []string{"cix", "all", "name:p", "path:p"},
		},
		{
			name: "name with slash yields name:/path: prefixes",
			p:    Project{Name: "a/b", Path: "a/b", Groups: "x"},
			want: []string{"x", "all", "name:a/b", "path:a/b"},
		},
		{
			name: "groups with only whitespace treated as no groups",
			p:    Project{Name: "p", Path: "p", Groups: " , "},
			want: []string{"all", "name:p", "path:p"},
		},
		{
			name: "default,cix,soc split into three plus implicit",
			p:    Project{Name: "p", Path: "p", Groups: "default,cix,soc"},
			want: []string{"default", "cix", "soc", "all", "name:p", "path:p"},
		},
		{
			name: "explicit duplicates removed",
			p:    Project{Name: "p", Path: "p", Groups: "cix,cix,soc"},
			want: []string{"cix", "soc", "all", "name:p", "path:p"},
		},
		{
			name: "explicit duplicates with whitespace removed",
			p:    Project{Name: "p", Path: "p", Groups: "cix, cix , soc"},
			want: []string{"cix", "soc", "all", "name:p", "path:p"},
		},
		{
			name: "explicit name: collides with implicit and is deduped",
			p:    Project{Name: "p", Path: "p", Groups: "name:p,cix"},
			want: []string{"name:p", "cix", "all", "path:p"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.p.EffectiveGroups()
			if !equalStrings(got, tt.want) {
				t.Errorf("EffectiveGroups() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestMatchesGroupFilter 验证组过滤判定：去空白、default 归属、all、-排除、
// name:/path: 隐式组、空 filter。
func TestMatchesGroupFilter(t *testing.T) {
	// 基准项目：Name=p, Path=p，故隐式组为 name:p / path:p
	base := func(groups string) Project {
		return Project{Name: "p", Path: "p", Groups: groups}
	}

	tests := []struct {
		name   string
		p      Project
		filter []string
		want   bool
	}{
		{"empty filter matches all", base("cix"), nil, true},
		{"empty filter matches all (empty groups)", base(""), nil, true},

		{"exact single group", base("cix"), []string{"cix"}, true},
		{"no match", base("cix"), []string{"dev"}, false},
		{"second group with space after comma", base("cix, dev"), []string{"dev"}, true},
		{"leading/trailing whitespace around group", base(" cix "), []string{"cix"}, true},

		{"no-groups project matches default filter", base(""), []string{"default"}, true},
		{"no-groups project excluded by non-default filter", base(""), []string{"cix"}, false},

		{"all in filter matches any", base("cix"), []string{"all"}, true},
		{"all in filter matches empty-groups project", base(""), []string{"all"}, true},

		{"exclusion wins over inclusion", base("cix,test"), []string{"cix", "-test"}, false},
		{"inclusion after exclusion re-enables (ordered)", base("cix,test"), []string{"-test", "cix"}, true},
		// 上游顺序求值：纯排除 filter 无正向命中时最终状态为 false
		{"exclusion only, project not excluded", base("cix"), []string{"-dev"}, false},
		{"exclusion only, project excluded", base("cix"), []string{"-cix"}, false},
		{"all minus excluded group", base("cix"), []string{"all", "-dev"}, true},
		{"all minus excluded group that hits", base("cix"), []string{"all", "-cix"}, false},

		{"implicit default: explicit groups still belong to default", base("cix"), []string{"default"}, true},
		{"notdefault project excluded by default filter", base("notdefault,cix"), []string{"default"}, false},

		{"implicit name: group matches", base("cix"), []string{"name:p"}, true},
		{"implicit path: group matches", base("cix"), []string{"path:p"}, true},
		{"implicit name: not matching", base("cix"), []string{"name:other"}, false},

		{"filter with whitespace entries trimmed", base("cix"), []string{" cix "}, true},
		{"lone dash entry ignored", base("cix"), []string{"-", "cix"}, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.p.MatchesGroupFilter(tt.filter)
			if got != tt.want {
				t.Errorf("MatchesGroupFilter(filter=%v) = %v, want %v", tt.filter, got, tt.want)
			}
		})
	}
}

// equalStrings 判等辅助：避免引入 reflect/slices 依赖，保持与既有测试一致风格。
func equalStrings(a, b []string) bool {
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
