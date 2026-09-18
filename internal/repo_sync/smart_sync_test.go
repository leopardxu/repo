package repo_sync

import (
	"testing"

	"github.com/leopardxu/repo-go/internal/logger"
	"github.com/leopardxu/repo-go/internal/manifest"
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

// TestHandleSmartSync_NoManifestServer_FallsBack 验证：清单未定义 manifest-server
// 且未通过 --manifest-server-url 指定时，智能同步给出告警并退化为常规同步（返回 nil），
// 而非硬性失败。对齐 preSync 注释约定与上游 repo 行为。
func TestHandleSmartSync_NoManifestServer_FallsBack(t *testing.T) {
	// ManifestServer 为 nil，且未提供 --manifest-server-url
	e := newSmartSyncTestEngine(&Options{}, &manifest.Manifest{})

	if err := e.handleSmartSync(); err != nil {
		t.Fatalf("期望未配置清单服务器时退化为常规同步（返回 nil），实际得到错误: %v", err)
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

// TestResolveSmartSyncTarget 验证智能同步目标的环境变量解析契约：
// REPO_SYNC_TARGET（本仓 REPO_ 前缀约定）优先；为空时回退 AOSP 惯例变量
// SYNC_TARGET 与 TARGET_PRODUCT+TARGET_BUILD_VARIANT（上游 smartsync 兼容）。
func TestResolveSmartSyncTarget(t *testing.T) {
	tests := []struct {
		name           string
		repoSyncTarget string
		syncTarget     string
		targetProduct  string
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
			t.Setenv("TARGET_BUILD_VARIANT", tt.buildVariant)

			if got := resolveSmartSyncTarget(); got != tt.want {
				t.Errorf("resolveSmartSyncTarget() = %q, 期望 %q", got, tt.want)
			}
		})
	}
}
