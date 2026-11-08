package commands

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/leopardxu/repo-go/internal/git"
	"github.com/leopardxu/repo-go/internal/logger"
	"github.com/leopardxu/repo-go/internal/manifest"
	"github.com/leopardxu/repo-go/internal/project"
)

// readCmdRunCaptured 把结果命令的 stdout/stderr 重定向到临时文件后执行 fn，
// 返回捕获内容。用于验证结果 payload 只写 stdout、日志不污染 stdout。
func readCmdRunCaptured(t *testing.T, fn func() error) (stdout string, stderr string, err error) {
	t.Helper()
	outFile, cerr := os.CreateTemp(t.TempDir(), "stdout-*")
	if cerr != nil {
		t.Fatalf("创建 stdout 临时文件failed: %v", cerr)
	}
	errFile, cerr := os.CreateTemp(t.TempDir(), "stderr-*")
	if cerr != nil {
		t.Fatalf("创建 stderr 临时文件failed: %v", cerr)
	}
	oldStdout, oldStderr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = outFile, errFile
	defer func() {
		os.Stdout, os.Stderr = oldStdout, oldStderr
		outFile.Close()
		errFile.Close()
	}()

	runErr := fn()
	os.Stdout, os.Stderr = oldStdout, oldStderr

	outData, rerr := os.ReadFile(outFile.Name())
	if rerr != nil {
		t.Fatalf("读取 stdout failed: %v", rerr)
	}
	errData, rerr := os.ReadFile(errFile.Name())
	if rerr != nil {
		t.Fatalf("读取 stderr failed: %v", rerr)
	}
	return string(outData), string(errData), runErr
}

// readCmdTestRepoRoot 创建最小 repo 根目录（.repo/config.json + 主清单），
// 主清单含 remote/default 及给定 projects XML 片段。测试自包含，避免依赖
// 其它测试文件中的辅助函数。
func readCmdTestRepoRoot(t *testing.T, projectsXML string) string {
	t.Helper()
	t.Setenv("REPO_MANIFEST_NAME", "")
	t.Setenv("REPO_MANIFEST_URL", "")
	t.Setenv("REPO_GROUPS", "")
	repoRoot := t.TempDir()
	repoDir := filepath.Join(repoRoot, ".repo")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatalf("创建 .repo failed: %v", err)
	}
	manifestXML := `<manifest>
  <remote name="origin" fetch="https://example.com" />
  <default remote="origin" revision="main" />
` + projectsXML + `
</manifest>
`
	if err := os.WriteFile(filepath.Join(repoDir, "manifest.xml"), []byte(manifestXML), 0o644); err != nil {
		t.Fatalf("写入 manifest.xml failed: %v", err)
	}
	configJSON := `{"version":1,"manifest_url":"https://example.com/manifest","manifest_name":".repo/manifest.xml","jobs":2}`
	if err := os.WriteFile(filepath.Join(repoDir, "config.json"), []byte(configJSON), 0o644); err != nil {
		t.Fatalf("写入 config.json failed: %v", err)
	}
	return repoRoot
}

// readCmdWithCwd 切换到 dir 执行 fn，结束后恢复原目录。
func readCmdWithCwd(t *testing.T, dir string, fn func()) {
	t.Helper()
	oldwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("获取当前目录failed: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("切换到 %s failed: %v", dir, err)
	}
	defer func() {
		if err := os.Chdir(oldwd); err != nil {
			t.Errorf("恢复工作目录failed: %v", err)
		}
	}()
	fn()
}

