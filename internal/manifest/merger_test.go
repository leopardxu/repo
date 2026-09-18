package manifest

import (
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
  <remote name="upstream" fetch="https://upstream.example.com/" />
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

// TestMergeLocalManifestRemoveThenExtend 回归测试：
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
