package repo_sync

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/leopardxu/repo-go/internal/config"
	"github.com/leopardxu/repo-go/internal/logger"
	"github.com/leopardxu/repo-go/internal/manifest"
	"github.com/leopardxu/repo-go/internal/project"
)

// newSmartSyncTestEngine 构造一个仅包含 handleSmartSync 所需最小字段的引擎，便于测试。
func newSmartSyncTestEngine(opts *Options, mani *manifest.Manifest) *Engine {
	if opts == nil {
		opts = &Options{}
	}
	if mani == nil {
		mani = &manifest.Manifest{}
	}
	return &Engine{
		manifest: mani,
		options:  opts,
		logger:   logger.NewDefaultLogger(),
	}
}

const smartSyncApprovedManifest = `<?xml version="1.0" encoding="UTF-8"?>
<manifest>
  <remote name="origin" fetch="https://example.com" />
  <default remote="origin" revision="main" />
  <project name="approved" path="approved" />
</manifest>`

// xmlRPCArrayResponse 构造清单服务器返回值 [success, manifest/错误文本]。
func xmlRPCArrayResponse(t *testing.T, success bool, payload string) string {
	t.Helper()
	var escaped bytes.Buffer
	if err := xml.EscapeText(&escaped, []byte(payload)); err != nil {
		t.Fatalf("escape payload: %v", err)
	}
	boolean := "1"
	if !success {
		boolean = "0"
	}
	return fmt.Sprintf(`<?xml version="1.0"?>
<methodResponse><params><param><value><array><data>
<value><boolean>%s</boolean></value>
<value><string>%s</string></value>
</data></array></value></param></params></methodResponse>`, boolean, escaped.String())
}

// TestHandleSmartSyncXMLRPC 验证智能同步按上游 XML-RPC 协议 POST
// methodCall，并分别调用 GetApprovedManifest(branch,target) 与 GetManifest(tag)。
func TestHandleSmartSyncXMLRPC(t *testing.T) {
	tests := []struct {
		name       string
		opts       Options
		syncBranch string
		syncTarget string
		wantMethod string
		wantParams []string
	}{
		{
			name:       "approved manifest with branch and target",
			opts:       Options{SmartSync: true, Quiet: true},
			syncBranch: "main",
			syncTarget: "coral-userdebug",
			wantMethod: "GetApprovedManifest",
			wantParams: []string{"main", "coral-userdebug"},
		},
		{
			name:       "approved manifest without target",
			opts:       Options{SmartSync: true, Quiet: true},
			syncBranch: "main",
			wantMethod: "GetApprovedManifest",
			wantParams: []string{"main"},
		},
		{
			name:       "manifest by tag",
			opts:       Options{SmartTag: "v1.2.3", Quiet: true},
			syncBranch: "main",
			wantMethod: "GetManifest",
			wantParams: []string{"v1.2.3"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("SYNC_BRANCH", tt.syncBranch)
			t.Setenv("SYNC_TARGET", tt.syncTarget)
			t.Setenv("REPO_SYNC_TARGET", "")
			t.Setenv("TARGET_PRODUCT", "")
			t.Setenv("TARGET_RELEASE", "")
			t.Setenv("TARGET_BUILD_VARIANT", "")

			var requestBody string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost {
					t.Errorf("method = %s, want POST", r.Method)
				}
				if ct := r.Header.Get("Content-Type"); !strings.Contains(ct, "text/xml") {
					t.Errorf("Content-Type = %q, want text/xml", ct)
				}
				data, err := io.ReadAll(r.Body)
				if err != nil {
					t.Errorf("read request body: %v", err)
				}
				requestBody = string(data)
				fmt.Fprint(w, xmlRPCArrayResponse(t, true, smartSyncApprovedManifest))
			}))
			defer server.Close()

			mani := &manifest.Manifest{
				Default:        manifest.Default{Revision: "main"},
				ManifestServer: &manifest.ManifestServer{URL: server.URL},
			}
			e := newSmartSyncTestEngine(&tt.opts, mani)
			if err := e.handleSmartSync(); err != nil {
				t.Fatalf("handleSmartSync() error = %v", err)
			}

			if !strings.Contains(requestBody, "<methodCall>") {
				t.Errorf("request body is not XML-RPC methodCall:\n%s", requestBody)
			}
			if !strings.Contains(requestBody, "<methodName>"+tt.wantMethod+"</methodName>") {
				t.Errorf("request body missing method %s:\n%s", tt.wantMethod, requestBody)
			}
			for _, param := range tt.wantParams {
				if !strings.Contains(requestBody, "<string>"+param+"</string>") {
					t.Errorf("request body missing param %q:\n%s", param, requestBody)
				}
			}
			if string(e.manifestCache) != smartSyncApprovedManifest {
				t.Errorf("manifest cache not updated with server manifest")
			}
		})
	}
}

