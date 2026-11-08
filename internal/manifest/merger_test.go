package manifest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMergerMergeBasic(t *testing.T) {
	parser := NewParser()

	m1XML := `<?xml version="1.0" encoding="UTF-8"?>
<manifest>
  <remote name="origin" fetch="https://example.com/" />
  <default remote="origin" revision="main" />
  <project path="a" name="a" />
</manifest>`

	m2XML := `<?xml version="1.0" encoding="UTF-8"?>
<manifest>
  <remote name="origin" fetch="https://example.com/" />
  <default remote="origin" revision="main" />
  <project path="b" name="b" />
</manifest>`

	m1, err := parser.Parse([]byte(m1XML), nil)
	if err != nil {
		t.Fatalf("Parse m1: %v", err)
	}
	m2, err := parser.Parse([]byte(m2XML), nil)
	if err != nil {
		t.Fatalf("Parse m2: %v", err)
	}

	merger := NewMerger(parser, ".")
	merged, err := merger.Merge([]*Manifest{m1, m2})
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}

	if len(merged.Projects) != 2 {
		t.Errorf("Merged projects = %d, want 2", len(merged.Projects))
	}
	if merged.Projects[0].Name != "a" {
		t.Errorf("Project[0] Name = %q, want a", merged.Projects[0].Name)
	}
	if merged.Projects[1].Name != "b" {
		t.Errorf("Project[1] Name = %q, want b", merged.Projects[1].Name)
	}
}

func TestMergerMergeWithRemoveProject(t *testing.T) {
	parser := NewParser()

	m1XML := `<?xml version="1.0" encoding="UTF-8"?>
<manifest>
  <remote name="origin" fetch="https://example.com/" />
  <default remote="origin" revision="main" />
  <project path="a" name="a" />
  <project path="b" name="b" />
</manifest>`

	m2XML := `<?xml version="1.0" encoding="UTF-8"?>
<manifest>
  <remove-project name="a" />
</manifest>`

	m1, err := parser.Parse([]byte(m1XML), nil)
	if err != nil {
		t.Fatalf("Parse m1: %v", err)
	}
	// m2 模拟待合并的增量清单（类 local manifest）：其中 remove-project
	// 的目标在主清单中，须以子清单上下文解析（悬空保留，由 Merge 执行）
	parser.includeDepth++
	m2, err := parser.Parse([]byte(m2XML), nil)
	parser.includeDepth--
	if err != nil {
		t.Fatalf("Parse m2: %v", err)
	}

	merger := NewMerger(parser, ".")
	merged, err := merger.Merge([]*Manifest{m1, m2})
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}

	// After remove-project, only "b" should remain
	if len(merged.Projects) != 1 {
		t.Errorf("Merged projects = %d, want 1 (after remove-project)", len(merged.Projects))
	}
	if len(merged.Projects) > 0 && merged.Projects[0].Name != "b" {
		t.Errorf("Remaining project Name = %q, want b", merged.Projects[0].Name)
	}
}

func TestMergerMergeRemoteDedup(t *testing.T) {
	parser := NewParser()

	m1XML := `<?xml version="1.0" encoding="UTF-8"?>
<manifest>
  <remote name="origin" fetch="https://example.com/" />
  <default remote="origin" revision="main" />
  <project path="a" name="a" />
</manifest>`

	m2XML := `<?xml version="1.0" encoding="UTF-8"?>
<manifest>
  <remote name="origin" fetch="https://example.com/" />
  <remote name="upstream" fetch="https://upstream.example.com/" revision="main" />
  <project path="b" name="b" remote="upstream" />
</manifest>`

	m1, err := parser.Parse([]byte(m1XML), nil)
	if err != nil {
		t.Fatalf("Parse m1: %v", err)
	}
	m2, err := parser.Parse([]byte(m2XML), nil)
	if err != nil {
		t.Fatalf("Parse m2: %v", err)
	}

	merger := NewMerger(parser, ".")
	merged, err := merger.Merge([]*Manifest{m1, m2})
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}

	// "origin" should be deduplicated, "upstream" should be added
	if len(merged.Remotes) != 2 {
		t.Errorf("Merged remotes = %d, want 2", len(merged.Remotes))
	}
}

func TestMergerMergeEmpty(t *testing.T) {
	parser := NewParser()
	merger := NewMerger(parser, ".")

	// Merge(nil) should return an error (no manifests to merge)
	_, err := merger.Merge(nil)
	if err == nil {
		t.Error("Merge(nil) should return error for empty input")
	}
}

