package manifest

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

// TestLocalManifestDocumentOrder 验证 local manifest 按 XML 顶层元素顺序应用：
// <remove-project name="foo"/><project name="foo" .../> 必须保留重新定义的
// 项目；反向顺序则最终不存在该项目。
func TestLocalManifestDocumentOrder(t *testing.T) {
	tests := []struct {
		name      string
		localXML  string
		wantNames []string
	}{
		{
			name: "remove then redefine keeps project",
			localXML: `<manifest>
  <remove-project name="foo" />
  <project name="foo" path="foo" revision="new" groups="local" />
</manifest>`,
			wantNames: []string{"keep", "foo"},
		},
		{
			name: "define then remove drops project",
			localXML: `<manifest>
  <project name="newp" path="newp" revision="v1" />
  <remove-project name="newp" />
</manifest>`,
			wantNames: []string{"keep", "foo"},
		},
		{
			name: "extend then remove drops extended project",
			localXML: `<manifest>
  <extend-project name="foo" groups="ext" />
  <remove-project name="foo" />
</manifest>`,
			wantNames: []string{"keep"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := chdirTemp(t)
			writeFile(t, filepath.Join(dir, ".repo", "manifests", "default.xml"), `<?xml version="1.0" encoding="UTF-8"?>
<manifest>
  <remote name="origin" fetch="https://example.com/" />
  <default remote="origin" revision="main" />
  <project name="keep" path="keep" />
  <project name="foo" path="foo" />
</manifest>`)
			writeFile(t, filepath.Join(dir, ".repo", "local_manifests", "local.xml"), tt.localXML)

			parser := NewParser()
			m, err := parser.ParseFromFile("default.xml", nil)
			if err != nil {
				t.Fatalf("ParseFromFile: %v", err)
			}
			if got := projectNames(m); strings.Join(got, ",") != strings.Join(tt.wantNames, ",") {
				t.Fatalf("Projects = %v, want %v", got, tt.wantNames)
			}
			if tt.name == "remove then redefine keeps project" {
				for i := range m.Projects {
					if m.Projects[i].Name == "foo" {
						if m.Projects[i].Revision != "new" || m.Projects[i].Groups != "local" {
							t.Errorf("redefined foo = %+v, want revision=new groups=local", m.Projects[i])
						}
					}
				}
			}
		})
	}
}

// TestIncludeGroupsAndRevisionInheritance 验证 include 的 groups 并集与
// revision 补全（对齐上游 _ParseManifestXml 的 parent_groups/parent_node）。
func TestIncludeGroupsAndRevisionInheritance(t *testing.T) {
	dir := chdirTemp(t)
	writeFile(t, filepath.Join(dir, ".repo", "manifests", "default.xml"), `<?xml version="1.0" encoding="UTF-8"?>
<manifest>
  <remote name="origin" fetch="https://example.com/" />
  <default remote="origin" revision="main" />
  <include name="inc.xml" groups="g1,g2" revision="inc-rev" />
</manifest>`)
	writeFile(t, filepath.Join(dir, ".repo", "manifests", "inc.xml"), `<manifest>
  <project name="p" path="p" groups="g0" />
  <project name="q" path="q" revision="own" />
  <extend-project name="p" groups="ext" />
</manifest>`)

	parser := NewParser()
	m, err := parser.ParseFromFile("default.xml", nil)
	if err != nil {
		t.Fatalf("ParseFromFile: %v", err)
	}
	if len(m.Projects) != 2 {
		t.Fatalf("Projects = %v, want [p q]", projectNames(m))
	}
	byName := make(map[string]*Project, len(m.Projects))
	for i := range m.Projects {
		byName[m.Projects[i].Name] = &m.Projects[i]
	}
	p := byName["p"]
	if p == nil {
		t.Fatal("project p missing")
	}
	if p.Revision != "inc-rev" {
		t.Errorf("Revision = %q, want inc-rev（include revision 补全）", p.Revision)
	}
	if byName["q"].Revision != "own" {
		t.Errorf("q Revision = %q, want own（显式 revision 不被 include 覆盖）", byName["q"].Revision)
	}
	if p.Groups != "g0,g1,g2,ext" {
		t.Errorf("Groups = %q, want g0,g1,g2,ext（include groups 并集 + extend 并集）", p.Groups)
	}
	// ToXML 保留 include 的 groups/revision
	if inc := m.Includes; len(inc) != 1 || inc[0].Groups != "g1,g2" || inc[0].Revision != "inc-rev" {
		t.Fatalf("Include fields = %+v, want groups/revision preserved", inc)
	}
}