// readCmdStdoutRepo 创建最小 repo 根目录及一个已检出项目 foo，返回 repo 根目录
// 与项目工作树 HEAD/parent 提交 id。工作树保留一个未暂存修改，便于 status/diff 断言。
func readCmdStdoutRepo(t *testing.T) (repoRoot, headSHA, parentSHA string) {
	t.Helper()
	repoRoot = readCmdTestRepoRoot(t, `  <project name="foo" path="foo" revision="main" />`)

	dir := filepath.Join(repoRoot, "foo")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("创建项目目录failed: %v", err)
	}
	runner := git.NewRunner()
	runGit := func(args ...string) string {
		t.Helper()
		out, gerr := runner.RunInDir(dir, args...)
		if gerr != nil {
			t.Fatalf("git %v failed: %v: %s", args, gerr, out)
		}
		return strings.TrimSpace(string(out))
	}
	runGit("init", "-q")
	runGit("config", "user.email", "test@example.com")
	runGit("config", "user.name", "Test")

	versionFile := filepath.Join(dir, "version.txt")
	if err := os.WriteFile(versionFile, []byte("first\n"), 0o644); err != nil {
		t.Fatalf("写入初始文件failed: %v", err)
	}
	runGit("add", "version.txt")
	runGit("commit", "-q", "-m", "initial")
	parentSHA = runGit("rev-parse", "HEAD")

	if err := os.WriteFile(versionFile, []byte("first\nsecond\n"), 0o644); err != nil {
		t.Fatalf("修改文件failed: %v", err)
	}
	runGit("add", "version.txt")
	runGit("commit", "-q", "-m", "second")
	headSHA = runGit("rev-parse", "HEAD")

	// 保留未暂存的修改，供 status/diff 输出断言使用
	if err := os.WriteFile(versionFile, []byte("first\nsecond\nthird\n"), 0o644); err != nil {
		t.Fatalf("写入工作树修改failed: %v", err)
	}
	return repoRoot, headSHA, parentSHA
}

func readCmdAssertStdoutPayload(t *testing.T, stdout string, wants ...string) {
	t.Helper()
	for _, want := range wants {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout 缺少 %q，实际输出:\n%s", want, stdout)
		}
	}
	if strings.Contains(stdout, "[INFO]") || strings.Contains(stdout, "[ERROR]") || strings.Contains(stdout, "level=") {
		t.Errorf("stdout 不得混入日志，实际输出:\n%s", stdout)
	}
}

// TestRunStatusStdoutPayload 验证 status 结果写 stdout，且 -q 不吞结果。
func TestRunStatusStdoutPayload(t *testing.T) {
	repoRoot, _, _ := readCmdStdoutRepo(t)

	readCmdWithCwd(t, repoRoot, func() {
		for _, quiet := range []bool{false, true} {
			stdout, _, err := readCmdRunCaptured(t, func() error {
				return runStatus(&StatusOptions{Jobs: 1, Quiet: quiet}, nil, logger.NewDefaultLogger())
			})
			if err != nil {
				t.Fatalf("runStatus(quiet=%v) failed: %v", quiet, err)
			}
			readCmdAssertStdoutPayload(t, stdout, "project foo/")
			if quiet {
				// quiet 只输出项目头，不输出 branch/文件明细。
				if strings.Contains(stdout, "version.txt") || strings.Contains(stdout, "branch ") {
					t.Errorf("quiet status 不应输出 branch/文件明细:\n%s", stdout)
				}
			} else {
				readCmdAssertStdoutPayload(t, stdout, "branch ", "version.txt")
			}
		}
	})
}

// TestRunStatusMissingStdoutPayload 验证缺失项目提示写 stdout 且使用 Relpath。
func TestRunStatusMissingStdoutPayload(t *testing.T) {
	repoRoot := readCmdTestRepoRoot(t, `  <project name="absent" path="absent" revision="main" />`)

	readCmdWithCwd(t, repoRoot, func() {
		stdout, _, err := readCmdRunCaptured(t, func() error {
			return runStatus(&StatusOptions{Jobs: 1}, nil, logger.NewDefaultLogger())
		})
		if err != nil {
			t.Fatalf("runStatus failed: %v", err)
		}
		readCmdAssertStdoutPayload(t, stdout, "project absent/", `missing (run "repo sync")`)
	})
}

// TestRunDiffStdoutPayload 验证 diff 的项目头与内容写 stdout，-q 不吞结果。
func TestRunDiffStdoutPayload(t *testing.T) {
	repoRoot, _, _ := readCmdStdoutRepo(t)

	readCmdWithCwd(t, repoRoot, func() {
		for _, quiet := range []bool{false, true} {
			stdout, _, err := readCmdRunCaptured(t, func() error {
				return runDiff(&DiffOptions{Jobs: 1, Quiet: quiet}, nil)
			})
			if err != nil {
				t.Fatalf("runDiff(quiet=%v) failed: %v", quiet, err)
			}
			readCmdAssertStdoutPayload(t, stdout, "--- foo ---", "version.txt", "third")
		}
	})
}

