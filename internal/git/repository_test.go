package git

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/leopardxu/repo-go/internal/config"
)

func TestResolveRepositoryURLAbsolute(t *testing.T) {
	tests := []struct {
		name string
		url  string
		want string
	}{
		{"HTTPS", "https://example.com/repo.git", "https://example.com/repo.git"},
		{"HTTP", "http://example.com/repo.git", "http://example.com/repo.git"},
		{"SSH", "ssh://git@example.com/repo.git", "ssh://git@example.com/repo.git"},
		{"SCP format", "git@example.com:repo.git", "git@example.com:repo.git"},
		{"file protocol", "file:///path/to/repo", "file:///path/to/repo"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveRepositoryURL(tt.url, nil)
			if err != nil {
				t.Fatalf("resolveRepositoryURL() error: %v", err)
			}
			if got != tt.want {
				t.Errorf("resolveRepositoryURL(%q) = %q, want %q", tt.url, got, tt.want)
			}
		})
	}
}

func TestResolveRepositoryURLRelativeWithConfig(t *testing.T) {
	cfg := &config.Config{
		ManifestURL: "https://example.com/path/to/manifest.git",
	}

	got, err := resolveRepositoryURL("../repo.git", cfg)
	if err != nil {
		t.Fatalf("resolveRepositoryURL() error: %v", err)
	}

	// urljoin 语义：以 manifest URL 自身为基准解析相对路径
	// ExtractBaseURLFromManifestURL 只取 scheme://host 的旧行为会把路径段丢掉，
	// 现在 "../repo.git" 相对 "https://example.com/path/to/" 得到 path/repo.git
	if got != "https://example.com/path/repo.git" {
		t.Errorf("resolveRepositoryURL(../repo.git) = %q, want https://example.com/path/repo.git", got)
	}
}

func TestResolveRepositoryURLRelativeWithoutConfig(t *testing.T) {
	// Clear URL cache to ensure clean test
	urlCacheMutex.Lock()
	urlCache = make(map[string]string)
	urlCacheMutex.Unlock()

	// Without config, relative URL should be returned as-is (no hardcoded fallback)
	got, err := resolveRepositoryURL("../repo.git", nil)
	if err != nil {
		t.Fatalf("resolveRepositoryURL() error: %v", err)
	}

	// Should return the original URL since no config to resolve against
	// Note: If a previous test cached this with config, it may return a resolved URL.
	// We cleared the cache above to ensure clean behavior.
	if got != "../repo.git" {
		t.Errorf("resolveRepositoryURL(../repo.git, nil) = %q, want ../repo.git (no hardcoded fallback)", got)
	}
}

func TestResolveRepositoryURLAbsoluteFilePath(t *testing.T) {
	got, err := resolveRepositoryURL("/absolute/path/to/repo", nil)
	if err != nil {
		t.Fatalf("resolveRepositoryURL() error: %v", err)
	}
	if got != "file:///absolute/path/to/repo" {
		t.Errorf("resolveRepositoryURL(/absolute/path) = %q, want file:///absolute/path/to/repo", got)
	}
}

func TestResolveRepositoryURLCaching(t *testing.T) {
	// The URL cache should return the same result for the same input
	url1, _ := resolveRepositoryURL("https://cached.example.com/repo.git", nil)
	url2, _ := resolveRepositoryURL("https://cached.example.com/repo.git", nil)
	if url1 != url2 {
		t.Errorf("Cached URLs should match: %q vs %q", url1, url2)
	}
}

func TestNewRepository(t *testing.T) {
	runner := NewRunner()
	repo := NewRepository("/path/to/repo", runner)

	if repo.Path != "/path/to/repo" {
		t.Errorf("Path = %q, want /path/to/repo", repo.Path)
	}
	if repo.Runner == nil {
		t.Error("Runner should not be nil")
	}
	if repo.cacheExpiration == 0 {
		t.Error("cacheExpiration should have a default value")
	}
}

func TestRepositorySetCacheExpiration(t *testing.T) {
	repo := NewRepository("/path", NewRunner())
	repo.SetCacheExpiration(60 * 1e9) // 1 minute in nanoseconds

	if repo.cacheExpiration != 60*1e9 {
		t.Errorf("cacheExpiration = %v, want 60s", repo.cacheExpiration)
	}
}