// TestExtendProjectDestPathAndFields 验证 extend-project 的 path 仅用于匹配、
// dest-path 改路径，且 dest-branch/upstream/base-rev 生效、groups 取并集。
func TestExtendProjectDestPathAndFields(t *testing.T) {
	parser := NewParser()
	m, err := parser.Parse([]byte(`<manifest>
  <remote name="origin" fetch="https://example.com/" />
  <default remote="origin" revision="main" />
  <project name="a" path="p1" groups="old" revision="r1" dest-branch="db1" upstream="up1" />
  <extend-project name="a" path="p1" dest-path="p2" groups="new" revision="r2" base-rev="r1" dest-branch="db2" upstream="up2" />
  <extend-project name="a" path="p2" groups="more" revision="r3" base-rev="r2" />
</manifest>`), nil)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(m.Projects) != 1 {
		t.Fatalf("Projects = %v, want 1", projectNames(m))
	}
	p := m.Projects[0]
	if p.Path != "p2" {
		t.Errorf("Path = %q, want p2（dest-path 改路径，path 仅匹配）", p.Path)
	}
	if p.Groups != "old,new,more" {
		t.Errorf("Groups = %q, want old,new,more（并集）", p.Groups)
	}
	if p.Revision != "r3" || p.DestBranch != "db2" || p.Upstream != "up2" {
		t.Errorf("project = %+v, want revision/dest-branch/upstream applied", p)
	}
}

// TestExtendProjectBaseRevMismatch 验证 base-rev 与当前 revision 不符时报错。
func TestExtendProjectBaseRevMismatch(t *testing.T) {
	parser := NewParser()
	_, err := parser.Parse([]byte(`<manifest>
  <remote name="origin" fetch="https://example.com/" />
  <default remote="origin" revision="main" />
  <project name="a" path="a" revision="actual" />
  <extend-project name="a" revision="next" base-rev="expected" />
</manifest>`), nil)
	if err == nil {
		t.Fatal("expected base-rev mismatch error")
	}
	if !strings.Contains(err.Error(), "actual") || !strings.Contains(err.Error(), "expected") {
		t.Errorf("error = %v, want base/revision values", err)
	}
}

// TestExtendProjectDestPathMultipleMatches 验证无 path 时 dest-path 不能匹配
// 多个同名项目（上游显式报错）。
func TestExtendProjectDestPathMultipleMatches(t *testing.T) {
	parser := NewParser()
	_, err := parser.Parse([]byte(`<manifest>
  <remote name="origin" fetch="https://example.com/" />
  <default remote="origin" revision="main" />
  <project name="a" path="p1" />
  <project name="a" path="p2" />
  <extend-project name="a" dest-path="p3" />
</manifest>`), nil)
	if err == nil || !strings.Contains(err.Error(), "dest-path") {
		t.Fatalf("error = %v, want dest-path multiple matches error", err)
	}
}

// TestProjectPathEscapeRejected 验证上游 _CheckLocalPath 等价的路径校验：
// 绝对路径、.. 逃逸、.git/.repo*/~ 组件（大小写不敏感）均被拒绝。
func TestProjectPathEscapeRejected(t *testing.T) {
	tests := []struct {
		name string
		xml  string
	}{
		{"name parent escape", `<manifest>
  <remote name="origin" fetch="https://example.com/" /><default remote="origin" revision="main" />
  <project name="../escape" path="x" />
</manifest>`},
		{"name absolute", `<manifest>
  <remote name="origin" fetch="https://example.com/" /><default remote="origin" revision="main" />
  <project name="/abs/path" path="x" />
</manifest>`},
		{"path parent escape", `<manifest>
  <remote name="origin" fetch="https://example.com/" /><default remote="origin" revision="main" />
  <project name="p" path="../escape" />
</manifest>`},
		{"path dot", `<manifest>
  <remote name="origin" fetch="https://example.com/" /><default remote="origin" revision="main" />
  <project name="p" path="a/../b" />
</manifest>`},
		{"path git dir", `<manifest>
  <remote name="origin" fetch="https://example.com/" /><default remote="origin" revision="main" />
  <project name="p" path="a/.git/config" />
</manifest>`},
		{"path git dir uppercase", `<manifest>
  <remote name="origin" fetch="https://example.com/" /><default remote="origin" revision="main" />
  <project name="p" path="a/.GIT/config" />
</manifest>`},
		{"path repo dir", `<manifest>
  <remote name="origin" fetch="https://example.com/" /><default remote="origin" revision="main" />
  <project name="p" path=".repo-secret/x" />
</manifest>`},
		{"path tilde", `<manifest>
  <remote name="origin" fetch="https://example.com/" /><default remote="origin" revision="main" />
  <project name="p" path="~/x" />
</manifest>`},
		{"path dotdot exact", `<manifest>
  <remote name="origin" fetch="https://example.com/" /><default remote="origin" revision="main" />
  <project name="p" path=".." />
</manifest>`},
		{"path windows root", `<manifest>
  <remote name="origin" fetch="https://example.com/" /><default remote="origin" revision="main" />
  <project name="p" path="\foo" />
</manifest>`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			parser := NewParser()
			_, err := parser.Parse([]byte(tt.xml), nil)
			if err == nil {
				t.Fatal("expected invalid path error")
			}
			var manifestErr *ManifestError
			if !errors.As(err, &manifestErr) {
				t.Fatalf("error = %T %v, want *ManifestError", err, err)
			}
			if !strings.Contains(err.Error(), "project") {
				t.Errorf("error = %v, want project name/context", err)
			}
		})
	}
}