// TestRunInfoStdoutPayload 验证 info 头部与正文写 stdout，路径使用 Relpath。
func TestRunInfoStdoutPayload(t *testing.T) {
	repoRoot, _, _ := readCmdStdoutRepo(t)

	readCmdWithCwd(t, repoRoot, func() {
		stdout, _, err := readCmdRunCaptured(t, func() error {
			return runInfo(&InfoOptions{Jobs: 1}, nil)
		})
		if err != nil {
			t.Fatalf("runInfo failed: %v", err)
		}
		readCmdAssertStdoutPayload(t, stdout, "--- foo ---", "Project: foo", "Path: foo", "Manifest revision: main")
	})
}

// TestRunDiffManifestsStdoutPayload 验证 diffmanifests 结果写 stdout，且相对
// 清单名按 .repo/manifests 解析、默认按解析后的 revision 比较。
func TestRunDiffManifestsStdoutPayload(t *testing.T) {
	repoRoot, headSHA, parentSHA := readCmdStdoutRepo(t)

	manifestsDir := filepath.Join(repoRoot, ".repo", "manifests")
	if err := os.MkdirAll(manifestsDir, 0o755); err != nil {
		t.Fatalf("创建 .repo/manifests failed: %v", err)
	}
	writeManifest := func(name, revision string) {
		t.Helper()
		content := `<manifest>
  <remote name="origin" fetch="https://example.com" />
  <default remote="origin" revision="main" />
  <project name="foo" path="foo" revision="` + revision + `" />
</manifest>
`
		if err := os.WriteFile(filepath.Join(manifestsDir, name), []byte(content), 0o644); err != nil {
			t.Fatalf("写入 %s failed: %v", name, err)
		}
	}
	writeManifest("m1.xml", parentSHA)
	writeManifest("m2.xml", headSHA)

	readCmdWithCwd(t, repoRoot, func() {
		stdout, _, err := readCmdRunCaptured(t, func() error {
			return runDiffManifests(&DiffManifestsOptions{}, []string{"m1.xml", "m2.xml"})
		})
		if err != nil {
			t.Fatalf("runDiffManifests failed: %v", err)
		}
		readCmdAssertStdoutPayload(t, stdout, "发现 1 处差异", "changed project: foo", parentSHA, headSHA)

		stdout, _, err = readCmdRunCaptured(t, func() error {
			return runDiffManifests(&DiffManifestsOptions{Raw: true}, []string{"m1.xml", "m2.xml"})
		})
		if err != nil {
			t.Fatalf("runDiffManifests(--raw) failed: %v", err)
		}
		readCmdAssertStdoutPayload(t, stdout, "C foo "+parentSHA+" "+headSHA)
	})
}

// TestRunOverviewStdoutPayload 验证 overview 结果写 stdout。
func TestRunOverviewStdoutPayload(t *testing.T) {
	repoRoot, headSHA, _ := readCmdStdoutRepo(t)

	// 清单 revision 为 main：确保本地存在 main 分支，使 overview 可解析基准
	// （解析失败现在会显式标注，而不是静默当作 0 个未推送提交）。
	if out, err := git.NewRunner().RunInDir(filepath.Join(repoRoot, "foo"),
		"update-ref", "refs/heads/main", headSHA); err != nil {
		t.Fatalf("创建 main 分支 failed: %v: %s", err, out)
	}

	readCmdWithCwd(t, repoRoot, func() {
		stdout, _, err := readCmdRunCaptured(t, func() error {
			return runOverview(&OverviewOptions{Jobs: 1}, nil)
		})
		if err != nil {
			t.Fatalf("runOverview failed: %v", err)
		}
		readCmdAssertStdoutPayload(t, stdout, "没有未推送的本地提交")
	})
}

// TestReadCmdProjectRelpathMirror 验证镜像项目展示使用 Relpath 而非绝对裸库路径。
func TestReadCmdProjectRelpathMirror(t *testing.T) {
	p := &project.Project{
		Name:    "mirror/foo",
		Path:    "/abs/checkout/mirror/foo.git",
		Relpath: "mirror/foo",
		Mirror:  true,
		Gitdir:  "/abs/checkout/mirror/foo.git",
	}
	if got, want := readCmdProjectRelpath(p), "mirror/foo"; got != want {
		t.Errorf("readCmdProjectRelpath() = %q, want %q", got, want)
	}
	if got := listProjectOutput(p, readCmdProjectRelpath(p), "", &ListOptions{}); got != "mirror/foo : mirror/foo" {
		t.Errorf("listProjectOutput() = %q, want %q", got, "mirror/foo : mirror/foo")
	}
}

