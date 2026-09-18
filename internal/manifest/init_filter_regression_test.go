package manifest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestParseWithIncludesPreservesAllProjects 回归测试 init 空 manifest 问题：
// 源 default.xml 自身无 <project>，项目全在 <include> 文件里，且带显式 groups="cix"
// （非 default）。解析时若传组过滤 ["default"]，会把 cix 组项目全部删除，导致
// init 写出的 manifest.xml 为空。修复后 init 解析时不传组过滤（nil），应保留全部项目；
// 组过滤由 sync 在解析后的完整清单上按 -g 执行。
func TestParseWithIncludesPreservesAllProjects(t *testing.T) {
	dir := t.TempDir()

	// 源 default.xml：仅 default + remote + 4 个 include（无自身 project）
	defaultXML := `<?xml version="1.0" encoding="UTF-8"?>
<manifest>
  <remote name="cix" fetch=".." review="ssh://git@gitmirror.example.com/" />
  <default revision="master" remote="cix" sync-j="8" />
  <include name="cix.xml" />
  <include name="plfm.xml" />
</manifest>
`
	mustWrite(t, filepath.Join(dir, "default.xml"), defaultXML)

	// include 文件：项目带显式 groups="cix"（非 default）
	cixXML := `<?xml version="1.0" encoding="UTF-8"?>
<manifest>
  <project name="cix/a" path="cix/a" groups="cix" />
  <project name="cix/b" path="cix/b" groups="cix" />
</manifest>
`
	mustWrite(t, filepath.Join(dir, "cix.xml"), cixXML)

	// include 文件：项目无 groups（隐式 default）
	plfmXML := `<?xml version="1.0" encoding="UTF-8"?>
<manifest>
  <project name="plfm/x" path="plfm/x" />
</manifest>
`
	mustWrite(t, filepath.Join(dir, "plfm.xml"), plfmXML)

	// 切到临时目录解析（processIncludes 在 .repo/manifests 与 cwd 下查找 include）
	prev, _ := os.Getwd()
	defer os.Chdir(prev)
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	parser := NewParser()
	// 修复要点：init 解析时不传组过滤（nil），保留全部 include 项目
	m, err := parser.ParseFromFile("default.xml", nil)
	if err != nil {
		t.Fatalf("ParseFromFile 失败: %v", err)
	}

	// 全部 3 个项目应被保留（include 已展开、未按组过滤）
	if len(m.Projects) != 3 {
		names := projectNames(m)
		t.Fatalf("解析后项目数 = %d %v，want 3（include 未过滤）", len(m.Projects), names)
	}

	// 进一步验证：组过滤应能在完整清单上正确区分
	// - ["default"]：上游语义下显式 groups 不剥夺 default 归属
	//   （仅 notdefault 才会），故全部 3 个项目均保留
	gotDefault := filterByGroups(t, m, []string{"default"})
	if len(gotDefault) != 3 {
		t.Errorf("过滤 [default] = %v, want 3 个项目（上游语义：显式组隐含 default）", projectNames(&Manifest{Projects: gotDefault}))
	}
	// - ["cix"]：保留 cix 组项目
	gotCix := filterByGroups(t, m, []string{"cix"})
	if len(gotCix) != 2 {
		t.Errorf("过滤 [cix] = %v, want 2 个 cix 项目", projectNames(&Manifest{Projects: gotCix}))
	}
	// - ["all"] 或 nil：全部保留
	if len(filterByGroups(t, m, []string{"all"})) != 3 {
		t.Errorf("过滤 [all] 应保留全部 3 个项目")
	}
	// - ["-cix"]：上游顺序求值下纯排除 filter 无正向命中，全不保留
	if gotExcl := filterByGroups(t, m, []string{"-cix"}); len(gotExcl) != 0 {
		t.Errorf("过滤 [-cix] = %v, want 0（上游：纯排除无正向命中为 false）", projectNames(&Manifest{Projects: gotExcl}))
	}
}