// TestProjectValidSlashedName 验证正常的多级 name/path 不被误拒。
func TestProjectValidSlashedName(t *testing.T) {
	parser := NewParser()
	m, err := parser.Parse([]byte(`<manifest>
  <remote name="origin" fetch="https://example.com/" />
  <default remote="origin" revision="main" />
  <project name="platform/frameworks/base" path="frameworks/base" />
</manifest>`), nil)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(m.Projects) != 1 {
		t.Fatalf("Projects = %v, want 1", projectNames(m))
	}
}

// TestUseLocalManifestsSwitchAndCacheKey 验证 Parser.SetUseLocalManifests 生效，
// 且两种模式的缓存互不污染。
func TestUseLocalManifestsSwitchAndCacheKey(t *testing.T) {
	dir := chdirTemp(t)
	writeFile(t, filepath.Join(dir, ".repo", "manifests", "default.xml"), `<?xml version="1.0" encoding="UTF-8"?>
<manifest>
  <remote name="origin" fetch="https://example.com/" />
  <default remote="origin" revision="main" />
  <project name="base" path="base" />
</manifest>`)
	writeFile(t, filepath.Join(dir, ".repo", "local_manifests", "local.xml"), `<manifest>
  <project name="local" path="local" />
</manifest>`)

	parser := NewParser()
	withLocal, err := parser.ParseFromFile("default.xml", nil)
	if err != nil {
		t.Fatalf("parse with local: %v", err)
	}
	if len(withLocal.Projects) != 2 {
		t.Fatalf("with local Projects = %v, want 2", projectNames(withLocal))
	}

	parser.SetUseLocalManifests(false)
	withoutLocal, err := parser.ParseFromFile("default.xml", nil)
	if err != nil {
		t.Fatalf("parse without local: %v", err)
	}
	if len(withoutLocal.Projects) != 1 {
		t.Fatalf("without local Projects = %v, want 1", projectNames(withoutLocal))
	}

	// 切回默认模式必须命中各自缓存且结果正确
	parser.SetUseLocalManifests(true)
	again, err := parser.ParseFromFile("default.xml", nil)
	if err != nil {
		t.Fatalf("parse with local again: %v", err)
	}
	if len(again.Projects) != 2 {
		t.Fatalf("with local again Projects = %v, want 2", projectNames(again))
	}
}

// TestCacheInvalidatedByRecursiveInclude 验证依赖指纹递归 include 子目录文件。
func TestCacheInvalidatedByRecursiveInclude(t *testing.T) {
	dir := chdirTemp(t)
	writeFile(t, filepath.Join(dir, ".repo", "manifests", "default.xml"), `<?xml version="1.0" encoding="UTF-8"?>
<manifest>
  <remote name="origin" fetch="https://example.com/" />
  <default remote="origin" revision="main" />
  <include name="sub/one.xml" />
</manifest>`)
	writeFile(t, filepath.Join(dir, ".repo", "manifests", "sub", "one.xml"), `<manifest>
  <include name="sub/two.xml" />
</manifest>`)
	twoPath := filepath.Join(dir, ".repo", "manifests", "sub", "two.xml")
	writeFile(t, twoPath, `<manifest>
  <project name="a" path="a" />
</manifest>`)

	parser := NewParser()
	first, err := parser.ParseFromFile("default.xml", nil)
	if err != nil {
		t.Fatalf("first parse: %v", err)
	}
	if len(first.Projects) != 1 {
		t.Fatalf("first Projects = %v, want [a]", projectNames(first))
	}

	writeFile(t, twoPath, `<manifest>
  <project name="a" path="a" />
  <project name="b" path="b" />
</manifest>`)
	second, err := parser.ParseFromFile("default.xml", nil)
	if err != nil {
		t.Fatalf("second parse: %v", err)
	}
	if len(second.Projects) != 2 {
		t.Fatalf("second Projects = %v, want [a b]（子目录 include 变化应失效缓存）", projectNames(second))
	}
}

// TestParseCustomAttrsNoPathMatchesImplicitPath 验证无 path 的自定义属性节点
// 只匹配“有效 path == name”的项目，避免同名不同 path 串写。
func TestParseCustomAttrsNoPathMatchesImplicitPath(t *testing.T) {
	parser := NewParser()
	m, err := parser.Parse([]byte(`<manifest>
  <remote name="origin" fetch="https://example.com/" />
  <default remote="origin" revision="main" />
  <project name="a" path="p1" custom="one" />
  <project name="a" custom="two" />
</manifest>`), nil)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(m.Projects) != 2 {
		t.Fatalf("Projects = %v, want 2", projectNames(m))
	}
	byPath := make(map[string]*Project, len(m.Projects))
	for i := range m.Projects {
		byPath[m.Projects[i].Path] = &m.Projects[i]
	}
	if got, _ := byPath["p1"].GetCustomAttr("custom"); got != "one" {
		t.Errorf("path=p1 custom = %q, want one", got)
	}
	if got, _ := byPath["a"].GetCustomAttr("custom"); got != "two" {
		t.Errorf("path=a custom = %q, want two", got)
	}
}

