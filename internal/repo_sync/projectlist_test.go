package repo_sync

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/leopardxu/repo-go/internal/config"
	"github.com/leopardxu/repo-go/internal/logger"
	"github.com/leopardxu/repo-go/internal/manifest"
	"github.com/leopardxu/repo-go/internal/project"
)

// newProjectListEngine 构造用于 project.list 清理测试的引擎。
func newProjectListEngine(t *testing.T, root string, opts *Options, runner *funcRunner) *Engine {
	t.Helper()
	if opts == nil {
		opts = &Options{}
	}
	opts.Jobs = 1
	opts.Quiet = true
	m := &manifest.Manifest{Topdir: root, RepoDir: filepath.Join(root, ".repo")}
	e := NewEngine(opts, m, logger.NewDefaultLogger())
	e.gitRunner = runner
	return e
}

// mkObsoleteProject 在 root 下创建一个带 .git 的过期项目检出。
func mkObsoleteProject(t *testing.T, root, rel string) string {
	t.Helper()
	dir := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir %s failed: %v", dir, err)
	}
	return dir
}

// TestUpdateProjectListRemovesCleanObsolete 验证干净且已从清单移除的项目
// 会被删除，当前清单项目路径被写入 project.list。
func TestUpdateProjectListRemovesCleanObsolete(t *testing.T) {
	root := t.TempDir()
	oldDir := mkObsoleteProject(t, root, "old")
	runner := &funcRunner{run: func(_ string, _ []string) ([]byte, error) {
		return []byte(""), nil // 干净
	}}
	e := newProjectListEngine(t, root, &Options{}, runner)
	e.SetProjects([]*project.Project{{Name: "keep", Relpath: "keep"}})
	if err := writeProjectList(e.projectListPath(), []string{"keep", "old"}); err != nil {
		t.Fatal(err)
	}

	if errs := e.updateProjectList(); len(errs) > 0 {
		t.Fatalf("updateProjectList() errors: %v", errs)
	}
	if _, err := os.Stat(oldDir); !os.IsNotExist(err) {
		t.Errorf("过期项目应被删除，stat err = %v", err)
	}
	paths, err := readProjectList(e.projectListPath())
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 1 || paths[0] != "keep" {
		t.Errorf("project.list = %v, want [keep]", paths)
	}
}

// TestUpdateProjectListDirtyRequiresForce 验证脏工作树在未指定
// --force-remove-dirty 时报错且不删除、不写回新列表。
func TestUpdateProjectListDirtyRequiresForce(t *testing.T) {
	root := t.TempDir()
	oldDir := mkObsoleteProject(t, root, "old")
	runner := &funcRunner{run: func(_ string, _ []string) ([]byte, error) {
		return []byte(" M file.txt\n"), nil // 脏
	}}
	e := newProjectListEngine(t, root, &Options{}, runner)
	e.SetProjects([]*project.Project{{Name: "keep", Relpath: "keep"}})
	if err := writeProjectList(e.projectListPath(), []string{"old"}); err != nil {
		t.Fatal(err)
	}

	errs := e.updateProjectList()
	if len(errs) != 1 {
		t.Fatalf("updateProjectList() = %v, want 1 error", errs)
	}
	if !strings.Contains(errs[0].Error(), "--force-remove-dirty") {
		t.Errorf("错误应提示 --force-remove-dirty，实际: %v", errs[0])
	}
	if _, err := os.Stat(oldDir); err != nil {
		t.Errorf("未强制时不得删除脏工作树: %v", err)
	}
	paths, err := readProjectList(e.projectListPath())
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 1 || paths[0] != "old" {
		t.Errorf("失败时不应写回新列表，project.list = %v", paths)
	}
}

