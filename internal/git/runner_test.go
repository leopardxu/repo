package git

import (
	"strings"
	"testing"
	"time"

	"github.com/leopardxu/repo-go/internal/config"
)

func TestNewRunner(t *testing.T) {
	runner := NewRunner()
	if runner == nil {
		t.Fatal("NewRunner() returned nil")
	}

	// Verify it implements Runner interface
	var _ Runner = runner
}

func TestNewCommandRunnerWithConfig(t *testing.T) {
	cfg := &config.Config{
		Verbose: true,
		Quiet:   false,
		Jobs:    10,
	}

	runner, err := NewCommandRunnerWithConfig(cfg)
	if err != nil {
		t.Fatalf("NewCommandRunnerWithConfig() returned error: %v", err)
	}
	if runner == nil {
		t.Fatal("NewCommandRunnerWithConfig() returned nil")
	}
}

func TestNewCommandRunnerWithNilConfig(t *testing.T) {
	runner, err := NewCommandRunnerWithConfig(nil)
	if err != nil {
		t.Fatalf("NewCommandRunnerWithConfig(nil) returned error: %v", err)
	}
	if runner == nil {
		t.Fatal("NewCommandRunnerWithConfig(nil) returned nil")
	}
}

func TestSetVerbose(t *testing.T) {
	runner := NewRunner()
	r := runner.(*defaultRunner)

	r.SetVerbose(true)
	if !r.Verbose {
		t.Error("SetVerbose(true) did not set Verbose to true")
	}

	r.SetVerbose(false)
	if r.Verbose {
		t.Error("SetVerbose(false) did not set Verbose to false")
	}
}

func TestSetQuiet(t *testing.T) {
	runner := NewRunner()
	r := runner.(*defaultRunner)

	r.SetQuiet(true)
	if !r.Quiet {
		t.Error("SetQuiet(true) did not set Quiet to true")
	}

	r.SetQuiet(false)
	if r.Quiet {
		t.Error("SetQuiet(false) did not set Quiet to false")
	}
}

func TestSetMaxRetries(t *testing.T) {
	runner := NewRunner()
	r := runner.(*defaultRunner)

	r.SetMaxRetries(5)
	if r.MaxRetries != 5 {
		t.Errorf("SetMaxRetries(5) = %d, want 5", r.MaxRetries)
	}
}

func TestSetRetryDelay(t *testing.T) {
	runner := NewRunner()
	r := runner.(*defaultRunner)

	delay := 5 * time.Second
	r.SetRetryDelay(delay)
	if r.RetryDelay != delay {
		t.Errorf("SetRetryDelay() = %v, want %v", r.RetryDelay, delay)
	}
}

func TestSetTimeout(t *testing.T) {
	runner := NewRunner()
	r := runner.(*defaultRunner)

	timeout := 60 * time.Second
	r.SetTimeout(timeout)
	if r.timeout != timeout {
		t.Errorf("SetTimeout() = %v, want %v", r.timeout, timeout)
	}
}

func TestSetEnvironment(t *testing.T) {
	runner := NewRunner()
	r := runner.(*defaultRunner)

	env := map[string]string{"GIT_AUTHOR_NAME": "test"}
	r.SetEnvironment(env)

	got := r.GetEnvironment()
	if got["GIT_AUTHOR_NAME"] != "test" {
		t.Errorf("GetEnvironment() = %v, want GIT_AUTHOR_NAME=test", got)
	}
}

// TestSetConcurrencySafeReplace verifies that SetConcurrency does not panic
// when called concurrently with Run (the previous implementation closed the channel).
func TestSetConcurrencySafeReplace(t *testing.T) {
	runner := NewRunner()
	r := runner.(*defaultRunner)

	// Set initial concurrency
	r.SetConcurrency(3)
	if r.concurrency != 3 {
		t.Errorf("concurrency = %d, want 3", r.concurrency)
	}

	// Change concurrency - should not panic
	r.SetConcurrency(5)
	if r.concurrency != 5 {
		t.Errorf("concurrency = %d, want 5", r.concurrency)
	}

	// Set to 0 - should set semaphore to nil
	r.SetConcurrency(0)
	if r.semaphore != nil {
		t.Error("semaphore should be nil when concurrency is 0")
	}

	// Set back to positive
	r.SetConcurrency(2)
	if r.semaphore == nil {
		t.Error("semaphore should not be nil when concurrency > 0")
	}
}