func TestMergerMergeSingle(t *testing.T) {
	parser := NewParser()

	m1XML := `<?xml version="1.0" encoding="UTF-8"?>
<manifest>
  <remote name="origin" fetch="https://example.com/" />
  <default remote="origin" revision="main" />
  <project path="a" name="a" />
</manifest>`

	m1, err := parser.Parse([]byte(m1XML), nil)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	merger := NewMerger(parser, ".")
	merged, err := merger.Merge([]*Manifest{m1})
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}

	if len(merged.Projects) != 1 {
		t.Errorf("Merged projects = %d, want 1", len(merged.Projects))
	}
}

// TestMergerOptionalDanglingRemoveNotFilter 验证 dst 中悬挂的 optional
// remove-project 不得作为后续清单项目的过滤条件；非 optional 的 remove
// 仍应过滤匹配项目。
func TestMergerOptionalDanglingRemoveNotFilter(t *testing.T) {
	tests := []struct {
		name      string
		dstXML    string
		wantNames []string
	}{
		{
			name: "optional dangling remove does not filter",
			dstXML: `<manifest>
  <remote name="origin" fetch="https://example.com/" />
  <default remote="origin" revision="main" />
  <project name="a" path="a" />
  <remove-project name="b" optional="true" />
</manifest>`,
			wantNames: []string{"a", "b"},
		},
		{
			name: "non-optional dangling remove filters",
			dstXML: `<manifest>
  <remote name="origin" fetch="https://example.com/" />
  <default remote="origin" revision="main" />
  <project name="a" path="a" />
  <remove-project name="b" />
</manifest>`,
			wantNames: []string{"a"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			parser := NewParser()
			// 以子清单上下文解析：悬空的非 optional remove 在此保留，
			// 由合并器决定是否作为后续清单的过滤条件。
			parser.includeDepth++
			dst, err := parser.Parse([]byte(tt.dstXML), nil)
			parser.includeDepth--
			if err != nil {
				t.Fatalf("Parse dst: %v", err)
			}
			src, err := parser.Parse([]byte(`<manifest>
  <remote name="origin" fetch="https://example.com/" />
  <default remote="origin" revision="main" />
  <project name="b" path="b" />
</manifest>`), nil)
			if err != nil {
				t.Fatalf("Parse src: %v", err)
			}

			merger := NewMerger(parser, ".")
			merged, err := merger.Merge([]*Manifest{dst, src})
			if err != nil {
				t.Fatalf("Merge: %v", err)
			}
			if got := strings.Join(projectNames(merged), ","); got != strings.Join(tt.wantNames, ",") {
				t.Fatalf("Projects = %v, want %v", projectNames(merged), tt.wantNames)
			}
		})
	}
}

// local manifest 先 remove-project 再 extend-project 时，
// 旧实现使用删除前构建的下标映射，会修改错误的项目或触发越界 panic。
func TestMergeLocalManifestRemoveThenExtend(t *testing.T) {
	parser := NewParser()

	mainXML := `<?xml version="1.0" encoding="UTF-8"?>
<manifest>
  <remote name="origin" fetch="https://example.com/" />
  <default remote="origin" revision="main" />
  <project path="a" name="a" />
  <project path="b" name="b" />
  <project path="c" name="c" />
</manifest>`

	// 移除位于 extend 目标（c）之前的项目 b，再扩展 c 的 revision
	localXML := `<?xml version="1.0" encoding="UTF-8"?>
<manifest>
  <remove-project name="b" />
  <extend-project name="c" revision="refs/tags/v2" />
</manifest>`

	main, err := parser.Parse([]byte(mainXML), nil)
	if err != nil {
		t.Fatalf("Parse main: %v", err)
	}
	// local manifest 以子清单上下文解析：remove-project 的目标在主清单，
	// 独立解析悬空保留，由 mergeLocalManifest 在主清单上执行
	parser.includeDepth++
	local, err := parser.Parse([]byte(localXML), nil)
	parser.includeDepth--
	if err != nil {
		t.Fatalf("Parse local: %v", err)
	}

	p := NewParser()
	if err := p.mergeLocalManifest(main, local); err != nil {
		t.Fatalf("mergeLocalManifest: %v", err)
	}

	if len(main.Projects) != 2 {
		t.Fatalf("Projects = %d, want 2 (b removed)", len(main.Projects))
	}
	var projC *Project
	for i := range main.Projects {
		if main.Projects[i].Name == "c" {
			projC = &main.Projects[i]
		}
	}
	if projC == nil {
		t.Fatal("project c missing after merge")
	}
	if projC.Revision != "refs/tags/v2" {
		t.Errorf("project c Revision = %q, want refs/tags/v2", projC.Revision)
	}
	// a 不应受 extend 影响
	if main.Projects[0].Name != "a" || main.Projects[0].Revision == "refs/tags/v2" {
		t.Errorf("project a unexpectedly modified: %+v", main.Projects[0])
	}
}

