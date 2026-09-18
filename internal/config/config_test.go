package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestConfigValidate(t *testing.T) {
	tests := []struct {
		name        string
		config      Config
		wantErr     bool
		errContains string
	}{
		{
			name:        "empty config",
			config:      Config{},
			wantErr:     true,
			errContains: "manifest_url is required",
		},
		{
			name: "valid config",
			config: Config{
				ManifestURL:  "https://example.com/manifest.git",
				ManifestName: "default.xml",
			},
			wantErr: false,
		},
		{
			name: "missing manifest name",
			config: Config{
				ManifestURL: "https://example.com/manifest.git",
			},
			wantErr:     true,
			errContains: "manifest_name is required",
		},
		{
			name: "negative depth",
			config: Config{
				ManifestURL:  "https://example.com/manifest.git",
				ManifestName: "default.xml",
				Depth:        -1,
			},
			wantErr:     true,
			errContains: "depth must be non-negative",
		},
		{
			name: "negative jobs",
			config: Config{
				ManifestURL:  "https://example.com/manifest.git",
				ManifestName: "default.xml",
				Jobs:         -1,
			},
			wantErr:     true,
			errContains: "jobs must be non-negative",
		},
		{
			name: "mirror and archive mutually exclusive",
			config: Config{
				ManifestURL:  "https://example.com/manifest.git",
				ManifestName: "default.xml",
				Mirror:       true,
				Archive:      true,
			},
			wantErr:     true,
			errContains: "mutually exclusive",
		},
		{
			name: "current_branch and no_current_branch mutually exclusive",
			config: Config{
				ManifestURL:     "https://example.com/manifest.git",
				ManifestName:    "default.xml",
				CurrentBranch:   true,
				NoCurrentBranch: true,
			},
			wantErr:     true,
			errContains: "mutually exclusive",
		},
		{
			name: "tags and no_tags mutually exclusive",
			config: Config{
				ManifestURL:  "https://example.com/manifest.git",
				ManifestName: "default.xml",
				Tags:         true,
				NoTags:       true,
			},
			wantErr:     true,
			errContains: "mutually exclusive",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.config.Validate()
			if tt.wantErr {
				if err == nil {
					t.Errorf("Validate() should return error, got nil")
					return
				}
				if tt.errContains != "" && !contains(err.Error(), tt.errContains) {
					t.Errorf("Validate() error = %q, should contain %q", err.Error(), tt.errContains)
				}
			} else {
				if err != nil {
					t.Errorf("Validate() should return nil, got: %v", err)
				}
			}
		})
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(substr) == 0 ||
		(len(s) > 0 && len(substr) > 0 && findSubstring(s, substr)))
}

func findSubstring(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

func TestGetJobs(t *testing.T) {
	cfg := Config{Jobs: 0}
	if jobs := cfg.GetJobs(); jobs <= 0 {
		t.Errorf("GetJobs() with 0 should return CPU count > 0, got %d", jobs)
	}

	cfg2 := Config{Jobs: 8}
	if jobs := cfg2.GetJobs(); jobs != 8 {
		t.Errorf("GetJobs() = %d, want 8", jobs)
	}
}

func TestExtractBaseURLFromManifestURL(t *testing.T) {
	cfg := &Config{}
	tests := []struct {
		name string
		url  string
		want string
	}{
		{
			name: "SSH URL",
			url:  "ssh://git@example.com/path/to/manifest.git",
			want: "ssh://git@example.com",
		},
		{
			name: "HTTPS URL",
			url:  "https://example.com/path/to/manifest.git",
			want: "https://example.com",
		},
		{
			name: "HTTP URL",
			url:  "http://example.com/path/to/manifest.git",
			want: "http://example.com",
		},
		{
			name: "SCP format",
			url:  "git@example.com:path/to/manifest.git",
			want: "git@example.com",
		},
		{
			name: "plain path (no protocol)",
			url:  "some/path/to/repo",
			want: "some/path/to/repo",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := cfg.ExtractBaseURLFromManifestURL(tt.url)
			if got != tt.want {
				t.Errorf("ExtractBaseURLFromManifestURL(%q) = %q, want %q", tt.url, got, tt.want)
			}
		})
	}
}