// TestReadCmdProjectExistsMirror 验证镜像项目按裸库目录判定存在性。
func TestReadCmdProjectExistsMirror(t *testing.T) {
	gitdir := t.TempDir()
	p := &project.Project{Name: "foo", Relpath: "foo", Mirror: true, Gitdir: gitdir}
	if !readCmdProjectExists(p) {
		t.Error("镜像裸库目录存在时应判定为已初始化")
	}
	p.Gitdir = filepath.Join(gitdir, "missing.git")
	if readCmdProjectExists(p) {
		t.Error("镜像裸库目录不存在时应判定为缺失")
	}
}

// TestResolveDiffManifestPath 验证相对清单名优先按 .repo/manifests 解析。
func TestResolveDiffManifestPath(t *testing.T) {
	repoRoot := t.TempDir()
	manifestsDir := filepath.Join(repoRoot, ".repo", "manifests")
	if err := os.MkdirAll(manifestsDir, 0o755); err != nil {
		t.Fatalf("创建 .repo/manifests failed: %v", err)
	}
	manifestName := filepath.Join(manifestsDir, "m1.xml")
	if err := os.WriteFile(manifestName, []byte("<manifest/>"), 0o644); err != nil {
		t.Fatalf("写入清单failed: %v", err)
	}
	rootManifest := filepath.Join(repoRoot, ".repo", "manifest.xml")
	if err := os.WriteFile(rootManifest, []byte("<manifest/>"), 0o644); err != nil {
		t.Fatalf("写入清单failed: %v", err)
	}

	readCmdWithCwd(t, repoRoot, func() {
		got, err := resolveDiffManifestPath("m1.xml")
		if err != nil {
			t.Fatalf("resolveDiffManifestPath(m1.xml) failed: %v", err)
		}
		if want := filepath.Join(repoRoot, ".repo", "manifests", "m1.xml"); got != want {
			t.Errorf("相对清单名解析 = %q, want %q", got, want)
		}

		got, err = resolveDiffManifestPath(".repo/manifest.xml")
		if err != nil {
			t.Fatalf("resolveDiffManifestPath(.repo/manifest.xml) failed: %v", err)
		}
		if want := filepath.Join(repoRoot, ".repo", "manifest.xml"); got != want {
			t.Errorf("携带 .repo 前缀的路径 = %q, want %q", got, want)
		}

		if _, err := resolveDiffManifestPath("nope.xml"); err == nil {
			t.Error("不存在的清单应报错")
		}

		got, err = resolveDiffManifestPath(manifestName)
		if err != nil {
			t.Fatalf("绝对路径解析 failed: %v", err)
		}
		if got != manifestName {
			t.Errorf("绝对路径解析 = %q, want %q", got, manifestName)
		}
	})
}

// TestCompareManifestsDefaultResolvedRevision 验证默认比较解析后的提交 id，
// revisionExpr 不同但解析结果相同不算变更。
func TestCompareManifestsDefaultResolvedRevision(t *testing.T) {
	resolver := func(p manifest.Project) (string, bool) {
		switch p.Revision {
		case "rev1", "alias-of-rev1":
			return "commit-1", true
		case "rev2":
			return "commit-2", true
		}
		return "", false
	}
	m1 := &manifest.Manifest{Projects: []manifest.Project{{Name: "foo", Path: "foo", Revision: "rev1"}}}
	m2 := &manifest.Manifest{Projects: []manifest.Project{{Name: "foo", Path: "foo", Revision: "rev2"}}}
	log := logger.NewDefaultLogger()
	log.SetLevel(logger.LogLevelError)

	diffs := compareManifestsWithResolvers(m1, m2, &DiffManifestsOptions{}, log, resolver, resolver)
	if len(diffs) != 1 || !strings.Contains(diffs[0], "changed project: foo") {
		t.Fatalf("默认 revision 比较结果 = %v, want changed project", diffs)
	}

	m2.Projects[0].Revision = "alias-of-rev1"
	if diffs := compareManifestsWithResolvers(m1, m2, &DiffManifestsOptions{}, log, resolver, resolver); len(diffs) != 0 {
		t.Errorf("解析后提交 id 相同不应有差异，得到 %v", diffs)
	}
}

