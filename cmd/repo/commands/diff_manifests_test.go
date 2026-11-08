package commands

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/leopardxu/repo-go/internal/config"
	"github.com/leopardxu/repo-go/internal/git"
	"github.com/leopardxu/repo-go/internal/logger"
	"github.com/leopardxu/repo-go/internal/manifest"
)

// diffTestInitRepo 初始化一个临时 git 仓库并提交一个空 commit，返回提交 id。
func diffTestInitRepo(t *testing.T, dir string) string {
	t.Helper()
	runner := git.NewRunner()
	run := func(args ...string) string {
		t.Helper()
		out, err := runner.RunInDir(dir, args...)
		if err != nil {
			t.Fatalf("git %v failed: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run("init", "-q")
	run("config", "user.email", "test@example.com")
	run("config", "user.name", "Test")
	run("commit", "--allow-empty", "-q", "-m", "initial")
	return run("rev-parse", "HEAD")
}

// diffTestCommit 在仓库创建一个新空提交并返回提交 id。
func diffTestCommit(t *testing.T, dir, message string) string {
	t.Helper()
	runner := git.NewRunner()
	if out, err := runner.RunInDir(dir,
		"-c", "user.name=repo-test",
		"-c", "user.email=repo-test@example.com",
		"commit", "--allow-empty", "-q", "-m", message,
	); err != nil {
		t.Fatalf("git commit %s failed: %v: %s", message, err, out)
	}
	out, err := runner.RunInDir(dir, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("git rev-parse HEAD failed: %v: %s", err, out)
	}
	return strings.TrimSpace(string(out))
}

// TestResolveDiffManifestPathFromSubdir 验证相对清单名从任意 cwd 优先按
// repo 根目录的 .repo/manifests 解析（并兼容 .repo/manifest.xml 写法）。
func TestResolveDiffManifestPathFromSubdir(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".repo", "manifests"), 0o755); err != nil {
		t.Fatalf("创建 .repo/manifests failed: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(root, "sub", "deep"), 0o755); err != nil {
		t.Fatalf("创建子目录 failed: %v", err)
	}
	manifestFile := filepath.Join(root, ".repo", "manifests", "a.xml")
	if err := os.WriteFile(manifestFile, []byte("<manifest/>"), 0o644); err != nil {
		t.Fatalf("写入 a.xml failed: %v", err)
	}
	defaultFile := filepath.Join(root, ".repo", "manifest.xml")
	if err := os.WriteFile(defaultFile, []byte("<manifest/>"), 0o644); err != nil {
		t.Fatalf("写入 manifest.xml failed: %v", err)
	}
	// cwd 下也放一份同名文件，验证 .repo/manifests 优先。
	if err := os.WriteFile(filepath.Join(root, "sub", "deep", "a.xml"), []byte("<manifest/>"), 0o644); err != nil {
		t.Fatalf("写入 cwd a.xml failed: %v", err)
	}

	readCmdWithCwd(t, filepath.Join(root, "sub", "deep"), func() {
		got, err := resolveDiffManifestPath("a.xml")
		if err != nil {
			t.Fatalf("resolveDiffManifestPath(a.xml) failed: %v", err)
		}
		if got != manifestFile {
			t.Errorf("resolveDiffManifestPath(a.xml) = %q, want %q", got, manifestFile)
		}

		got, err = resolveDiffManifestPath(".repo/manifest.xml")
		if err != nil {
			t.Fatalf("resolveDiffManifestPath(.repo/manifest.xml) failed: %v", err)
		}
		if got != defaultFile {
			t.Errorf("resolveDiffManifestPath(.repo/manifest.xml) = %q, want %q", got, defaultFile)
		}
	})
}

// TestDiffRevisionResolverRemoteFirst 验证 revision 解析顺序：远端跟踪引用
// 优先于同名本地分支；远端缺失时才回退本地；SHA/tag 原样解析。
func TestDiffRevisionResolverRemoteFirst(t *testing.T) {
	dir := t.TempDir()
	localID := diffTestInitRepo(t, dir)
	remoteID := diffTestCommit(t, dir, "remote")

	runner := git.NewRunner()
	runGit := func(args ...string) {
		t.Helper()
		if out, err := runner.RunInDir(dir, args...); err != nil {
			t.Fatalf("git %v failed: %v: %s", args, err, out)
		}
	}
	// 本地分支 dev 指向 localID，远端 origin/dev 指向 remoteID。
	runGit("branch", "dev", localID)
	runGit("update-ref", "refs/remotes/origin/dev", remoteID)
	runGit("tag", "v-tag", remoteID)

	resolver := diffDefaultRevisionResolver("origin", nil)
	tests := []struct {
		name string
		proj manifest.Project
		want string
		ok   bool
	}{
		{
			name: "分支名远端优先",
			proj: manifest.Project{Name: "foo", Path: dir, Revision: "dev", Remote: "origin"},
			want: remoteID,
			ok:   true,
		},
		{
			name: "refs/heads 远端优先",
			proj: manifest.Project{Name: "foo", Path: dir, Revision: "refs/heads/dev", Remote: "origin"},
			want: remoteID,
			ok:   true,
		},
		{
			name: "SHA 原样",
			proj: manifest.Project{Name: "foo", Path: dir, Revision: localID},
			want: localID,
			ok:   true,
		},
		{
			name: "refs/tags 原样",
			proj: manifest.Project{Name: "foo", Path: dir, Revision: "refs/tags/v-tag"},
			want: remoteID,
			ok:   true,
		},
		{
			// 裸 revision 只按分支名处理：存在同名 tag 也不能解析成 tag。
			name: "裸 tag 名不按 tag 解析",
			proj: manifest.Project{Name: "foo", Path: dir, Revision: "v-tag", Remote: "origin"},
			want: "",
			ok:   false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := resolver(tt.proj)
			if ok != tt.ok || got != tt.want {
				t.Errorf("resolver(%q) = (%q, %v), want (%q, %v)", tt.proj.Revision, got, ok, tt.want, tt.ok)
			}
		})
	}

	// remote 声明 alias 时，git 里的远端名是 alias，清单项目引用 name 也要解析。
	runGit("update-ref", "refs/remotes/up/dev", remoteID)
	aliasResolver := diffDefaultRevisionResolver("upstream", []manifest.Remote{
		{Name: "upstream", Alias: "up", Fetch: "../up"},
	})
	got, ok := aliasResolver(manifest.Project{Name: "foo", Path: dir, Revision: "dev", Remote: "upstream"})
	if !ok || got != remoteID {
		t.Errorf("alias remote 解析 = (%q, %v), want (%q, true)", got, ok, remoteID)
	}

	// 删除远端跟踪引用后回退本地分支。
	runGit("update-ref", "-d", "refs/remotes/origin/dev")
	got, ok = resolver(manifest.Project{Name: "foo", Path: dir, Revision: "dev", Remote: "origin"})
	if !ok || got != localID {
		t.Errorf("远端缺失时回退本地分支 = (%q, %v), want (%q, true)", got, ok, localID)
	}
}

// TestDiffRevisionStatusResolverDefaultRemoteAlias 验证未声明项目 remote 时
// 默认 remote 的 alias 同样会被映射。
func TestDiffRevisionStatusResolverDefaultRemoteAlias(t *testing.T) {
	dir := t.TempDir()
	localID := diffTestInitRepo(t, dir)
	runner := git.NewRunner()
	if out, err := runner.RunInDir(dir, "update-ref", "refs/remotes/up/dev", localID); err != nil {
		t.Fatalf("update-ref failed: %v: %s", err, out)
	}
	resolver := diffDefaultRevisionResolver("upstream", []manifest.Remote{{Name: "upstream", Alias: "up"}})
	got, ok := resolver(manifest.Project{Name: "foo", Path: dir, Revision: "dev"})
	if !ok || got != localID {
		t.Errorf("默认 remote alias 解析 = (%q, %v), want (%q, true)", got, ok, localID)
	}
}

// TestDiffRevisionStatusResolverMissing 验证未检出项目返回 missing 而不是不可解析。
func TestDiffRevisionStatusResolverMissing(t *testing.T) {
	dir := t.TempDir()
	status := diffDefaultRevisionStatusResolver("origin", nil)
	res := status(manifest.Project{Name: "absent", Path: filepath.Join(dir, "absent"), Revision: "main"})
	if !res.Missing {
		t.Errorf("未检出项目应返回 Missing=true, got %+v", res)
	}
	if res.Resolved {
		t.Errorf("未检出项目不应解析成功, got %+v", res)
	}
}

// TestCompareManifestsMissingRawSilent 验证 missing 项目在 raw 模式不输出，
// 非 raw 明确标注 missing 而非 unreachable。
func TestCompareManifestsMissingRawSilent(t *testing.T) {
	manifest1 := &manifest.Manifest{Projects: []manifest.Project{
		{Name: "foo", Path: "foo", Revision: "main"},
	}}
	manifest2 := &manifest.Manifest{Projects: []manifest.Project{
		{Name: "foo", Path: "foo", Revision: "main"},
	}}
	missing := func(manifest.Project) diffRevisionResolution {
		return diffRevisionResolution{Missing: true}
	}
	log := logger.NewDefaultLogger()

	raw := compareManifestsWithStatusResolvers(manifest1, manifest2, &DiffManifestsOptions{Raw: true}, log, missing, missing)
	if len(raw) != 0 {
		t.Errorf("raw 模式 missing 项目不应输出, got %v", raw)
	}

	text := compareManifestsWithStatusResolvers(manifest1, manifest2, &DiffManifestsOptions{}, log, missing, missing)
	if len(text) != 1 || !strings.Contains(text[0], "missing project: foo") {
		t.Errorf("非 raw 模式应标注 missing project, got %v", text)
	}
	if len(text) == 1 && strings.Contains(text[0], "unreachable") {
		t.Errorf("missing 不得报 unreachable: %v", text)
	}
}

// TestCompareManifestsSameRevisionEqual 验证 revision 字符串相同时直接判等，
// 即使解析结果不同或不可解析也不输出差异。
func TestCompareManifestsSameRevisionEqual(t *testing.T) {
	manifest1 := &manifest.Manifest{Projects: []manifest.Project{
		{Name: "foo", Path: "foo", Revision: "dev"},
	}}
	manifest2 := &manifest.Manifest{Projects: []manifest.Project{
		{Name: "foo", Path: "foo", Revision: "dev"},
	}}
	log := logger.NewDefaultLogger()

	different := func(id string) diffRevisionStatusResolver {
		return func(manifest.Project) diffRevisionResolution {
			return diffRevisionResolution{CommitID: id, Resolved: true}
		}
	}
	got := compareManifestsWithStatusResolvers(manifest1, manifest2, &DiffManifestsOptions{Raw: true}, log,
		different("aaa"), different("bbb"))
	if len(got) != 0 {
		t.Errorf("相同 revision 应直接判等, got %v", got)
	}

	unresolved := func(manifest.Project) diffRevisionResolution { return diffRevisionResolution{} }
	got = compareManifestsWithStatusResolvers(manifest1, manifest2, &DiffManifestsOptions{Raw: true}, log, unresolved, unresolved)
	if len(got) != 0 {
		t.Errorf("相同 revision 不可解析时也应判等, got %v", got)
	}
}

// TestCompareManifestsUnreachableAndChanged 验证不同 revision 的 unreachable
// 与 changed 输出。
func TestCompareManifestsUnreachableAndChanged(t *testing.T) {
	log := logger.NewDefaultLogger()
	unresolved := func(manifest.Project) diffRevisionResolution { return diffRevisionResolution{} }
	resolver := func(id string) diffRevisionStatusResolver {
		return func(manifest.Project) diffRevisionResolution {
			return diffRevisionResolution{CommitID: id, Resolved: true}
		}
	}

	unreachableManifests := func() (*manifest.Manifest, *manifest.Manifest) {
		return &manifest.Manifest{Projects: []manifest.Project{{Name: "foo", Path: "foo", Revision: "a"}}},
			&manifest.Manifest{Projects: []manifest.Project{{Name: "foo", Path: "foo", Revision: "b"}}}
	}

	m1, m2 := unreachableManifests()
	got := compareManifestsWithStatusResolvers(m1, m2, &DiffManifestsOptions{Raw: true}, log, unresolved, unresolved)
	if len(got) != 1 || !strings.HasPrefix(got[0], "U foo ") {
		t.Errorf("不可解析 revision 应输出 U, got %v", got)
	}

	m1, m2 = unreachableManifests()
	got = compareManifestsWithStatusResolvers(m1, m2, &DiffManifestsOptions{Raw: true}, log, resolver("aaa"), resolver("bbb"))
	if len(got) != 1 || !strings.HasPrefix(got[0], "C foo a b") {
		t.Errorf("提交不同应输出 C, got %v", got)
	}
}

// TestDiffSecondManifestInput 验证省略第二个清单时优先合并快照
// `.repo/manifest.xml`，快照缺失才回退配置的 ManifestName。
func TestDiffSecondManifestInput(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".repo"), 0o755); err != nil {
		t.Fatalf("创建 .repo failed: %v", err)
	}
	snapshot := filepath.Join(root, ".repo", "manifest.xml")
	if err := os.WriteFile(snapshot, []byte("<manifest/>"), 0o644); err != nil {
		t.Fatalf("写入合并清单 failed: %v", err)
	}
	cfg := &config.Config{ManifestName: "from-config.xml"}

	readCmdWithCwd(t, root, func() {
		if got := diffSecondManifestInput(cfg); got != defaultDiffManifestPath {
			t.Errorf("存在合并快照时应优先使用 %q, got %q", defaultDiffManifestPath, got)
		}

		if err := os.Remove(snapshot); err != nil {
			t.Fatalf("删除合并快照 failed: %v", err)
		}
		if got := diffSecondManifestInput(cfg); got != "from-config.xml" {
			t.Errorf("快照缺失时应回退 ManifestName, got %q", got)
		}
		if got := diffSecondManifestInput(nil); got != defaultDiffManifestPath {
			t.Errorf("无配置时应回退默认路径, got %q", got)
		}
	})
}

