package manifest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// chdirTemp 切换到临时目录并在测试结束后恢复，供依赖 cwd 的 .repo 解析使用。
func chdirTemp(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	prev, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Chdir(prev)
	})
	return dir
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(strings.TrimSpace(content)+"\n"), 0644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// TestLocalManifestsMergedOnce 回归 C1：标准 .repo/local_manifests/local.xml
// 只应被合并一次，Parse 不得因递归扫描 local_manifests 无限递归。
func TestLocalManifestsMergedOnce(t *testing.T) {
	dir := chdirTemp(t)

	writeFile(t, filepath.Join(dir, ".repo", "manifests", "default.xml"), `<?xml version="1.0" encoding="UTF-8"?>
<manifest>
  <remote name="origin" fetch="https://example.com/" />
  <default remote="origin" revision="main" />
  <project name="base" path="base" />
</manifest>`)

	writeFile(t, filepath.Join(dir, ".repo", "local_manifests", "local.xml"), `<?xml version="1.0" encoding="UTF-8"?>
<manifest>
  <project name="local" path="local" />
</manifest>`)

	parser := NewParser()
	m, err := parser.ParseFromFile("default.xml", nil)
	if err != nil {
		t.Fatalf("ParseFromFile: %v", err)
	}
	if len(m.Projects) != 2 {
		t.Fatalf("Projects = %v, want [base local]（local manifest 只合并一次）", projectNames(m))
	}
	counts := map[string]int{}
	for _, p := range m.Projects {
		counts[p.Name]++
	}
	if counts["local"] != 1 {
		t.Errorf("project local merged %d times, want 1", counts["local"])
	}
}

