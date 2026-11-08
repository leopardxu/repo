package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
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
			name: "HTTPS URL with userinfo",
			url:  "https://user@example.com/path/to/manifest.git",
			want: "https://user@example.com",
		},
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

func TestEffectiveGroups(t *testing.T) {
	tests := []struct {
		name   string
		cfg    *Config
		groups string
		want   string
	}{
		{name: "empty groups fall back to default", cfg: &Config{}, want: "default"},
		{name: "whitespace groups fall back to default", cfg: &Config{Groups: "  "}, want: "default"},
		{name: "explicit groups kept verbatim", cfg: &Config{Groups: "linux,arm"}, want: "linux,arm"},
		{name: "single group kept", cfg: &Config{Groups: "default"}, want: "default"},
		{name: "nil config falls back to default", cfg: nil, want: "default"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.cfg.EffectiveGroups(); got != tt.want {
				t.Errorf("EffectiveGroups() = %q, want %q", got, tt.want)
			}
		})
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
	// always/never 与终端无关
	if cfg := (Config{Color: "never"}); cfg.IsColorEnabled() {
		t.Error("Color=never should disable color")
	}
	if cfg := (Config{Color: "always"}); !cfg.IsColorEnabled() {
		t.Error("Color=always should enable color")
	}

	// auto/空值仅在 stdout 是终端时启用。把 stdout 重定向到普通文件模拟
	// 重定向/CI 场景，必须返回 false。
	origStdout := os.Stdout
	f, err := os.CreateTemp(t.TempDir(), "stdout")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	defer func() {
		os.Stdout = origStdout
		f.Close()
	}()
	os.Stdout = f

	for _, color := range []string{"auto", ""} {
		cfg := Config{Color: color}
		if cfg.IsColorEnabled() {
			t.Errorf("IsColorEnabled() with Color=%q and redirected stdout = true, want false", color)
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

// TestResolveRelativeURL 覆盖 urljoin 语义：无 base 返回空串（不得把相对值
// 当完整地址）；普通相对 fetch（含不带点号的 "platform"）与 "."/".." 均按
// base 解析；scp 规范化；文件路径语义；已完整地址原样返回。
func TestResolveRelativeURLNoBase(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
		rel  string
		want string
	}{
		{name: "no base returns empty", cfg: Config{}, rel: "../foo", want: ""},
		{name: "no base dotdot returns empty", cfg: Config{}, rel: "..", want: ""},
		{name: "no base dot returns empty", cfg: Config{}, rel: ".", want: ""},
		{name: "no base relative path returns empty", cfg: Config{}, rel: "foo/bar", want: ""},
		{
			name: "manifest url join",
			cfg:  Config{ManifestURL: "https://example.com/a/manifest"},
			rel:  "../foo",
			want: "https://example.com/foo",
		},
		{
			name: "dotless relative fetch is urljoined",
			cfg:  Config{ManifestURL: "https://example.com/a/manifest"},
			rel:  "platform",
			want: "https://example.com/a/platform",
		},
		{
			name: "dotless nested relative fetch is urljoined",
			cfg:  Config{ManifestURL: "ssh://git@example.com/base/manifest.git"},
			rel:  "platform/build",
			want: "ssh://git@example.com/base/platform/build",
		},
		{
			name: "absolute url is returned unchanged",
			cfg:  Config{ManifestURL: "https://example.com/a/manifest"},
			rel:  "ssh://git@other.example.com/base",
			want: "ssh://git@other.example.com/base",
		},
		{
			name: "scp style url is returned unchanged",
			cfg:  Config{ManifestURL: "https://example.com/a/manifest"},
			rel:  "git@other.example.com:base",
			want: "git@other.example.com:base",
		},
		{
			name: "scp style url without user is returned unchanged",
			cfg:  Config{ManifestURL: "/srv/git/manifests/default.xml"},
			rel:  "other.example.com:base",
			want: "other.example.com:base",
		},
		{
			name: "scheme url with file base is returned unchanged",
			cfg:  Config{ManifestURL: "/srv/git/manifests/default.xml"},
			rel:  "ssh://other.example.com/base",
			want: "ssh://other.example.com/base",
		},
		{
			name: "scp style base without user normalized to ssh",
			cfg:  Config{ManifestURL: "example.com:base/manifest"},
			rel:  "../foo",
			want: "ssh://example.com/foo",
		},
		{
			name: "absolute file path is returned unchanged",
			cfg:  Config{ManifestURL: "/srv/git/manifests/default.xml"},
			rel:  "/srv/git/other",
			want: "/srv/git/other",
		},
		{
			name: "manifest url join dot",
			cfg:  Config{ManifestURL: "https://example.com/a/manifest"},
			rel:  ".",
			want: "https://example.com/a/",
		},
		{
			name: "manifest url join dot slash",
			cfg:  Config{ManifestURL: "https://example.com/a/manifest"},
			rel:  "./x",
			want: "https://example.com/a/x",
		},
		{
			name: "manifest url join dotdot",
			cfg:  Config{ManifestURL: "https://example.com/a/b/manifest"},
			rel:  "..",
			want: "https://example.com/a/",
		},
		{
			name: "default remote url join",
			cfg:  Config{DefaultRemoteURL: "ssh://git@example.com/base/manifest"},
			rel:  "../foo",
			want: "ssh://git@example.com/foo",
		},
		{
			name: "scp style base normalized to ssh",
			cfg:  Config{ManifestURL: "git@example.com:base/manifest"},
			rel:  "../foo",
			want: "ssh://git@example.com/foo",
		},
		{
			name: "absolute file path uses filepath semantics",
			cfg:  Config{ManifestURL: "/srv/git/manifests/default.xml"},
			rel:  "../platform",
			want: "/srv/git/platform",
		},
		{
			name: "absolute file path dot",
			cfg:  Config{ManifestURL: "/srv/git/manifests/default.xml"},
			rel:  ".",
			want: "/srv/git/manifests",
		},
		{
			name: "relative file base unresolved",
			cfg:  Config{ManifestURL: "relative/manifests/default.xml"},
			rel:  "../foo",
			want: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.cfg.ResolveRelativeURL(tt.rel); got != tt.want {
				t.Errorf("ResolveRelativeURL(%q) = %q, want %q", tt.rel, got, tt.want)
			}
		})
	}
}

// TestExtractBaseURLHTTPSUserinfo 回归：https://user@host/... 必须先按
// http(s) 解析，返回 scheme://user@host，而不是被 scp 分支截成 "https"。
func TestExtractBaseURLHTTPSUserinfo(t *testing.T) {
	cfg := &Config{}
	got := cfg.ExtractBaseURLFromManifestURL("https://user@example.com/path/to/manifest.git")
	if want := "https://user@example.com"; got != want {
		t.Errorf("ExtractBaseURLFromManifestURL() = %q, want %q", got, want)
	}
}

// resetConfigCache 清空包级缓存，保证缓存相关测试互不干扰。
func resetConfigCache() {
	configMutex.Lock()
	configCache = nil
	configCachePath = ""
	configLastModTime = time.Time{}
	configMutex.Unlock()
}

// writeRepoConfig 在 root/.repo/config.json 写入最小可加载配置。
func writeRepoConfig(t *testing.T, root, manifestURL string) string {
	t.Helper()
	repoDir := filepath.Join(root, ".repo")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatalf("mkdir .repo: %v", err)
	}
	configPath := filepath.Join(repoDir, "config.json")
	data := []byte(`{"version":1,"manifest_url":"` + manifestURL + `","manifest_name":"default.xml"}`)
	if err := os.WriteFile(configPath, data, 0o644); err != nil {
		t.Fatalf("write config.json: %v", err)
	}
	return configPath
}