func TestRepositoryClearCache(t *testing.T) {
	repo := NewRepository("/path", NewRunner())
	repo.statusCache = "cached"
	repo.branchCache = "main"
	repo.ClearCache()

	if repo.statusCache != "" {
		t.Error("statusCache should be empty after ClearCache")
	}
	if repo.branchCache != "" {
		t.Error("branchCache should be empty after ClearCache")
	}
}

func TestRepositoryError(t *testing.T) {
	err := &RepositoryError{
		Op:      "clone",
		Path:    "/path/to/repo",
		Command: "git clone",
		Err:     nil,
	}

	msg := err.Error()
	if msg == "" {
		t.Error("RepositoryError.Error() should not be empty")
	}
}

func TestIsImmutable(t *testing.T) {
	tests := []struct {
		revision string
		want     bool
	}{
		{"abc1234", true},    // 7-char hex (short SHA)
		{"abc1234567", true}, // 11-char hex
		{"0123456789abcdef0123456789abcdef01234567", true}, // 40-char SHA
		{"refs/tags/v1.0", true},                           // tag ref
		{"refs/changes/12/1234/1", true},                   // change ref
		{"main", false},                                    // branch name
		{"master", false},                                  // branch name
		{"HEAD", false},                                    // HEAD
		{"", false},                                        // empty
		{"feature-branch", false},                          // branch name with dash
	}

	for _, tt := range tests {
		t.Run(tt.revision, func(t *testing.T) {
			got := IsImmutable(tt.revision)
			if got != tt.want {
				t.Errorf("IsImmutable(%q) = %v, want %v", tt.revision, got, tt.want)
			}
		})
	}
}

// recordingRunner 记录所有 git 调用，供仓库行为测试断言参数。
type recordingRunner struct {
	mu      sync.Mutex
	calls   [][]string
	dirs    []string
	handler func(args []string) ([]byte, error)
}

func (r *recordingRunner) record(dir string, args []string) ([]byte, error) {
	r.mu.Lock()
	r.calls = append(r.calls, append([]string(nil), args...))
	r.dirs = append(r.dirs, dir)
	r.mu.Unlock()
	if r.handler != nil {
		return r.handler(args)
	}
	return nil, nil
}

func (r *recordingRunner) Run(args ...string) ([]byte, error) {
	return r.record("", args)
}

func (r *recordingRunner) RunInDir(dir string, args ...string) ([]byte, error) {
	return r.record(dir, args)
}

func (r *recordingRunner) RunWithTimeout(_ time.Duration, args ...string) ([]byte, error) {
	return r.record("", args)
}

func (r *recordingRunner) RunInDirWithTimeout(dir string, _ time.Duration, args ...string) ([]byte, error) {
	return r.record(dir, args)
}

func (r *recordingRunner) SetVerbose(bool)             {}
func (r *recordingRunner) SetQuiet(bool)               {}
func (r *recordingRunner) SetMaxRetries(int)           {}
func (r *recordingRunner) SetRetryDelay(time.Duration) {}
func (r *recordingRunner) SetConcurrency(int)          {}

func (r *recordingRunner) callCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}

func (r *recordingRunner) lastCall() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.calls) == 0 {
		return nil
	}
	return r.calls[len(r.calls)-1]
}

// recordingInteractiveRunner 在 recordingRunner 基础上实现可选的交互执行方法。
type recordingInteractiveRunner struct {
	*recordingRunner
	interactiveDirs []string
	interactiveArgs [][]string
	interactiveErr  error
}

func (r *recordingInteractiveRunner) RunInteractiveInDir(dir string, args ...string) error {
	r.mu.Lock()
	r.interactiveDirs = append(r.interactiveDirs, dir)
	r.interactiveArgs = append(r.interactiveArgs, append([]string(nil), args...))
	r.mu.Unlock()
	return r.interactiveErr
}

func resetURLCache() {
	urlCacheMutex.Lock()
	urlCache = make(map[string]string)
	urlCacheMutex.Unlock()
}

