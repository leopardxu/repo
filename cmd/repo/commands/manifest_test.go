package commands

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/leopardxu/repo-go/internal/config"
	"github.com/leopardxu/repo-go/internal/git"
	"github.com/leopardxu/repo-go/internal/logger"
	"github.com/leopardxu/repo-go/internal/manifest"
	"github.com/leopardxu/repo-go/internal/project"
)

// testManifest 返回用于输出测试的最小清单。
func testManifest() *manifest.Manifest {
	return &manifest.Manifest{
		Default: manifest.Default{Remote: "origin", Revision: "main"},
		Remotes: []manifest.Remote{{Name: "origin", Fetch: "https://example.com"}},
		Projects: []manifest.Project{
			{Name: "alpha", Path: "alpha", Revision: "main"},
		},
	}
}

// TestResolveManifestFormat 验证 --format/--json 的解析与非法格式拒绝。
func TestResolveManifestFormat(t *testing.T) {
	tests := []struct {
		name      string
		format    string
		jsonFlag  bool
		want      string
		wantErr   bool
		errSubstr string
	}{
		{name: "默认xml", want: "xml"},
		{name: "显式json", format: "json", want: "json"},
		{name: "显式xml", format: "xml", want: "xml"},
		{name: "json旗标别名", jsonFlag: true, want: "json"},
		{name: "format优先于json旗标", format: "xml", jsonFlag: true, want: "xml"},
		{name: "非法格式拒绝", format: "yaml", wantErr: true, errSubstr: "invalid format"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveManifestFormat(&ManifestOptions{Format: tt.format, JSONOutput: tt.jsonFlag})
			if tt.wantErr {
				if err == nil {
					t.Fatalf("期望拒绝格式 %q，实际得到 %q", tt.format, got)
				}
				if !strings.Contains(err.Error(), tt.errSubstr) {
					t.Errorf("错误信息应包含 %q，实际: %v", tt.errSubstr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("期望格式 %q 合法，实际得到错误: %v", tt.want, err)
			}
			if got != tt.want {
				t.Errorf("resolveManifestFormat() = %q, 期望 %q", got, tt.want)
			}
		})
	}
}

// TestWriteManifestOutputDashIsStdout 验证 -o -（以及默认空路径）写 stdout，
// 绝不创建名为 "-" 的文件。
func TestWriteManifestOutputDashIsStdout(t *testing.T) {
	dir := t.TempDir()
	oldwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("获取当前目录failed: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("切换到临时目录failed: %v", err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(oldwd); err != nil {
			t.Errorf("恢复工作目录failed: %v", err)
		}
	})

	for _, outputFile := range []string{"-", ""} {
		t.Run("output="+outputFile, func(t *testing.T) {
			var stdout bytes.Buffer
			if err := writeManifestOutput(testManifest(), "xml", outputFile, &stdout); err != nil {
				t.Fatalf("输出清单failed: %v", err)
			}
			if !strings.Contains(stdout.String(), `<project name="alpha"`) {
				t.Errorf("stdout 应包含项目 alpha，实际: %q", stdout.String())
			}
			if _, err := os.Stat("-"); !os.IsNotExist(err) {
				t.Errorf("不得创建名为 \"-\" 的文件，stat err=%v", err)
			}
		})
	}
}

// TestWriteManifestOutputJSONFile 验证 --format=json 与 -o 组合时写入 JSON
// （而非旧行为中的 XML），并验证 JSON 内容可解析。
func TestWriteManifestOutputJSONFile(t *testing.T) {
	outPath := filepath.Join(t.TempDir(), "out.json")
	opts := &ManifestOptions{Format: "json", OutputFile: outPath}
	format, err := resolveManifestFormat(opts)
	if err != nil {
		t.Fatalf("解析格式failed: %v", err)
	}
	if err := writeManifestOutput(testManifest(), format, opts.OutputFile, io.Discard); err != nil {
		t.Fatalf("写入 JSON 清单failed: %v", err)
	}

	data, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("读取输出文件failed: %v", err)
	}
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		t.Fatalf("输出文件应为 JSON，实际开头: %q", string(trimmed[:min(len(trimmed), 32)]))
	}
	if !json.Valid(trimmed) {
		t.Fatalf("输出文件不是合法 JSON: %s", string(trimmed))
	}
	var doc struct {
		Projects []struct {
			Name string `json:"name"`
		} `json:"projects"`
	}
	if err := json.Unmarshal(trimmed, &doc); err != nil {
		t.Fatalf("解析 JSON failed: %v", err)
	}
	if len(doc.Projects) != 1 || doc.Projects[0].Name != "alpha" {
		t.Errorf("JSON projects = %+v, 期望包含 alpha", doc.Projects)
	}
}

