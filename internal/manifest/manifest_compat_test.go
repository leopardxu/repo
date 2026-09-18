package manifest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeManifestFile 在临时目录写入清单文件（测试辅助）。
func writeManifestFile(t *testing.T, dir, name, content string) {
	t.Helper()
	mustWrite(t, filepath.Join(dir, name), content)
}

// chdirTo 切换工作目录并返回恢复函数（processIncludes 依赖 cwd 查找 include）。
func chdirTo(t *testing.T, dir string) func() {
	t.Helper()
	prev, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	return func() { _ = os.Chdir(prev) }
}

// TestIncludeProjectsInheritDefault 回归测试：include 引入的项目缺省
// remote/revision 时继承外层清单的 default 元素（旧实现只补全主清单自身
// 项目，include 项目两者皆为空）。
func TestIncludeProjectsInheritDefault(t *testing.T) {
	dir := t.TempDir()

	defaultXML := `<?xml version="1.0" encoding="UTF-8"?>
<manifest>
  <remote name="origin" fetch="https://example.com/" />
  <default remote="origin" revision="main" />
  <include name="inc.xml" />
</manifest>
`
	writeManifestFile(t, dir, "default.xml", defaultXML)

	incXML := `<?xml version="1.0" encoding="UTF-8"?>
<manifest>
  <project name="inc-p" path="inc-p" />
</manifest>
`
	writeManifestFile(t, dir, "inc.xml", incXML)

	defer chdirTo(t, dir)()

	parser := NewParser()
	m, err := parser.ParseFromFile("default.xml", nil)
	if err != nil {
		t.Fatalf("ParseFromFile 失败: %v", err)
	}

	if len(m.Projects) != 1 {
		t.Fatalf("Projects = %v, want [inc-p]", projectNames(m))
	}
	p := m.Projects[0]
	if p.Remote != "origin" {
		t.Errorf("include 项目 Remote = %q, want origin（继承外层 default）", p.Remote)
	}
	if p.Revision != "main" {
		t.Errorf("include 项目 Revision = %q, want main（继承外层 default）", p.Revision)
	}
}

// TestIncludeProjectUsesOwnDefault 子清单自带 default 时项目优先使用子清单
// default（上游节点流语义：default 状态随文档顺序推进）。
func TestIncludeProjectUsesOwnDefault(t *testing.T) {
	dir := t.TempDir()

	defaultXML := `<?xml version="1.0" encoding="UTF-8"?>
<manifest>
  <remote name="origin" fetch="https://example.com/" />
  <remote name="other" fetch="https://other.example.com/" />
  <default remote="origin" revision="main" />
  <include name="inc.xml" />
</manifest>
`
	writeManifestFile(t, dir, "default.xml", defaultXML)

	incXML := `<?xml version="1.0" encoding="UTF-8"?>
<manifest>
  <default remote="other" revision="refs/heads/stable" />
  <project name="inc-p" path="inc-p" />
</manifest>
`
	writeManifestFile(t, dir, "inc.xml", incXML)

	defer chdirTo(t, dir)()

	parser := NewParser()
	m, err := parser.ParseFromFile("default.xml", nil)
	if err != nil {
		t.Fatalf("ParseFromFile 失败: %v", err)
	}

	if len(m.Projects) != 1 {
		t.Fatalf("Projects = %v, want [inc-p]", projectNames(m))
	}
	p := m.Projects[0]
	if p.Remote != "other" {
		t.Errorf("include 项目 Remote = %q, want other（子清单自身 default）", p.Remote)
	}
	if p.Revision != "refs/heads/stable" {
		t.Errorf("include 项目 Revision = %q, want refs/heads/stable（子清单自身 default）", p.Revision)
	}
}