func TestRepositoryStatusCacheEmptyResult(t *testing.T) {
	resetURLCache()
	statusCalls := 0
	runner := &recordingRunner{handler: func(args []string) ([]byte, error) {
		if args[0] == "status" {
			statusCalls++
			return []byte(""), nil // 干净仓库
		}
		return nil, nil
	}}
	repo := NewRepository("/repo", runner)

	for i := 0; i < 2; i++ {
		status, err := repo.Status()
		if err != nil {
			t.Fatalf("Status() error: %v", err)
		}
		if status != "" {
			t.Fatalf("Status() = %q, want empty", status)
		}
	}
	if statusCalls != 1 {
		t.Errorf("status executed %d times, want 1 (empty status must be cached)", statusCalls)
	}

	// 变更类命令必须让缓存失效
	if _, err := repo.RunCommand("add", "-A"); err != nil {
		t.Fatalf("RunCommand() error: %v", err)
	}
	if _, err := repo.Status(); err != nil {
		t.Fatalf("Status() after invalidation error: %v", err)
	}
	if statusCalls != 2 {
		t.Errorf("status executed %d times after RunCommand, want 2", statusCalls)
	}
}

func TestRepositoryCurrentBranchCacheInvalidation(t *testing.T) {
	branchCalls := 0
	runner := &recordingRunner{handler: func(args []string) ([]byte, error) {
		switch args[0] {
		case "symbolic-ref":
			branchCalls++
			return []byte("main\n"), nil
		case "checkout":
			return nil, nil
		}
		return nil, nil
	}}
	repo := NewRepository("/repo", runner)

	for i := 0; i < 2; i++ {
		if _, err := repo.CurrentBranch(); err != nil {
			t.Fatalf("CurrentBranch() error: %v", err)
		}
	}
	if branchCalls != 1 {
		t.Fatalf("branch resolved %d times, want 1 (cached)", branchCalls)
	}

	if err := repo.Checkout("main"); err != nil {
		t.Fatalf("Checkout() error: %v", err)
	}
	if _, err := repo.CurrentBranch(); err != nil {
		t.Fatalf("CurrentBranch() after checkout error: %v", err)
	}
	if branchCalls != 2 {
		t.Errorf("branch resolved %d times after checkout, want 2 (cache invalidated)", branchCalls)
	}

	// checkout 应使用 -- 分隔
	runner.mu.Lock()
	var checkoutArgs []string
	for _, c := range runner.calls {
		if len(c) > 0 && c[0] == "checkout" {
			checkoutArgs = c
		}
	}
	runner.mu.Unlock()
	if len(checkoutArgs) == 0 || checkoutArgs[len(checkoutArgs)-1] != "--" {
		t.Errorf("checkout args = %v, want trailing -- separator", checkoutArgs)
	}
}

func TestHasChangesToPushDetachedHead(t *testing.T) {
	runner := &recordingRunner{handler: func(args []string) ([]byte, error) {
		switch args[0] {
		case "symbolic-ref":
			return nil, errors.New("fatal: ref HEAD is not a symbolic ref")
		case "rev-parse":
			return []byte("deadbee\n"), nil
		case "rev-list":
			t.Error("rev-list should not run for detached HEAD")
		}
		return nil, nil
	}}
	repo := NewRepository("/repo", runner)

	changed, err := repo.HasChangesToPush("origin")
	if err != nil {
		t.Fatalf("HasChangesToPush() error: %v", err)
	}
	if changed {
		t.Error("detached HEAD should report no changes to push")
	}
}

func TestHasChangesToPushUsesShortBranchName(t *testing.T) {
	runner := &recordingRunner{handler: func(args []string) ([]byte, error) {
		switch args[0] {
		case "symbolic-ref":
			return []byte("feature/x\n"), nil
		case "rev-parse":
			// 远程分支不存在 -> 新分支，需要推送
			return nil, errors.New("unknown revision")
		}
		return nil, nil
	}}
	repo := NewRepository("/repo", runner)

	changed, err := repo.HasChangesToPush("origin")
	if err != nil {
		t.Fatalf("HasChangesToPush() error: %v", err)
	}
	if !changed {
		t.Error("new branch should report changes to push")
	}

	call := runner.lastCall()
	want := "origin/feature/x"
	found := false
	for _, arg := range call {
		if arg == want {
			found = true
		}
		if arg == "origin/HEAD" {
			t.Errorf("must not use origin/HEAD, args = %v", call)
		}
	}
	if !found {
		t.Errorf("rev-parse args = %v, want to contain %q", call, want)
	}
}

