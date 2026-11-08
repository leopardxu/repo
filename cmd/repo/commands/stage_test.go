package commands

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/leopardxu/repo-go/internal/config"
	"github.com/leopardxu/repo-go/internal/project"
)

func testStageProject(name, worktree string) *project.Project {
	return &project.Project{Name: name, Worktree: worktree, Path: worktree}
}

func TestSplitStageArgs(t *testing.T) {
	projects := []*project.Project{
		testStageProject("a", "/repo/a"),
		testStageProject("b", "/repo/b"),
	}

	tests := []struct {
		name        string
		args        []string
		wantProject []string
		wantFiles   []string
	}{
		{
			name:        "no args",
			args:        nil,
			wantProject: nil,
			wantFiles:   nil,
		},
		{
			name:        "projects then files",
			args:        []string{"a", "b", "a/x", "b/y"},
			wantProject: []string{"a", "b"},
			wantFiles:   []string{"a/x", "b/y"},
		},
		{
			name:        "first non-project stops project parsing",
			args:        []string{"a/x", "a"},
			wantProject: nil,
			wantFiles:   []string{"a/x", "a"},
		},
		{
			name:        "project by worktree path",
			args:        []string{"/repo/a", "f"},
			wantProject: []string{"/repo/a"},
			wantFiles:   []string{"f"},
		},
		{
			name:        "trailing slash matches worktree",
			args:        []string{"a/", "f"},
			wantProject: []string{"a/"},
			wantFiles:   []string{"f"},
		},
		{
			name:        "dot inside project is a file pathspec",
			args:        []string{"b", "."},
			wantProject: []string{"b"},
			wantFiles:   []string{"."},
		},
		{
			name:        "subdirectory file is not a project selector",
			args:        []string{"a/sub/f.txt"},
			wantProject: nil,
			wantFiles:   []string{"a/sub/f.txt"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotProjects, gotFiles := splitStageArgs(projects, tt.args, "/repo", "/repo")
			if !reflect.DeepEqual(gotProjects, tt.wantProject) {
				t.Errorf("projectArgs = %v, want %v", gotProjects, tt.wantProject)
			}
			if !reflect.DeepEqual(gotFiles, tt.wantFiles) {
				t.Errorf("files = %v, want %v", gotFiles, tt.wantFiles)
			}
		})
	}
}

func TestAssignStageFilesScopesToOwningProject(t *testing.T) {
	a := testStageProject("a", "/repo/a")
	b := testStageProject("b", "/repo/b")
	projects := []*project.Project{a, b}

	assigned, unassigned := assignStageFiles(projects, projects, []string{"a/x", "b/y", "a/z"}, "/repo", "/repo", true)
	if len(unassigned) != 0 {
		t.Fatalf("unassigned = %v, want none", unassigned)
	}
	if got, want := assigned[a], []string{"x", "z"}; !reflect.DeepEqual(got, want) {
		t.Errorf("project a files = %v, want %v", got, want)
	}
	if got, want := assigned[b], []string{"y"}; !reflect.DeepEqual(got, want) {
		t.Errorf("project b files = %v, want %v", got, want)
	}
}

func TestAssignStageFilesSkipsProjectsWithoutPath(t *testing.T) {
	a := testStageProject("a", "/repo/a")
	b := testStageProject("b", "/repo/b")

	assigned, unassigned := assignStageFiles([]*project.Project{a, b}, []*project.Project{a, b}, []string{"a/x"}, "/repo", "/repo", true)
	if len(unassigned) != 0 {
		t.Fatalf("unassigned = %v, want none", unassigned)
	}
	if got := assigned[a]; !reflect.DeepEqual(got, []string{"x"}) {
		t.Errorf("project a files = %v, want [x]", got)
	}
	if got := assigned[b]; len(got) != 0 {
		t.Errorf("project b files = %v, want empty (must be skipped)", got)
	}
}

func TestAssignStageFilesReportsUnowned(t *testing.T) {
	a := testStageProject("a", "/repo/a")
	assigned, unassigned := assignStageFiles([]*project.Project{a}, []*project.Project{a}, []string{"c/z"}, "/repo", "/repo", true)
	if len(assigned[a]) != 0 {
		t.Errorf("project a files = %v, want empty", assigned[a])
	}
	if len(unassigned) != 1 {
		t.Fatalf("unassigned = %v, want one entry", unassigned)
	}
	if unassigned[0].File != "c/z" || unassigned[0].Owner != nil {
		t.Errorf("unassigned[0] = %+v, want {File:c/z Owner:nil}", unassigned[0])
	}
}

