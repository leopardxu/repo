package network

import (
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/leopardxu/repo-go/internal/logger"
)

// Client 网络客户端
type Client struct {
	httpClient         *http.Client
	userAgent          string
	retryCount         int
	retryDelay         time.Duration
	maxBodySize        int64  // 最大响应体大小
	proxyURL           string // 代理URL
	insecureSkipVerify bool   // 是否跳过证书验证
}

// ClientOption 客户端选项函数类型
type ClientOption func(*Client)

// WithUserAgent 设置用户代理
func WithUserAgent(userAgent string) ClientOption {
	return func(c *Client) {
		c.userAgent = userAgent
	}
}

// WithTimeout 设置超时时间
func WithTimeout(timeout time.Duration) ClientOption {
	return func(c *Client) {
		c.httpClient.Timeout = timeout
	}
}

// WithRetry 设置重试参数
func WithRetry(count int, delay time.Duration) ClientOption {
	return func(c *Client) {
		c.retryCount = count
		c.retryDelay = delay
	}
}

// WithMaxBodySize 设置最大响应体大小
func WithMaxBodySize(size int64) ClientOption {
	return func(c *Client) {
		c.maxBodySize = size
	}
}

// WithProxy 设置代理
func WithProxy(proxyURL string) ClientOption {
	return func(c *Client) {
		c.proxyURL = proxyURL
	}
}

// WithInsecureSkipVerify 设置是否跳过证书验证
func WithInsecureSkipVerify(skip bool) ClientOption {
	return func(c *Client) {
		c.insecureSkipVerify = skip
	}
}

// NewClient 创建网络客户
func NewClient(options ...ClientOption) *Client {
	// 创建HTTP客户
	// Proxy 默认使用 http.ProxyFromEnvironment，自动读取 HTTP_PROXY/HTTPS_PROXY/NO_PROXY，
	// 对齐上游 repo 与标准 Go HTTP 行为。WithProxy 可覆盖。
	httpClient := &http.Client{
		Timeout: 60 * time.Second,
		Transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			TLSClientConfig: &tls.Config{
				MinVersion:         tls.VersionTLS12,
				InsecureSkipVerify: false, // 默认不跳过证书验证
			},
			DisableCompression:    false,
			MaxIdleConns:          100,
			MaxIdleConnsPerHost:   10,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
		},
	}

	client := &Client{
		httpClient:  httpClient,
		userAgent:   "repo-go",
		retryCount:  3,
		retryDelay:  2 * time.Second,
		maxBodySize: 100 << 20, // 默认100MB
	}

	// 应用选项
	for _, option := range options {
		option(client)
	}

	// 如果设置了代理，则配置传输层
	if client.proxyURL != "" {
		if proxyURL, err := url.Parse(client.proxyURL); err == nil {
			httpClient.Transport.(*http.Transport).Proxy = http.ProxyURL(proxyURL)
		}
	}

	// 如果设置了跳过证书验证，则更新TLS配置
	if client.insecureSkipVerify {
		httpClient.Transport.(*http.Transport).TLSClientConfig.InsecureSkipVerify = true
	}

	return client
}

// SetUserAgent 设置用户代理
func (c *Client) SetUserAgent(userAgent string) {
	c.userAgent = userAgent
}

// SetTimeout 设置超时时间
func (c *Client) SetTimeout(timeout time.Duration) {
	c.httpClient.Timeout = timeout
}

// Download 下载文件
func (c *Client) Download(url string, destPath string) error {
	logger.Info("开始下载文 %s", url)
	logger.Debug("下载目标路径: %s", destPath)

	// 确保目标目录存在
	if err := os.MkdirAll(filepath.Dir(destPath), 0755); err != nil {
		logger.Error("failed to create target directoryfailed: %v", err)
		return fmt.Errorf("failed to create target directoryfailed: %w", err)
	}

	// 实现重试逻辑
	var lastErr error
	for attempt := 0; attempt <= c.retryCount; attempt++ {
		if attempt > 0 {
			logger.Warn("下载重试 (%d/%d): %s", attempt, c.retryCount, url)
			time.Sleep(c.retryDelay)
		}

		err := c.doDownload(url, destPath)
		if err == nil {
			logger.Info("文件下载成功: %s", destPath)
			return nil
		}

		lastErr = err
		logger.Warn("下载failed (%d/%d): %v", attempt+1, c.retryCount+1, err)
	}

	logger.Error("下载failed，已达到最大重试次 %v", lastErr)
	return fmt.Errorf("下载failed，已达到最大重试次 %w", lastErr)
}

// ProgressCallback 进度回调函数
type ProgressCallback func(current, total int64)

// doDownload 执行实际的下载操
func (c *Client) doDownload(url string, destPath string) error {
	// 创建请求
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return fmt.Errorf("创建请求failed: %w", err)
	}

	// 设置请求
	req.Header.Set("User-Agent", c.userAgent)

	// 发送请
	logger.Debug("发送HTTP请求: %s", url)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("发送请求失 %w", err)
	}
	defer resp.Body.Close()

	// 检查响应状
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("服务器返回非成功状态码: %s", resp.Status)
	}

	// 检查响应体大小
	contentLength := resp.ContentLength
	if contentLength > c.maxBodySize && c.maxBodySize > 0 {
		return fmt.Errorf("响应体过大: %d bytes, 超过限制 %d bytes", contentLength, c.maxBodySize)
	}

	// 创建目标文件
	out, err := os.Create(destPath)
	if err != nil {
		return fmt.Errorf("创建目标文件failed: %w", err)
	}
	defer out.Close()

	// 使用带进度的复制
	if contentLength > 0 {
		_, err = c.copyWithProgress(out, resp.Body, contentLength, func(_, _ int64) {
			// 进度回调：此处无需展示进度
		})
	} else {
		// 如果不知道大小，直接复制
		_, err = io.Copy(out, resp.Body)
	}
	if err != nil {
		return fmt.Errorf("写入文件failed: %w", err)
	}

	return nil
}

// copyWithProgress 带进度的复制
func (c *Client) copyWithProgress(dst io.Writer, src io.Reader, total int64, callback ProgressCallback) (int64, error) {
	buf := make([]byte, 32*1024) // 32KB缓冲区
	var copied int64

	for {
		n, err := src.Read(buf)
		if n > 0 {
			written, writeErr := dst.Write(buf[0:n])
			copied += int64(written)
			if callback != nil {
				callback(copied, total)
			}

			if writeErr != nil {
				return copied, writeErr
			}
			if n != written {
				return copied, io.ErrShortWrite
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return copied, err
		}
	}
	return copied, nil
}

// Get 发送GET请求并返回响
func (c *Client) Get(url string, headers map[string]string) (*http.Response, error) {
	logger.Debug("发送GET请求: %s", url)

	// 创建请求
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("创建请求failed: %w", err)
	}

	// 设置请求
	req.Header.Set("User-Agent", c.userAgent)
	for key, value := range headers {
		req.Header.Set(key, value)
	}

	// 发送请
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("发送请求失 %w", err)
	}

	return resp, nil
}

// IsValidURL 检查URL是否有效
func IsValidURL(rawURL string) bool {
	_, err := url.ParseRequestURI(rawURL)
	return err == nil
}