func TestFetchArgs(t *testing.T) {
	tests := []struct {
		name   string
		remote string
		cfg    *config.Config
		want   []string
	}{
		{
			name:   "bare remote name passed through",
			remote: "origin",
			want:   []string{"fetch", "--", "origin"},
		},
		{
			name:   "absolute path kept as-is",
			remote: "/tmp/some/repo.git",
			want:   []string{"fetch", "--", "/tmp/some/repo.git"},
		},
		{
			name:   "parent relative resolved against manifest base",
			remote: "../repo.git",
			cfg:    &config.Config{ManifestURL: "https://example.com/path/to/manifest.git"},
			want:   []string{"fetch", "--", "https://example.com/path/repo.git"},
		},
		{
			name:   "dot relative resolved against manifest base",
			remote: "./repo.git",
			cfg:    &config.Config{ManifestURL: "https://example.com/path/to/manifest.git"},
			want:   []string{"fetch", "--", "https://example.com/path/to/repo.git"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resetURLCache()
			runner := &recordingRunner{}
			repo := NewRepository("/repo", runner)
			if err := repo.Fetch(tt.remote, FetchOptions{Config: tt.cfg}); err != nil {
				t.Fatalf("Fetch() error: %v", err)
			}
			got := runner.lastCall()
			if len(got) != len(tt.want) {
				t.Fatalf("fetch args = %v, want %v", got, tt.want)
			}
			for i := range tt.want {
				if got[i] != tt.want[i] {
					t.Errorf("fetch args[%d] = %q, want %q (args = %v)", i, got[i], tt.want[i], got)
				}
			}
		})
	}
}

func TestResolveRepositoryURLCacheIncludesBaseURL(t *testing.T) {
	resetURLCache()
	cfgA := &config.Config{ManifestURL: "https://a.example.com/path/manifest.git"}
	cfgB := &config.Config{ManifestURL: "https://b.example.com/path/manifest.git"}

	gotA, err := resolveRepositoryURL("../repo.git", cfgA)
	if err != nil {
		t.Fatalf("resolve cfgA error: %v", err)
	}
	gotB, err := resolveRepositoryURL("../repo.git", cfgB)
	if err != nil {
		t.Fatalf("resolve cfgB error: %v", err)
	}
	if gotA != "https://a.example.com/repo.git" {
		t.Errorf("cfgA resolved = %q, want https://a.example.com/repo.git", gotA)
	}
	if gotB != "https://b.example.com/repo.git" {
		t.Errorf("cfgB resolved = %q, want https://b.example.com/repo.git", gotB)
	}
}

func TestResolveRepositoryURLNormalizesDots(t *testing.T) {
	resetURLCache()
	cfg := &config.Config{ManifestURL: "https://example.com/path/manifest.git"}
	got, err := resolveRepositoryURL("../a/../b.git", cfg)
	if err != nil {
		t.Fatalf("resolveRepositoryURL error: %v", err)
	}
	// urljoin 语义会规范化中间的 .. 段（与上游 urljoin 一致）
	if got != "https://example.com/b.git" {
		t.Errorf("resolved = %q, want https://example.com/b.git", got)
	}
}

func TestCloneUsesDoubleDash(t *testing.T) {
	runner := &recordingRunner{}
	repo := NewRepository("/repo", runner)
	if err := repo.Clone("/tmp/source", CloneOptions{}); err != nil {
		t.Fatalf("Clone() error: %v", err)
	}
	got := runner.lastCall()
	if len(got) < 2 || got[0] != "clone" || got[1] != "--" {
		t.Errorf("clone args = %v, want [clone -- ...]", got)
	}
}

func TestCheckoutRejectsOptionLikeRevision(t *testing.T) {
	runner := &recordingRunner{}
	repo := NewRepository("/repo", runner)
	if err := repo.Checkout("-b"); err == nil {
		t.Error("Checkout(-b) should be rejected")
	}
	if runner.callCount() != 0 {
		t.Errorf("runner should not be invoked for invalid revision, calls = %v", runner.calls)
	}
}