// TestCompareManifestsPathKey 验证以 path 为项目键：改路径/改名（无显式 path）
// 表现为 removed + added。
func TestCompareManifestsPathKey(t *testing.T) {
	never := func(manifest.Project) (string, bool) { return "", false }
	always := func(manifest.Project) (string, bool) { return "same", true }
	log := logger.NewDefaultLogger()
	log.SetLevel(logger.LogLevelError)

	tests := []struct {
		name string
		m1   *manifest.Manifest
		m2   *manifest.Manifest
		want []string
	}{
		{
			name: "改路径产生 removed 和 added",
			m1:   &manifest.Manifest{Projects: []manifest.Project{{Name: "foo", Path: "old/path", Revision: "r1"}}},
			m2:   &manifest.Manifest{Projects: []manifest.Project{{Name: "foo", Path: "new/path", Revision: "r1"}}},
			want: []string{"added project: new/path (revision r1)", "removed project: old/path (revision r1)"},
		},
		{
			name: "无显式 path 时改名等价于改路径",
			m1:   &manifest.Manifest{Projects: []manifest.Project{{Name: "foo", Revision: "r1"}}},
			m2:   &manifest.Manifest{Projects: []manifest.Project{{Name: "bar", Revision: "r1"}}},
			want: []string{"added project: bar (revision r1)", "removed project: foo (revision r1)"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := compareManifestsWithResolvers(tt.m1, tt.m2, &DiffManifestsOptions{}, log, always, always)
			if len(got) != len(tt.want) {
				t.Fatalf("diffs = %v, want %v", got, tt.want)
			}
			for i := range tt.want {
				if got[i] != tt.want[i] {
					t.Errorf("diffs[%d] = %q, want %q", i, got[i], tt.want[i])
				}
			}
			// raw 输出同样以 path 标识
			raw := compareManifestsWithResolvers(tt.m1, tt.m2, &DiffManifestsOptions{Raw: true}, log, never, never)
			if len(raw) != 2 || !strings.HasPrefix(raw[0], "A ") || !strings.HasPrefix(raw[1], "R ") {
				t.Errorf("raw diffs = %v, want A/R 行", raw)
			}
		})
	}
}

// TestCompareManifestsGroupsAndUnreachable 验证 --groups 选择与 unreachable 标记。
func TestCompareManifestsGroupsAndUnreachable(t *testing.T) {
	log := logger.NewDefaultLogger()
	log.SetLevel(logger.LogLevelError)
	always := func(manifest.Project) (string, bool) { return "same", true }

	m1 := &manifest.Manifest{Projects: []manifest.Project{{Name: "foo", Path: "foo", Revision: "r1", Groups: "a"}}}
	m2 := &manifest.Manifest{Projects: []manifest.Project{{Name: "foo", Path: "foo", Revision: "r1", Groups: "b"}}}

	if diffs := compareManifestsWithResolvers(m1, m2, &DiffManifestsOptions{}, log, always, always); len(diffs) != 0 {
		t.Errorf("默认不比较 groups，得到 %v", diffs)
	}
	diffs := compareManifestsWithResolvers(m1, m2, &DiffManifestsOptions{Groups: true}, log, always, always)
	if len(diffs) != 1 || !strings.Contains(diffs[0], "groups a -> b") {
		t.Errorf("--groups 结果 = %v", diffs)
	}

	unreachable := func(manifest.Project) (string, bool) { return "", false }
	m2u := &manifest.Manifest{Projects: []manifest.Project{{Name: "foo", Path: "foo", Revision: "r2", Groups: "b"}}}
	raw := compareManifestsWithResolvers(m1, m2u, &DiffManifestsOptions{Raw: true}, log, unreachable, unreachable)
	if len(raw) != 1 || !strings.HasPrefix(raw[0], "U foo ") {
		t.Errorf("unreachable raw = %v, want U 行", raw)
	}
}