// TestToXMLPreservesIncludeExtendAndManifestServer 验证 ToXML 序列化 include
// 的 groups/revision、extend-project 扩展字段与 manifest-server helper。
func TestToXMLPreservesIncludeExtendAndManifestServer(t *testing.T) {
	m := &Manifest{
		Default:  Default{Remote: "origin", Revision: "main"},
		Projects: []Project{{Name: "a", Path: "p1"}},
		Includes: []Include{{Name: "inc.xml", Groups: "g1", Revision: "r1", CustomAttrs: map[string]string{}}},
		ExtendProjects: []ExtendProject{{
			Name: "a", Path: "p1", DestPath: "p2", Groups: "g", Revision: "r2",
			BaseRev: "r1", Remote: "origin", DestBranch: "db", Upstream: "up",
		}},
		ManifestServer: &ManifestServer{URL: "https://manifest.example.com", Helper: "helper.bin"},
		CustomAttrs:    map[string]string{},
	}
	out, err := m.ToXML()
	if err != nil {
		t.Fatalf("ToXML: %v", err)
	}
	for _, want := range []string{
		`<include name="inc.xml" groups="g1" revision="r1" />`,
		`dest-path="p2"`,
		`base-rev="r1"`,
		`dest-branch="db"`,
		`upstream="up"`,
		`<manifest-server url="https://manifest.example.com" helper="helper.bin" />`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("ToXML missing %q:\n%s", want, out)
		}
	}
}

// TestManifestServerHelperParse 验证 manifest-server 的 helper 属性可解析。
func TestManifestServerHelperParse(t *testing.T) {
	parser := NewParser()
	m, err := parser.Parse([]byte(`<manifest>
  <remote name="origin" fetch="https://example.com/" />
  <default remote="origin" revision="main" />
  <manifest-server url="https://manifest.example.com" helper="helper.bin" />
</manifest>`), nil)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if m.ManifestServer == nil || m.ManifestServer.Helper != "helper.bin" {
		t.Fatalf("ManifestServer = %+v, want helper.bin", m.ManifestServer)
	}
}

// TestExtendProjectMissingTopLevel 回归：顶层 extend-project 引用不存在的
// 项目必须报错（上游 fail-fast），不得静默跳过。
func TestExtendProjectMissingTopLevel(t *testing.T) {
	parser := NewParser()
	_, err := parser.Parse([]byte(`<manifest>
  <remote name="origin" fetch="https://example.com/" />
  <default remote="origin" revision="main" />
  <project name="a" path="a" />
  <extend-project name="missing" groups="g" />
</manifest>`), nil)
	if err == nil {
		t.Fatal("expected extend-project non-existent project error")
	}
	if !strings.Contains(err.Error(), "extend-project") || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("error = %v, want extend-project missing project", err)
	}
}

// TestIncludeRemoveThenRedefineKeepsProject 验证 include 段按子清单元素顺序
// 重放：include 内先 remove 外层项目再重新定义同名项目时，重新定义必须
// 保留；反向顺序（project 后 remove）则最终不存在该项目。
func TestIncludeRemoveThenRedefineKeepsProject(t *testing.T) {
	tests := []struct {
		name      string
		include   string
		wantNames []string
		wantRev   string
	}{
		{
			name: "remove then redefine keeps project",
			include: `<manifest>
  <remove-project name="a" />
  <project name="a" path="a" revision="redefined" />
</manifest>`,
			wantNames: []string{"a"},
			wantRev:   "redefined",
		},
		{
			name: "project then remove deletes project",
			include: `<manifest>
  <project name="a" path="a" revision="redefined" />
  <remove-project name="a" />
</manifest>`,
			wantNames: []string{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := chdirTemp(t)
			writeFile(t, filepath.Join(dir, ".repo", "manifests", "default.xml"), `<manifest>
  <remote name="origin" fetch="https://example.com/" />
  <default remote="origin" revision="main" />
  <project name="a" path="a" revision="outer" />
  <include name="inc.xml" />
</manifest>`)
			writeFile(t, filepath.Join(dir, ".repo", "manifests", "inc.xml"), tt.include)

			parser := NewParser()
			m, err := parser.ParseFromFile("default.xml", nil)
			if err != nil {
				t.Fatalf("ParseFromFile: %v", err)
			}
			if got := strings.Join(projectNames(m), ","); got != strings.Join(tt.wantNames, ",") {
				t.Fatalf("Projects = %v, want %v", projectNames(m), tt.wantNames)
			}
			if len(tt.wantNames) > 0 && m.Projects[0].Revision != tt.wantRev {
				t.Errorf("Revision = %q, want %q", m.Projects[0].Revision, tt.wantRev)
			}
		})
	}
}