func TestHasRevisionUsesSeparatorAndRejectsOptions(t *testing.T) {
	runner := &recordingRunner{}
	repo := NewRepository("/repo", runner)
	if _, err := repo.HasRevision("-x"); err == nil {
		t.Error("HasRevision(-x) should return an error")
	}
	if runner.callCount() != 0 {
		t.Errorf("runner should not be invoked for option-like revision")
	}

	if _, err := repo.HasRevision("main"); err != nil {
		t.Fatalf("HasRevision(main) error: %v", err)
	}
	got := runner.lastCall()
	if len(got) < 2 || got[0] != "rev-parse" || got[len(got)-1] != "--" {
		t.Errorf("rev-parse args = %v, want trailing -- separator", got)
	}
}

func TestRunInteractiveUnsupportedRunner(t *testing.T) {
	repo := NewRepository("/repo", &recordingRunner{})
	if err := repo.RunInteractive("add", "--interactive"); err == nil {
		t.Error("RunInteractive should fail when runner lacks interactive support")
	}
}

func TestRunInteractiveSupportedRunner(t *testing.T) {
	base := &recordingRunner{}
	runner := &recordingInteractiveRunner{recordingRunner: base}
	repo := NewRepository("/repo", runner)

	if err := repo.RunInteractive("add", "--interactive"); err != nil {
		t.Fatalf("RunInteractive() error: %v", err)
	}
	if len(runner.interactiveArgs) != 1 {
		t.Fatalf("interactive calls = %d, want 1", len(runner.interactiveArgs))
	}
	if runner.interactiveDirs[0] != "/repo" {
		t.Errorf("interactive dir = %q, want /repo", runner.interactiveDirs[0])
	}
	got := runner.interactiveArgs[0]
	if len(got) != 2 || got[0] != "add" || got[1] != "--interactive" {
		t.Errorf("interactive args = %v", got)
	}
}

// seedRepositoryCache 预置状态/分支缓存，用于验证命令执行后缓存失效。
func seedRepositoryCache(repo *Repository) {
	repo.cacheMutex.Lock()
	defer repo.cacheMutex.Unlock()
	repo.statusCache = "cached-status"
	repo.statusCacheValid = true
	repo.branchCache = "cached-branch"
	repo.branchCacheValid = true
}

// assertRepositoryCacheCleared 断言状态与分支缓存均已失效。
func assertRepositoryCacheCleared(t *testing.T, repo *Repository) {
	t.Helper()
	repo.cacheMutex.Lock()
	defer repo.cacheMutex.Unlock()
	if repo.statusCache != "" || repo.statusCacheValid {
		t.Errorf("statusCache 未清除: %q (valid=%v)", repo.statusCache, repo.statusCacheValid)
	}
	if repo.branchCache != "" || repo.branchCacheValid {
		t.Errorf("branchCache 未清除: %q (valid=%v)", repo.branchCache, repo.branchCacheValid)
	}
}

func TestRunInteractiveClearsCache(t *testing.T) {
	tests := []struct {
		name           string
		interactiveErr error
	}{
		{name: "success", interactiveErr: nil},
		{name: "failure", interactiveErr: errors.New("interrupted")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runner := &recordingInteractiveRunner{
				recordingRunner: &recordingRunner{},
				interactiveErr:  tt.interactiveErr,
			}
			repo := NewRepository("/repo", runner)
			seedRepositoryCache(repo)

			err := repo.RunInteractive("add", "--patch")
			if tt.interactiveErr == nil {
				if err != nil {
					t.Fatalf("RunInteractive() error: %v", err)
				}
			} else {
				if err == nil {
					t.Fatal("RunInteractive() 期望失败")
				}
				if !errors.Is(err, tt.interactiveErr) {
					t.Errorf("RunInteractive() error = %v, want 包含 %v", err, tt.interactiveErr)
				}
			}

			assertRepositoryCacheCleared(t, repo)
		})
	}
}

func TestResolveRepositoryURLUsesDefaultRemoteURLJoin(t *testing.T) {
	resetURLCache()
	cfg := &config.Config{
		ManifestURL:      "https://example.com/ignored/manifest.git",
		DefaultRemoteURL: "ssh://git@example.com/base/manifest",
	}
	got, err := resolveRepositoryURL("../repo.git", cfg)
	if err != nil {
		t.Fatalf("resolveRepositoryURL error: %v", err)
	}
	// DefaultRemoteURL 优先作为 base，urljoin 后 /base/manifest 的 ../ 落到根
	if got != "ssh://git@example.com/repo.git" {
		t.Errorf("resolved = %q, want ssh://git@example.com/repo.git", got)
	}
}

