package repo_sync

import (
	"testing"

	"github.com/leopardxu/repo-go/internal/manifest"
)

// TestHyperSyncAvailable 验证 HyperSync 只在确实配置了清单服务器时启用：
// runner 会无条件传 --hyper-sync，未配置服务器时必须安静退化为全量同步。
func TestHyperSyncAvailable(t *testing.T) {
	serverManifest := &manifest.Manifest{
		ManifestServer: &manifest.ManifestServer{URL: "http://manifest.example.com"},
	}

	tests := []struct {
		name string
		opts *Options
		m    *manifest.Manifest
		want bool
	}{
		{"hyper-sync not requested", &Options{}, serverManifest, false},
		{"requested without server", &Options{HyperSync: true}, &manifest.Manifest{}, false},
		{"requested with manifest server", &Options{HyperSync: true}, serverManifest, true},
		{"requested with cli server url", &Options{HyperSync: true, ManifestServerURL: "https://cli.example.com"}, &manifest.Manifest{}, true},
		{"no-manifest-server disables", &Options{HyperSync: true, NoManifestServer: true}, serverManifest, false},
		{"nil manifest", &Options{HyperSync: true}, nil, false},
		{"nil options", nil, serverManifest, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := &Engine{options: tt.opts, manifest: tt.m}
			if got := e.hyperSyncAvailable(); got != tt.want {
				t.Errorf("hyperSyncAvailable() = %v, want %v", got, tt.want)
			}
		})
	}
}
