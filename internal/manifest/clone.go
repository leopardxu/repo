package manifest

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// extractBaseURL 从清单URL中提取基础URL
func extractBaseURL(url string) string {
	if url == "" {
		return ""
	}

	// 处理SSH URL格式: ssh://git@example.com/path/to/repo
	if strings.HasPrefix(url, "ssh://") {
		// 查找第三个斜杠的位置（ssh://后的第一个斜杠）
		parts := strings.SplitN(url, "/", 4)
		if len(parts) >= 3 {
			// 返回 ssh://hostname 部分
			return strings.Join(parts[:3], "/")
		}
	}

	// 处理SCP格式: git@example.com:path/to/repo
	if strings.Contains(url, "@") && strings.Contains(url, ":") {
		// 查找冒号的位置
		parts := strings.SplitN(url, ":", 2)
		if len(parts) == 2 {
			// 返回 user@hostname 部分
			return parts[0]
		}
	}

	// 处理HTTP/HTTPS URL
	if strings.HasPrefix(url, "http://") || strings.HasPrefix(url, "https://") {
		// 查找第三个斜杠后的位置
		parts := strings.SplitN(url, "/", 4)
		if len(parts) >= 3 {
			// 返回 protocol://hostname 部分
			return strings.Join(parts[:3], "/")
		}
	}

	// 无法解析的情况下返回空字符串
	return ""
}

// CloneManifestRepo 克隆清单仓库
func CloneManifestRepo(gitRunner GitRunner, cfg *Config) error {
	if cfg == nil {
		return fmt.Errorf("配置不能为空")
	}

	if cfg.ManifestURL == "" {
		return fmt.Errorf("清单仓库URL不能为空")
	}

	// 开始克隆清单仓库

	// 创建.repo目录
	repoDir := ".repo"
	if err := os.MkdirAll(repoDir, 0755); err != nil {
		return fmt.Errorf("创建 %s 目录failed: %w", repoDir, err)
	}

	// 创建.repo/manifests目录
	manifestsDir := filepath.Join(repoDir, "manifests")
	if err := os.MkdirAll(manifestsDir, 0755); err != nil {
		return fmt.Errorf("创建 %s 目录failed: %w", manifestsDir, err)
	}

	// 处理URL中的..替换
	manifestURL := cfg.ManifestURL
	if strings.Contains(manifestURL, "..") {
		// 从清单URL中提取基础URL
		baseURL := extractBaseURL(cfg.ManifestURL)
		if baseURL != "" {
			// 替换..为baseURL
			manifestURL = strings.ReplaceAll(manifestURL, "..", baseURL)
		}
	}

	// 构建git clone命令参数
	args := []string{"clone"}

	// 添加深度参数
	if cfg.Depth > 0 {
		args = append(args, "--depth", fmt.Sprintf("%d", cfg.Depth))
	}

	// 添加分支参数
	if cfg.ManifestBranch != "" {
		args = append(args, "--branch", cfg.ManifestBranch)
	}

	// 添加镜像参数
	if cfg.Mirror {
		args = append(args, "--mirror")
	}

	// 添加引用参数
	if cfg.Reference != "" {
		args = append(args, "--reference", cfg.Reference)
	}

	// 添加URL和目标目录
	args = append(args, manifestURL, manifestsDir)

	// 执行git clone命令
	_, err := gitRunner.Run(args...)
	if err != nil {
		return fmt.Errorf("failed to clone manifest repository: %w", err)
	}

	// Do NOT create a symlink from .repo/manifest.xml to manifests/<name>.
	// The .repo/manifest.xml file is a generated (merged) file written by init/sync.
	// Symlinking it into the manifests git repo would cause WriteFile to follow
	// the symlink and overwrite the tracked file, corrupting the manifests repo
	// and breaking subsequent git fetch/reset operations.
	//
	// The caller (init.go) is responsible for writing .repo/manifest.xml after
	// parsing and merging the manifest.

	return nil
}
