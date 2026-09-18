package manifest

import (
	"strings"
	"testing"
)

func TestParseBasicManifest(t *testing.T) {
	xml := `<?xml version="1.0" encoding="UTF-8"?>
<manifest>
  <remote name="origin" fetch="https://example.com/" />
  <default remote="origin" revision="main" />
  <project path="frameworks/base" name="platform/frameworks/base" />
  <project path="frameworks/native" name="platform/frameworks/native" revision="dev" />
</manifest>`

	parser := NewParser()
	m, err := parser.Parse([]byte(xml), nil)
	if err != nil {
		t.Fatalf("Parse() error: %v", err)
	}

	if len(m.Remotes) != 1 {
		t.Errorf("Remotes count = %d, want 1", len(m.Remotes))
	}
	if m.Remotes[0].Name != "origin" {
		t.Errorf("Remote[0] Name = %q, want origin", m.Remotes[0].Name)
	}
	if m.Default.Remote != "origin" {
		t.Errorf("Default.Remote = %q, want origin", m.Default.Remote)
	}
	if m.Default.Revision != "main" {
		t.Errorf("Default.Revision = %q, want main", m.Default.Revision)
	}
	if len(m.Projects) != 2 {
		t.Fatalf("Projects count = %d, want 2", len(m.Projects))
	}
	if m.Projects[0].Path != "frameworks/base" {
		t.Errorf("Project[0] Path = %q, want frameworks/base", m.Projects[0].Path)
	}
	if m.Projects[0].Remote != "origin" {
		t.Errorf("Project[0] Remote = %q, want origin (from default)", m.Projects[0].Remote)
	}
	if m.Projects[0].Revision != "main" {
		t.Errorf("Project[0] Revision = %q, want main (from default)", m.Projects[0].Revision)
	}
	if m.Projects[1].Revision != "dev" {
		t.Errorf("Project[1] Revision = %q, want dev", m.Projects[1].Revision)
	}
}

func TestParseDefaultRemoteFromSingleRemote(t *testing.T) {
	xml := `<?xml version="1.0" encoding="UTF-8"?>
<manifest>
  <remote name="aosp" fetch="https://android.googlesource.com/" revision="main" />
  <project path="build" name="platform/build" />
</manifest>`

	parser := NewParser()
	m, err := parser.Parse([]byte(xml), nil)
	if err != nil {
		t.Fatalf("Parse() error: %v", err)
	}

	// With only one remote and no default, it should use that remote
	if m.Default.Remote != "aosp" {
		t.Errorf("Default.Remote = %q, want aosp (auto-assigned)", m.Default.Remote)
	}
	if m.Default.Revision != "main" {
		t.Errorf("Default.Revision = %q, want main (from remote)", m.Default.Revision)
	}
}

func TestParseProjectPathDefaultsToName(t *testing.T) {
	xml := `<?xml version="1.0" encoding="UTF-8"?>
<manifest>
  <remote name="origin" fetch="https://example.com/" />
  <default remote="origin" revision="main" />
  <project name="platform/build" />
</manifest>`

	parser := NewParser()
	m, err := parser.Parse([]byte(xml), nil)
	if err != nil {
		t.Fatalf("Parse() error: %v", err)
	}

	if len(m.Projects) != 1 {
		t.Fatalf("Projects count = %d, want 1", len(m.Projects))
	}
	if m.Projects[0].Path != "platform/build" {
		t.Errorf("Path should default to name, got %q", m.Projects[0].Path)
	}
}

func TestParseGroupFiltering(t *testing.T) {
	xml := `<?xml version="1.0" encoding="UTF-8"?>
<manifest>
  <remote name="origin" fetch="https://example.com/" />
  <default remote="origin" revision="main" />
  <project path="a" name="a" groups="linux" />
  <project path="b" name="b" groups="arm" />
  <project path="c" name="c" groups="linux,arm" />
  <project path="d" name="d" />
</manifest>`

	tests := []struct {
		name      string
		groups    []string
		wantCount int
	}{
		{"no groups (all)", nil, 4},
		{"all", []string{"all"}, 4},
		{"linux only", []string{"linux"}, 2},
		{"arm only", []string{"arm"}, 2},
		{"linux and arm", []string{"linux", "arm"}, 3},
		// 上游语义：显式 groups 不剥夺 default 归属（仅 notdefault 才会），
		// 故 4 个项目全部隐式归属 default
		{"default group", []string{"default"}, 4},
		{"nonexistent group", []string{"nonexistent"}, 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			parser := NewParser()
			m, err := parser.Parse([]byte(xml), tt.groups)
			if err != nil {
				t.Fatalf("Parse() error: %v", err)
			}
			if len(m.Projects) != tt.wantCount {
				t.Errorf("Projects count = %d, want %d (groups: %v)", len(m.Projects), tt.wantCount, tt.groups)
			}
		})
	}
}