func TestGetDefaultGroups(t *testing.T) {
	cfg := Config{Groups: ""}
	groups := cfg.GetDefaultGroups()
	if len(groups) != 1 || groups[0] != "all" {
		t.Errorf("GetDefaultGroups() with empty Groups = %v, want [all]", groups)
	}

	cfg2 := Config{Groups: "linux,arm"}
	groups2 := cfg2.GetDefaultGroups()
	if len(groups2) != 2 {
		t.Errorf("GetDefaultGroups() = %v, want 2 groups", groups2)
	}
}

func TestIsCurrentBranchMode(t *testing.T) {
	cfg := Config{CurrentBranch: true, NoCurrentBranch: false}
	if !cfg.IsCurrentBranchMode() {
		t.Error("IsCurrentBranchMode() should be true when CurrentBranch=true")
	}

	cfg2 := Config{CurrentBranch: true, NoCurrentBranch: true}
	if cfg2.IsCurrentBranchMode() {
		t.Error("IsCurrentBranchMode() should be false when NoCurrentBranch=true")
	}
}

func TestIsTagsEnabled(t *testing.T) {
	cfg := Config{Tags: true, NoTags: false}
	if !cfg.IsTagsEnabled() {
		t.Error("IsTagsEnabled() should be true when Tags=true")
	}

	cfg2 := Config{Tags: true, NoTags: true}
	if cfg2.IsTagsEnabled() {
		t.Error("IsTagsEnabled() should be false when NoTags=true")
	}
}

func TestIsColorEnabled(t *testing.T) {
	tests := []struct {
		color string
		want  bool
	}{
		{"never", false},
		{"always", true},
		{"auto", true},
		{"", true},
	}

	for _, tt := range tests {
		cfg := Config{Color: tt.color}
		if got := cfg.IsColorEnabled(); got != tt.want {
			t.Errorf("IsColorEnabled() with Color=%q = %v, want %v", tt.color, got, tt.want)
		}
	}
}

func TestApplyEnvironmentWithRepoPrefix(t *testing.T) {
	// Save and restore env vars
	defer func() {
		os.Unsetenv("REPO_MANIFEST_URL")
		os.Unsetenv("REPO_MANIFEST_BRANCH")
		os.Unsetenv("REPO_MANIFEST_NAME")
		os.Unsetenv("REPO_GROUPS")
		os.Unsetenv("REPO_VERBOSE")
		os.Unsetenv("REPO_QUIET")
		os.Unsetenv("REPO_MIRROR")
		os.Unsetenv("REPO_DEPTH")
	}()

	os.Setenv("REPO_MANIFEST_URL", "https://repo-prefix.example.com/manifest.git")
	os.Setenv("REPO_MANIFEST_BRANCH", "main")
	os.Setenv("REPO_MANIFEST_NAME", "custom.xml")
	os.Setenv("REPO_GROUPS", "linux,arm")
	os.Setenv("REPO_VERBOSE", "true")
	os.Setenv("REPO_QUIET", "false")
	os.Setenv("REPO_MIRROR", "true")
	os.Setenv("REPO_DEPTH", "5")

	cfg := &Config{}
	cfg.ApplyEnvironment()

	if cfg.ManifestURL != "https://repo-prefix.example.com/manifest.git" {
		t.Errorf("ManifestURL = %q, want https://repo-prefix.example.com/manifest.git", cfg.ManifestURL)
	}
	if cfg.ManifestBranch != "main" {
		t.Errorf("ManifestBranch = %q, want main", cfg.ManifestBranch)
	}
	if cfg.ManifestName != "custom.xml" {
		t.Errorf("ManifestName = %q, want custom.xml", cfg.ManifestName)
	}
	if cfg.Groups != "linux,arm" {
		t.Errorf("Groups = %q, want linux,arm", cfg.Groups)
	}
	if !cfg.Verbose {
		t.Error("Verbose should be true")
	}
	if cfg.Quiet {
		t.Error("Quiet should be false")
	}
	if !cfg.Mirror {
		t.Error("Mirror should be true")
	}
	if cfg.Depth != 5 {
		t.Errorf("Depth = %d, want 5", cfg.Depth)
	}
}