func TestAssignStageFilesAbsolutePath(t *testing.T) {
	a := testStageProject("a", "/repo/a")
	b := testStageProject("b", "/repo/b")
	assigned, unassigned := assignStageFiles([]*project.Project{a, b}, []*project.Project{a, b}, []string{"/repo/b/y"}, "/repo", "/repo", true)
	if len(unassigned) != 0 {
		t.Fatalf("unassigned = %v, want none", unassigned)
	}
	if got, want := assigned[b], []string{"y"}; !reflect.DeepEqual(got, want) {
		t.Errorf("project b files = %v, want %v", got, want)
	}
}

func TestAssignStageFilesDeepestProjectWins(t *testing.T) {
	outer := testStageProject("outer", "/repo")
	inner := testStageProject("inner", "/repo/a")

	assigned, unassigned := assignStageFiles([]*project.Project{outer, inner}, []*project.Project{outer, inner}, []string{"a/x"}, "/repo", "/repo", true)
	if len(unassigned) != 0 {
		t.Fatalf("unassigned = %v, want none", unassigned)
	}
	if got := assigned[inner]; !reflect.DeepEqual(got, []string{"x"}) {
		t.Errorf("inner files = %v, want [x]", got)
	}
	if got := assigned[outer]; len(got) != 0 {
		t.Errorf("outer files = %v, want empty (deepest project wins)", got)
	}
}

func TestAssignStageFilesSingleProjectRelativeFallback(t *testing.T) {
	repoRoot := t.TempDir()
	worktree := filepath.Join(repoRoot, "a")
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	a := testStageProject("a", worktree)
	// 路径可不存在（已删除文件）或为 git pathspec glob，不能依赖 os.Stat。
	files := []string{"src/deleted.go", "src/*.go"}
	assigned, unassigned := assignStageFiles([]*project.Project{a}, []*project.Project{a}, files, repoRoot, repoRoot, true)
	if len(unassigned) != 0 {
		t.Fatalf("unassigned = %v, want none", unassigned)
	}
	if got := assigned[a]; !reflect.DeepEqual(got, files) {
		t.Errorf("project a files = %v, want %v", got, files)
	}

	// 逃逸出工作树的相对路径仍必须拒绝。
	_, unassigned = assignStageFiles([]*project.Project{a}, []*project.Project{a}, []string{"../b/x"}, repoRoot, repoRoot, true)
	if len(unassigned) != 1 || unassigned[0].Owner != nil {
		t.Fatalf("unassigned = %+v, want one unowned entry", unassigned)
	}
}

func TestAssignStageFilesNestedProjectNotSelected(t *testing.T) {
	outer := testStageProject("outer", "/repo")
	inner := testStageProject("inner", "/repo/a")
	all := []*project.Project{outer, inner}

	// 只选中父项目：文件实际属于嵌套子项目，必须明确报错而不是交给父项目。
	assigned, unassigned := assignStageFiles(all, []*project.Project{outer}, []string{"a/x"}, "/repo", "/repo", true)
	if len(assigned[outer]) != 0 {
		t.Errorf("outer files = %v, want empty (nested owner not selected)", assigned[outer])
	}
	if len(unassigned) != 1 {
		t.Fatalf("unassigned = %v, want one entry", unassigned)
	}
	if unassigned[0].File != "a/x" || unassigned[0].Owner != inner {
		t.Errorf("unassigned[0] = %+v, want owner inner", unassigned[0])
	}
	if !strings.Contains(unassigned[0].String(), "inner") {
		t.Errorf("error message %q must name the unselected project", unassigned[0].String())
	}

	// 同时选中父与子项目时，最深项目胜出。
	assigned, unassigned = assignStageFiles(all, all, []string{"a/x"}, "/repo", "/repo", true)
	if len(unassigned) != 0 {
		t.Fatalf("unassigned = %v, want none", unassigned)
	}
	if got := assigned[inner]; !reflect.DeepEqual(got, []string{"x"}) {
		t.Errorf("inner files = %v, want [x]", got)
	}
	if len(assigned[outer]) != 0 {
		t.Errorf("outer files = %v, want empty", assigned[outer])
	}
}