// TestCompareManifestsNormalizesTrailingSlash 验证 path/name 尾部 "/" 归一化，
// 同一路径不会因书写形式不同被判为 removed + added。
func TestCompareManifestsNormalizesTrailingSlash(t *testing.T) {
	log := logger.NewDefaultLogger()
	resolver := func(manifest.Project) diffRevisionResolution {
		return diffRevisionResolution{CommitID: "same", Resolved: true}
	}
	// path 带尾部 "/" 与不带时视为同一项目。
	m1 := &manifest.Manifest{Projects: []manifest.Project{{Name: "foo", Path: "foo/", Revision: "dev"}}}
	m2 := &manifest.Manifest{Projects: []manifest.Project{{Name: "foo", Path: "foo", Revision: "dev"}}}
	if got := compareManifestsWithStatusResolvers(m1, m2, &DiffManifestsOptions{Raw: true}, log, resolver, resolver); len(got) != 0 {
		t.Errorf("尾部 / 应归一化为同一键, got %v", got)
	}

	// 无 path 时回退 name，name 尾部 "/" 同样归一化。
	m1 = &manifest.Manifest{Projects: []manifest.Project{{Name: "bar/", Revision: "dev"}}}
	m2 = &manifest.Manifest{Projects: []manifest.Project{{Name: "bar", Revision: "dev"}}}
	if got := compareManifestsWithStatusResolvers(m1, m2, &DiffManifestsOptions{Raw: true}, log, resolver, resolver); len(got) != 0 {
		t.Errorf("name 尾部 / 应归一化为同一键, got %v", got)
	}
}