func TestFetchFlagsBeforeRemoteSeparator(t *testing.T) {
	runner := &recordingRunner{}
	repo := NewRepository("/repo", runner)

	err := repo.Fetch("origin", FetchOptions{Prune: true, Tags: true, Depth: 1})
	if err != nil {
		t.Fatalf("Fetch() error: %v", err)
	}

	want := []string{"fetch", "--prune", "--tags", "--depth", "1", "--", "origin"}
	got := runner.lastCall()
	if len(got) != len(want) {
		t.Fatalf("fetch args = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("fetch args[%d] = %q, want %q (args = %v)", i, got[i], want[i], got)
		}
	}
}

func TestFetchOptionLikeRemoteNotTreatedAsOption(t *testing.T) {
	runner := &recordingRunner{}
	repo := NewRepository("/repo", runner)

	// 以 - 开头的 remote 名必须出现在 -- 之后，git 才不会把它当选项解析
	if err := repo.Fetch("-d", FetchOptions{}); err != nil {
		t.Fatalf("Fetch() error: %v", err)
	}

	got := runner.lastCall()
	sep := -1
	for i, arg := range got {
		if arg == "--" {
			sep = i
			break
		}
	}
	if sep < 0 {
		t.Fatalf("fetch args = %v, 缺少 -- 分隔符", got)
	}
	found := false
	for _, arg := range got[sep+1:] {
		if arg == "-d" {
			found = true
		}
	}
	if !found {
		t.Errorf("fetch args = %v, want %q after --", got, "-d")
	}
}

func TestRunCommandFailureInvalidatesCache(t *testing.T) {
	runner := &recordingRunner{handler: func(args []string) ([]byte, error) {
		switch args[0] {
		case "status":
			return []byte(""), nil // 干净仓库，空字符串也必须被缓存
		case "add":
			return nil, errors.New("index.lock exists")
		}
		return nil, nil
	}}
	repo := NewRepository("/repo", runner)

	if _, err := repo.Status(); err != nil {
		t.Fatalf("Status() error: %v", err)
	}
	repo.cacheMutex.RLock()
	cached := repo.statusCacheValid
	repo.cacheMutex.RUnlock()
	if !cached {
		t.Fatal("Status() 结果应已缓存")
	}

	// 失败命令也可能已改动索引，必须同样失效缓存
	if _, err := repo.RunCommand("add", "-A"); err == nil {
		t.Fatal("RunCommand() 应返回错误")
	}
	repo.cacheMutex.RLock()
	stillValid := repo.statusCacheValid
	repo.cacheMutex.RUnlock()
	if stillValid {
		t.Error("失败命令后缓存必须失效")
	}
}

// stderrRecordingRunner 实现可选的 RunInDirWithStderr 能力，供
// RunCommandWithStderr 直通路径测试使用。
type stderrRecordingRunner struct {
	*recordingRunner
	stdout []byte
	stderr []byte
	err    error
}

func (r *stderrRecordingRunner) RunInDirWithStderr(dir string, args ...string) ([]byte, []byte, error) {
	_, _ = r.record(dir, args)
	return r.stdout, r.stderr, r.err
}

// TestRepositoryRunCommandWithStderrPassthrough 验证底层 Runner 支持可选
// 接口时，stdout/stderr 在失败场景也完整返回。
func TestRepositoryRunCommandWithStderrPassthrough(t *testing.T) {
	runner := &stderrRecordingRunner{
		recordingRunner: &recordingRunner{},
		stdout:          []byte("pushed"),
		stderr:          []byte("remote: rejected"),
		err:             errors.New("exit status 1"),
	}
	repo := NewRepository("/tmp/repo", runner)

	stdout, stderr, err := repo.RunCommandWithStderr("push", "origin", "HEAD")
	if err == nil {
		t.Fatal("RunCommandWithStderr() 应返回错误")
	}
	var repoErr *RepositoryError
	if !errors.As(err, &repoErr) {
		t.Errorf("error = %T, want *RepositoryError", err)
	}
	if string(stdout) != "pushed" {
		t.Errorf("stdout = %q, want pushed", stdout)
	}
	if string(stderr) != "remote: rejected" {
		t.Errorf("stderr = %q, want remote: rejected", stderr)
	}
	if calls := runner.calls; len(calls) != 1 || calls[0][0] != "push" {
		t.Errorf("calls = %v, want one push call", calls)
	}
}