func TestAssignStageFilesDotFromAncestorDir(t *testing.T) {
	a := testStageProject("a", "/repo/a")
	b := testStageProject("b", "/repo/b")
	all := []*project.Project{a, b}

	// 在 repo 根目录执行 `repo stage .`：每个选中项目都暂存其工作树根。
	assigned, unassigned := assignStageFiles(all, all, []string{"."}, "/repo", "/repo", true)
	if len(unassigned) != 0 {
		t.Fatalf("unassigned = %v, want none", unassigned)
	}
	for _, p := range all {
		if got := assigned[p]; !reflect.DeepEqual(got, []string{"."}) {
			t.Errorf("project %s files = %v, want [.]", p.Name, got)
		}
	}

	// 在项目 a 内部执行 `repo stage .`：只影响 a，不应波及其他项目。
	assigned, unassigned = assignStageFiles(all, all, []string{"."}, "/repo/a", "/repo", true)
	if len(unassigned) != 0 {
		t.Fatalf("unassigned = %v, want none", unassigned)
	}
	if got := assigned[a]; !reflect.DeepEqual(got, []string{"."}) {
		t.Errorf("project a files = %v, want [.]", got)
	}
	if len(assigned[b]) != 0 {
		t.Errorf("project b files = %v, want empty", assigned[b])
	}
}

// TestSplitStageArgsDotAtProjectRoot 验证 cwd 在项目根时 "." 是 pathspec，
// 不会被工作树路径匹配误判为项目选择参数。
func TestSplitStageArgsDotAtProjectRoot(t *testing.T) {
	projects := []*project.Project{testStageProject("a", "/repo/a")}

	projectArgs, files := splitStageArgs(projects, []string{"."}, "/repo/a", "/repo")
	if len(projectArgs) != 0 {
		t.Errorf("projectArgs = %v, want none", projectArgs)
	}
	if !reflect.DeepEqual(files, []string{"."}) {
		t.Errorf("files = %v, want [.]", files)
	}
}