// TestHandleSmartSyncManifestServerURLOverridesManifest 验证 CLI
// --manifest-server-url 覆盖清单中的 <manifest-server>。
func TestHandleSmartSyncManifestServerURLOverridesManifest(t *testing.T) {
	var hits int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		fmt.Fprint(w, xmlRPCArrayResponse(t, true, smartSyncApprovedManifest))
	}))
	defer server.Close()

	mani := &manifest.Manifest{
		Default:        manifest.Default{Revision: "main"},
		ManifestServer: &manifest.ManifestServer{URL: "http://127.0.0.1:1/unreachable"},
	}
	e := newSmartSyncTestEngine(&Options{
		SmartSync:         true,
		Quiet:             true,
		ManifestServerURL: server.URL,
	}, mani)
	if err := e.handleSmartSync(); err != nil {
		t.Fatalf("handleSmartSync() error = %v", err)
	}
	if hits != 1 {
		t.Errorf("CLI --manifest-server-url should be used, hits = %d", hits)
	}
}

// TestHandleSmartSyncExplicitRequiresServer 验证显式 -s/--smart-tag 缺少
// manifest-server 时必须报错，不能静默退化为常规同步。
func TestHandleSmartSyncExplicitRequiresServer(t *testing.T) {
	tests := []struct {
		name string
		opts *Options
	}{
		{"explicit smart-sync", &Options{SmartSync: true}},
		{"explicit smart-tag", &Options{SmartTag: "v1.0"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newSmartSyncTestEngine(tt.opts, &manifest.Manifest{})
			err := e.handleSmartSync()
			if err == nil || !strings.Contains(err.Error(), "no manifest server") {
				t.Fatalf("显式智能同步缺少服务器应报错，实际: %v", err)
			}
		})
	}
}

// TestHandleSmartSyncImplicitDefaultFallsBack 验证清单
// <default sync-smartsync="true"> 隐式启用时，无服务器只告警回退。
func TestHandleSmartSyncImplicitDefaultFallsBack(t *testing.T) {
	mani := &manifest.Manifest{
		Default: manifest.Default{CustomAttrs: map[string]string{"sync-smartsync": "true"}},
	}
	e := newSmartSyncTestEngine(&Options{}, mani)
	if err := e.preSync(context.Background()); err != nil {
		t.Fatalf("隐式智能同步无服务器应回退，实际错误: %v", err)
	}
	if !e.options.SmartSync {
		t.Error("清单 sync-smartsync=true 应隐式启用 SmartSync")
	}
}

// TestPreSyncExplicitSmartSyncWithoutServerFails 验证显式 -s 经 preSync 仍报错。
func TestPreSyncExplicitSmartSyncWithoutServerFails(t *testing.T) {
	e := newSmartSyncTestEngine(&Options{SmartSync: true}, &manifest.Manifest{})
	err := e.preSync(context.Background())
	if err == nil || !strings.Contains(err.Error(), "smart-sync failed") {
		t.Fatalf("显式智能同步无服务器应经 preSync 报错，实际: %v", err)
	}
}