// TestIncludeRemoveAndExtendPropagation 表驱动验证 include 内的
// remove-project / extend-project / default / 嵌套项目语义。
func TestIncludeRemoveAndExtendPropagation(t *testing.T) {
	tests := []struct {
		name       string
		mainXML    string
		includeXML string
		check      func(t *testing.T, m *Manifest)
	}{
		{
			name: "include remove targets outer project",
			mainXML: `<manifest>
  <remote name="origin" fetch="https://example.com/" />
  <default remote="origin" revision="main" />
  <project name="a" path="a" />
  <project name="b" path="b" />
  <include name="inc.xml" />
</manifest>`,
			includeXML: `<manifest>
  <remove-project name="a" />
</manifest>`,
			check: func(t *testing.T, m *Manifest) {
				if len(m.Projects) != 1 || m.Projects[0].Name != "b" {
					t.Fatalf("Projects = %v, want [b]", projectNames(m))
				}
			},
		},
		{
			name: "include extend propagates to outer project",
			mainXML: `<manifest>
  <remote name="origin" fetch="https://example.com/" />
  <default remote="origin" revision="main" />
  <project name="a" path="a" />
  <include name="inc.xml" />
</manifest>`,
			includeXML: `<manifest>
  <extend-project name="a" revision="refs/tags/v2" />
</manifest>`,
			check: func(t *testing.T, m *Manifest) {
				if len(m.Projects) != 1 {
					t.Fatalf("Projects = %v, want [a]", projectNames(m))
				}
				if m.Projects[0].Revision != "refs/tags/v2" {
					t.Errorf("Revision = %q, want refs/tags/v2（include 内 extend 应传播到外层）", m.Projects[0].Revision)
				}
			},
		},
		{
			name: "include project inherits outer default",
			mainXML: `<manifest>
  <remote name="origin" fetch="https://example.com/" />
  <default remote="origin" revision="main" />
  <include name="inc.xml" />
</manifest>`,
			includeXML: `<manifest>
  <project name="a" path="a" />
</manifest>`,
			check: func(t *testing.T, m *Manifest) {
				if len(m.Projects) != 1 {
					t.Fatalf("Projects = %v, want [a]", projectNames(m))
				}
				if m.Projects[0].Remote != "origin" || m.Projects[0].Revision != "main" {
					t.Errorf("project = %+v, want default remote/revision inheritance", m.Projects[0])
				}
			},
		},
		{
			name: "nested project in include keeps parent-prefixed path",
			mainXML: `<manifest>
  <remote name="origin" fetch="https://example.com/" />
  <default remote="origin" revision="main" />
  <include name="inc.xml" />
</manifest>`,
			includeXML: `<manifest>
  <project name="parent" path="src/parent">
    <project name="child" path="c">
      <project name="grand" />
    </project>
  </project>
</manifest>`,
			check: func(t *testing.T, m *Manifest) {
				if len(m.Projects) != 3 {
					t.Fatalf("Projects = %v, want [parent parent/child parent/child/grand]", projectNames(m))
				}
				// 上游实测 semantics（GetSubprojectPaths）：显式 path 相对父路径，
				// 缺省 path = 父路径 + 拼接后的 name（会重复父段）。
				want := []struct{ name, path string }{
					{"parent", "src/parent"},
					{"parent/child", "src/parent/c"},
					{"parent/child/grand", "src/parent/c/parent/child/grand"},
				}
				for i, w := range want {
					if m.Projects[i].Name != w.name || m.Projects[i].Path != w.path {
						t.Errorf("Projects[%d] = {name:%q path:%q}, want {name:%q path:%q}",
							i, m.Projects[i].Name, m.Projects[i].Path, w.name, w.path)
					}
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := chdirTemp(t)
			writeFile(t, filepath.Join(dir, ".repo", "manifests", "default.xml"), tt.mainXML)
			writeFile(t, filepath.Join(dir, ".repo", "manifests", "inc.xml"), tt.includeXML)

			parser := NewParser()
			m, err := parser.ParseFromFile("default.xml", nil)
			if err != nil {
				t.Fatalf("ParseFromFile: %v", err)
			}
			tt.check(t, m)
		})
	}
}

// TestParseRemoveProjectImplicitNamePath 验证项目 Path 为空时按 Name 视为
// 隐式 path 的 remove-project 匹配语义。
func TestParseRemoveProjectImplicitNamePath(t *testing.T) {
	parser := NewParser()
	m, err := parser.Parse([]byte(`<manifest>
  <remote name="origin" fetch="https://example.com/" />
  <default remote="origin" revision="main" />
  <project name="a" />
  <project name="a" path="a2" />
  <remove-project name="a" path="a" />
</manifest>`), nil)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(m.Projects) != 1 || m.Projects[0].Path != "a2" {
		t.Fatalf("Projects = %+v, want 仅 path=a2 保留", m.Projects)
	}
}

// TestMergeLocalManifestRemoveSemantics 验证 local manifest 合并复用解析期
// remove-project 匹配语义：path 显式匹配、optional 未命中忽略、非 optional
// 未命中报错。
func TestMergeLocalManifestRemoveSemantics(t *testing.T) {
	tests := []struct {
		name      string
		localXML  string
		wantErr   bool
		wantNames []string
	}{
		{
			name: "path match removes only target",
			localXML: `<manifest>
  <remove-project name="a" path="a2" />
</manifest>`,
			wantNames: []string{"a"},
		},
		{
			name: "optional missing ignored",
			localXML: `<manifest>
  <remove-project name="missing" optional="true" />
</manifest>`,
			wantNames: []string{"a", "a2"},
		},
		{
			name: "non optional missing errors",
			localXML: `<manifest>
  <remove-project name="missing" />
</manifest>`,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			parser := NewParser()
			main, err := parser.Parse([]byte(`<manifest>
  <remote name="origin" fetch="https://example.com/" />
  <default remote="origin" revision="main" />
  <project name="a" path="a" />
  <project name="a" path="a2" />
</manifest>`), nil)
			if err != nil {
				t.Fatalf("Parse main: %v", err)
			}
			parser.includeDepth++
			local, err := parser.Parse([]byte(tt.localXML), nil)
			parser.includeDepth--
			if err != nil {
				t.Fatalf("Parse local: %v", err)
			}
			err = parser.mergeLocalManifest(main, local)
			if tt.wantErr {
				if err == nil {
					t.Fatal("mergeLocalManifest expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("mergeLocalManifest: %v", err)
			}
			if len(main.Projects) != len(tt.wantNames) {
				t.Fatalf("Projects = %v, want %v", projectNames(main), tt.wantNames)
			}
			for i, path := range tt.wantNames {
				if main.Projects[i].Path != path {
					t.Errorf("Projects[%d].Path = %q, want %q", i, main.Projects[i].Path, path)
				}
			}
		})
	}
}

// TestOptBoolLenientParsing 验证 bool 属性接受 true/false/yes/no/1/0，
// 非法值忽略且不使整个清单解析失败。
func TestOptBoolLenientParsing(t *testing.T) {
	xml := `<manifest>
  <remote name="origin" fetch="https://example.com/" />
  <default remote="origin" revision="main" sync-c="yes" sync-s="1" />
  <project name="a" path="a" />
  <project name="b" path="b" sync-c="no" sync-s="0" force-path="yes" />
  <project name="c" path="c" sync-c="maybe" force-path="bogus" />
</manifest>`

	parser := NewParser()
	m, err := parser.Parse([]byte(xml), nil)
	if err != nil {
		t.Fatalf("Parse with lenient bool attrs failed: %v", err)
	}

	if !m.Default.SyncC.Get(false) || !m.Default.SyncS.Get(false) {
		t.Errorf("default sync-c/sync-s = %v/%v, want true/true",
			m.Default.SyncC.Get(false), m.Default.SyncS.Get(false))
	}
	// a 继承 default 的 true
	if !m.Projects[0].SyncC.Get(false) || !m.Projects[0].SyncS.Get(false) {
		t.Errorf("project a sync-c/sync-s = %v/%v, want true/true（继承 default）",
			m.Projects[0].SyncC.Get(false), m.Projects[0].SyncS.Get(false))
	}
	// b 显式 false 覆盖 default；force-path yes 为 true
	if m.Projects[1].SyncC.Get(true) || m.Projects[1].SyncS.Get(true) {
		t.Errorf("project b sync-c/sync-s should be false")
	}
	if !m.Projects[1].ForcePath.Get(false) {
		t.Errorf("project b force-path should be true")
	}
	// c 的非法 force-path 值按未声明处理（force-path 不继承 default）
	if m.Projects[2].ForcePath.Set {
		t.Errorf("project c force-path invalid value should be unset, got %+v", m.Projects[2].ForcePath)
	}
	// c 的非法 sync-c 值按未声明处理，随后继承 default 的 true
	if !m.Projects[2].SyncC.Get(false) {
		t.Errorf("project c invalid sync-c should fall back to default true")
	}

	// 无 default 值时可明确观察非法枚举值不被置位
	parser2 := NewParser()
	m2, err := parser2.Parse([]byte(`<manifest>
  <remote name="origin" fetch="https://example.com/" />
  <default remote="origin" revision="main" />
  <project name="x" path="x" sync-c="maybe" />
</manifest>`), nil)
	if err != nil {
		t.Fatalf("Parse invalid enum values should not fail: %v", err)
	}
	if m2.Projects[0].SyncC.Set {
		t.Errorf("invalid sync-c should leave OptBool unset, got %+v", m2.Projects[0].SyncC)
	}

	// optional="0" 解析为显式 false：include 上下文中悬空保留
	parser3 := NewParser()
	parser3.includeDepth++
	m3, err := parser3.Parse([]byte(`<manifest>
  <remove-project name="missing" optional="0" />
</manifest>`), nil)
	parser3.includeDepth--
	if err != nil {
		t.Fatalf("Parse optional=0: %v", err)
	}
	if len(m3.RemoveProjects) != 1 {
		t.Fatalf("RemoveProjects = %+v, want dangling entry", m3.RemoveProjects)
	}
	if m3.RemoveProjects[0].Optional.Get(true) {
		t.Errorf("optional=0 should parse as false")
	}
}

// TestToXMLRoundTripExtendedFields 验证 ToXML/deepCopy 保留 references、
// annotation、repo-hooks、manifest-server，且写出的清单可再次 Parse。
func TestToXMLRoundTripExtendedFields(t *testing.T) {
	m := &Manifest{
		Remotes: []Remote{{
			Name:        "origin",
			Fetch:       "https://example.com/",
			CustomAttrs: map[string]string{},
			Annotations: []Annotation{{Name: "rkey", Value: "rval"}},
		}},
		Default: Default{Remote: "origin", Revision: "main", CustomAttrs: map[string]string{}},
		Projects: []Project{{
			Name:        "a",
			Path:        "a",
			Revision:    "main",
			References:  "refs/heads/main",
			SyncC:       OptBool{Set: true, Value: true},
			ForcePath:   OptBool{Set: true, Value: true},
			CustomAttrs: map[string]string{},
			Annotations: []Annotation{{Name: "key", Value: "value"}},
		}},
		RepoHooks:      &RepoHooks{InProject: "hooks", EnabledList: "pre-upload"},
		ManifestServer: &ManifestServer{URL: "https://manifest.example.com"},
		CustomAttrs:    map[string]string{},
	}

	copied := deepCopyManifest(m)
	if copied.RepoHooks == nil || copied.ManifestServer == nil {
		t.Fatal("deepCopyManifest dropped repo-hooks/manifest-server")
	}
	if len(copied.Projects[0].Annotations) != 1 || len(copied.Remotes[0].Annotations) != 1 {
		t.Fatal("deepCopyManifest dropped annotations")
	}

	xmlStr, err := m.ToXML()
	if err != nil {
		t.Fatalf("ToXML: %v", err)
	}
	for _, want := range []string{
		`references="refs/heads/main"`,
		`force-path="true"`,
		`<annotation`,
		`<repo-hooks`,
		`<manifest-server`,
	} {
		if !strings.Contains(xmlStr, want) {
			t.Errorf("ToXML output missing %q:\n%s", want, xmlStr)
		}
	}

	parser := NewParser()
	parsed, err := parser.Parse([]byte(xmlStr), nil)
	if err != nil {
		t.Fatalf("re-parse ToXML output: %v", err)
	}
	if parsed.RepoHooks == nil || parsed.RepoHooks.InProject != "hooks" {
		t.Errorf("RepoHooks round-trip = %+v", parsed.RepoHooks)
	}
	if parsed.ManifestServer == nil || parsed.ManifestServer.URL != "https://manifest.example.com" {
		t.Errorf("ManifestServer round-trip = %+v", parsed.ManifestServer)
	}
	if len(parsed.Projects) != 1 || parsed.Projects[0].References != "refs/heads/main" {
		t.Errorf("References round-trip = %+v", parsed.Projects)
	}
	if len(parsed.Projects[0].Annotations) != 1 || parsed.Projects[0].Annotations[0].Name != "key" {
		t.Errorf("Project annotations round-trip = %+v", parsed.Projects[0].Annotations)
	}
	if !parsed.Projects[0].SyncC.Get(false) || !parsed.Projects[0].ForcePath.Get(false) {
		t.Errorf("project bools round-trip = %+v", parsed.Projects[0])
	}
}

// TestCacheInvalidatedByIncludeAndLocalManifest 验证缓存指纹包含 include 文件与
// .repo/local_manifests/*.xml 的 mtime：依赖变化后必须重新解析。
func TestCacheInvalidatedByIncludeAndLocalManifest(t *testing.T) {
	dir := chdirTemp(t)

	writeFile(t, filepath.Join(dir, ".repo", "manifests", "default.xml"), `<?xml version="1.0" encoding="UTF-8"?>
<manifest>
  <remote name="origin" fetch="https://example.com/" />
  <default remote="origin" revision="main" />
  <include name="inc.xml" />
</manifest>`)
	incPath := filepath.Join(dir, ".repo", "manifests", "inc.xml")
	writeFile(t, incPath, `<manifest>
  <project name="a" path="a" />
</manifest>`)

	parser := NewParser()
	first, err := parser.ParseFromFile("default.xml", nil)
	if err != nil {
		t.Fatalf("first ParseFromFile: %v", err)
	}
	if len(first.Projects) != 1 {
		t.Fatalf("first Projects = %v, want [a]", projectNames(first))
	}

	// 修改 include 文件（内容长度不同，mtime/size 均变化）
	writeFile(t, incPath, `<manifest>
  <project name="a" path="a" />
  <project name="b" path="b" />
</manifest>`)
	second, err := parser.ParseFromFile("default.xml", nil)
	if err != nil {
		t.Fatalf("second ParseFromFile: %v", err)
	}
	if len(second.Projects) != 2 {
		t.Fatalf("second Projects = %v, want [a b]（include 变化应使缓存失效）", projectNames(second))
	}

	// 新增 local manifest 后同样必须失效
	writeFile(t, filepath.Join(dir, ".repo", "local_manifests", "local.xml"), `<manifest>
  <project name="local" path="local" />
</manifest>`)
	third, err := parser.ParseFromFile("default.xml", nil)
	if err != nil {
		t.Fatalf("third ParseFromFile: %v", err)
	}
	if len(third.Projects) != 3 {
		t.Fatalf("third Projects = %v, want [a b local]（local manifest 变化应使缓存失效）", projectNames(third))
	}
}

// TestParseCustomAttrsMatchNameAndPath 验证同名不同 path 的两个项目各自拿到
// 自己的自定义属性，不会互相串写。
func TestParseCustomAttrsMatchNameAndPath(t *testing.T) {
	xml := `<manifest>
  <remote name="origin" fetch="https://example.com/" />
  <default remote="origin" revision="main" />
  <project name="a" path="p1" custom="one" />
  <project name="a" path="p2" custom="two" />
</manifest>`
	parser := NewParser()
	m, err := parser.Parse([]byte(xml), nil)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(m.Projects) != 2 {
		t.Fatalf("Projects = %+v, want 2", m.Projects)
	}
	if got, _ := m.Projects[0].GetCustomAttr("custom"); got != "one" {
		t.Errorf("project p1 custom = %q, want one", got)
	}
	if got, _ := m.Projects[1].GetCustomAttr("custom"); got != "two" {
		t.Errorf("project p2 custom = %q, want two", got)
	}
}

// TestProjectRevisionInheritanceChain 验证上游 _ParseProject 的 revision 解析：
// project.revision → 该项目 remote.revision → default.revision。
// 注意 remote.revision 不会灌入 Default.Revision。
func TestProjectRevisionInheritanceChain(t *testing.T) {
	tests := []struct {
		name    string
		xml     string
		want    string
		wantErr bool
	}{
		{
			name: "project revision wins",
			xml: `<manifest>
  <remote name="a" fetch="https://a.example/" revision="rb" />
  <default remote="a" revision="dr" />
  <project name="p" path="p" revision="pr" />
</manifest>`,
			want: "pr",
		},
		{
			name: "project remote revision before default",
			xml: `<manifest>
  <remote name="a" fetch="https://a.example/" />
  <remote name="b" fetch="https://b.example/" revision="rb" />
  <default remote="a" revision="dr" />
  <project name="p" path="p" remote="b" />
  <project name="q" path="q" />
</manifest>`,
			want: "rb",
		},
		{
			name: "default revision fallback",
			xml: `<manifest>
  <remote name="a" fetch="https://a.example/" />
  <default remote="a" revision="dr" />
  <project name="p" path="p" />
</manifest>`,
			want: "dr",
		},
		{
			name: "no revision anywhere errors",
			xml: `<manifest>
  <remote name="a" fetch="https://a.example/" />
  <default remote="a" />
  <project name="p" path="p" />
</manifest>`,
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			parser := NewParser()
			m, err := parser.Parse([]byte(tt.xml), nil)
			if tt.wantErr {
				if err == nil {
					t.Fatal("Parse expected error for missing revision")
				}
				if !strings.Contains(err.Error(), "no revision for project") {
					t.Fatalf("error = %v, want no revision for project", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if m.Projects[0].Revision != tt.want {
				t.Errorf("Project revision = %q, want %q", m.Projects[0].Revision, tt.want)
			}
		})
	}
}

// TestIncludeProjectsNotPrematurelyGroupFiltered 回归：include 解析必须传
// nil groups，否则 include 内项目会先被 -g 过滤，导致外层 remove-project
// 误判目标不存在而报错。
func TestIncludeProjectsNotPrematurelyGroupFiltered(t *testing.T) {
	dir := chdirTemp(t)
	writeFile(t, filepath.Join(dir, ".repo", "manifests", "default.xml"), `<?xml version="1.0" encoding="UTF-8"?>
<manifest>
  <remote name="origin" fetch="https://example.com/" />
  <default remote="origin" revision="main" />
  <project name="keep" path="keep" />
  <include name="inc.xml" />
  <remove-project name="x" />
</manifest>`)
	writeFile(t, filepath.Join(dir, ".repo", "manifests", "inc.xml"), `<manifest>
  <project name="x" path="x" groups="notdefault" />
</manifest>`)

	data, err := os.ReadFile(filepath.Join(dir, ".repo", "manifests", "default.xml"))
	if err != nil {
		t.Fatalf("read main manifest: %v", err)
	}
	parser := NewParser()
	// 直接以 default 组调用 Parse：include 内 notdefault 项目若被提前过滤，
	// 外层 remove 将找不到目标并报错。
	m, err := parser.Parse(data, []string{"default"})
	if err != nil {
		t.Fatalf("Parse with group filter failed: %v", err)
	}
	for _, p := range m.Projects {
		if p.Name == "x" {
			t.Errorf("project x should be excluded by final group filter, got %v", projectNames(m))
		}
	}
	if len(m.Projects) != 1 || m.Projects[0].Name != "keep" {
		t.Fatalf("Projects = %v, want [keep]", projectNames(m))
	}
}

// TestIncludeRemoveNoResurrection 回归：remove-project 删除 include 引入的
// 项目后，replayDocumentOrder 的兜底不得把 include 段项目再次追加（复活）。
// 不使用组过滤，确保结果不依赖随后的 -g 过滤掩盖问题。
func TestIncludeRemoveNoResurrection(t *testing.T) {
	dir := chdirTemp(t)
	writeFile(t, filepath.Join(dir, ".repo", "manifests", "default.xml"), `<?xml version="1.0" encoding="UTF-8"?>
<manifest>
  <remote name="origin" fetch="https://example.com/" />
  <default remote="origin" revision="main" />
  <project name="keep" path="keep" />
  <include name="inc1.xml" />
  <include name="inc2.xml" />
  <remove-project name="x" />
  <remove-project name="y" />
</manifest>`)
	writeFile(t, filepath.Join(dir, ".repo", "manifests", "inc1.xml"), `<manifest>
  <project name="x" path="x" />
</manifest>`)
	writeFile(t, filepath.Join(dir, ".repo", "manifests", "inc2.xml"), `<manifest>
  <project name="y" path="y" />
  <project name="z" path="z" />
</manifest>`)

	parser := NewParser()
	m, err := parser.ParseFromFile("default.xml", nil)
	if err != nil {
		t.Fatalf("ParseFromFile: %v", err)
	}
	got := strings.Join(projectNames(m), ",")
	if got != "keep,z" {
		t.Fatalf("Projects = %v, want [keep z]（x/y 被 remove 后不得复活）", projectNames(m))
	}
}

// TestMergeLocalManifestTransactional 回归：local manifest 合并要么完整生效，
// 要么完全不修改主清单。remove-project 未命中导致失败时，前面已重放的
// 新增项目也不得残留在主清单中。
func TestMergeLocalManifestTransactional(t *testing.T) {
	parser := NewParser()
	main, err := parser.Parse([]byte(`<manifest>
  <remote name="origin" fetch="https://example.com/" />
  <default remote="origin" revision="main" />
  <project name="a" path="a" />
</manifest>`), nil)
	if err != nil {
		t.Fatalf("Parse main: %v", err)
	}
	parser.includeDepth++
	local, err := parser.Parse([]byte(`<manifest>
  <project name="b" path="b" revision="dev" />
  <remove-project name="missing" />
</manifest>`), nil)
	parser.includeDepth--
	if err != nil {
		t.Fatalf("Parse local: %v", err)
	}

	if err := parser.mergeLocalManifest(main, local); err == nil {
		t.Fatal("mergeLocalManifest expected non-existent remove error")
	}
	if got := strings.Join(projectNames(main), ","); got != "a" {
		t.Fatalf("main Projects = %v, want [a]（失败合并不得留下 b）", projectNames(main))
	}
	if len(main.RemoveProjects) != 0 {
		t.Fatalf("main RemoveProjects = %+v, want empty", main.RemoveProjects)
	}
}

// TestLocalManifestParseErrorFatal 回归：local manifest 解析/合并错误即使
// 在 silent 模式下也必须返回错误（上游视为致命），不得仅打印警告后继续。
func TestLocalManifestParseErrorFatal(t *testing.T) {
	dir := chdirTemp(t)
	writeFile(t, filepath.Join(dir, ".repo", "manifests", "default.xml"), `<?xml version="1.0" encoding="UTF-8"?>
<manifest>
  <remote name="origin" fetch="https://example.com/" />
  <default remote="origin" revision="main" />
  <project name="a" path="a" />
</manifest>`)
	writeFile(t, filepath.Join(dir, ".repo", "local_manifests", "bad.xml"), `<manifest>
  <remove-project name="missing" />
</manifest>`)

	parser := NewParser()
	parser.SetSilentMode(true)
	if _, err := parser.ParseFromFile("default.xml", nil); err == nil {
		t.Fatal("ParseFromFile expected error for invalid local manifest in silent mode")
	}
}

// TestRemoveProjectBaseRev 验证 remove-project 的 base-rev：有效 revision
// 匹配时移除成功，不匹配时报 base-revision mismatch；字段可解析/写回。
func TestRemoveProjectBaseRev(t *testing.T) {
	t.Run("match removes project", func(t *testing.T) {
		parser := NewParser()
		m, err := parser.Parse([]byte(`<manifest>
  <remote name="origin" fetch="https://example.com/" />
  <default remote="origin" revision="main" />
  <project name="a" path="a" revision="r1" />
  <remove-project name="a" base-rev="r1" />
</manifest>`), nil)
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		if len(m.Projects) != 0 {
			t.Fatalf("Projects = %v, want empty", projectNames(m))
		}
	})

	t.Run("mismatch reports error", func(t *testing.T) {
		parser := NewParser()
		_, err := parser.Parse([]byte(`<manifest>
  <remote name="origin" fetch="https://example.com/" />
  <default remote="origin" revision="main" />
  <project name="a" path="a" revision="r1" />
  <remove-project name="a" base-rev="r0" />
</manifest>`), nil)
		if err == nil {
			t.Fatal("expected base-revision mismatch error")
		}
		if !strings.Contains(err.Error(), "base-revision mismatch") {
			t.Fatalf("error = %v, want base-revision mismatch", err)
		}
	})

	t.Run("field and ToXML round trip", func(t *testing.T) {
		parser := NewParser()
		parser.includeDepth++
		m, err := parser.Parse([]byte(`<manifest>
  <remove-project name="x" path="x" base-rev="r1" />
</manifest>`), nil)
		parser.includeDepth--
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		if len(m.RemoveProjects) != 1 || m.RemoveProjects[0].BaseRev != "r1" {
			t.Fatalf("RemoveProjects = %+v, want BaseRev r1", m.RemoveProjects)
		}
		if _, ok := m.RemoveProjects[0].CustomAttrs["base-rev"]; ok {
			t.Errorf("base-rev should be a known attr, not a custom attr")
		}
		if m.CustomAttrs == nil {
			m.CustomAttrs = map[string]string{}
		}
		out, err := m.ToXML()
		if err != nil {
			t.Fatalf("ToXML: %v", err)
		}
		if !strings.Contains(out, `base-rev="r1"`) {
			t.Fatalf("ToXML missing base-rev:\n%s", out)
		}
	})
}

// TestLocalManifestExtendMissingErrors 验证 local manifest 的 extend-project
// 未命中同样按上游 fail-fast 报错（不再静默跳过）。
func TestLocalManifestExtendMissingErrors(t *testing.T) {
	dir := chdirTemp(t)
	writeFile(t, filepath.Join(dir, ".repo", "manifests", "default.xml"), `<?xml version="1.0" encoding="UTF-8"?>
<manifest>
  <remote name="origin" fetch="https://example.com/" />
  <default remote="origin" revision="main" />
  <project name="a" path="a" />
</manifest>`)
	writeFile(t, filepath.Join(dir, ".repo", "local_manifests", "local.xml"), `<manifest>
  <extend-project name="missing" groups="g" />
</manifest>`)

	parser := NewParser()
	_, err := parser.ParseFromFile("default.xml", nil)
	if err == nil {
		t.Fatal("expected local manifest extend-project non-existent project error")
	}
	if !strings.Contains(err.Error(), "extend-project") || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("error = %v, want extend-project missing project", err)
	}
}