// TestMergerMergeFillsOnlyEmptyFields 验证合并只补空/追加，不覆盖目标清单
// 已有的非空字段。
func TestMergerMergeFillsOnlyEmptyFields(t *testing.T) {
	parser := NewParser()

	dst, err := parser.Parse([]byte(`<manifest>
  <remote name="origin" fetch="https://example.com/" />
  <default remote="origin" revision="main" />
  <project name="a" path="a" revision="main" groups="g1" />
</manifest>`), nil)
	if err != nil {
		t.Fatalf("Parse dst: %v", err)
	}
	src, err := parser.Parse([]byte(`<manifest>
  <project name="a" path="a" revision="dev" groups="g2" upstream="up" />
</manifest>`), nil)
	if err != nil {
		t.Fatalf("Parse src: %v", err)
	}

	merger := NewMerger(parser, ".")
	merged, err := merger.Merge([]*Manifest{dst, src})
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if len(merged.Projects) != 1 {
		t.Fatalf("Projects = %+v, want 1", merged.Projects)
	}
	got := merged.Projects[0]
	if got.Revision != "main" {
		t.Errorf("Revision = %q, want main（不得被覆盖）", got.Revision)
	}
	if got.Groups != "g1" {
		t.Errorf("Groups = %q, want g1（不得被覆盖）", got.Groups)
	}
	if got.Upstream != "up" {
		t.Errorf("Upstream = %q, want up（空字段应补齐）", got.Upstream)
	}
}

// TestMergerProcessIncludesDanglingRemove 验证 include 内的 remove-project
// 指向外层项目时不会在单独解析 include 时硬报错，而是在合并时生效。
func TestMergerProcessIncludesDanglingRemove(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "inc.xml"), []byte(`<manifest>
  <remove-project name="a" />
</manifest>`), 0644); err != nil {
		t.Fatalf("write include: %v", err)
	}

	parser := NewParser()
	main := &Manifest{
		Projects: []Project{{Name: "a", Path: "a"}},
		Includes: []Include{{Name: "inc.xml"}},
	}
	merger := NewMerger(parser, dir)
	merged, err := merger.ProcessIncludes(main, nil)
	if err != nil {
		t.Fatalf("ProcessIncludes should not fail on dangling include remove: %v", err)
	}
	if len(merged.Projects) != 0 {
		t.Fatalf("Projects = %+v, want empty（include remove 移除外层 a）", merged.Projects)
	}
}

// TestMergerThreeManifestsNoMisSkip 回归：已生效的 remove-project 不应留在
// dst.RemoveProjects，否则合并第三个清单时会把同名项目误判为已移除而跳过。
func TestMergerThreeManifestsNoMisSkip(t *testing.T) {
	parser := NewParser()

	m1, err := parser.Parse([]byte(`<manifest>
  <remote name="origin" fetch="https://example.com/" />
  <default remote="origin" revision="main" />
  <project name="a" path="a" />
  <project name="b" path="b" />
</manifest>`), nil)
	if err != nil {
		t.Fatalf("Parse m1: %v", err)
	}
	parser.includeDepth++
	m2, err := parser.Parse([]byte(`<manifest>
  <remove-project name="a" />
</manifest>`), nil)
	parser.includeDepth--
	if err != nil {
		t.Fatalf("Parse m2: %v", err)
	}
	m3, err := parser.Parse([]byte(`<manifest>
  <project name="a" path="a2" revision="dev" />
</manifest>`), nil)
	if err != nil {
		t.Fatalf("Parse m3: %v", err)
	}

	merger := NewMerger(parser, ".")
	merged, err := merger.Merge([]*Manifest{m1, m2, m3})
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if got := projectNames(merged); len(got) != 2 {
		t.Fatalf("Projects = %v, want [b a]（第三个清单的 a 不得被误跳过）", got)
	}
	for _, p := range merged.Projects {
		if p.Name == "a" && p.Path != "a2" {
			t.Errorf("project a Path = %q, want a2（来自第三个清单）", p.Path)
		}
	}
	// 已生效的 remove 不应残留，ToXML 输出必须能再次解析（无悬空 remove）。
	if len(merged.RemoveProjects) != 0 {
		t.Fatalf("RemoveProjects = %+v, want empty（已生效的 remove 应被消费）", merged.RemoveProjects)
	}
	out, err := merged.ToXML()
	if err != nil {
		t.Fatalf("ToXML: %v", err)
	}
	reparsed, err := parser.Parse([]byte(out), nil)
	if err != nil {
		t.Fatalf("re-parse merged XML: %v\n%s", err, out)
	}
	if len(reparsed.Projects) != 2 {
		t.Fatalf("reparsed Projects = %v, want 2", projectNames(reparsed))
	}
}