// TestHandleSmartSyncUsesHelper 验证 <manifest-server helper> 被执行，并以
// 其返回的代理地址转发 XML-RPC POST。
func TestHandleSmartSyncUsesHelper(t *testing.T) {
	var requestBody string
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		requestBody = string(data)
		fmt.Fprint(w, xmlRPCArrayResponse(t, true, smartSyncApprovedManifest))
	}))
	defer proxy.Close()

	helperPath := filepath.Join(t.TempDir(), "repo-remote-test-helper")
	script := fmt.Sprintf("#!/bin/sh\necho '{\"status\":\"ok\",\"message\":\"%s\"}'\n", proxy.URL)
	if err := os.WriteFile(helperPath, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}

	mani := &manifest.Manifest{
		Default: manifest.Default{Revision: "main"},
		ManifestServer: &manifest.ManifestServer{
			URL:    "http://manifest.invalid/rpc",
			Helper: helperPath,
		},
	}
	e := newSmartSyncTestEngine(&Options{SmartSync: true, Quiet: true}, mani)
	if err := e.handleSmartSync(); err != nil {
		t.Fatalf("handleSmartSync() via helper error = %v", err)
	}
	if !strings.Contains(requestBody, "<methodName>GetApprovedManifest</methodName>") {
		t.Errorf("helper 代理未收到 XML-RPC 请求:\n%s", requestBody)
	}
}

// TestHandleSmartSyncHelperMissing 验证 manifest 声明的 helper 不在 PATH 时报错。
func TestHandleSmartSyncHelperMissing(t *testing.T) {
	mani := &manifest.Manifest{
		ManifestServer: &manifest.ManifestServer{
			URL:    "http://example.com/rpc",
			Helper: "repo-helper-does-not-exist",
		},
	}
	e := newSmartSyncTestEngine(&Options{SmartSync: true, Quiet: true}, mani)
	err := e.handleSmartSync()
	if err == nil || !strings.Contains(err.Error(), "not found in PATH") {
		t.Fatalf("缺失 helper 应报错，实际: %v", err)
	}
}

// TestHandleSmartSyncRPCFailure 验证服务器返回 success=false 时按错误处理，
// 不把错误文本当作清单写入缓存。
func TestHandleSmartSyncRPCFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, xmlRPCArrayResponse(t, false, "no approved build"))
	}))
	defer server.Close()

	mani := &manifest.Manifest{
		Default:        manifest.Default{Revision: "main"},
		ManifestServer: &manifest.ManifestServer{URL: server.URL},
	}
	e := newSmartSyncTestEngine(&Options{SmartSync: true, Quiet: true}, mani)
	err := e.handleSmartSync()
	if err == nil || !strings.Contains(err.Error(), "no approved build") {
		t.Fatalf("RPC success=false 应报错并保留错误文本，实际: %v", err)
	}
}

// TestHandleSmartSync_NoManifestServerFlag_Skips 验证：--no-manifest-server
// 显式禁用清单服务器时跳过智能同步，即使清单中配置了 manifest-server 也返回 nil。
func TestHandleSmartSync_NoManifestServerFlag_Skips(t *testing.T) {
	e := newSmartSyncTestEngine(
		&Options{NoManifestServer: true},
		&manifest.Manifest{ManifestServer: &manifest.ManifestServer{URL: "http://example.com/api"}},
	)

	if err := e.handleSmartSync(); err != nil {
		t.Fatalf("期望 --no-manifest-server 时跳过智能同步（返回 nil），实际得到错误: %v", err)
	}
}