// TestIncludeRemoveProjectTargetsOuterProject 上游 include 为节点流展平：
// 子清单的 remove-project 可移除外层清单先前定义的项目。
func TestIncludeRemoveProjectTargetsOuterProject(t *testing.T) {
	dir := t.TempDir()

	defaultXML := `<?xml version="1.0" encoding="UTF-8"?>
<manifest>
  <remote name="origin" fetch="https://example.com/" />
  <default remote="origin" revision="main" />
  <project name="a" path="a" />
  <include name="inc.xml" />
</manifest>
`
	writeManifestFile(t, dir, "default.xml", defaultXML)

	incXML := `<?xml version="1.0" encoding="UTF-8"?>
<manifest>
  <remove-project name="a" />
  <project name="b" path="b" />
</manifest>
`
	writeManifestFile(t, dir, "inc.xml", incXML)

	defer chdirTo(t, dir)()

	parser := NewParser()
	m, err := parser.ParseFromFile("default.xml", nil)
	if err != nil {
		t.Fatalf("ParseFromFile 失败: %v", err)
	}

	if len(m.Projects) != 1 || m.Projects[0].Name != "b" {
		t.Fatalf("Projects = %v, want [b]（外层 a 被 include 内 remove-project 移除）", projectNames(m))
	}
}

// TestNestedProjectFlattening 嵌套 <project>（上游 Git submodule 表达法）展平：
// 子项目 name = 父name/子name；显式 path 相对父路径展开；缺省 path 为
// 父path/父name/子name（上游 GetSubprojectPaths 规则）。
func TestNestedProjectFlattening(t *testing.T) {
	xml := `<?xml version="1.0" encoding="UTF-8"?>
<manifest>
  <remote name="origin" fetch="https://example.com/" />
  <default remote="origin" revision="main" />
  <project name="parent" path="plat/parent">
    <project name="child1" path="sub" />
    <project name="child2" />
  </project>
  <project name="top" path="top" />
</manifest>`

	parser := NewParser()
	m, err := parser.Parse([]byte(xml), nil)
	if err != nil {
		t.Fatalf("Parse() error: %v", err)
	}

	want := []struct{ name, path string }{
		{"parent", "plat/parent"},
		{"parent/child1", "plat/parent/sub"},           // 显式 path 相对父展开
		{"parent/child2", "plat/parent/parent/child2"}, // 缺省 path：父path/父name/子name
		{"top", "top"},
	}
	if len(m.Projects) != len(want) {
		t.Fatalf("Projects = %v, want %d 个（嵌套子项目应展平）", projectNames(m), len(want))
	}
	for i, w := range want {
		if m.Projects[i].Name != w.name || m.Projects[i].Path != w.path {
			t.Errorf("Projects[%d] = {name:%q path:%q}, want {name:%q path:%q}",
				i, m.Projects[i].Name, m.Projects[i].Path, w.name, w.path)
		}
	}
	// 展平后嵌套字段清空
	for _, p := range m.Projects {
		if len(p.Projects) != 0 {
			t.Errorf("项目 %s 展平后仍含嵌套子项目", p.Name)
		}
	}
	// 子项目继承 default
	if m.Projects[1].Revision != "main" {
		t.Errorf("子项目 Revision = %q, want main（继承 default）", m.Projects[1].Revision)
	}
}

// TestDefaultDestBranchUpstreamPropagation default 的 dest-branch/upstream
// 传播到 Project 字段（旧实现不传播，恒为空）。
func TestDefaultDestBranchUpstreamPropagation(t *testing.T) {
	xml := `<?xml version="1.0" encoding="UTF-8"?>
<manifest>
  <remote name="origin" fetch="https://example.com/" />
  <default remote="origin" revision="main" dest-branch="main" upstream="master" />
  <project name="a" path="a" />
  <project name="b" path="b" dest-branch="release" upstream="up-rel" />
</manifest>`

	parser := NewParser()
	m, err := parser.Parse([]byte(xml), nil)
	if err != nil {
		t.Fatalf("Parse() error: %v", err)
	}

	if m.Projects[0].DestBranch != "main" {
		t.Errorf("项目 a DestBranch = %q, want main（继承 default）", m.Projects[0].DestBranch)
	}
	if m.Projects[0].Upstream != "master" {
		t.Errorf("项目 a Upstream = %q, want master（继承 default）", m.Projects[0].Upstream)
	}
	// 项目自身声明优先于 default
	if m.Projects[1].DestBranch != "release" {
		t.Errorf("项目 b DestBranch = %q, want release（自身声明优先）", m.Projects[1].DestBranch)
	}
	if m.Projects[1].Upstream != "up-rel" {
		t.Errorf("项目 b Upstream = %q, want up-rel（自身声明优先）", m.Projects[1].Upstream)
	}
}