// TestWriteManifestOutputInvalidFormat 验证非法格式即使指定 -o 也报错。
func TestWriteManifestOutputInvalidFormat(t *testing.T) {
	outPath := filepath.Join(t.TempDir(), "out.txt")
	err := writeManifestOutput(testManifest(), "yaml", outPath, io.Discard)
	if err == nil {
		t.Fatal("期望非法格式报错，实际成功")
	}
	if !strings.Contains(err.Error(), "invalid format") {
		t.Errorf("错误信息应包含 invalid format，实际: %v", err)
	}
	if _, statErr := os.Stat(outPath); !os.IsNotExist(statErr) {
		t.Errorf("非法格式不应写出文件，stat err=%v", statErr)
	}
}

// gitInitWithCommit 在 dir 创建一个带空提交的 git 仓库并返回 HEAD sha。
func gitInitWithCommit(t *testing.T, dir, message string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("创建目录 %s failed: %v", dir, err)
	}
	runner := git.NewRunner()
	if out, err := runner.RunInDir(dir, "init", "-q"); err != nil {
		t.Fatalf("git init %s failed: %v: %s", dir, err, out)
	}
	// 固定分支名为 main：镜像快照按清单 revision 解析 refs/heads/main。
	if out, err := runner.RunInDir(dir, "symbolic-ref", "HEAD", "refs/heads/main"); err != nil {
		t.Fatalf("git symbolic-ref %s failed: %v: %s", dir, err, out)
	}
	if out, err := runner.RunInDir(dir,
		"-c", "user.name=repo-test",
		"-c", "user.email=repo-test@example.com",
		"commit", "--allow-empty", "-q", "-m", message,
	); err != nil {
		t.Fatalf("git commit %s failed: %v: %s", dir, err, out)
	}
	out, err := runner.RunInDir(dir, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("git rev-parse %s failed: %v: %s", dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

// TestSnapshotSameNameMultiplePaths 验证同名多 path 项目分别锁定自己的
// 工作树 HEAD，而不是全部取第一个同名项目的 HEAD；同时验证 upstream/
// dest-branch 默认生成与 Suppress 选项。
func TestSnapshotSameNameMultiplePaths(t *testing.T) {
	repoDir := t.TempDir()
	firstDir := filepath.Join(repoDir, "first")
	secondDir := filepath.Join(repoDir, "second")
	firstSHA := gitInitWithCommit(t, firstDir, "first")
	secondSHA := gitInitWithCommit(t, secondDir, "second")
	if firstSHA == secondSHA {
		t.Fatalf("测试前置失败：两个仓库的提交哈希不应相同")
	}

	tests := []struct {
		name     string
		opts     *ManifestOptions
		wantUp   []string
		wantDest []string
	}{
		{
			name:     "默认生成upstream与dest-branch",
			opts:     &ManifestOptions{Jobs: 2},
			wantUp:   []string{"refs/heads/main", "dev"},
			wantDest: []string{"refs/heads/main", "dev"},
		},
		{
			name: "suppress清空两字段",
			opts: &ManifestOptions{
				Jobs:                     2,
				SuppressUpstreamRevision: true,
				SuppressDestBranch:       true,
			},
			wantUp:   []string{"", ""},
			wantDest: []string{"", ""},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := &manifest.Manifest{
				Topdir:  repoDir,
				Default: manifest.Default{Remote: "origin", Revision: "main"},
				Remotes: []manifest.Remote{{Name: "origin", Fetch: "https://example.com"}},
				Projects: []manifest.Project{
					{Name: "dup", Path: "first", Revision: "refs/heads/main"},
					{Name: "dup", Path: "second", Revision: "dev"},
				},
			}
			log := logger.NewDefaultLogger()
			log.SetLevel(logger.LogLevelError)

			snapshot, err := createSnapshotManifest(m, nil, tt.opts, log)
			if err != nil {
				t.Fatalf("创建快照failed: %v", err)
			}
			if snapshot.Projects[0].Revision != firstSHA {
				t.Errorf("first revision = %q, 期望 %q", snapshot.Projects[0].Revision, firstSHA)
			}
			if snapshot.Projects[1].Revision != secondSHA {
				t.Errorf("second revision = %q, 期望 %q", snapshot.Projects[1].Revision, secondSHA)
			}
			for i := range snapshot.Projects {
				if snapshot.Projects[i].Upstream != tt.wantUp[i] {
					t.Errorf("project[%d].Upstream = %q, 期望 %q", i, snapshot.Projects[i].Upstream, tt.wantUp[i])
				}
				if snapshot.Projects[i].DestBranch != tt.wantDest[i] {
					t.Errorf("project[%d].DestBranch = %q, 期望 %q", i, snapshot.Projects[i].DestBranch, tt.wantDest[i])
				}
			}
			// 快照不得反过来修改调用方清单的 revision 表达式
			if m.Projects[0].Revision != "refs/heads/main" || m.Projects[1].Revision != "dev" {
				t.Errorf("快照不应修改调用方清单: %q, %q", m.Projects[0].Revision, m.Projects[1].Revision)
			}
		})
	}
}

// TestSnapshotMissingProjectReturnsError 验证项目缺失或 HEAD 无法解析时
// 返回错误而非静默产出混合快照。
func TestSnapshotMissingProjectReturnsError(t *testing.T) {
	repoDir := t.TempDir()
	okDir := filepath.Join(repoDir, "ok")
	gitInitWithCommit(t, okDir, "ok")

	tests := []struct {
		name  string
		projs []manifest.Project
	}{
		{
			name: "项目工作树缺失",
			projs: []manifest.Project{
				{Name: "ok", Path: "ok", Revision: "main"},
				{Name: "missing", Path: "missing", Revision: "main"},
			},
		},
		{
			name: "项目无HEAD提交",
			projs: []manifest.Project{
				{Name: "nocommit", Path: "nocommit", Revision: "main"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.name == "项目无HEAD提交" {
				dir := filepath.Join(repoDir, "nocommit")
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatalf("创建目录failed: %v", err)
				}
				if out, err := git.NewRunner().RunInDir(dir, "init", "-q"); err != nil {
					t.Fatalf("git init failed: %v: %s", err, out)
				}
			}
			m := &manifest.Manifest{
				RepoDir:  repoDir,
				Default:  manifest.Default{Remote: "origin", Revision: "main"},
				Remotes:  []manifest.Remote{{Name: "origin", Fetch: "https://example.com"}},
				Projects: tt.projs,
			}
			log := logger.NewDefaultLogger()
			log.SetLevel(logger.LogLevelError)
			if _, err := createSnapshotManifest(m, nil, &ManifestOptions{Jobs: 2}, log); err == nil {
				t.Fatal("期望快照创建失败，实际成功")
			}
		})
	}
}

// gitBareWithCommit 在 dir 创建含单个提交的裸仓库并返回 HEAD sha。
func gitBareWithCommit(t *testing.T, dir, message string) string {
	t.Helper()
	work := t.TempDir()
	sha := gitInitWithCommit(t, work, message)
	if out, err := git.NewRunner().RunInDir(filepath.Dir(dir), "clone", "-q", "--bare", work, dir); err != nil {
		t.Fatalf("git clone --bare %s failed: %v: %s", dir, err, out)
	}
	return sha
}

// TestSnapshotMirrorUsesGitdir 验证镜像项目（Worktree 为空）执行 manifest -r
// 时改用 Project.Gitdir 作为仓库目录读取 HEAD。
func TestSnapshotMirrorUsesGitdir(t *testing.T) {
	repoRoot := t.TempDir()
	sha := gitBareWithCommit(t, filepath.Join(repoRoot, "pkg.git"), "mirror")

	m := &manifest.Manifest{
		Topdir:  repoRoot,
		RepoDir: filepath.Join(repoRoot, ".repo"),
		Default: manifest.Default{Remote: "origin", Revision: "main"},
		Remotes: []manifest.Remote{{Name: "origin", Fetch: "https://example.com"}},
		// Path 为清单 relpath，与镜像仓库目录（<topdir>/pkg.git）不同。
		Projects: []manifest.Project{{Name: "pkg", Path: "mirror-path", Revision: "main"}},
	}
	cfg := &config.Config{Mirror: true}
	log := logger.NewDefaultLogger()
	log.SetLevel(logger.LogLevelError)

	snapshot, err := createSnapshotManifest(m, cfg, &ManifestOptions{Jobs: 1}, log)
	if err != nil {
		t.Fatalf("镜像快照创建failed: %v", err)
	}
	if got := snapshot.Projects[0].Revision; got != sha {
		t.Errorf("revision = %q, want mirror HEAD %q", got, sha)
	}
}

// manifestTestRepoRoot 创建最小 repo 根目录（.repo/config.json + 主清单），
// 主清单含 remote/default 及给定 projects XML 片段。
func manifestTestRepoRoot(t *testing.T, projectsXML string) string {
	t.Helper()
	// 避免宿主环境的 REPO_* 覆盖测试配置。
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

// withManifestTestCwd 切换到 repoRoot 执行 fn，结束后恢复原目录。
func withManifestTestCwd(t *testing.T, repoRoot string, fn func()) {
	t.Helper()
	oldwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("获取当前目录failed: %v", err)
	}
	if err := os.Chdir(repoRoot); err != nil {
		t.Fatalf("切换到 repo 根目录failed: %v", err)
	}
	defer func() {
		if err := os.Chdir(oldwd); err != nil {
			t.Errorf("恢复工作目录failed: %v", err)
		}
	}()
	fn()
}

// TestRunManifestSnapshotStdoutOnlyPayload 验证 --snapshot -o - 的 stdout 只含清单
// payload：默认日志级别下不得出现 [INFO] 或日志文本（依赖 logger 非 Error 输出到 stderr）。
func TestRunManifestSnapshotStdoutOnlyPayload(t *testing.T) {
	repoRoot := manifestTestRepoRoot(t, "")

	withManifestTestCwd(t, repoRoot, func() {
		stdoutFile, err := os.CreateTemp(t.TempDir(), "manifest-stdout-*")
		if err != nil {
			t.Fatalf("创建 stdout 临时文件failed: %v", err)
		}
		stderrFile, err := os.CreateTemp(t.TempDir(), "manifest-stderr-*")
		if err != nil {
			t.Fatalf("创建 stderr 临时文件failed: %v", err)
		}
		oldStdout, oldStderr := os.Stdout, os.Stderr
		os.Stdout, os.Stderr = stdoutFile, stderrFile
		defer func() {
			os.Stdout, os.Stderr = oldStdout, oldStderr
			stdoutFile.Close()
			stderrFile.Close()
		}()

		if err := runManifest(&ManifestOptions{Snapshot: true, OutputFile: "-", Jobs: 2}, nil); err != nil {
			t.Fatalf("runManifest(--snapshot -o -) failed: %v", err)
		}
		os.Stdout, os.Stderr = oldStdout, oldStderr

		stdoutData, err := os.ReadFile(stdoutFile.Name())
		if err != nil {
			t.Fatalf("读取 stdout failed: %v", err)
		}
		out := string(stdoutData)
		if !strings.Contains(out, "<manifest") {
			t.Errorf("stdout 应包含清单 payload，实际: %q", out)
		}
		if strings.Contains(out, "[INFO]") {
			t.Errorf("stdout 不得包含日志 [INFO]，实际: %q", out)
		}
		if strings.Contains(out, "清单快照") {
			t.Errorf("stdout 不得包含日志文本，实际: %q", out)
		}
	})
}

// TestRunManifestNoLocalManifests 验证 --no-local-manifests 关闭 local_manifests
// 合并；未指定时 local manifest 中的项目仍被纳入。
func TestRunManifestNoLocalManifests(t *testing.T) {
	tests := []struct {
		name     string
		noLocal  bool
		wantBeta bool
	}{
		{name: "默认合并 local_manifests", noLocal: false, wantBeta: true},
		{name: "--no-local-manifests 忽略 local_manifests", noLocal: true, wantBeta: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repoRoot := manifestTestRepoRoot(t, `  <project name="alpha" path="alpha" />`)

			// local manifest 追加 beta 项目。
			localDir := filepath.Join(repoRoot, ".repo", "local_manifests")
			if err := os.MkdirAll(localDir, 0o755); err != nil {
				t.Fatalf("创建 local_manifests failed: %v", err)
			}
			localXML := `<manifest>
  <project name="beta" path="beta" />
</manifest>
`
			if err := os.WriteFile(filepath.Join(localDir, "local.xml"), []byte(localXML), 0o644); err != nil {
				t.Fatalf("写入 local.xml failed: %v", err)
			}

			outPath := filepath.Join(t.TempDir(), "out.xml")
			withManifestTestCwd(t, repoRoot, func() {
				opts := &ManifestOptions{
					OutputFile:       outPath,
					NoLocalManifests: tt.noLocal,
					Quiet:            true,
					Jobs:             2,
				}
				if err := runManifest(opts, nil); err != nil {
					t.Fatalf("runManifest failed: %v", err)
				}
			})

			data, err := os.ReadFile(outPath)
			if err != nil {
				t.Fatalf("读取输出 failed: %v", err)
			}
			out := string(data)
			if !strings.Contains(out, `name="alpha"`) {
				t.Errorf("输出应包含 alpha，实际: %s", out)
			}
			gotBeta := strings.Contains(out, `name="beta"`)
			if gotBeta != tt.wantBeta {
				t.Errorf("beta present = %v, want %v; output: %s", gotBeta, tt.wantBeta, out)
			}
		})
	}
}