// TestParseRemoveProjects 验证 remove-project 按上游语义即时执行：
// 项目 a 被移除、b 保留；已生效的 remove 从 RemoveProjects 字段清除
// （快照无需再输出）；remove 之后定义的同名项目不受影响。
func TestParseRemoveProjects(t *testing.T) {
	xml := `<?xml version="1.0" encoding="UTF-8"?>
<manifest>
  <remote name="origin" fetch="https://example.com/" />
  <default remote="origin" revision="main" />
  <project path="a" name="a" />
  <project path="b" name="b" />
  <remove-project name="a" />
</manifest>`

	parser := NewParser()
	m, err := parser.Parse([]byte(xml), nil)
	if err != nil {
		t.Fatalf("Parse() error: %v", err)
	}

	if len(m.Projects) != 1 || m.Projects[0].Name != "b" {
		t.Fatalf("Projects = %v, want [b]（a 应被 remove-project 移除）", projectNames(m))
	}
	if len(m.RemoveProjects) != 0 {
		t.Errorf("RemoveProjects count = %d, want 0（已生效的 remove 已清除）", len(m.RemoveProjects))
	}
}

// TestParseRemoveProjectsOrdering 验证 remove-project 的文档顺序语义：
// remove 仅移除此前已定义的项目，其后同名重定义不受影响；
// name+path 双指定时精确匹配单个项目；optional 悬空 remove 不报错。
func TestParseRemoveProjectsOrdering(t *testing.T) {
	t.Run("remove before redefine keeps new project", func(t *testing.T) {
		xml := `<?xml version="1.0" encoding="UTF-8"?>
<manifest>
  <remote name="origin" fetch="https://example.com/" />
  <default remote="origin" revision="main" />
  <project path="a" name="a" />
  <remove-project name="a" />
  <project path="a" name="a" revision="refs/heads/new" />
</manifest>`
		parser := NewParser()
		m, err := parser.Parse([]byte(xml), nil)
		if err != nil {
			t.Fatalf("Parse() error: %v", err)
		}
		if len(m.Projects) != 1 {
			t.Fatalf("Projects = %v, want 1（重定义的 a 保留）", projectNames(m))
		}
		if m.Projects[0].Revision != "refs/heads/new" {
			t.Errorf("重定义项目 Revision = %q, want refs/heads/new", m.Projects[0].Revision)
		}
	})

	t.Run("path match removes single project", func(t *testing.T) {
		xml := `<?xml version="1.0" encoding="UTF-8"?>
<manifest>
  <remote name="origin" fetch="https://example.com/" />
  <default remote="origin" revision="main" />
  <project path="a" name="a" />
  <project path="a2" name="a" />
  <remove-project name="a" path="a2" />
</manifest>`
		parser := NewParser()
		m, err := parser.Parse([]byte(xml), nil)
		if err != nil {
			t.Fatalf("Parse() error: %v", err)
		}
		if len(m.Projects) != 1 || m.Projects[0].Path != "a" {
			t.Fatalf("Projects = %v, want 仅 path=a 的项目（name+path 精确匹配）", projectNames(m))
		}
	})

	t.Run("optional dangling remove is ignored", func(t *testing.T) {
		xml := `<?xml version="1.0" encoding="UTF-8"?>
<manifest>
  <remote name="origin" fetch="https://example.com/" />
  <default remote="origin" revision="main" />
  <project path="a" name="a" />
  <remove-project name="missing" optional="true" />
</manifest>`
		parser := NewParser()
		m, err := parser.Parse([]byte(xml), nil)
		if err != nil {
			t.Fatalf("optional 悬空 remove 不应报错: %v", err)
		}
		if len(m.Projects) != 1 {
			t.Fatalf("Projects = %v, want [a]", projectNames(m))
		}
	})

	t.Run("non-optional dangling remove fails", func(t *testing.T) {
		xml := `<?xml version="1.0" encoding="UTF-8"?>
<manifest>
  <remote name="origin" fetch="https://example.com/" />
  <default remote="origin" revision="main" />
  <project path="a" name="a" />
  <remove-project name="missing" />
</manifest>`
		parser := NewParser()
		if _, err := parser.Parse([]byte(xml), nil); err == nil {
			t.Fatal("非 optional 悬空 remove 应报错（上游 fail-fast）")
		}
	})
}