// TestNoticeParsingAndToXML <notice> 元素解析与 ToXML 输出。
func TestNoticeParsingAndToXML(t *testing.T) {
	xml := `<?xml version="1.0" encoding="UTF-8"?>
<manifest>
  <remote name="origin" fetch="https://example.com/" />
  <default remote="origin" revision="main" />
  <notice>Your organization's policy</notice>
  <project name="a" path="a" />
</manifest>`

	parser := NewParser()
	m, err := parser.Parse([]byte(xml), nil)
	if err != nil {
		t.Fatalf("Parse() error: %v", err)
	}
	if m.Notice != "Your organization's policy" {
		t.Fatalf("Notice = %q, want 提示文本", m.Notice)
	}

	out, err := m.ToXML()
	if err != nil {
		t.Fatalf("ToXML() error: %v", err)
	}
	if !strings.Contains(out, "Your organization's policy") {
		t.Errorf("ToXML 输出缺少 notice 内容:\n%s", out)
	}

	// 往返：输出可再次解析且 notice 保留
	m2, err := parser.Parse([]byte(out), nil)
	if err != nil {
		t.Fatalf("ToXML 输出重新解析失败: %v\n%s", err, out)
	}
	if m2.Notice != m.Notice {
		t.Errorf("往返解析 Notice = %q, want %q", m2.Notice, m.Notice)
	}
}

// TestIncludeNoticeMerged 子清单 notice 合并进外层（上游 repo sync 展示
// 所有清单的 notice，去重）。
func TestIncludeNoticeMerged(t *testing.T) {
	dir := t.TempDir()

	defaultXML := `<?xml version="1.0" encoding="UTF-8"?>
<manifest>
  <remote name="origin" fetch="https://example.com/" />
  <default remote="origin" revision="main" />
  <notice>main notice</notice>
  <include name="inc.xml" />
  <include name="inc2.xml" />
</manifest>
`
	writeManifestFile(t, dir, "default.xml", defaultXML)

	writeManifestFile(t, dir, "inc.xml", `<?xml version="1.0" encoding="UTF-8"?>
<manifest>
  <notice>inc notice</notice>
</manifest>
`)

	// 与主清单相同的 notice：合并时应去重
	writeManifestFile(t, dir, "inc2.xml", `<?xml version="1.0" encoding="UTF-8"?>
<manifest>
  <notice>main notice</notice>
</manifest>
`)

	defer chdirTo(t, dir)()

	parser := NewParser()
	m, err := parser.ParseFromFile("default.xml", nil)
	if err != nil {
		t.Fatalf("ParseFromFile 失败: %v", err)
	}
	if !strings.Contains(m.Notice, "main notice") || !strings.Contains(m.Notice, "inc notice") {
		t.Fatalf("Notice = %q, want 含主清单与子清单 notice", m.Notice)
	}
	if got := strings.Count(m.Notice, "main notice"); got != 1 {
		t.Errorf("重复 notice 未去重: %q", m.Notice)
	}
}