// TestIncludePropagatedExtendTargetRemoved 验证 include 传播的未命中 extend：
// 目标随后被 remove 删除时应静默丢弃，不得误报“项目不存在”。
func TestIncludePropagatedExtendTargetRemoved(t *testing.T) {
	dir := chdirTemp(t)
	writeFile(t, filepath.Join(dir, ".repo", "manifests", "default.xml"), `<manifest>
  <remote name="origin" fetch="https://example.com/" />
  <default remote="origin" revision="main" />
  <include name="inc.xml" />
  <project name="a" path="a" />
  <remove-project name="a" />
</manifest>`)
	writeFile(t, filepath.Join(dir, ".repo", "manifests", "inc.xml"), `<manifest>
  <extend-project name="a" revision="next" />
</manifest>`)

	parser := NewParser()
	m, err := parser.ParseFromFile("default.xml", nil)
	if err != nil {
		t.Fatalf("ParseFromFile: %v", err)
	}
	if len(m.Projects) != 0 {
		t.Fatalf("Projects = %v, want empty（a 被 remove）", projectNames(m))
	}
	if len(m.ExtendProjects) != 0 {
		t.Errorf("ExtendProjects = %+v, want empty（目标已删除应丢弃）", m.ExtendProjects)
	}
}

// TestIncludePropagatedExtendNeverExisted 验证 include 传播的未命中 extend
// 若目标从未出现，顶层解析必须报错。
func TestIncludePropagatedExtendNeverExisted(t *testing.T) {
	dir := chdirTemp(t)
	writeFile(t, filepath.Join(dir, ".repo", "manifests", "default.xml"), `<manifest>
  <remote name="origin" fetch="https://example.com/" />
  <default remote="origin" revision="main" />
  <project name="b" path="b" />
  <include name="inc.xml" />
</manifest>`)
	writeFile(t, filepath.Join(dir, ".repo", "manifests", "inc.xml"), `<manifest>
  <extend-project name="missing" revision="next" />
</manifest>`)

	parser := NewParser()
	_, err := parser.ParseFromFile("default.xml", nil)
	if err == nil {
		t.Fatal("expected extend-project non-existent project error")
	}
	if !strings.Contains(err.Error(), "extend-project") || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("error = %v, want extend-project missing project", err)
	}
}

// TestExtendBeforeRemoveIsLegal 验证文档顺序中 extend 在 remove 之前合法：
// 先扩展已存在项目，再由后续 remove 删除。
func TestExtendBeforeRemoveIsLegal(t *testing.T) {
	parser := NewParser()
	m, err := parser.Parse([]byte(`<manifest>
  <remote name="origin" fetch="https://example.com/" />
  <default remote="origin" revision="main" />
  <project name="a" path="a" />
  <extend-project name="a" revision="next" />
  <remove-project name="a" />
</manifest>`), nil)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(m.Projects) != 0 {
		t.Fatalf("Projects = %v, want empty（extend 后 remove 应删除）", projectNames(m))
	}
}

// TestExtendProjectPathNoMatchIsNoop 验证 extend-project 先按 name 判存在：
// name 存在但 path 无命中是 no-op，不报“项目不存在”。
func TestExtendProjectPathNoMatchIsNoop(t *testing.T) {
	parser := NewParser()
	m, err := parser.Parse([]byte(`<manifest>
  <remote name="origin" fetch="https://example.com/" />
  <default remote="origin" revision="main" />
  <project name="a" path="p1" groups="old" />
  <extend-project name="a" path="p2" groups="new" revision="next" />
</manifest>`), nil)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(m.Projects) != 1 {
		t.Fatalf("Projects = %v, want [a]", projectNames(m))
	}
	if m.Projects[0].Path != "p1" {
		t.Errorf("Path = %q, want p1（path 无命中应 no-op）", m.Projects[0].Path)
	}
	if m.Projects[0].Groups != "old" {
		t.Errorf("Groups = %q, want old（path 无命中不得修改项目）", m.Projects[0].Groups)
	}
	if m.Projects[0].Revision != "main" {
		t.Errorf("Revision = %q, want main（path 无命中不得修改项目）", m.Projects[0].Revision)
	}
}

// TestToXMLDropsDanglingExtend 验证写盘前过滤目标不存在的 extend-project，
// 避免生成再次解析即失败的清单。
func TestToXMLDropsDanglingExtend(t *testing.T) {
	m := &Manifest{
		Default:        Default{Remote: "origin", Revision: "main"},
		Projects:       []Project{{Name: "a", Path: "a"}},
		ExtendProjects: []ExtendProject{{Name: "ghost", Groups: "g"}},
		CustomAttrs:    map[string]string{},
	}
	out, err := m.ToXML()
	if err != nil {
		t.Fatalf("ToXML: %v", err)
	}
	if strings.Contains(out, "ghost") || strings.Contains(out, "<extend-project") {
		t.Fatalf("ToXML should drop dangling extend-project:\n%s", out)
	}
}