// manifestGitAddCommit 在已有仓库中追加一个空提交并返回 sha。
func manifestGitAddCommit(t *testing.T, dir, message string) string {
	t.Helper()
	runner := git.NewRunner()
	if out, err := runner.RunInDir(dir,
		"-c", "user.name=repo-test",
		"-c", "user.email=repo-test@example.com",
		"commit", "--allow-empty", "-q", "-m", message,
	); err != nil {
		t.Fatalf("git commit %s failed: %v: %s", dir, err, out)
	}
	out, err := runner.RunInDir(dir, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("git rev-parse %s failed: %v: %s", dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

// manifestMirrorBareFixture 创建一个含两个提交和 v1 tag 的裸镜像仓库：
// HEAD/main 指向 B，refs/tags/v1 指向 A。
func manifestMirrorBareFixture(t *testing.T) (bare, shaA, shaB string) {
	t.Helper()
	work := t.TempDir()
	shaA = gitInitWithCommit(t, work, "A")
	runner := git.NewRunner()
	if out, err := runner.RunInDir(work, "tag", "v1", shaA); err != nil {
		t.Fatalf("git tag failed: %v: %s", err, out)
	}
	shaB = manifestGitAddCommit(t, work, "B")
	bare = filepath.Join(t.TempDir(), "pkg.git")
	if out, err := runner.RunInDir(filepath.Dir(bare), "clone", "-q", "--bare", work, bare); err != nil {
		t.Fatalf("git clone --bare failed: %v: %s", err, out)
	}
	return bare, shaA, shaB
}

// TestManifestResolveMirrorRevision 验证镜像裸库按清单 revision 解析提交：
// 支持 refs/heads、refs/tags、refs/remotes 与裸 SHA，空/不存在时返回错误。
func TestManifestResolveMirrorRevision(t *testing.T) {
	bare, shaA, shaB := manifestMirrorBareFixture(t)
	runner := git.NewRunner()
	if out, err := runner.RunInDir(bare, "update-ref", "refs/remotes/origin/dev", shaB); err != nil {
		t.Fatalf("update-ref failed: %v: %s", err, out)
	}
	proj := &project.Project{
		Name:       "pkg",
		RemoteName: "origin",
		Mirror:     true,
		GitRepo:    git.NewRepository(bare, runner),
	}

	tests := []struct {
		name     string
		revision string
		want     string
		wantErr  bool
	}{
		{name: "分支名解析 refs/heads", revision: "main", want: shaB},
		{name: "refs/heads 完整 ref", revision: "refs/heads/main", want: shaB},
		{name: "refs/tags", revision: "refs/tags/v1", want: shaA},
		{name: "裸 tag 名回退 refs/tags", revision: "v1", want: shaA},
		{name: "refs/remotes", revision: "refs/remotes/origin/dev", want: shaB},
		{name: "裸 SHA", revision: shaA, want: shaA},
		{name: "空 revision 报错", revision: "", wantErr: true},
		{name: "不存在 revision 报错", revision: "missing", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := manifestResolveMirrorRevision(proj, bare, tt.revision)
			if (err != nil) != tt.wantErr {
				t.Fatalf("error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if got != tt.want {
				t.Errorf("manifestResolveMirrorRevision(%q) = %q, want %q", tt.revision, got, tt.want)
			}
		})
	}
}

// TestSnapshotMirrorUsesManifestRevisionNotHEAD 验证镜像 -r/--snapshot 使用清单
// revision（refs/tags）解析提交，而不是裸库 HEAD。
func TestSnapshotMirrorUsesManifestRevisionNotHEAD(t *testing.T) {
	bare, shaA, shaB := manifestMirrorBareFixture(t)
	if shaA == shaB {
		t.Fatal("测试前置失败：两个提交 sha 不应相同")
	}
	repoRoot := filepath.Dir(bare)

	m := &manifest.Manifest{
		Topdir:  repoRoot,
		RepoDir: filepath.Join(repoRoot, ".repo"),
		Default: manifest.Default{Remote: "origin", Revision: "main"},
		Remotes: []manifest.Remote{{Name: "origin", Fetch: "https://example.com"}},
		Projects: []manifest.Project{
			{Name: "pkg", Path: "pkg.git", Revision: "refs/tags/v1"},
		},
	}
	cfg := &config.Config{Mirror: true}
	log := logger.NewDefaultLogger()
	log.SetLevel(logger.LogLevelError)

	snapshot, err := createSnapshotManifest(m, cfg, &ManifestOptions{Jobs: 1}, log)
	if err != nil {
		t.Fatalf("镜像快照创建failed: %v", err)
	}
	if got := snapshot.Projects[0].Revision; got != shaA {
		t.Errorf("revision = %q, want tag commit %q (HEAD is %q)", got, shaA, shaB)
	}
}

// TestSnapshotMirrorMissingRevisionReturnsError 验证镜像 revision 无法解析时
// 快照返回错误，绝不回退 HEAD。
func TestSnapshotMirrorMissingRevisionReturnsError(t *testing.T) {
	bare, _, _ := manifestMirrorBareFixture(t)
	repoRoot := filepath.Dir(bare)

	m := &manifest.Manifest{
		Topdir:  repoRoot,
		RepoDir: filepath.Join(repoRoot, ".repo"),
		Default: manifest.Default{Remote: "origin", Revision: "main"},
		Remotes: []manifest.Remote{{Name: "origin", Fetch: "https://example.com"}},
		Projects: []manifest.Project{
			{Name: "pkg", Path: "pkg.git", Revision: "no-such-revision"},
		},
	}
	log := logger.NewDefaultLogger()
	log.SetLevel(logger.LogLevelError)

	if _, err := createSnapshotManifest(m, &config.Config{Mirror: true}, &ManifestOptions{Jobs: 1}, log); err == nil {
		t.Fatal("期望镜像快照解析失败，实际成功")
	}
}