// TestSplitGroupsMultiSeparator 组分隔符对齐上游 _ParseList（re.split(r"[,\s]+")）：
// 逗号与任意空白混合分隔；分号不是分隔符。
func TestSplitGroupsMultiSeparator(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []string
	}{
		{"comma separated", "a,b,c", []string{"a", "b", "c"}},
		{"space separated", "a b c", []string{"a", "b", "c"}},
		{"tab separated", "a\tb", []string{"a", "b"}},
		{"mixed separators", "a, b\tc,  d", []string{"a", "b", "c", "d"}},
		{"dedup preserves order", "b,a,b", []string{"b", "a"}},
		{"empty", "", nil},
		{"only separators", " ,\t, ", nil},
		{"semicolon is part of group name", "a;b", []string{"a;b"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := SplitGroups(tt.in)
			if !equalStrings(got, tt.want) {
				t.Errorf("SplitGroups(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

// TestProjectSyncTagsInheritance 项目级 sync-tags 三态语义对齐上游：
// 显式声明优先；缺省继承 default；default 亦未声明时最终值 true。
// OptBool 取值对齐 XmlBool（true/1/yes/false/0/no 大小写不敏感，非法值忽略）。
func TestProjectSyncTagsInheritance(t *testing.T) {
	xml := `<?xml version="1.0" encoding="UTF-8"?>
<manifest>
  <remote name="origin" fetch="https://example.com/" />
  <default remote="origin" revision="main" sync-tags="false" />
  <project name="a" path="a" />
  <project name="b" path="b" sync-tags="true" />
  <project name="c" path="c" sync-tags="yes" />
  <project name="d" path="d" sync-tags="invalid-value" />
</manifest>`

	parser := NewParser()
	m, err := parser.Parse([]byte(xml), nil)
	if err != nil {
		t.Fatalf("Parse() error: %v", err)
	}

	byName := make(map[string]*Project, len(m.Projects))
	for i := range m.Projects {
		byName[m.Projects[i].Name] = &m.Projects[i]
	}

	// a 未声明 -> 继承 default 的 false
	if got := byName["a"].SyncTags.Get(true); got {
		t.Errorf("项目 a SyncTags = true, want false（继承 default）")
	}
	// b/c 显式 true（yes 等价形式）
	if got := byName["b"].SyncTags.Get(true); !got {
		t.Errorf("项目 b SyncTags = false, want true（显式声明）")
	}
	if got := byName["c"].SyncTags.Get(true); !got {
		t.Errorf("项目 c SyncTags = false, want true（yes 等价 true）")
	}
	// d 非法值按未声明处理 -> 继承 default
	if got := byName["d"].SyncTags.Get(true); got {
		t.Errorf("项目 d SyncTags = true, want false（非法值忽略后继承 default）")
	}

	// ToXML：上游 ProjectToXml 仅最终值为 false 时输出 sync-tags="false"
	out, err := m.ToXML()
	if err != nil {
		t.Fatalf("ToXML() error: %v", err)
	}
	if !strings.Contains(out, `sync-tags="false"`) {
		t.Errorf("ToXML 应为 false 项目/default 输出 sync-tags=\"false\":\n%s", out)
	}
	// b 为 true 不输出该属性
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), `<project name="b"`) && strings.Contains(line, "sync-tags") {
			t.Errorf("true 项目不应输出 sync-tags 属性:\n%s", line)
		}
	}
	// default sync-tags="false" 输出
	if !strings.Contains(out, `<default remote="origin" revision="main" sync-tags="false"`) {
		t.Errorf("ToXML 应输出 default sync-tags=\"false\":\n%s", out)
	}
}

// TestProjectSyncTagsDefaultOmitted default 未声明 sync-tags 时：项目最终值
// 按上游缺省 true；ToXML 仅显式 false 时输出。
func TestProjectSyncTagsDefaultOmitted(t *testing.T) {
	xml := `<?xml version="1.0" encoding="UTF-8"?>
<manifest>
  <remote name="origin" fetch="https://example.com/" />
  <default remote="origin" revision="main" />
  <project name="a" path="a" />
  <project name="b" path="b" sync-tags="false" />
</manifest>`

	parser := NewParser()
	m, err := parser.Parse([]byte(xml), nil)
	if err != nil {
		t.Fatalf("Parse() error: %v", err)
	}

	byName := make(map[string]*Project, len(m.Projects))
	for i := range m.Projects {
		byName[m.Projects[i].Name] = &m.Projects[i]
	}
	// default 未声明 -> 上游缺省 true
	if got := byName["a"].SyncTags.Get(true); !got {
		t.Errorf("项目 a SyncTags = false, want true（上游缺省）")
	}
	if got := byName["b"].SyncTags.Get(true); got {
		t.Errorf("项目 b SyncTags = true, want false（显式声明）")
	}

	out, err := m.ToXML()
	if err != nil {
		t.Fatalf("ToXML() error: %v", err)
	}
	if !strings.Contains(out, `sync-tags="false"`) {
		t.Errorf("项目 b 显式 false 应输出 sync-tags=\"false\":\n%s", out)
	}
	if strings.Count(out, `sync-tags="false"`) != 1 {
		t.Errorf("缺省 true 项目/default 不应输出 sync-tags:\n%s", out)
	}
}

// TestToXMLRoundTripCopyfiles 回归测试：含 copyfile/linkfile 的项目
// 开标签必须以 ">\n" 闭合后再写子元素（缺失则输出非法 XML，
// 再 Parse 必失败）。往返后子元素数量保真。
func TestToXMLRoundTripCopyfiles(t *testing.T) {
	xml := `<?xml version="1.0" encoding="UTF-8"?>
<manifest>
  <remote name="origin" fetch="https://example.com/" />
  <default remote="origin" revision="main" />
  <project name="a" path="a">
    <copyfile src="build.gradle" dest="root-build.gradle" />
    <copyfile src="settings.gradle" dest="root-settings.gradle" />
    <linkfile src="Makefile" dest="Makefile" />
  </project>
  <project name="b" path="b" />
</manifest>`

	parser := NewParser()
	m, err := parser.Parse([]byte(xml), nil)
	if err != nil {
		t.Fatalf("Parse() error: %v", err)
	}
	if len(m.Projects[0].Copyfiles) != 2 || len(m.Projects[0].Linkfiles) != 1 {
		t.Fatalf("解析后 Copyfiles=%d Linkfiles=%d, want 2/1", len(m.Projects[0].Copyfiles), len(m.Projects[0].Linkfiles))
	}

	out, err := m.ToXML()
	if err != nil {
		t.Fatalf("ToXML() error: %v", err)
	}
	// 开标签闭合后才能写子元素
	if !strings.Contains(out, ">\n    <copyfile") {
		t.Errorf("输出应在开标签闭合后写 copyfile 子元素:\n%s", out)
	}
	if !strings.Contains(out, "</project>") {
		t.Errorf("含子元素的项目应有 </project> 闭合:\n%s", out)
	}

	// 往返：输出必须可再次解析且子元素保真
	m2, err := parser.Parse([]byte(out), nil)
	if err != nil {
		t.Fatalf("ToXML 输出重新解析失败: %v\n%s", err, out)
	}
	if len(m2.Projects) != 2 {
		t.Fatalf("往返项目数 = %d, want 2", len(m2.Projects))
	}
	if len(m2.Projects[0].Copyfiles) != 2 {
		t.Errorf("往返 Copyfiles = %d, want 2", len(m2.Projects[0].Copyfiles))
	}
	if len(m2.Projects[0].Linkfiles) != 1 {
		t.Errorf("往返 Linkfiles = %d, want 1", len(m2.Projects[0].Linkfiles))
	}
	if m2.Projects[0].Copyfiles[0].Src != "build.gradle" ||
		m2.Projects[0].Copyfiles[0].Dest != "root-build.gradle" {
		t.Errorf("往返 copyfile 内容失真: %+v", m2.Projects[0].Copyfiles[0])
	}
}