// TestManifestCacheKeyMode 验证缓存 key 区分 include 链解析深度与 local
// fragment 模式（requireRevision / remove-extend 应用语义不同）。
func TestManifestCacheKeyMode(t *testing.T) {
	base := manifestCacheKey("/m/default.xml", true, 0, false)
	if base == manifestCacheKey("/m/default.xml", true, 1, false) {
		t.Error("cache key must distinguish includeDepth (requireRevision mode)")
	}
	if base == manifestCacheKey("/m/default.xml", true, 0, true) {
		t.Error("cache key must distinguish local fragment mode")
	}
	if base == manifestCacheKey("/m/default.xml", false, 0, false) {
		t.Error("cache key must distinguish useLocalManifests mode")
	}
}

// projectSignatures 返回项目的稳定签名（name|path|revision|groups），用于
// 比较 parse→ToXML→parse 前后项目集合是否一致。
func projectSignatures(m *Manifest) []string {
	out := make([]string, 0, len(m.Projects))
	for _, p := range m.Projects {
		out = append(out, p.Name+"|"+p.Path+"|"+p.Revision+"|"+p.Groups)
	}
	return out
}

// TestFlattenWriteReparseInvariant 验证扁平化写盘不输出“会在二次解析生效”的
// 残留 remove-project（optional/悬空），且 parse→ToXML→parse 项目集合不变。
func TestFlattenWriteReparseInvariant(t *testing.T) {
	tests := []struct {
		name     string
		mainXML  string
		include  string
		wantSkip string
	}{
		{
			name: "optional remove before include-defined project",
			mainXML: `<manifest>
  <remote name="origin" fetch="https://example.com/" />
  <default remote="origin" revision="main" />
  <remove-project name="a" optional="true" />
  <include name="inc.xml" />
</manifest>`,
			include: `<manifest>
  <project name="a" path="a" />
</manifest>`,
			wantSkip: "remove-project",
		},
		{
			name: "path-only optional remove before later project",
			mainXML: `<manifest>
  <remote name="origin" fetch="https://example.com/" />
  <default remote="origin" revision="main" />
  <remove-project path="x" optional="true" />
  <project name="a" path="x" />
</manifest>`,
			wantSkip: "remove-project",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := chdirTemp(t)
			writeFile(t, filepath.Join(dir, ".repo", "manifests", "default.xml"), tt.mainXML)
			if tt.include != "" {
				writeFile(t, filepath.Join(dir, ".repo", "manifests", "inc.xml"), tt.include)
			}

			parser := NewParser()
			first, err := parser.ParseFromFile("default.xml", nil)
			if err != nil {
				t.Fatalf("ParseFromFile: %v", err)
			}
			out, err := first.ToXML()
			if err != nil {
				t.Fatalf("ToXML: %v", err)
			}
			if strings.Contains(out, tt.wantSkip) {
				t.Fatalf("扁平输出残留会在二次解析生效的 %s:\n%s", tt.wantSkip, out)
			}
			if strings.Contains(out, "<include") {
				t.Fatalf("已展开的 include 不应再次输出（会导致重复展开）:\n%s", out)
			}

			second, err := parser.Parse([]byte(out), nil)
			if err != nil {
				t.Fatalf("re-parse ToXML output: %v\n%s", err, out)
			}
			got, want := projectSignatures(second), projectSignatures(first)
			if strings.Join(got, ",") != strings.Join(want, ",") {
				t.Fatalf("parse→ToXML→parse 项目集合变化:\n got %v\nwant %v", got, want)
			}
		})
	}
}

// TestToXMLReparseInvariantIncludes 验证含 include 的已解析清单可直接写回并
// 再次解析：include 内容已展开进 Projects，ToXML 不得再输出 include 标签。
func TestToXMLReparseInvariantIncludes(t *testing.T) {
	dir := chdirTemp(t)
	writeFile(t, filepath.Join(dir, ".repo", "manifests", "default.xml"), `<manifest>
  <remote name="origin" fetch="https://example.com/" />
  <default remote="origin" revision="main" />
  <project name="outer" path="outer" />
  <include name="inc.xml" />
</manifest>`)
	writeFile(t, filepath.Join(dir, ".repo", "manifests", "inc.xml"), `<manifest>
  <project name="inner" path="inner" groups="g" />
</manifest>`)

	parser := NewParser()
	first, err := parser.ParseFromFile("default.xml", nil)
	if err != nil {
		t.Fatalf("ParseFromFile: %v", err)
	}
	out, err := first.ToXML()
	if err != nil {
		t.Fatalf("ToXML: %v", err)
	}
	second, err := parser.Parse([]byte(out), nil)
	if err != nil {
		t.Fatalf("re-parse: %v\n%s", err, out)
	}
	if got, want := strings.Join(projectSignatures(second), ","), strings.Join(projectSignatures(first), ","); got != want {
		t.Fatalf("round trip 项目集合变化:\n got %s\nwant %s", got, want)
	}
}