// TestConfigCacheKeyedByPath 回归：缓存 key 必须包含配置文件路径/RepoRoot，
// 在不同 repo 根目录之间不能复用旧配置；命中也返回副本。
func TestConfigCacheKeyedByPath(t *testing.T) {
	resetConfigCache()
	defer resetConfigCache()

	root1 := t.TempDir()
	root2 := t.TempDir()
	writeRepoConfig(t, root1, "https://one.example.com/manifest.git")
	writeRepoConfig(t, root2, "https://two.example.com/manifest.git")

	origDir, _ := os.Getwd()
	defer os.Chdir(origDir)

	if err := os.Chdir(root1); err != nil {
		t.Fatalf("chdir root1: %v", err)
	}
	cfg1, err := Load()
	if err != nil {
		t.Fatalf("Load root1: %v", err)
	}
	if cfg1.ManifestURL != "https://one.example.com/manifest.git" {
		t.Fatalf("root1 ManifestURL = %q", cfg1.ManifestURL)
	}

	// 修改 Load 返回的副本后再次 Load（同一路径应命中缓存），缓存不能被污染
	cfg1.ManifestURL = "mutated"
	cfg1Hit, err := Load()
	if err != nil {
		t.Fatalf("Load root1 (cache hit): %v", err)
	}
	if cfg1Hit.ManifestURL != "https://one.example.com/manifest.git" {
		t.Errorf("cached copy leaked mutation: ManifestURL = %q", cfg1Hit.ManifestURL)
	}

	if err := os.Chdir(root2); err != nil {
		t.Fatalf("chdir root2: %v", err)
	}
	cfg2, err := Load()
	if err != nil {
		t.Fatalf("Load root2: %v", err)
	}
	if cfg2.ManifestURL != "https://two.example.com/manifest.git" {
		t.Errorf("root2 ManifestURL = %q, want config from root2 (cache must be path-keyed)", cfg2.ManifestURL)
	}

	// 回到 root1 仍应得到 root1 的配置，不能被 root2 的缓存干扰
	if err := os.Chdir(root1); err != nil {
		t.Fatalf("chdir root1 again: %v", err)
	}
	cfg1b, err := Load()
	if err != nil {
		t.Fatalf("Load root1 again: %v", err)
	}
	if cfg1b.ManifestURL != "https://one.example.com/manifest.git" {
		t.Errorf("root1 ManifestURL after root2 load = %q, want root1 config", cfg1b.ManifestURL)
	}
}