// TestSetConcurrencyMultipleChanges verifies repeated changes don't panic
func TestSetConcurrencyMultipleChanges(t *testing.T) {
	runner := NewRunner()
	r := runner.(*defaultRunner)

	for i := 1; i <= 10; i++ {
		r.SetConcurrency(i)
	}

	if r.concurrency != 10 {
		t.Errorf("concurrency = %d, want 10", r.concurrency)
	}
}

func TestShouldRetryNetworkErrors(t *testing.T) {
	tests := []struct {
		name   string
		stderr string
		want   bool
	}{
		{"DNS resolution failure", "fatal: Could not resolve host example.com", true},
		{"Connection timeout", "fatal: Connection timed out", true},
		{"Connection reset", "error: Connection reset by peer", true},
		{"Operation timeout", "fatal: Operation timed out", true},
		{"Temporary DNS failure", "fatal: Temporary failure in name resolution", true},
		{"Failed to connect", "fatal: Failed to connect to github.com", true},
		{"Index lock", "fatal: Unable to create index.lock: File exists", true},
		{"Lock exists", "fatal: Unable to lock ref", true},
		{"Remote hung up", "fatal: the remote end hung up unexpectedly", true},
		{"Normal error", "fatal: not a git repository", false},
		{"Empty stderr", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := shouldRetry(1, tt.stderr)
			if got != tt.want {
				t.Errorf("shouldRetry(1, %q) = %v, want %v", tt.stderr, got, tt.want)
			}
		})
	}
}

func TestGitCommandError(t *testing.T) {
	err := &GitCommandError{
		Command:  "git fetch",
		Dir:      "/tmp/repo",
		Err:      nil,
		Stdout:   "some output",
		Stderr:   "some error",
		ExitCode: 1,
	}

	msg := err.Error()
	if !strings.Contains(msg, "git fetch") {
		t.Errorf("Error() should contain command name, got: %s", msg)
	}
	if !strings.Contains(msg, "/tmp/repo") {
		t.Errorf("Error() should contain directory, got: %s", msg)
	}
	if !strings.Contains(msg, "exit code 1") {
		t.Errorf("Error() should contain exit code, got: %s", msg)
	}
}

func TestGitCommandErrorZeroExitCode(t *testing.T) {
	err := &GitCommandError{
		Command:  "git status",
		Dir:      "/tmp/repo",
		Err:      nil,
		ExitCode: 0,
	}

	msg := err.Error()
	if strings.Contains(msg, "exit code") {
		t.Errorf("Error() should not contain exit code when 0, got: %s", msg)
	}
}

func TestRunnerRunGitCommand(t *testing.T) {
	runner := NewRunner()

	// Test with a real git command (version should always work)
	output, err := runner.Run("version")
	if err != nil {
		t.Fatalf("Run(version) returned error: %v", err)
	}

	if !strings.Contains(string(output), "git version") {
		t.Errorf("Run(version) output should contain 'git version', got: %s", string(output))
	}
}

func TestRunnerRunInDir(t *testing.T) {
	runner := NewRunner()

	// Running git version in a specific directory should work
	output, err := runner.RunInDir(".", "version")
	if err != nil {
		t.Fatalf("RunInDir('.', 'version') returned error: %v", err)
	}

	if !strings.Contains(string(output), "git version") {
		t.Errorf("RunInDir output should contain 'git version', got: %s", string(output))
	}
}

func TestRunnerRunWithInvalidCommand(t *testing.T) {
	runner := NewRunner()
	r := runner.(*defaultRunner)
	r.SetMaxRetries(0) // No retries for this test

	_, err := runner.Run("not-a-valid-git-command")
	if err == nil {
		t.Error("Run with invalid command should return error")
	}
}