func TestParseExtendProject(t *testing.T) {
	xml := `<?xml version="1.0" encoding="UTF-8"?>
<manifest>
  <remote name="origin" fetch="https://example.com/" />
  <default remote="origin" revision="main" />
  <project path="a" name="a" groups="old" />
  <extend-project name="a" groups="new" revision="dev" />
</manifest>`

	parser := NewParser()
	m, err := parser.Parse([]byte(xml), nil)
	if err != nil {
		t.Fatalf("Parse() error: %v", err)
	}

	if len(m.Projects) != 1 {
		t.Fatalf("Projects count = %d, want 1", len(m.Projects))
	}
	if m.Projects[0].Groups != "new" {
		t.Errorf("Project groups = %q, want new (extended)", m.Projects[0].Groups)
	}
	if m.Projects[0].Revision != "dev" {
		t.Errorf("Project revision = %q, want dev (extended)", m.Projects[0].Revision)
	}
}

func TestParseDeduplication(t *testing.T) {
	xml := `<?xml version="1.0" encoding="UTF-8"?>
<manifest>
  <remote name="origin" fetch="https://example.com/" />
  <default remote="origin" revision="main" />
  <project path="a" name="a" />
  <project path="a" name="a" />
  <project path="b" name="b" />
</manifest>`

	parser := NewParser()
	m, err := parser.Parse([]byte(xml), nil)
	if err != nil {
		t.Fatalf("Parse() error: %v", err)
	}

	// Duplicate project (same name+path) should be removed
	if len(m.Projects) != 2 {
		t.Errorf("Projects count = %d, want 2 (deduplicated)", len(m.Projects))
	}
}

func TestParseEmptyData(t *testing.T) {
	parser := NewParser()
	_, err := parser.Parse([]byte{}, nil)
	if err == nil {
		t.Error("Parse with empty data should return error")
	}
}

func TestParseInvalidXML(t *testing.T) {
	parser := NewParser()
	_, err := parser.Parse([]byte("not valid xml"), nil)
	if err == nil {
		t.Error("Parse with invalid XML should return error")
	}
}

func TestToXMLRoundTrip(t *testing.T) {
	original := &Manifest{
		Remotes: []Remote{
			{Name: "origin", Fetch: "https://example.com/", Review: "https://review.example.com"},
		},
		Default: Default{Remote: "origin", Revision: "main"},
		Projects: []Project{
			{Name: "platform/build", Path: "build", Revision: "main"},
		},
	}
	original.CustomAttrs = make(map[string]string)
	original.Default.CustomAttrs = make(map[string]string)
	for i := range original.Remotes {
		original.Remotes[i].CustomAttrs = make(map[string]string)
	}
	for i := range original.Projects {
		original.Projects[i].CustomAttrs = make(map[string]string)
	}

	xmlStr, err := original.ToXML()
	if err != nil {
		t.Fatalf("ToXML() error: %v", err)
	}

	if !strings.Contains(xmlStr, `<remote name="origin"`) {
		t.Errorf("XML should contain remote tag, got: %s", xmlStr)
	}
	if !strings.Contains(xmlStr, `<project name="platform/build"`) {
		t.Errorf("XML should contain project tag, got: %s", xmlStr)
	}

	// Parse back
	parser := NewParser()
	parsed, err := parser.Parse([]byte(xmlStr), nil)
	if err != nil {
		t.Fatalf("Parse(ToXML()) error: %v", err)
	}

	if len(parsed.Remotes) != 1 {
		t.Errorf("Round-trip remotes = %d, want 1", len(parsed.Remotes))
	}
	if len(parsed.Projects) != 1 {
		t.Errorf("Round-trip projects = %d, want 1", len(parsed.Projects))
	}
	if parsed.Projects[0].Name != "platform/build" {
		t.Errorf("Round-trip project name = %q, want platform/build", parsed.Projects[0].Name)
	}
}