// TestRepositoryRunCommandWithStderrFallback 验证 Runner 不支持可选接口时，
// 从 *GitCommandError 中取回 stderr，且 stdout 不丢失。
func TestRepositoryRunCommandWithStderrFallback(t *testing.T) {
	cmdErr := &GitCommandError{
		Command:  "git push",
		Dir:      "/tmp/repo",
		Err:      errors.New("boom"),
		Stdout:   "pushed",
		Stderr:   "fatal: nope",
		ExitCode: 1,
	}
	runner := &recordingRunner{handler: func([]string) ([]byte, error) {
		return []byte("pushed"), cmdErr
	}}
	repo := NewRepository("/tmp/repo", runner)

	stdout, stderr, err := repo.RunCommandWithStderr("push")
	if err == nil {
		t.Fatal("RunCommandWithStderr() 应返回错误")
	}
	if string(stdout) != "pushed" {
		t.Errorf("stdout = %q, want pushed", stdout)
	}
	if string(stderr) != "fatal: nope" {
		t.Errorf("stderr = %q, want fatal: nope", stderr)
	}
}

func containsArg(args []string, want string) bool {
	for _, arg := range args {
		if arg == want {
			return true
		}
	}
	return false
}

// TestCurrentBranchStripsFullSymbolicRef 验证 CurrentBranch/GetBranchName
// 从完整 symbolic-ref 提取短分支名（同名 tag 下不会得到 "heads/main"）。
func TestCurrentBranchStripsFullSymbolicRef(t *testing.T) {
	tests := []struct {
		name string
		ref  string
		want string
	}{
		{"branch", "refs/heads/main\n", "main"},
		{"nested branch", "refs/heads/feature/x\n", "feature/x"},
		{"same-name tag ambiguity", "refs/heads/release\n", "release"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runner := &recordingRunner{handler: func(args []string) ([]byte, error) {
				if args[0] == "symbolic-ref" {
					if len(args) != 2 || args[1] != "HEAD" {
						t.Errorf("symbolic-ref args = %v, want [symbolic-ref HEAD]", args)
					}
					return []byte(tt.ref), nil
				}
				t.Errorf("未预期的命令: %v", args)
				return nil, nil
			}}
			repo := NewRepository("/repo", runner)

			got, err := repo.CurrentBranch()
			if err != nil {
				t.Fatalf("CurrentBranch() error: %v", err)
			}
			if got != tt.want {
				t.Errorf("CurrentBranch() = %q, want %q", got, tt.want)
			}
			if strings.HasPrefix(got, "refs/") || strings.HasPrefix(got, "heads/") {
				t.Errorf("CurrentBranch() 返回了未剥离的引用名: %q", got)
			}

			got, err = repo.GetBranchName()
			if err != nil {
				t.Fatalf("GetBranchName() error: %v", err)
			}
			if got != tt.want {
				t.Errorf("GetBranchName() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestHasChangesToPushSameNameTagUsesShortBranch 验证同名 tag 存在时
// 远程分支仍拼接为 origin/main，而不是 origin/heads/main。
func TestHasChangesToPushSameNameTagUsesShortBranch(t *testing.T) {
	runner := &recordingRunner{handler: func(args []string) ([]byte, error) {
		switch args[0] {
		case "symbolic-ref":
			return []byte("refs/heads/main\n"), nil
		case "rev-parse":
			return []byte("sha\n"), nil
		case "rev-list":
			return []byte("0\n"), nil
		}
		return nil, nil
	}}
	repo := NewRepository("/repo", runner)

	changed, err := repo.HasChangesToPush("origin")
	if err != nil {
		t.Fatalf("HasChangesToPush() error: %v", err)
	}
	if changed {
		t.Error("rev-list 输出 0 时不应报告待推送变更")
	}

	runner.mu.Lock()
	var revParseArgs, revListArgs []string
	for _, call := range runner.calls {
		switch call[0] {
		case "rev-parse":
			revParseArgs = call
		case "rev-list":
			revListArgs = call
		}
	}
	runner.mu.Unlock()

	if !containsArg(revParseArgs, "origin/main") {
		t.Errorf("rev-parse args = %v, want 包含 origin/main", revParseArgs)
	}
	if containsArg(revParseArgs, "origin/heads/main") {
		t.Errorf("rev-parse args = %v, 不应包含 origin/heads/main", revParseArgs)
	}
	if !containsArg(revListArgs, "main") || !containsArg(revListArgs, "^origin/main") {
		t.Errorf("rev-list args = %v, want main 与 ^origin/main", revListArgs)
	}
}

// TestRunCommandFailureReturnsCapturedOutput 验证失败命令仍返回已捕获 stdout，
// 供 upload/rebase/stage 的错误信息展示 git 输出。
func TestRunCommandFailureReturnsCapturedOutput(t *testing.T) {
	runner := &recordingRunner{handler: func(_ []string) ([]byte, error) {
		return []byte("partial output"), errors.New("push rejected")
	}}
	repo := NewRepository("/repo", runner)

	output, err := repo.RunCommand("push")
	if err == nil {
		t.Fatal("RunCommand() 应返回错误")
	}
	if string(output) != "partial output" {
		t.Errorf("RunCommand() output = %q, want partial output", string(output))
	}
	var repoErr *RepositoryError
	if !errors.As(err, &repoErr) {
		t.Errorf("错误类型 = %T, want *RepositoryError", err)
	}
}

// TestRunCommandWithStderrSuccessReturnsStderr 验证成功执行时也返回 stderr，
// upload 依赖该输出提取 Gerrit review URL。
func TestRunCommandWithStderrSuccessReturnsStderr(t *testing.T) {
	runner := &stderrRecordingRunner{
		recordingRunner: &recordingRunner{},
		stdout:          []byte("push stdout"),
		stderr:          []byte("remote: review https://example.com/+/1"),
	}
	repo := NewRepository("/repo", runner)

	stdout, stderr, err := repo.RunCommandWithStderr("push")
	if err != nil {
		t.Fatalf("RunCommandWithStderr() error: %v", err)
	}
	if string(stdout) != "push stdout" {
		t.Errorf("stdout = %q", string(stdout))
	}
	if !strings.Contains(string(stderr), "review https://example.com/+/1") {
		t.Errorf("stderr = %q, want 包含 review URL", string(stderr))
	}
	if len(runner.dirs) != 1 || runner.dirs[0] != "/repo" {
		t.Errorf("执行目录 = %v, want /repo", runner.dirs)
	}
}

// TestCurrentBranchWithSameNameTagRealGit 用真实 git 验证：分支与 tag 同名时
// CurrentBranch 仍返回无歧义短名（旧实现 symbolic-ref --short 会得到 heads/main）。
func TestCurrentBranchWithSameNameTagRealGit(t *testing.T) {
	dir := t.TempDir()
	runner := NewRunner()
	r := runner.(*defaultRunner)
	r.SetMaxRetries(0)

	mustRun := func(args ...string) {
		t.Helper()
		if output, err := runner.RunInDir(dir, args...); err != nil {
			t.Fatalf("git %v 失败: %v (%s)", args, err, string(output))
		}
	}
	mustRun("init", "-q", "-b", "main")
	mustRun("-c", "user.email=test@example.com", "-c", "user.name=test", "commit", "-q", "--allow-empty", "-m", "init")
	mustRun("tag", "main")

	repo := NewRepository(dir, runner)
	got, err := repo.CurrentBranch()
	if err != nil {
		t.Fatalf("CurrentBranch() error: %v", err)
	}
	if got != "main" {
		t.Errorf("CurrentBranch() = %q, want main（同名 tag 下不应返回 heads/main）", got)
	}

	// 记录旧实现的行为，便于诊断：同名 tag 下 --short 返回歧义名。
	if short, err := runner.RunInDir(dir, "symbolic-ref", "--short", "HEAD"); err == nil {
		t.Logf("symbolic-ref --short HEAD = %q", strings.TrimSpace(string(short)))
	}
}