// TestDefaultSyncSmartSyncEnabled 验证 <default sync-smartsync> 只接受 true。
func TestDefaultSyncSmartSyncEnabled(t *testing.T) {
	tests := []struct {
		name  string
		attrs map[string]string
		want  bool
	}{
		{"nil manifest", nil, false},
		{"missing attr", map[string]string{}, false},
		{"true", map[string]string{"sync-smartsync": "true"}, true},
		{"TRUE", map[string]string{"sync-smartsync": "TRUE"}, true},
		{"false", map[string]string{"sync-smartsync": "false"}, false},
		{"other", map[string]string{"sync-smartsync": "yes"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var m *manifest.Manifest
			if tt.attrs != nil {
				m = &manifest.Manifest{Default: manifest.Default{CustomAttrs: tt.attrs}}
			}
			if got := defaultSyncSmartSyncEnabled(m); got != tt.want {
				t.Errorf("defaultSyncSmartSyncEnabled() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestResolveSmartSyncTarget 验证智能同步目标的环境变量解析契约：
// REPO_SYNC_TARGET（本仓 REPO_ 前缀约定）优先；为空时回退 AOSP 惯例变量
// SYNC_TARGET 与 TARGET_PRODUCT+TARGET_BUILD_VARIANT（上游 smartsync 兼容）。
func TestResolveSmartSyncTarget(t *testing.T) {
	tests := []struct {
		name           string
		repoSyncTarget string
		syncTarget     string
		targetProduct  string
		targetRelease  string
		buildVariant   string
		want           string
	}{
		{
			name:           "REPO_SYNC_TARGET优先于AOSP惯例变量",
			repoSyncTarget: "alpha-user",
			syncTarget:     "legacy-target",
			targetProduct:  "coral",
			buildVariant:   "userdebug",
			want:           "alpha-user",
		},
		{
			name:           "回退SYNC_TARGET",
			repoSyncTarget: "",
			syncTarget:     "legacy-target",
			targetProduct:  "coral",
			buildVariant:   "userdebug",
			want:           "legacy-target",
		},
		{
			name:           "回退TARGET_PRODUCT与VARIANT拼接",
			repoSyncTarget: "",
			syncTarget:     "",
			targetProduct:  "coral",
			buildVariant:   "userdebug",
			want:           "coral-userdebug",
		},
		{
			name:           "product+release+variant拼接",
			repoSyncTarget: "",
			syncTarget:     "",
			targetProduct:  "coral",
			targetRelease:  "cheetah",
			buildVariant:   "userdebug",
			want:           "coral-cheetah-userdebug",
		},
		{
			name:           "全部未设置返回空",
			repoSyncTarget: "",
			syncTarget:     "",
			targetProduct:  "",
			buildVariant:   "",
			want:           "",
		},
		{
			name:           "product与variant缺一不拼接",
			repoSyncTarget: "",
			syncTarget:     "",
			targetProduct:  "coral",
			buildVariant:   "",
			want:           "",
		},
		{
			name:           "variant有值product为空不拼接",
			repoSyncTarget: "",
			syncTarget:     "",
			targetProduct:  "",
			buildVariant:   "userdebug",
			want:           "",
		},
		{
			name:           "REPO_SYNC_TARGET为空串视为未设置",
			repoSyncTarget: "",
			syncTarget:     "legacy-target",
			targetProduct:  "",
			buildVariant:   "",
			want:           "legacy-target",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("REPO_SYNC_TARGET", tt.repoSyncTarget)
			t.Setenv("SYNC_TARGET", tt.syncTarget)
			t.Setenv("TARGET_PRODUCT", tt.targetProduct)
			t.Setenv("TARGET_RELEASE", tt.targetRelease)
			t.Setenv("TARGET_BUILD_VARIANT", tt.buildVariant)

			if got := resolveSmartSyncTarget(); got != tt.want {
				t.Errorf("resolveSmartSyncTarget() = %q, 期望 %q", got, tt.want)
			}
		})
	}
}

// TestReloadManifestFromCachePreservesProjectSubset 验证 smart-sync 重载清单后
// 仍只保留 SetProjectSubset 传入的项目子集，不会把新清单里的其他项目一并同步。
func TestReloadManifestFromCachePreservesProjectSubset(t *testing.T) {
	const manifestXML = `<?xml version="1.0" encoding="UTF-8"?>
<manifest>
  <remote name="origin" fetch="https://example.com" />
  <default remote="origin" revision="main" />
  <project name="a" path="a" />
  <project name="b" path="b" />
  <project name="c" path="c" />
</manifest>`

	tests := []struct {
		name      string
		selected  []string
		wantNames []string
	}{
		{
			name:      "no subset keeps all reloaded projects",
			selected:  nil,
			wantNames: []string{"a", "b", "c"},
		},
		{
			name:      "subset filters reloaded projects by name",
			selected:  []string{"b"},
			wantNames: []string{"b"},
		},
		{
			name:      "multiple subset names preserved",
			selected:  []string{"a", "c"},
			wantNames: []string{"a", "c"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := &Engine{
				options:       &Options{},
				logger:        logger.NewDefaultLogger(),
				manifestCache: []byte(manifestXML),
			}
			e.SetProjectSubset(tt.selected)

			if err := e.reloadManifestFromCache(); err != nil {
				t.Fatalf("reloadManifestFromCache() error = %v", err)
			}

			got := make([]string, 0, len(e.projects))
			for _, p := range e.projects {
				got = append(got, p.Name)
			}
			if len(got) != len(tt.wantNames) {
				t.Fatalf("reloaded projects = %v, want %v", got, tt.wantNames)
			}
			for i := range tt.wantNames {
				if got[i] != tt.wantNames[i] {
					t.Errorf("reloaded projects = %v, want %v", got, tt.wantNames)
				}
			}
		})
	}
}

// TestGetBranchUsesConfigAndSyncBranch 验证 getBranch 不再读取 ManifestProject：
// SYNC_BRANCH 覆盖 config.ManifestBranch；两者皆空时回退清单 default revision。
func TestGetBranchUsesConfigAndSyncBranch(t *testing.T) {
	tests := []struct {
		name       string
		syncBranch string
		cfgBranch  string
		defaultRev string
		want       string
	}{
		{"SYNC_BRANCH overrides config", "cix_master", "main", "fallback", "cix_master"},
		{"config branch when no env", "", "main", "fallback", "main"},
		{"manifest default revision fallback", "", "", "refs/heads/fallback", "fallback"},
		{"all empty", "", "", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("SYNC_BRANCH", tt.syncBranch)
			e := &Engine{
				config:   &config.Config{ManifestBranch: tt.cfgBranch},
				manifest: &manifest.Manifest{Default: manifest.Default{Revision: tt.defaultRev}},
			}
			if got := e.getBranch(); got != tt.want {
				t.Errorf("getBranch() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestReloadManifestFromCacheNoSubsetKeepsNewProjects 验证无项目参数时，
// smart-sync 重载后的新项目不会因旧的 SetProjects 列表而被过滤。
func TestReloadManifestFromCacheNoSubsetKeepsNewProjects(t *testing.T) {
	const manifestXML = `<?xml version="1.0" encoding="UTF-8"?>
<manifest>
  <remote name="origin" fetch="https://example.com" />
  <default remote="origin" revision="main" />
  <project name="old" path="old" />
  <project name="new" path="new" />
</manifest>`

	e := &Engine{
		options:       &Options{},
		logger:        logger.NewDefaultLogger(),
		manifestCache: []byte(manifestXML),
	}
	// 模拟命令行只解析到旧项目（SetProjects 不应记录过滤集合）。
	e.SetProjects([]*project.Project{{Name: "old"}})

	if err := e.reloadManifestFromCache(); err != nil {
		t.Fatalf("reloadManifestFromCache() error = %v", err)
	}
	got := make([]string, 0, len(e.projects))
	for _, p := range e.projects {
		got = append(got, p.Name)
	}
	if len(got) != 2 || got[0] != "old" || got[1] != "new" {
		t.Errorf("reloaded projects = %v, want [old new]", got)
	}
}