// TestParseWithGroupFilterDropsNonDefaultProjects 固化上游组过滤语义：
// 解析期组过滤按 MatchesGroupFilter 判定，cix 组项目在 ["other"] 过滤下
// 被剔除；显式 groups 不剥夺 default 归属（仅 notdefault 才会）。
func TestParseWithGroupFilterDropsNonDefaultProjects(t *testing.T) {
	m := &Manifest{
		Projects: []Project{
			{Name: "a", Path: "a", Groups: "cix"},
			{Name: "b", Path: "b"}, // 无组 -> 隐式 default
		},
	}
	// cix 项目在 [other] 下应被排除
	if (&m.Projects[0]).MatchesGroupFilter([]string{"other"}) {
		t.Errorf("groups=cix 的项目在 [other] 过滤下应被排除")
	}
	// 上游语义：显式 groups 不剥夺 default 归属，[default] 过滤保留
	if !(&m.Projects[0]).MatchesGroupFilter([]string{"default"}) {
		t.Errorf("groups=cix 的项目在 [default] 过滤下应被保留（上游隐式 default 归属）")
	}
	if !(&m.Projects[1]).MatchesGroupFilter([]string{"default"}) {
		t.Errorf("无组项目在 [default] 过滤下应被保留（隐式 default）")
	}
}

// TestParseFromFileCacheNotPoisonedByGroupFilter 回归测试：缓存污染。
// 旧实现把组过滤后的清单写入缓存；同一 Parser 先以 ["cix"] 解析再以 nil 解析
// 同一文件时，第二次命中缓存拿到的是被过滤后的项目列表。缓存必须保存
// 未过滤的完整清单。
func TestParseFromFileCacheNotPoisonedByGroupFilter(t *testing.T) {
	dir := t.TempDir()

	defaultXML := `<?xml version="1.0" encoding="UTF-8"?>
<manifest>
  <remote name="cix" fetch="https://example.com/" />
  <default revision="master" remote="cix" />
  <project name="cix/a" path="cix/a" groups="cix" />
  <project name="plfm/x" path="plfm/x" />
</manifest>
`
	mustWrite(t, filepath.Join(dir, "default.xml"), defaultXML)

	prev, _ := os.Getwd()
	defer os.Chdir(prev)
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	parser := NewParser()

	// 第一次：按 cix 组过滤解析（只应返回 1 个项目）
	filtered, err := parser.ParseFromFile("default.xml", []string{"cix"})
	if err != nil {
		t.Fatalf("第一次 ParseFromFile 失败: %v", err)
	}
	if len(filtered.Projects) != 1 || filtered.Projects[0].Name != "cix/a" {
		t.Fatalf("组过滤结果 = %v, want [cix/a]", projectNames(filtered))
	}

	// 第二次：不带组过滤解析同一文件（缓存命中，应返回全部 2 个项目）
	full, err := parser.ParseFromFile("default.xml", nil)
	if err != nil {
		t.Fatalf("第二次 ParseFromFile 失败: %v", err)
	}
	if len(full.Projects) != 2 {
		t.Fatalf("缓存命中后项目数 = %d %v, want 2（缓存被组过滤结果污染）",
			len(full.Projects), projectNames(full))
	}

	// 第三次：换一组过滤再解析（应能在完整缓存上重新过滤）；
	// 上游语义下显式 groups="cix" 不剥夺 default 归属，故两个项目均保留
	other, err := parser.ParseFromFile("default.xml", []string{"default"})
	if err != nil {
		t.Fatalf("第三次 ParseFromFile 失败: %v", err)
	}
	if len(other.Projects) != 2 {
		t.Fatalf("default 组过滤结果 = %v, want 2（显式 cix 组隐含 default 归属）", projectNames(other))
	}
}

func filterByGroups(t *testing.T, m *Manifest, groups []string) []Project {
	t.Helper()
	var out []Project
	for i := range m.Projects {
		if m.Projects[i].MatchesGroupFilter(groups) {
			out = append(out, m.Projects[i])
		}
	}
	return out
}

func projectNames(m *Manifest) []string {
	out := make([]string, 0, len(m.Projects))
	for _, p := range m.Projects {
		out = append(out, p.Name)
	}
	return out
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(strings.TrimSpace(content)+"\n"), 0644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
