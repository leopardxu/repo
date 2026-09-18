package manifest

import (
	"strings"
	"testing"
)

// TestToXML_EscapesAttrValues 验证 ToXML 对属性值做 XML 转义，
// 确保含 & < > 的 fetch URL / 项目名不会产出非法 manifest（High：原实现用 fmt.Sprintf 直接拼接未转义）。
func TestToXML_EscapesAttrValues(t *testing.T) {
	m := &Manifest{
		Remotes: []Remote{
			{Name: "origin", Fetch: "https://host/path?a=1&b=2"},
		},
		Default: Default{Remote: "origin", Revision: "main"},
		Projects: []Project{
			{Name: "p<name>", Path: "p/path", Revision: "main"},
		},
	}

	out, err := m.ToXML()
	if err != nil {
		t.Fatalf("ToXML failed: %v", err)
	}

	// & 必须转义为 &amp;，不能出现裸 & 后跟 b
	if strings.Contains(out, "a=1&b=2") {
		t.Errorf("fetch URL 的 & 未转义: %s", out)
	}
	if !strings.Contains(out, "a=1&amp;b=2") {
		t.Errorf("期望 fetch URL 含转义后的 &amp;，实际: %s", out)
	}

	// 项目名中的 < > 必须转义
	if strings.Contains(out, "p<name>") {
		t.Errorf("项目名 < > 未转义: %s", out)
	}
	if !strings.Contains(out, "p&lt;name&gt;") {
		t.Errorf("期望项目名转义为 p&lt;name&gt;，实际: %s", out)
	}
}

// TestToXML_RootCustomAttrs 验证根级 CustomAttrs（如 --platform）被序列化输出。
func TestToXML_RootCustomAttrs(t *testing.T) {
	m := &Manifest{
		CustomAttrs: map[string]string{"platform": "true"},
	}
	out, err := m.ToXML()
	if err != nil {
		t.Fatalf("ToXML failed: %v", err)
	}
	if !strings.Contains(out, `<manifest platform="true">`) {
		t.Errorf("期望根级 platform 属性被序列化，实际: %s", out)
	}
}