// TestParseUnknownRemoteRejected 验证显式指定（含 default 继承）不存在的
// remote 在解析期报错，不得静默生成空 FetchBase/“..”。
func TestParseUnknownRemoteRejected(t *testing.T) {
	tests := []struct {
		name    string
		xml     string
		wantErr string
	}{
		{
			name: "explicit project remote",
			xml: `<manifest>
  <remote name="origin" fetch="https://example.com/" />
  <default remote="origin" revision="main" />
  <project name="p" path="p" remote="ghost" />
</manifest>`,
			wantErr: "remote ghost not defined",
		},
		{
			name: "default remote inherited",
			xml: `<manifest>
  <remote name="origin" fetch="https://example.com/" />
  <default remote="ghost" revision="main" />
  <project name="p" path="p" />
</manifest>`,
			wantErr: "remote ghost not defined",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			parser := NewParser()
			_, err := parser.Parse([]byte(tt.xml), nil)
			if err == nil {
				t.Fatal("expected undefined remote error")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

// TestProjectRemoteAliasResolution 验证项目 remote 可引用 remote 的 alias，
// 且 GetRemoteURL 与 GetRemoteReview/GetRemotePushURL 一样匹配 alias。
func TestProjectRemoteAliasResolution(t *testing.T) {
	parser := NewParser()
	m, err := parser.Parse([]byte(`<manifest>
  <remote name="origin" alias="up" fetch="https://example.com/base" />
  <default remote="origin" revision="main" />
  <project name="p" path="p" remote="up" />
</manifest>`), nil)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got, _ := m.Projects[0].GetCustomAttr("__remote_fetch"); got != "https://example.com/base" {
		t.Errorf("__remote_fetch = %q, want alias remote fetch", got)
	}
	got, err := m.GetRemoteURL("up")
	if err != nil || got != "https://example.com/base" {
		t.Errorf("GetRemoteURL(alias) = %q, %v; want fetch by alias", got, err)
	}
	if _, err := m.GetRemoteURL("ghost"); err == nil {
		t.Error("GetRemoteURL(ghost) should error")
	}
}

// TestIncludeExtendPositionalSemantics 验证 include 传播的 extend 只在
// include 位置对已存在项目求值，不作用到其后才定义的项目，也不把 extend
// 烧进 project 快照。
func TestIncludeExtendPositionalSemantics(t *testing.T) {
	tests := []struct {
		name      string
		mainXML   string
		include   string
		target    string
		wantRev   string
		wantNames []string
	}{
		{
			name: "child extend targets outer project before include",
			mainXML: `<manifest>
  <remote name="origin" fetch="https://example.com/" />
  <default remote="origin" revision="main" />
  <project name="a" path="a" revision="parent" />
  <include name="inc.xml" />
</manifest>`,
			include: `<manifest>
  <extend-project name="a" revision="child-ext" />
  <project name="b" path="b" />
</manifest>`,
			target:    "a",
			wantRev:   "child-ext",
			wantNames: []string{"a", "b"},
		},
		{
			name: "child extend before later project is not applied",
			mainXML: `<manifest>
  <remote name="origin" fetch="https://example.com/" />
  <default remote="origin" revision="main" />
  <include name="inc.xml" />
</manifest>`,
			include: `<manifest>
  <extend-project name="b" path="b" revision="ext" />
  <project name="b" path="b" revision="orig" />
</manifest>`,
			target:    "b",
			wantRev:   "orig",
			wantNames: []string{"b"},
		},
		{
			name: "child extend after project is applied",
			mainXML: `<manifest>
  <remote name="origin" fetch="https://example.com/" />
  <default remote="origin" revision="main" />
  <include name="inc.xml" />
</manifest>`,
			include: `<manifest>
  <project name="b" path="b" revision="orig" />
  <extend-project name="b" path="b" revision="ext" />
</manifest>`,
			target:    "b",
			wantRev:   "ext",
			wantNames: []string{"b"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := chdirTemp(t)
			writeFile(t, filepath.Join(dir, ".repo", "manifests", "default.xml"), tt.mainXML)
			writeFile(t, filepath.Join(dir, ".repo", "manifests", "inc.xml"), tt.include)

			parser := NewParser()
			m, err := parser.ParseFromFile("default.xml", nil)
			if err != nil {
				t.Fatalf("ParseFromFile: %v", err)
			}
			if got := strings.Join(projectNames(m), ","); got != strings.Join(tt.wantNames, ",") {
				t.Fatalf("Projects = %v, want %v", projectNames(m), tt.wantNames)
			}
			for i := range m.Projects {
				if m.Projects[i].Name == tt.target {
					if m.Projects[i].Revision != tt.wantRev {
						t.Errorf("%s Revision = %q, want %q", tt.target, m.Projects[i].Revision, tt.wantRev)
					}
				}
			}
			if len(m.ExtendProjects) != 0 {
				t.Errorf("ExtendProjects = %+v, want empty（include 位置重放后不应残留）", m.ExtendProjects)
			}
		})
	}
}

// TestIncludeBaseRevDeferred 验证 include 子清单的 remove/extend base-rev
// 校验推迟到父层重放位置：子层缺少 remote/default 时不得误报，父层仍按
// 实际 remote/default 校验。
func TestIncludeBaseRevDeferred(t *testing.T) {
	tests := []struct {
		name    string
		include string
		wantErr bool
		loose   bool // 期望项目被移除
		wantRev string
	}{
		{
			name: "extend base-rev validated at parent",
			include: `<manifest>
  <project name="c" path="c" />
  <extend-project name="c" revision="next" base-rev="main" />
</manifest>`,
			wantRev: "next",
		},
		{
			name: "remove base-rev validated at parent",
			include: `<manifest>
  <project name="c" path="c" />
  <remove-project name="c" base-rev="main" />
</manifest>`,
			loose: true,
		},
		{
			name: "parent context detects real mismatch",
			include: `<manifest>
  <project name="c" path="c" />
  <extend-project name="c" revision="next" base-rev="wrong" />
</manifest>`,
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := chdirTemp(t)
			writeFile(t, filepath.Join(dir, ".repo", "manifests", "default.xml"), `<manifest>
  <remote name="origin" fetch="https://example.com/" />
  <default remote="origin" revision="main" />
  <include name="inc.xml" />
</manifest>`)
			writeFile(t, filepath.Join(dir, ".repo", "manifests", "inc.xml"), tt.include)

			parser := NewParser()
			m, err := parser.ParseFromFile("default.xml", nil)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected base-rev mismatch error")
				}
				if !strings.Contains(err.Error(), "base") {
					t.Fatalf("error = %v, want base-rev 信息", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseFromFile: %v（合法清单不得误报 base-rev）", err)
			}
			if tt.loose {
				if len(m.Projects) != 0 {
					t.Fatalf("Projects = %v, want empty（remove 在父层生效）", projectNames(m))
				}
				return
			}
			for i := range m.Projects {
				if m.Projects[i].Name == "c" && m.Projects[i].Revision != tt.wantRev {
					t.Errorf("c Revision = %q, want %q", m.Projects[i].Revision, tt.wantRev)
				}
			}
		})
	}
}

// TestPathTrailingSlashNormalization 验证 path/name 尾部 "/" 归一化参与
// 去重（duplicate path）、remove/extend 匹配、local 合并键与 ToXML 输出。
func TestPathTrailingSlashNormalization(t *testing.T) {
	t.Run("parse normalizes project path", func(t *testing.T) {
		parser := NewParser()
		m, err := parser.Parse([]byte(`<manifest>
  <remote name="origin" fetch="https://example.com/" />
  <default remote="origin" revision="main" />
  <project name="a" path="x/" />
</manifest>`), nil)
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		if m.Projects[0].Path != "x" {
			t.Errorf("Path = %q, want x（尾部 / 归一化）", m.Projects[0].Path)
		}
	})

	t.Run("duplicate path detected across trailing slash", func(t *testing.T) {
		parser := NewParser()
		_, err := parser.Parse([]byte(`<manifest>
  <remote name="origin" fetch="https://example.com/" />
  <default remote="origin" revision="main" />
  <project name="a" path="x/" />
  <project name="b" path="x" />
</manifest>`), nil)
		if err == nil || !strings.Contains(err.Error(), "duplicate path") {
			t.Fatalf("error = %v, want duplicate path", err)
		}
	})

	t.Run("remove and extend match normalized path", func(t *testing.T) {
		parser := NewParser()
		m, err := parser.Parse([]byte(`<manifest>
  <remote name="origin" fetch="https://example.com/" />
  <default remote="origin" revision="main" />
  <project name="a" path="p1/" />
  <extend-project name="a" path="p1" revision="next" />
  <project name="b" path="p2/" />
  <remove-project name="b" path="p2" />
</manifest>`), nil)
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		if len(m.Projects) != 1 || m.Projects[0].Name != "a" {
			t.Fatalf("Projects = %v, want [a]", projectNames(m))
		}
		if m.Projects[0].Path != "p1" || m.Projects[0].Revision != "next" {
			t.Errorf("project a = %+v, want path p1 revision next", m.Projects[0])
		}
	})

	t.Run("local merge key normalized", func(t *testing.T) {
		main := &Manifest{Projects: []Project{{Name: "a", Path: "foo", Revision: "old"}}}
		applyLocalProject(main, Project{Name: "a", Path: "foo/", Revision: "new"})
		if len(main.Projects) != 1 {
			t.Fatalf("Projects = %+v, want 1（foo 与 foo/ 视为同一 path）", main.Projects)
		}
		if main.Projects[0].Revision != "new" {
			t.Errorf("Revision = %q, want new", main.Projects[0].Revision)
		}
	})

	t.Run("ToXML writes normalized path", func(t *testing.T) {
		m := &Manifest{
			Remotes:     []Remote{{Name: "origin", Fetch: "https://example.com/"}},
			Default:     Default{Remote: "origin", Revision: "main"},
			Projects:    []Project{{Name: "a", Path: "foo/"}},
			CustomAttrs: map[string]string{},
		}
		out, err := m.ToXML()
		if err != nil {
			t.Fatalf("ToXML: %v", err)
		}
		if strings.Contains(out, `path="foo/"`) || !strings.Contains(out, `path="foo"`) {
			t.Fatalf("ToXML 应输出归一化 path=\"foo\":\n%s", out)
		}
	})
}