// TestConfigCacheInvalidatesOnMtimeChange 回归：mtime 不同即失效（旧实现用
// !After，会把时间回退误判为仍有效而返回过期内容）。
func TestConfigCacheInvalidatesOnMtimeChange(t *testing.T) {
	resetConfigCache()
	defer resetConfigCache()

	root := t.TempDir()
	configPath := writeRepoConfig(t, root, "https://old.example.com/manifest.git")

	origDir, _ := os.Getwd()
	defer os.Chdir(origDir)
	if err := os.Chdir(root); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ManifestURL != "https://old.example.com/manifest.git" {
		t.Fatalf("initial ManifestURL = %q", cfg.ManifestURL)
	}

	// 重写为不同的内容，并把 mtime 调早（旧缓存 mtime 更晚）
	data := []byte(`{"version":1,"manifest_url":"https://new.example.com/manifest.git","manifest_name":"default.xml"}`)
	if err := os.WriteFile(configPath, data, 0o644); err != nil {
		t.Fatalf("rewrite config.json: %v", err)
	}
	older := time.Now().Add(-time.Hour)
	if err := os.Chtimes(configPath, older, older); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	cfg2, err := Load()
	if err != nil {
		t.Fatalf("Load after rewrite: %v", err)
	}
	if cfg2.ManifestURL != "https://new.example.com/manifest.git" {
		t.Errorf("ManifestURL = %q, stale cache used (mtime decrease must invalidate)", cfg2.ManifestURL)
	}
}

// TestGetRemoteURLUsesDiscoveredRepoRoot 回归：从子目录执行时 GetRemoteURL
// 必须用已发现的 repoRoot 读取 .repo/config.json，而不是依赖 CWD。
func TestGetRemoteURLUsesDiscoveredRepoRoot(t *testing.T) {
	root := t.TempDir()
	writeRepoConfig(t, root, "https://git.example.com/manifests/manifest.git")

	subDir := filepath.Join(root, "a", "b")
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		t.Fatalf("mkdir subdir: %v", err)
	}
	origDir, _ := os.Getwd()
	defer os.Chdir(origDir)
	if err := os.Chdir(subDir); err != nil {
		t.Fatalf("chdir subdir: %v", err)
	}

	cfg := &Config{}
	if got, want := cfg.GetRemoteURL(), "https://git.example.com"; got != want {
		t.Errorf("GetRemoteURL() = %q, want %q (must find .repo/config.json from subdir)", got, want)
	}
}