// TestAssignStageFilesFallbackRechecksNestedOwner 验证单项目回退命中后，
// 会把路径解析为绝对路径并对全部项目重新做最深归属检查。
func TestAssignStageFilesFallbackRechecksNestedOwner(t *testing.T) {
	repoRoot := t.TempDir()
	outerWT := filepath.Join(repoRoot, "outer")
	innerWT := filepath.Join(outerWT, "inner")
	if err := os.MkdirAll(innerWT, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	outer := testStageProject("outer", outerWT)
	inner := testStageProject("inner", innerWT)
	all := []*project.Project{outer, inner}

	// 从 repo 根执行 `repo stage outer inner/src/a.go`：路径相对父项目解析命中，
	// 但绝对化后实际属于未选中的嵌套子项目，必须报错。
	assigned, unassigned := assignStageFiles(all, []*project.Project{outer}, []string{"inner/src/a.go"}, repoRoot, repoRoot, true)
	if len(assigned[outer]) != 0 {
		t.Errorf("outer files = %v, want empty (nested owner not selected)", assigned[outer])
	}
	if len(unassigned) != 1 {
		t.Fatalf("unassigned = %v, want one entry", unassigned)
	}
	if unassigned[0].Owner != inner {
		t.Errorf("unassigned owner = %v, want inner", unassigned[0].Owner)
	}

	// 相对父项目工作树且确属父项目的路径仍正常归属；父项目含嵌套项目时
	// 一并追加排除项，避免父仓库把嵌套项目写成 gitlink。
	assigned, unassigned = assignStageFiles(all, []*project.Project{outer}, []string{"src/a.go"}, repoRoot, repoRoot, true)
	if len(unassigned) != 0 {
		t.Fatalf("unassigned = %v, want none", unassigned)
	}
	if got, want := assigned[outer], []string{"src/a.go", ":(exclude)inner"}; !reflect.DeepEqual(got, want) {
		t.Errorf("outer files = %v, want %v", got, want)
	}
}

// TestAssignStageFilesSubdirExcludesNestedProjects 验证父项目的子目录 pathspec
// （如 `repo stage parent/sub`）同样排除内部嵌套项目；多个 pathspec 只追加一次排除项。
func TestAssignStageFilesSubdirExcludesNestedProjects(t *testing.T) {
	outer := testStageProject("outer", "/repo")
	inner := testStageProject("inner", "/repo/nested")
	all := []*project.Project{outer, inner}

	assigned, unassigned := assignStageFiles(all, []*project.Project{outer}, []string{"sub", "sub2/file.go"}, "/repo", "/repo", false)
	if len(unassigned) != 0 {
		t.Fatalf("unassigned = %v, want none", unassigned)
	}
	want := []string{"sub", "sub2/file.go", ":(exclude)nested"}
	if got := assigned[outer]; !reflect.DeepEqual(got, want) {
		t.Errorf("outer files = %v, want %v", got, want)
	}
	if len(assigned[inner]) != 0 {
		t.Errorf("inner files = %v, want empty (nested project not selected)", assigned[inner])
	}
}

// TestAssignStageFilesDotExcludesNestedProjects 验证暂存 "." 时排除嵌套项目路径，
// 避免父仓库把子项目目录写成 gitlink。
func TestAssignStageFilesDotExcludesNestedProjects(t *testing.T) {
	outer := testStageProject("outer", "/repo")
	inner := testStageProject("inner", "/repo/a")
	all := []*project.Project{outer, inner}

	// cwd 在父项目根（/repo），只选中父项目：父项目暂存 "." 并排除 inner。
	assigned, unassigned := assignStageFiles(all, []*project.Project{outer}, []string{"."}, "/repo", "/repo", true)
	if len(unassigned) != 0 {
		t.Fatalf("unassigned = %v, want none", unassigned)
	}
	want := []string{".", ":(exclude)a"}
	if got := assigned[outer]; !reflect.DeepEqual(got, want) {
		t.Errorf("outer files = %v, want %v", got, want)
	}

	// 父子同时选中：父项目仍排除 inner，inner 自己暂存 "."。
	assigned, unassigned = assignStageFiles(all, all, []string{"."}, "/repo", "/repo", true)
	if len(unassigned) != 0 {
		t.Fatalf("unassigned = %v, want none", unassigned)
	}
	if got := assigned[outer]; !reflect.DeepEqual(got, want) {
		t.Errorf("outer files = %v, want %v", got, want)
	}
	if got := assigned[inner]; !reflect.DeepEqual(got, []string{"."}) {
		t.Errorf("inner files = %v, want [.]", got)
	}
}

// TestSelectStageProjectsGroupFilter 验证未显式指定项目时按配置组过滤，
// 而 manager 的全部项目列表仍保持未过滤（供文件归属检查使用）。
func TestSelectStageProjectsGroupFilter(t *testing.T) {
	inGroup := &project.Project{Name: "in", Path: "in", Worktree: "in", Groups: []string{"cix"}}
	outGroup := &project.Project{Name: "out", Path: "out", Worktree: "out", Groups: []string{"other"}}
	manager := &project.Manager{Projects: []*project.Project{inGroup, outGroup}, TopDir: "/repo"}

	got, err := selectStageProjects(manager, nil, "/repo", []string{"cix"})
	if err != nil {
		t.Fatalf("selectStageProjects: %v", err)
	}
	if len(got) != 1 || got[0] != inGroup {
		t.Errorf("selected = %v, want [in]", projectNamesOf(got))
	}

	// allProjects 必须包含未选中组的项目，否则文件归属无法识别其 owner。
	if all := manager.GetProjects(); len(all) != 2 {
		t.Errorf("allProjects = %v, want 2 projects", projectNamesOf(all))
	}
}

// TestStageSelectionGroups 验证 stage 没有 -g 选项时使用配置的有效组：
// 空 Groups 回退 default（而不是选中全部项目），显式配置原样拆分。
func TestStageSelectionGroups(t *testing.T) {
	tests := []struct {
		name string
		cfg  *config.Config
		want []string
	}{
		{name: "空 Groups 回退 default", cfg: &config.Config{}, want: []string{"default"}},
		{name: "配置组原样拆分", cfg: &config.Config{Groups: "cix, other"}, want: []string{"cix", "other"}},
		{name: "nil 配置回退 default", cfg: nil, want: []string{"default"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := stageSelectionGroups(tt.cfg); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("stageSelectionGroups() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestSelectStageProjectsDedupesArgs 验证重复项目参数只处理一次。
func TestSelectStageProjectsDedupesArgs(t *testing.T) {
	a := &project.Project{Name: "a", Path: "a", Worktree: "a"}
	manager := &project.Manager{Projects: []*project.Project{a}, TopDir: "/repo"}

	got, err := selectStageProjects(manager, []string{"a", "a"}, "/repo", nil)
	if err != nil {
		t.Fatalf("selectStageProjects: %v", err)
	}
	if len(got) != 1 || got[0] != a {
		t.Errorf("selected = %v, want [a]", projectNamesOf(got))
	}
}

// projectNamesOf 返回项目名列表，便于错误信息输出。
func projectNamesOf(projects []*project.Project) []string {
	names := make([]string, 0, len(projects))
	for _, p := range projects {
		names = append(names, p.Name)
	}
	return names
}

func TestBuildStageArgs(t *testing.T) {
	tests := []struct {
		name  string
		opts  StageOptions
		files []string
		want  []string
	}{
		{
			name: "interactive only",
			opts: StageOptions{Interactive: true},
			want: []string{"add", "--interactive"},
		},
		{
			name:  "force with files uses separator",
			opts:  StageOptions{Force: true},
			files: []string{"a/x"},
			want:  []string{"add", "--force", "--", "a/x"},
		},
		{
			name: "all and update",
			opts: StageOptions{All: true, Update: true},
			want: []string{"add", "--all", "--update"},
		},
		{
			name: "patch",
			opts: StageOptions{Patch: true},
			want: []string{"add", "--patch"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildStageArgs(&tt.opts, tt.files)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("buildStageArgs() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestAssignStageFilesRelativeFallbackRequiresExplicitSelection 验证单项目相对路径
// 回退只在用户显式给出项目参数时启用；否则不在项目内的相对路径按 unowned 报错。
func TestAssignStageFilesRelativeFallbackRequiresExplicitSelection(t *testing.T) {
	repoRoot := t.TempDir()
	worktree := filepath.Join(repoRoot, "a")
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	a := testStageProject("a", worktree)
	all := []*project.Project{a}
	files := []string{"src/a.go"}

	// 未显式给出项目参数：相对 repo 根的路径不属于任何项目，必须报 unowned。
	assigned, unassigned := assignStageFiles(all, all, files, repoRoot, repoRoot, false)
	if len(assigned[a]) != 0 {
		t.Errorf("project a files = %v, want empty without explicit selection", assigned[a])
	}
	if len(unassigned) != 1 || unassigned[0].Owner != nil {
		t.Fatalf("unassigned = %+v, want one unowned entry", unassigned)
	}

	// 显式选择项目 `repo stage a src/a.go`：相对工作树解析并归属。
	assigned, unassigned = assignStageFiles(all, all, files, repoRoot, repoRoot, true)
	if len(unassigned) != 0 {
		t.Fatalf("unassigned = %v, want none", unassigned)
	}
	if got := assigned[a]; !reflect.DeepEqual(got, files) {
		t.Errorf("project a files = %v, want %v", got, files)
	}
}

// TestStageAllNestedExcludes 验证 --all 且无文件 pathspec 时为父项目排除内部
// 嵌套项目（包括未选中的嵌套项目），避免父仓库写入 gitlink。
func TestStageAllNestedExcludes(t *testing.T) {
	outer := testStageProject("outer", "/repo")
	inner := testStageProject("inner", "/repo/a")
	other := testStageProject("other", "/repo/other")
	all := []*project.Project{outer, inner, other}

	assigned := make(map[*project.Project][]string, len(all))
	// 只选中 outer：inner 未选中，但仍必须排除（nestedProjectExcludes 基于全部项目）。
	stageAllNestedExcludes(assigned, all, []*project.Project{outer}, nil, &StageOptions{All: true}, "/repo")
	if got, want := assigned[outer], []string{":(exclude)a", ":(exclude)other"}; !reflect.DeepEqual(got, want) {
		t.Errorf("outer files = %v, want %v", got, want)
	}
	if len(assigned[inner]) != 0 || len(assigned[other]) != 0 {
		t.Errorf("unselected projects must not be staged: inner=%v other=%v", assigned[inner], assigned[other])
	}

	// 父子同时选中：父项目仍排除 inner，inner 自身没有嵌套项目。
	assigned = make(map[*project.Project][]string, len(all))
	stageAllNestedExcludes(assigned, all, []*project.Project{outer, inner}, nil, &StageOptions{All: true}, "/repo")
	if got, want := assigned[outer], []string{":(exclude)a", ":(exclude)other"}; !reflect.DeepEqual(got, want) {
		t.Errorf("outer files = %v, want %v", got, want)
	}
	if len(assigned[inner]) != 0 {
		t.Errorf("inner files = %v, want empty", assigned[inner])
	}

	// 有显式文件 pathspec 或未使用 --all 时不追加排除项。
	assigned = make(map[*project.Project][]string, len(all))
	stageAllNestedExcludes(assigned, all, []*project.Project{outer}, []string{"a/x"}, &StageOptions{All: true}, "/repo")
	if len(assigned[outer]) != 0 {
		t.Errorf("explicit files must not get excludes, got %v", assigned[outer])
	}
	stageAllNestedExcludes(assigned, all, []*project.Project{outer}, nil, &StageOptions{Update: true}, "/repo")
	if len(assigned[outer]) != 0 {
		t.Errorf("non-all mode must not get excludes, got %v", assigned[outer])
	}
}