func TestToJSON(t *testing.T) {
	m := &Manifest{
		Remotes: []Remote{
			{Name: "origin", Fetch: "https://example.com/"},
		},
		Default: Default{Remote: "origin", Revision: "main"},
		Projects: []Project{
			{Name: "build", Path: "build", Revision: "main"},
		},
	}

	jsonStr, err := m.ToJSON()
	if err != nil {
		t.Fatalf("ToJSON() error: %v", err)
	}

	if !strings.Contains(jsonStr, `"name": "origin"`) {
		t.Errorf("JSON should contain remote name, got: %s", jsonStr)
	}
	if !strings.Contains(jsonStr, `"name": "build"`) {
		t.Errorf("JSON should contain project name, got: %s", jsonStr)
	}
}

func TestEscapeXMLAttr(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"plain", "plain"},
		{"a&b", "a&amp;b"},
		{"a<b", "a&lt;b"},
		{"a>b", "a&gt;b"},
		{`a"b`, "a&quot;b"},
	}

	for _, tt := range tests {
		got := escapeXMLAttr(tt.input)
		if got != tt.want {
			t.Errorf("escapeXMLAttr(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestGetRemoteURL(t *testing.T) {
	m := &Manifest{
		Remotes: []Remote{
			{Name: "origin", Fetch: "https://example.com/"},
			{Name: "aosp", Fetch: "https://android.googlesource.com/"},
		},
	}

	url, err := m.GetRemoteURL("origin")
	if err != nil {
		t.Fatalf("GetRemoteURL(origin) error: %v", err)
	}
	if url != "https://example.com/" {
		t.Errorf("GetRemoteURL(origin) = %q, want https://example.com/", url)
	}

	_, err = m.GetRemoteURL("nonexistent")
	if err == nil {
		t.Error("GetRemoteURL with nonexistent remote should return error")
	}
}

func TestManifestError(t *testing.T) {
	err := &ManifestError{Op: "parse", Path: "/path/to/manifest.xml", Err: nil}
	msg := err.Error()
	if !strings.Contains(msg, "parse") {
		t.Errorf("Error should contain op name, got: %s", msg)
	}
	if !strings.Contains(msg, "/path/to/manifest.xml") {
		t.Errorf("Error should contain path, got: %s", msg)
	}
}

func TestSetSilentMode(t *testing.T) {
	SetSilentMode(true)
	if !globalSilentMode {
		t.Error("SetSilentMode(true) should set globalSilentMode to true")
	}
	SetSilentMode(false)
	if globalSilentMode {
		t.Error("SetSilentMode(false) should set globalSilentMode to false")
	}
}

func TestParserSetCacheEnabled(t *testing.T) {
	parser := NewParser()
	parser.SetCacheEnabled(false)
	if parser.cacheEnabled {
		t.Error("SetCacheEnabled(false) should disable cache")
	}
	parser.SetCacheEnabled(true)
	if !parser.cacheEnabled {
		t.Error("SetCacheEnabled(true) should enable cache")
	}
}

func TestContainsAll(t *testing.T) {
	if !containsAll([]string{"all"}) {
		t.Error("containsAll with 'all' should return true")
	}
	if !containsAll([]string{"linux", "all"}) {
		t.Error("containsAll with 'all' in list should return true")
	}
	if containsAll([]string{"linux"}) {
		t.Error("containsAll without 'all' should return false")
	}
	if containsAll([]string{}) {
		t.Error("containsAll with empty slice should return false")
	}
}

func TestGetCurrentBranch(t *testing.T) {
	m := &Manifest{Default: Default{Revision: "main"}}
	if m.GetCurrentBranch() != "main" {
		t.Errorf("GetCurrentBranch() = %q, want main", m.GetCurrentBranch())
	}

	m2 := &Manifest{}
	if m2.GetCurrentBranch() != "" {
		t.Errorf("GetCurrentBranch() with empty revision = %q, want empty", m2.GetCurrentBranch())
	}

	var nilManifest *Manifest
	if nilManifest.GetCurrentBranch() != "" {
		t.Error("GetCurrentBranch on nil manifest should return empty string")
	}
}

func TestSortedKeys(t *testing.T) {
	m := map[string]string{
		"zebra": "1",
		"apple": "2",
		"mango": "3",
	}

	keys := sortedKeys(m)
	if len(keys) != 3 {
		t.Fatalf("sortedKeys length = %d, want 3", len(keys))
	}
	if keys[0] != "apple" || keys[1] != "mango" || keys[2] != "zebra" {
		t.Errorf("sortedKeys order = %v, want [apple mango zebra]", keys)
	}
}
