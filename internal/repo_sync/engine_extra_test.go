package repo_sync

import (
	"context"
	"testing"
	"time"

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
		{"default", 0, 3},
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

	if got := e.resolveRemoteURL(p); got != p.RemoteURL {
		t.Errorf("resolveRemoteURL(absolute) = %q, want passthrough %q", got, p.RemoteURL)
	}
}

// TestResolveRemoteURLRelativeAppendsName 验证相对 fetch 仍走解析路径：
// 基于 manifest 远程 URL 的父目录解析（对齐上游 urljoin 语义）并拼接项目名。
func TestResolveRemoteURLRelativeAppendsName(t *testing.T) {
	e := &Engine{
		logger:  logger.NewDefaultLogger(),
		options: &Options{},
		manifest: &manifest.Manifest{
			Remotes: []manifest.Remote{{Name: "origin", Fetch: "https://example.com/manifests"}},
		},
	}
	p := &project.Project{
		Name:       "platform/build",
		RemoteName: "origin",
		RemoteURL:  "..",
	}

	// ".." 相对 fetch：以 https://example.com/manifests 为基准取父目录，再拼项目名
	const want = "https://example.com/platform/build"
	if got := e.resolveRemoteURL(p); got != want {
		t.Errorf("resolveRemoteURL(..) = %q, want %q", got, want)
	}
}
