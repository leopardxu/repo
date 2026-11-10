package repo_sync

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/leopardxu/repo-go/internal/config"
	"github.com/leopardxu/repo-go/internal/git"
	"github.com/leopardxu/repo-go/internal/logger"
	"github.com/leopardxu/repo-go/internal/manifest"
	"github.com/leopardxu/repo-go/internal/project"
)

func TestNewEngine(t *testing.T) {
	opts := &Options{
		Jobs: 4,
	}
	log := logger.NewDefaultLogger()
	m := &manifest.Manifest{}

	engine := NewEngine(opts, m, log)
	if engine == nil {
		t.Fatal("NewEngine() returned nil")
	}
	if engine.gitRunner == nil {
		t.Error("gitRunner should be initialized")
	}
	if engine.workerPool == nil {
		t.Error("workerPool should be initialized")
	}
	if engine.networkSem == nil {
		t.Error("networkSem should be initialized")
	}
	if engine.checkoutSem == nil {
		t.Error("checkoutSem should be initialized")
	}
}

func TestNewEngineWithContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	opts := &Options{Jobs: 2}
	log := logger.NewDefaultLogger()
	m := &manifest.Manifest{}

	engine := NewEngineWithContext(ctx, opts, m, log)
	if engine == nil {
		t.Fatal("NewEngineWithContext() returned nil")
	}
	if engine.ctx != ctx {
		t.Error("Engine ctx should match passed context")
	}
}

func TestNewEngineDefaultJobs(t *testing.T) {
	opts := &Options{Jobs: 0}
	log := logger.NewDefaultLogger()
	m := &manifest.Manifest{}

	engine := NewEngine(opts, m, log)
	if engine.options.Jobs <= 0 {
		t.Error("Jobs should default to CPU count > 0")
	}
}