func TestApplyEnvironmentGogoNotUsed(t *testing.T) {
	// Verify GOGO_ prefix is no longer read
	defer func() {
		os.Unsetenv("GOGO_MANIFEST_URL")
		os.Unsetenv("GOGO_VERBOSE")
	}()

	// Ensure REPO_ prefix is not set
	os.Unsetenv("REPO_MANIFEST_URL")
	os.Unsetenv("REPO_VERBOSE")

	os.Setenv("GOGO_MANIFEST_URL", "https://gogo-prefix.example.com/manifest.git")
	os.Setenv("GOGO_VERBOSE", "true")

	cfg := &Config{}
	cfg.ApplyEnvironment()

	// GOGO_ should be ignored
	if cfg.ManifestURL != "" {
		t.Errorf("ManifestURL = %q, GOGO_ prefix should be ignored", cfg.ManifestURL)
	}
	if cfg.Verbose {
		t.Error("Verbose should be false, GOGO_ prefix should be ignored")
	}
}

func TestApplyEnvironmentRepoPrefixOnly(t *testing.T) {
	defer func() {
		os.Unsetenv("REPO_MANIFEST_URL")
	}()

	os.Setenv("REPO_MANIFEST_URL", "https://repo.example.com/manifest.git")

	cfg := &Config{}
	cfg.ApplyEnvironment()

	if cfg.ManifestURL != "https://repo.example.com/manifest.git" {
		t.Errorf("ManifestURL = %q, want https://repo.example.com/manifest.git", cfg.ManifestURL)
	}
}

func TestSaveAndLoad(t *testing.T) {
	// Create a temporary directory for .repo
	tmpDir := t.TempDir()
	repoDir := filepath.Join(tmpDir, ".repo")
	if err := os.MkdirAll(repoDir, 0755); err != nil {
		t.Fatalf("Failed to create .repo dir: %v", err)
	}

	// Change to temp dir
	origDir, _ := os.Getwd()
	defer os.Chdir(origDir)
	os.Chdir(tmpDir)

	// Clear config cache
	configMutex.Lock()
	configCache = nil
	configMutex.Unlock()

	cfg := &Config{
		ManifestURL:    "https://example.com/manifest.git",
		ManifestName:   "default.xml",
		ManifestBranch: "main",
	}

	if err := cfg.Save(); err != nil {
		t.Fatalf("Save() returned error: %v", err)
	}

	// Verify file was created
	configPath := filepath.Join(repoDir, "config.json")
	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		t.Fatal("config.json was not created")
	}

	// Clear cache again before loading
	configMutex.Lock()
	configCache = nil
	configMutex.Unlock()

	loaded, err := Load()
	if err != nil {
		t.Fatalf("Load() returned error: %v", err)
	}

	if loaded.ManifestURL != cfg.ManifestURL {
		t.Errorf("Loaded ManifestURL = %q, want %q", loaded.ManifestURL, cfg.ManifestURL)
	}
	if loaded.ManifestName != cfg.ManifestName {
		t.Errorf("Loaded ManifestName = %q, want %q", loaded.ManifestName, cfg.ManifestName)
	}
}

func TestGetRepoRootNotInRepo(t *testing.T) {
	tmpDir := t.TempDir()
	origDir, _ := os.Getwd()
	defer os.Chdir(origDir)
	os.Chdir(tmpDir)

	_, err := GetRepoRoot()
	if err == nil {
		t.Error("GetRepoRoot() should return error when not in a repo")
	}
}

func TestConfigError(t *testing.T) {
	err := &ConfigError{Op: "load", Path: "/path/to/config", Err: nil}
	msg := err.Error()
	if msg == "" {
		t.Error("ConfigError.Error() should not be empty")
	}
}

func TestMigrateConfig(t *testing.T) {
	// Version 1 should not need migration
	cfg := &Config{Version: 1}
	if err := migrateConfig(cfg); err != nil {
		t.Errorf("migrateConfig with version 1 returned error: %v", err)
	}

	// Version 0 should be migrated to version 1
	cfg2 := &Config{Version: 0}
	if err := migrateConfig(cfg2); err != nil {
		t.Errorf("migrateConfig with version 0 returned error: %v", err)
	}
	if cfg2.Version != 1 {
		t.Errorf("After migration, version = %d, want 1", cfg2.Version)
	}

	// Unsupported version should return error
	cfg3 := &Config{Version: 999}
	if err := migrateConfig(cfg3); err == nil {
		t.Error("migrateConfig with unsupported version should return error")
	}
}