// TestCompareManifestsMissingComparesAttributes 验证 missing 只短路 revision，
// name/groups 等属性仍然比较；raw 模式不输出 missing 行。
func TestCompareManifestsMissingComparesAttributes(t *testing.T) {
	log := logger.NewDefaultLogger()
	missing := func(manifest.Project) diffRevisionResolution {
		return diffRevisionResolution{Missing: true}
	}
	m1 := &manifest.Manifest{Projects: []manifest.Project{
		{Name: "old", Path: "foo", Revision: "a", Groups: "g1"},
	}}
	m2 := &manifest.Manifest{Projects: []manifest.Project{
		{Name: "new", Path: "foo", Revision: "b", Groups: "g2"},
	}}

	raw := compareManifestsWithStatusResolvers(m1, m2, &DiffManifestsOptions{Raw: true, All: true}, log, missing, missing)
	if len(raw) != 2 {
		t.Fatalf("raw 模式应比较 name 与 groups（2 行）, got %v", raw)
	}
	for _, line := range raw {
		if !strings.HasPrefix(line, "C foo a b") {
			t.Errorf("raw 行应为 C 而非 missing/U: %q", line)
		}
	}

	text := compareManifestsWithStatusResolvers(m1, m2, &DiffManifestsOptions{All: true}, log, missing, missing)
	joined := strings.Join(text, "\n")
	for _, want := range []string{"missing project: foo", "name old -> new", "groups g1 -> g2"} {
		if !strings.Contains(joined, want) {
			t.Errorf("非 raw 输出缺少 %q，实际:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "unreachable") {
		t.Errorf("missing 不得报 unreachable:\n%s", joined)
	}
}

// TestRunDiffManifestsPrefersMergedSnapshot 验证省略第二个清单时使用
// `.repo/manifest.xml` 合并快照，而不是配置里的 ManifestName。
func TestRunDiffManifestsPrefersMergedSnapshot(t *testing.T) {
	repoRoot, headSHA, parentSHA := readCmdStdoutRepo(t)

	manifestContent := func(revision string) string {
		return `<manifest>
  <remote name="origin" fetch="https://example.com" />
  <default remote="origin" revision="main" />
  <project name="foo" path="foo" revision="` + revision + `" />
</manifest>
`
	}
	// 合并快照 revision 为 parentSHA；config 指向不存在的清单，若误用配置会解析失败。
	if err := os.WriteFile(filepath.Join(repoRoot, ".repo", "manifest.xml"), []byte(manifestContent(parentSHA)), 0o644); err != nil {
		t.Fatalf("写入合并快照 failed: %v", err)
	}
	configJSON := `{"version":1,"manifest_name":"config-only.xml"}`
	if err := os.WriteFile(filepath.Join(repoRoot, ".repo", "config.json"), []byte(configJSON), 0o644); err != nil {
		t.Fatalf("写入 config.json failed: %v", err)
	}
	manifestsDir := filepath.Join(repoRoot, ".repo", "manifests")
	if err := os.MkdirAll(manifestsDir, 0o755); err != nil {
		t.Fatalf("创建 .repo/manifests failed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(manifestsDir, "m1.xml"), []byte(manifestContent(headSHA)), 0o644); err != nil {
		t.Fatalf("写入 m1.xml failed: %v", err)
	}

	readCmdWithCwd(t, repoRoot, func() {
		stdout, _, err := readCmdRunCaptured(t, func() error {
			return runDiffManifests(&DiffManifestsOptions{}, []string{"m1.xml"})
		})
		if err != nil {
			t.Fatalf("runDiffManifests 省略第二清单 failed: %v", err)
		}
		readCmdAssertStdoutPayload(t, stdout, "changed project: foo", headSHA, parentSHA)
	})
}