// TestUpdateProjectListForceRemovesDirty 验证 --force-remove-dirty 会强制删除
// 脏的过期项目。
func TestUpdateProjectListForceRemovesDirty(t *testing.T) {
	root := t.TempDir()
	oldDir := mkObsoleteProject(t, root, "old")
	runner := &funcRunner{run: func(_ string, _ []string) ([]byte, error) {
		return []byte(" M file.txt\n"), nil
	}}
	e := newProjectListEngine(t, root, &Options{ForceRemoveDirty: true}, runner)
	e.SetProjects([]*project.Project{{Name: "keep", Relpath: "keep"}})
	if err := writeProjectList(e.projectListPath(), []string{"old"}); err != nil {
		t.Fatal(err)
	}

	if errs := e.updateProjectList(); len(errs) > 0 {
		t.Fatalf("updateProjectList() errors: %v", errs)
	}
	if _, err := os.Stat(oldDir); !os.IsNotExist(err) {
		t.Errorf("--force-remove-dirty 应删除脏项目，stat err = %v", err)
	}
}

// TestUpdateProjectListCorruptedWithoutForce 验证损坏仓库在未强制时报错并保留。
func TestUpdateProjectListCorruptedWithoutForce(t *testing.T) {
	root := t.TempDir()
	oldDir := mkObsoleteProject(t, root, "old")
	runner := &funcRunner{run: func(_ string, _ []string) ([]byte, error) {
		return nil, os.ErrNotExist // git status 失败，视为损坏
	}}
	e := newProjectListEngine(t, root, &Options{}, runner)
	e.SetProjects([]*project.Project{{Name: "keep", Relpath: "keep"}})
	if err := writeProjectList(e.projectListPath(), []string{"old"}); err != nil {
		t.Fatal(err)
	}

	errs := e.updateProjectList()
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "损坏") {
		t.Fatalf("updateProjectList() = %v, want corrupted error", errs)
	}
	if _, err := os.Stat(oldDir); err != nil {
		t.Errorf("未强制时不得删除损坏项目: %v", err)
	}
}

// TestUpdateProjectListKeepsParentWithSelectedChild 验证祖先路径被移除但其下
// 仍有清单项目时，不得删除父目录（否则会销毁子项目检出）。
func TestUpdateProjectListKeepsParentWithSelectedChild(t *testing.T) {
	root := t.TempDir()
	parentDir := mkObsoleteProject(t, root, "parent")
	runner := &funcRunner{run: func(_ string, _ []string) ([]byte, error) {
		return []byte(""), nil
	}}
	e := newProjectListEngine(t, root, &Options{}, runner)
	e.SetProjects([]*project.Project{{Name: "child", Relpath: "parent/child"}})
	if err := writeProjectList(e.projectListPath(), []string{"parent"}); err != nil {
		t.Fatal(err)
	}

	if errs := e.updateProjectList(); len(errs) > 0 {
		t.Fatalf("updateProjectList() errors: %v", errs)
	}
	if _, err := os.Stat(parentDir); err != nil {
		t.Errorf("含有效子项目的父目录不得删除: %v", err)
	}
	paths, err := readProjectList(e.projectListPath())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"parent", "parent/child"}
	if len(paths) != len(want) || paths[0] != want[0] || paths[1] != want[1] {
		t.Errorf("project.list = %v, want %v", paths, want)
	}
}

// TestUpdateProjectListMirrorSkips 验证镜像模式没有工作树，不写/不删 project.list。
func TestUpdateProjectListMirrorSkips(t *testing.T) {
	root := t.TempDir()
	runner := &funcRunner{run: func(_ string, _ []string) ([]byte, error) {
		t.Error("镜像模式不应执行 git status")
		return nil, nil
	}}
	cfg := &config.Config{Mirror: true}
	e := newProjectListEngine(t, root, &Options{Config: cfg}, runner)
	e.SetProjects([]*project.Project{{Name: "keep", Relpath: "keep"}})

	if errs := e.updateProjectList(); len(errs) > 0 {
		t.Fatalf("updateProjectList() errors: %v", errs)
	}
	if _, err := os.Stat(e.projectListPath()); !os.IsNotExist(err) {
		t.Errorf("镜像模式不应写 project.list，stat err = %v", err)
	}
}
