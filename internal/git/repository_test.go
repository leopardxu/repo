package git

import (
	"testing"

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

	// Should resolve ../ relative to the base URL extracted from manifest URL
	// ExtractBaseURLFromManifestURL("https://example.com/path/to/manifest.git") returns "https://example.com"
	// Then "../repo.git" becomes "https://example.com/repo.git"
	if got != "https://example.com/repo.git" {
		t.Errorf("resolveRepositoryURL(../repo.git) = %q, want https://example.com/repo.git", got)
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
