package repo_sync

import (
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// handleSmartSync 处理智能同步
func (e *Engine) handleSmartSync() error {
	// --no-manifest-server：显式禁用清单服务器，跳过智能同步，按当前清单执行常规同步
	if e.options.NoManifestServer {
		e.logger.Warn("已通过 --no-manifest-server 禁用清单服务器，跳过智能同步，将按当前清单执行常规同步")
		return nil
	}

	// 智能同步依赖清单服务器获取已审批清单。优先使用清单中定义的 manifest-server，
	// 其次回退到命令行 --manifest-server-url；两者皆无时给出告警而非中断（对齐
	// preSync 的约定与上游 repo 行为），退化为常规同步。
	manifestServer := ""
	if e.manifest.ManifestServer != nil {
		manifestServer = e.manifest.ManifestServer.URL
	} else if e.options.ManifestServerURL != "" {
		manifestServer = e.options.ManifestServerURL
	} else {
		e.logger.Warn("manifest-server not defined in manifest 且未通过 --manifest-server-url 指定，无法进行智能同步，将按当前清单执行常规同步")
		return nil
	}

	if !e.options.Quiet {
		fmt.Printf("使用清单服务%s\n", manifestServer)
	}

	// 处理认证
	if !strings.Contains(manifestServer, "@") {
		username := e.options.ManifestServerUsername
		password := e.options.ManifestServerPassword

		if username != "" && password != "" {
			// 将用户名和密码添加到URL
			u, err := url.Parse(manifestServer)
			if err == nil {
				u.User = url.UserPassword(username, password)
				manifestServer = u.String()
			}
		}
	}

	// 创建临时清单文件
	smartSyncManifestPath := filepath.Join(e.manifest.RepoDir, "smart-sync-manifest.xml")

	// 获取分支名称
	branch := e.getBranch()

	// 构建请求
	// 使用 http.ProxyFromEnvironment 让 HTTP_PROXY/HTTPS_PROXY/NO_PROXY 生效
	httpTimeout := e.options.HTTPTimeout
	if httpTimeout <= 0 {
		httpTimeout = 60 * time.Second // 默认 60 秒，避免 0 表示无超时导致永久挂起
	}
	client := &http.Client{
		Timeout: httpTimeout,
		Transport: &http.Transport{
			Proxy:               http.ProxyFromEnvironment,
			MaxIdleConns:        10,
			IdleConnTimeout:     30 * time.Second,
			TLSHandshakeTimeout: 10 * time.Second,
			TLSClientConfig: &tls.Config{
				MinVersion: tls.VersionTLS12,
			},
		},
	}

	var requestURL string
	if e.options.SmartSync {
		// 环境变量契约：所有新增环境变量一律使用 REPO_ 前缀，因此目标标识
		// 优先读取 REPO_SYNC_TARGET；为空时回退 AOSP 构建环境惯例变量以保持
		// 上游 smartsync 语义兼容（先 SYNC_TARGET，再由 TARGET_PRODUCT 与
		// TARGET_BUILD_VARIANT 拼接为 "<product>-<variant>"）。详见
		// resolveSmartSyncTarget。
		target := resolveSmartSyncTarget()

		if target != "" {
			requestURL = fmt.Sprintf("%s/api/GetApprovedManifest?branch=%s&target=%s",
				manifestServer, url.QueryEscape(branch), url.QueryEscape(target))
		} else {
			requestURL = fmt.Sprintf("%s/api/GetApprovedManifest?branch=%s",
				manifestServer, url.QueryEscape(branch))
		}
	} else {
		requestURL = fmt.Sprintf("%s/api/GetManifest?tag=%s",
			manifestServer, url.QueryEscape(e.options.SmartTag))
	}

	// 发送请求，带重试机
	var resp *http.Response
	var err error
	maxRetries := 3
	for i := 0; i < maxRetries; i++ {
		resp, err = client.Get(requestURL)
		if err == nil {
			break
		}
		if i < maxRetries-1 {
			time.Sleep(time.Second * time.Duration(i+1))
		}
	}
	if err != nil {
		return fmt.Errorf("重试%d次后仍无法连接清单服务器: %w", maxRetries, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// 排空 body 以便连接复用；丢弃场景下复制出错无需处理
		_, _ = io.Copy(io.Discard, resp.Body)
		return fmt.Errorf("清单服务器返回状态码%d", resp.StatusCode)
	}

	// 读取响应
	manifestStr, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("从服务器读取清单时出错: %w", err)
	}

	// 使用内存缓存处理清单
	e.manifestCache = manifestStr

	// 重新加载清单
	if err := e.reloadManifestFromCache(); err != nil {
		return err
	}

	// 可选：写入临时文件用于调试
	if e.options.Debug {
		if err := os.WriteFile(smartSyncManifestPath, manifestStr, 0644); err != nil {
			return fmt.Errorf("将清单写%s 时出 %w", smartSyncManifestPath, err)
		}
	}

	return nil
}

// resolveSmartSyncTarget 决定智能同步请求的目标标识（GetApprovedManifest
// 的 target 参数）。环境变量契约：
//  1. 优先读取本仓约定的 REPO_SYNC_TARGET（新增环境变量一律使用 REPO_ 前缀）；
//  2. 为空时回退 AOSP 构建环境惯例变量，保持上游 repo smartsync 语义兼容：
//     先取 SYNC_TARGET；再由 TARGET_PRODUCT + TARGET_BUILD_VARIANT 拼接为
//     "<product>-<variant>"（两者均非空才生效）；
//  3. 三者皆无时返回空字符串，请求将不带 target 参数。
func resolveSmartSyncTarget() string {
	if target := os.Getenv("REPO_SYNC_TARGET"); target != "" {
		return target
	}
	if target := os.Getenv("SYNC_TARGET"); target != "" {
		return target
	}
	product := os.Getenv("TARGET_PRODUCT")
	variant := os.Getenv("TARGET_BUILD_VARIANT")
	if product != "" && variant != "" {
		return fmt.Sprintf("%s-%s", product, variant)
	}
	return ""
}

// getBranch 获取当前分支名称
func (e *Engine) getBranch() string {
	p := e.manifest.ManifestProject
	branch, err := p.GetBranch()
	if err != nil {
		return ""
	}
	branch = strings.TrimPrefix(branch, "refs/heads/")
	return branch
}