func TestEngineFetchRetries(t *testing.T) {
	tests := []struct {
		name       string
		retryFetch int
		want       int
	}{
		{"zero means no retries", 0, 0},
		{"custom", 5, 5},
		{"one", 1, 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			engine := NewEngine(&Options{RetryFetches: tt.retryFetch}, &manifest.Manifest{}, logger.NewDefaultLogger())
			got := engine.fetchRetries()
			if got != tt.want {
				t.Errorf("fetchRetries() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestSyncError(t *testing.T) {
	err := &SyncError{
		ProjectName: "test-project",
		Phase:       "fetch",
		Err:         nil,
		Timestamp:   time.Now(),
		RetryCount:  2,
	}

	msg := err.Error()
	if msg == "" {
		t.Error("SyncError.Error() should not be empty")
	}
}

func TestSyncErrorWithOutput(t *testing.T) {
	err := &SyncError{
		ProjectName: "test-project",
		Phase:       "clone",
		Err:         nil,
		Output:      "some stderr output",
		Timestamp:   time.Now(),
	}

	msg := err.Error()
	if msg == "" {
		t.Error("SyncError.Error() should not be empty")
	}
}

// TestSyncErrorDoesNotDuplicateDiagnostics 验证 git 诊断信息只出现一次：
// GitCommandError.Error() 已携带 stderr，SyncError.Output 保存的同一段文本
// 不应再追加一遍（CI 日志里曾出现同一段 kex_exchange_identification 重复输出）。
func TestSyncErrorDoesNotDuplicateDiagnostics(t *testing.T) {
	diagnostic := "kex_exchange_identification: read: Connection reset by peer"
	inner := &git.GitCommandError{
		Command:  "git clone ssh://example.com/repo",
		Dir:      "",
		Err:      errors.New("exit status 128"),
		Stderr:   diagnostic,
		ExitCode: 128,
	}
	err := &SyncError{
		ProjectName: "linux_repo/cix_debian12",
		Phase:       "clone",
		Err:         fmt.Errorf("clone failed: %w", inner),
		Output:      diagnostic,
		Timestamp:   time.Now(),
	}

	msg := err.Error()
	if got := strings.Count(msg, diagnostic); got != 1 {
		t.Errorf("诊断信息应只出现 1 次，实际 %d 次: %s", got, msg)
	}
}

// TestSyncErrorAppendsDistinctOutput 验证 Output 与底层错误文本不同时仍会被追加，
// 避免为去重而丢失额外的 stderr 信息。
func TestSyncErrorAppendsDistinctOutput(t *testing.T) {
	err := &SyncError{
		ProjectName: "p",
		Phase:       "clone",
		Err:         errors.New("exit status 128"),
		Output:      "fatal: unable to access",
		Timestamp:   time.Now(),
	}

	msg := err.Error()
	if !strings.Contains(msg, "fatal: unable to access") {
		t.Errorf("SyncError.Error() 应追加未包含的 Output，实际: %s", msg)
	}
}

// TestSummarizeSyncErrors 验证取消错误与真实失败被正确分流：
// fail-fast 打断的项目不计入失败数，真实失败保留原始错误。
func TestSummarizeSyncErrors(t *testing.T) {
	realErr := errors.New("clone failed")
	canceledErr := &SyncError{
		ProjectName: "p/canceled",
		Phase:       "checkout",
		Err:         fmt.Errorf("checkout canceled: %w", context.Canceled),
	}

	tests := []struct {
		name         string
		errs         []error
		wantReal     []error
		wantCanceled int
	}{
		{"empty", nil, []error{}, 0},
		{"only real failure", []error{realErr}, []error{realErr}, 0},
		{"only canceled", []error{canceledErr}, []error{}, 1},
		{"mixed", []error{canceledErr, realErr, canceledErr}, []error{realErr}, 2},
		{"nil entry ignored", []error{nil, realErr}, []error{realErr}, 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotReal, gotCanceled := summarizeSyncErrors(tt.errs)
			if gotCanceled != tt.wantCanceled {
				t.Errorf("canceled = %d, want %d", gotCanceled, tt.wantCanceled)
			}
			if !reflect.DeepEqual(gotReal, tt.wantReal) {
				t.Errorf("real = %v, want %v", gotReal, tt.wantReal)
			}
		})
	}
}

// TestSyncProgressStatus 验证进度行状态区分成功、真实失败与取消。
func TestSyncProgressStatus(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"success", nil, "完成"},
		{"real failure", errors.New("boom"), "失败"},
		{"wrapped cancel", fmt.Errorf("clone canceled: %w", context.Canceled), "已取消"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := syncProgressStatus(tt.err); got != tt.want {
				t.Errorf("syncProgressStatus() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestNewMultiError(t *testing.T) {
	// nil input
	if err := NewMultiError(nil); err != nil {
		t.Error("NewMultiError(nil) should return nil")
	}

	// empty slice
	if err := NewMultiError([]error{}); err != nil {
		t.Error("NewMultiError([]) should return nil")
	}

	// single error
	singleErr := NewMultiError([]error{nil})
	if singleErr != nil {
		t.Error("NewMultiError with single nil should return nil")
	}

	// Multiple errors should return non-nil
	multi := NewMultiError([]error{nil, nil})
	if multi != nil {
		t.Error("NewMultiError with all nils should return nil")
	}
}

func TestFormatDuration(t *testing.T) {
	tests := []struct {
		name string
		d    time.Duration
		want string
	}{
		{"seconds", 5 * time.Second, "5秒"},
		{"minutes", 90 * time.Second, "1分钟30秒"},
		{"hours", 3700 * time.Second, "1小时1分钟40秒"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := formatDuration(tt.d)
			if got != tt.want {
				t.Errorf("formatDuration(%v) = %q, want %q", tt.d, got, tt.want)
			}
		})
	}
}

func TestEngineSetProjects(_ *testing.T) {
	engine := NewEngine(&Options{Jobs: 1}, &manifest.Manifest{}, logger.NewDefaultLogger())
	engine.SetProjects(nil)
	// Should not panic
}

func TestEngineCleanup(_ *testing.T) {
	engine := NewEngine(&Options{Jobs: 2}, &manifest.Manifest{}, logger.NewDefaultLogger())
	engine.Cleanup()
	// Should not panic, and should clean up resources
}

func TestRunGitHelper(t *testing.T) {
	engine := NewEngine(&Options{Jobs: 1}, &manifest.Manifest{}, logger.NewDefaultLogger())

	// Test that runGit works (uses git version which always works)
	output, err := engine.runGit("version")
	if err != nil {
		t.Fatalf("runGit(version) error: %v", err)
	}
	if len(output) == 0 {
		t.Error("runGit(version) should return output")
	}
}

func TestRunGitInDirHelper(t *testing.T) {
	engine := NewEngine(&Options{Jobs: 1}, &manifest.Manifest{}, logger.NewDefaultLogger())

	output, err := engine.runGitInDir(".", "version")
	if err != nil {
		t.Fatalf("runGitInDir('.', 'version') error: %v", err)
	}
	if len(output) == 0 {
		t.Error("runGitInDir should return output")
	}
}

func TestRunGitWithStderrHelper(t *testing.T) {
	engine := NewEngine(&Options{Jobs: 1}, &manifest.Manifest{}, logger.NewDefaultLogger())

	// Test successful command
	_, stderr, err := engine.runGitWithStderr([]string{"version"})
	if err != nil {
		t.Fatalf("runGitWithStderr(version) error: %v", err)
	}
	if stderr != "" {
		t.Errorf("runGitWithStderr(version) stderr = %q, want empty", stderr)
	}
}

func TestRunGitWithStderrInDirHelper(t *testing.T) {
	engine := NewEngine(&Options{Jobs: 1}, &manifest.Manifest{}, logger.NewDefaultLogger())

	_, stderr, err := engine.runGitWithStderrInDir(".", []string{"version"})
	if err != nil {
		t.Fatalf("runGitWithStderrInDir('.', 'version') error: %v", err)
	}
	if stderr != "" {
		t.Errorf("stderr should be empty for successful command, got %q", stderr)
	}
}

func TestEnginePreSyncForceRemoveDirty(t *testing.T) {
	opts := &Options{
		Jobs:             1,
		ForceRemoveDirty: true,
	}
	engine := NewEngine(opts, &manifest.Manifest{}, logger.NewDefaultLogger())

	// preSync with ForceRemoveDirty should not panic and should return nil
	err := engine.preSync(context.Background())
	if err != nil {
		t.Errorf("preSync with ForceRemoveDirty returned error: %v", err)
	}
}

// TestResolveRemoteURLAbsolutePassthrough 是项目 URL 拼接修复的配套回归测试：
// manager 已按上游语义把 remote fetch 拼接为完整项目 URL（fetch + '/' + name），
// 引擎对绝对 URL 必须原样透传、不重复拼接项目名，
// 否则同一进程内二次解析（如重试/再次 sync）会把 URL 变成 .../name/name。
func TestResolveRemoteURLAbsolutePassthrough(t *testing.T) {
	e := &Engine{
		logger:  logger.NewDefaultLogger(),
		options: &Options{},
	}
	p := &project.Project{
		Name:       "cix_opensource/release/edk2-non-osi",
		RemoteName: "origin",
		RemoteURL:  "ssh://git@gitmirror.cixcomputing.com:29418/cix_opensource/release/edk2-non-osi",
	}

	if got, err := e.resolveRemoteURL(p); err != nil || got != p.RemoteURL {
		t.Errorf("resolveRemoteURL(absolute) = (%q, %v), want passthrough %q", got, err, p.RemoteURL)
	}
}

// TestResolveRemoteURLRelativeAppendsName 验证相对 fetch 仍走解析路径：
// 基于清单仓库 URL（urljoin 语义）解析并只拼接一次项目名。
func TestResolveRemoteURLRelativeAppendsName(t *testing.T) {
	e := &Engine{
		logger:  logger.NewDefaultLogger(),
		options: &Options{},
		manifest: &manifest.Manifest{
			Remotes: []manifest.Remote{{Name: "origin", Fetch: "https://example.com/manifests"}},
		},
	}
	p := &project.Project{
		Name:        "platform/build",
		RemoteName:  "origin",
		RemoteURL:   "..",
		ManifestURL: "https://example.com/manifests",
	}

	// ".." 相对 fetch：以 https://example.com/manifests 为基准做 urljoin，再拼项目名
	const want = "https://example.com/platform/build"
	got, err := e.resolveRemoteURL(p)
	if err != nil {
		t.Fatalf("resolveRemoteURL(..) error = %v", err)
	}
	if got != want {
		t.Errorf("resolveRemoteURL(..) = %q, want %q", got, want)
	}
}

// TestResolveRemoteURLRelativeVariants 覆盖相对 fetch 的 urljoin 语义：
// "."、"./"、".."、"../<path>"，以及解析后绝对 URL 的幂等透传。
func TestResolveRemoteURLRelativeVariants(t *testing.T) {
	tests := []struct {
		name        string
		manifestURL string
		remoteURL   string
		want        string
		wantErr     bool
	}{
		{
			name:        "dot stays in manifest dir",
			manifestURL: "https://example.com/manifests",
			remoteURL:   ".",
			want:        "https://example.com/proj",
		},
		{
			name:        "dot slash stays in manifest dir",
			manifestURL: "https://example.com/manifests",
			remoteURL:   "./",
			want:        "https://example.com/proj",
		},
		{
			name:        "dot dot pops manifest repo",
			manifestURL: "https://example.com/manifests",
			remoteURL:   "..",
			want:        "https://example.com/proj",
		},
		{
			name:        "parent with path",
			manifestURL: "https://example.com/cix/manifests",
			remoteURL:   "../platform",
			want:        "https://example.com/platform/proj",
		},
		{
			name:        "non-dot relative fetch",
			manifestURL: "https://example.com/manifests",
			remoteURL:   "platform",
			want:        "https://example.com/platform/proj",
		},
		{
			name:        "nested non-dot relative fetch",
			manifestURL: "https://example.com/manifests",
			remoteURL:   "platform/build",
			want:        "https://example.com/platform/build/proj",
		},
		{
			name:        "non-dot relative fetch with ssh base",
			manifestURL: "ssh://git@example.com:29418/manifests",
			remoteURL:   "platform",
			want:        "ssh://git@example.com:29418/platform/proj",
		},
		{
			name:        "ssh base",
			manifestURL: "ssh://git@example.com:29418/manifests",
			remoteURL:   "..",
			want:        "ssh://git@example.com:29418/proj",
		},
		{
			name:        "scp base normalized to ssh",
			manifestURL: "git@example.com:manifests/manifest.git",
			remoteURL:   "..",
			want:        "ssh://git@example.com/proj",
		},
		{
			name:        "local file path base",
			manifestURL: "/srv/manifests/manifest.git",
			remoteURL:   "..",
			want:        "/srv/proj",
		},
		{
			name:        "missing base fails",
			manifestURL: "",
			remoteURL:   "..",
			wantErr:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := &Engine{logger: logger.NewDefaultLogger(), options: &Options{}}
			p := &project.Project{
				Name:        "proj",
				RemoteName:  "origin",
				RemoteURL:   tt.remoteURL,
				ManifestURL: tt.manifestURL,
			}
			got, err := e.resolveRemoteURL(p)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("resolveRemoteURL(%q) = %q, want error", tt.remoteURL, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveRemoteURL(%q) error = %v", tt.remoteURL, err)
			}
			if got != tt.want {
				t.Errorf("resolveRemoteURL(%q) = %q, want %q", tt.remoteURL, got, tt.want)
			}
			// 幂等：解析后的绝对 URL 再次解析必须原样返回。
			p.RemoteURL = got
			again, err := e.resolveRemoteURL(p)
			if err != nil || again != got {
				t.Errorf("resolveRemoteURL(%q) second pass = (%q, %v), want %q", got, again, err, got)
			}
		})
	}
}

// TestManifestGroupsUsesEffectiveGroups 验证同步/智能同步解析清单时的组回退：
// 显式 Groups 优先；为空时使用 config.EffectiveGroups()（未配置时为 "default"）。
func TestManifestGroupsUsesEffectiveGroups(t *testing.T) {
	tests := []struct {
		name string
		opts []string
		cfg  *config.Config
		want []string
	}{
		{
			name: "explicit groups win",
			opts: []string{"cix"},
			cfg:  &config.Config{Groups: "default"},
			want: []string{"cix"},
		},
		{
			name: "config groups used",
			cfg:  &config.Config{Groups: "default,cix"},
			want: []string{"default", "cix"},
		},
		{
			name: "explicit empty groups cancels filtering",
			opts: []string{},
			cfg:  &config.Config{Groups: "default"},
			want: []string{},
		},
		{
			name: "empty config groups fall back to default",
			cfg:  &config.Config{},
			want: []string{"default"},
		},
		{
			name: "nil config keeps no filter",
			want: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := &Engine{options: &Options{Groups: tt.opts}, config: tt.cfg}
			if got := e.manifestGroups(); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("manifestGroups() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestResolveRemoteURLUsesFetchBaseWithoutDoubleName 验证清单项目的相对
// RemoteURL 已含项目名时，使用原始 FetchBase 重新解析，避免 /name/name。
func TestResolveRemoteURLUsesFetchBaseWithoutDoubleName(t *testing.T) {
	e := &Engine{logger: logger.NewDefaultLogger(), options: &Options{}}
	p := &project.Project{
		Name:        "proj",
		RemoteName:  "origin",
		FetchBase:   "..",
		RemoteURL:   "../proj", // manager 在无法解析基址时可能保留的中间形态
		ManifestURL: "https://example.com/manifests",
	}
	got, err := e.resolveRemoteURL(p)
	if err != nil {
		t.Fatalf("resolveRemoteURL() error = %v", err)
	}
	if want := "https://example.com/proj"; got != want {
		t.Errorf("resolveRemoteURL() = %q, want %q (must not duplicate project name)", got, want)
	}
}
