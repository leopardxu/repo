package network

import (
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/leopardxu/repo-go/internal/logger"
)

// 超时相关默认值
const (
	// defaultResponseHeaderTimeout 限制从请求发出到收到响应头的时间，
	// 避免连接建立后在等待响应头时无限挂起。
	defaultResponseHeaderTimeout = 60 * time.Second
	// defaultRequestTimeout 单次请求（含读取响应体）的总超时上限。
	// 本 API 未接受 context.Context，无法由调用方取消请求，因此使用较大的上限：
	// 100MB 慢链路不会因 60s 总超时必然失败，同时仍能避免永久挂起。
	defaultRequestTimeout = 30 * time.Minute
	// defaultDialTimeout 建立 TCP 连接的上限，避免黑洞地址/半开连接导致永久挂起。
	defaultDialTimeout = 30 * time.Second
	// defaultTCPKeepAlive TCP keepalive 间隔，用于探测已失效的长连接。
	defaultTCPKeepAlive = 30 * time.Second
)

// ErrBodyTooLarge 表示响应体实际读取字节数超过客户端允许的最大值。
// 该判断基于实际读取字节，不依赖 Content-Length（分块传输或服务端谎报同样生效）。
var ErrBodyTooLarge = errors.New("响应体超过大小限制")

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

// newDialer 返回带建连上限与 keepalive 的 TCP 拨号器。
// 黑洞地址最多等待 defaultDialTimeout，避免建连阶段无限挂起。
func newDialer() *net.Dialer {
	return &net.Dialer{
		Timeout:   defaultDialTimeout,
		KeepAlive: defaultTCPKeepAlive,
	}
}

// NewClient 创建网络客户
func NewClient(options ...ClientOption) *Client {
	// 创建HTTP客户
	// Proxy 默认使用 http.ProxyFromEnvironment，自动读取 HTTP_PROXY/HTTPS_PROXY/NO_PROXY，
	// 对齐上游 repo 与标准 Go HTTP 行为。WithProxy 可覆盖。
	httpClient := &http.Client{
		// 总超时（含读取响应体）使用较大上限，见 defaultRequestTimeout 注释。
		// WithTimeout 可覆盖此值。
		Timeout: defaultRequestTimeout,
		Transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			// 显式设置建连超时与 keepalive：http.Transport 默认不设 DialContext，
			// 仅靠 http.Client.Timeout 兜底时黑洞地址可能长时间占用调用方。
			DialContext: newDialer().DialContext,
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
			ResponseHeaderTimeout: defaultResponseHeaderTimeout,
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

// createDownloadTempFile 创建下载临时文件；抽成变量便于测试模拟磁盘满等失败。
var createDownloadTempFile = func(dir, pattern string) (*os.File, error) {
	return os.CreateTemp(dir, pattern)
}

// isPermissionLikeError 判断错误是否来自权限不足或只读文件系统。
// 仅这两类错误值得退化为直接写目标文件：目录不可新建文件时，已存在且可写的
// 目标文件仍可能完成下载。磁盘满（ENOSPC）、文件描述符耗尽（EMFILE）等其他
// 错误必须原样返回，退化写入只会掩盖真实故障并可能破坏旧文件。
func isPermissionLikeError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, fs.ErrPermission) {
		return true
	}
	// EROFS（只读文件系统）在部分平台不会映射为 fs.ErrPermission，
	// 这里用错误文本兜底识别，避免引入平台相关的 syscall 常量。
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "read-only file system") ||
		strings.Contains(msg, "read only file system")
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
		if errors.Is(err, ErrBodyTooLarge) {
			// 响应体超限是确定性错误，重试不会改变结果
			logger.Error("下载failed: %v", err)
			return fmt.Errorf("下载failed: %w", err)
		}

		lastErr = err
		logger.Warn("下载failed (%d/%d): %v", attempt+1, c.retryCount+1, err)
	}

	logger.Error("下载failed，已达到最大重试次 %v", lastErr)
	return fmt.Errorf("下载failed，已达到最大重试次 %w", lastErr)
}

// ProgressCallback 进度回调函数
type ProgressCallback func(current, total int64)

// doDownload 执行实际的下载操作。
// 优先写入目标目录下的临时文件，全部读取成功后再 rename 覆盖目标文件：
// 失败时会删除临时文件，不会提前截断已存在的文件。
// 若目标目录无法创建临时文件（只读挂载、目录权限不足等），
// 退化为直接写目标文件（非原子，失败时尽力删除半成品）。
func (c *Client) doDownload(url string, destPath string) error {
	// 创建请求
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return fmt.Errorf("创建请求failed: %w", err)
	}

	// 设置请求
	req.Header.Set("User-Agent", c.userAgent)

	// 发送请求
	logger.Debug("发送HTTP请求: %s", url)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("发送请求失败: %w", err)
	}
	defer resp.Body.Close()

	// 检查响应状态
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("服务器返回非成功状态码: %s", resp.Status)
	}

	// Content-Length 已知且超限时快速失败；未知（-1）或分块响应仍由 limitBody 兜底。
	if err := c.checkContentLength(resp.ContentLength); err != nil {
		return err
	}

	// 创建临时文件（与目标同目录，保证 rename 不跨文件系统）
	tmpFile, err := createDownloadTempFile(filepath.Dir(destPath), ".repo-download-*")
	if err != nil {
		// 只有权限不足或只读文件系统才退化为直接写目标文件：
		// 目录不允许新建文件时，已存在且可写的目标文件仍能完成下载。
		// 磁盘满、文件描述符耗尽等错误直接返回，重试或退化都无法解决，
		// 且退化写入可能破坏已存在的目标文件。
		if !isPermissionLikeError(err) {
			return fmt.Errorf("创建临时文件failed: %w", err)
		}
		logger.Warn("创建临时文件failed，退化为直接写入目标文件: %v", err)
		return c.writeInPlace(resp, destPath)
	}
	tmpPath := tmpFile.Name()
	// 任一步骤失败都清理临时文件，避免留下半成品
	succeeded := false
	defer func() {
		if !succeeded {
			_ = tmpFile.Close()
			_ = os.Remove(tmpPath)
		}
	}()

	// 临时文件默认 0600，显式设置为常规下载文件权限
	if err := tmpFile.Chmod(0644); err != nil {
		return fmt.Errorf("设置临时文件权限failed: %w", err)
	}

	// 实际读取时按 maxBodySize 限流，防止 Content-Length 缺失/谎报导致超限写入
	if err := c.writeResponseBody(tmpFile, resp); err != nil {
		return err
	}

	if err := tmpFile.Close(); err != nil {
		return fmt.Errorf("关闭临时文件failed: %w", err)
	}

	if err := os.Rename(tmpPath, destPath); err != nil {
		return fmt.Errorf("重命名临时文件failed: %w", err)
	}
	succeeded = true
	return nil
}

// writeResponseBody 将响应体按 maxBodySize 限流写入 dst，
// 并在服务端声明长度时校验完整性。临时文件与直接写入两条路径共用该逻辑，
// 保证回退路径同样受大小上限保护。
func (c *Client) writeResponseBody(dst io.Writer, resp *http.Response) error {
	body := limitBody(resp.Body, c.maxBodySize)

	var copied int64
	var err error
	if resp.ContentLength > 0 {
		copied, err = c.copyWithProgress(dst, body, resp.ContentLength, func(_, _ int64) {
			// 进度回调：此处无需展示进度
		})
	} else {
		copied, err = c.copyWithProgress(dst, body, -1, nil)
	}
	if err != nil {
		return fmt.Errorf("写入文件failed: %w", err)
	}
	// 服务端声明了长度却提前结束，说明下载不完整，不能覆盖目标文件
	if resp.ContentLength > 0 && copied != resp.ContentLength {
		return fmt.Errorf("下载不完整: 期望 %d 字节，实际 %d 字节", resp.ContentLength, copied)
	}
	return nil
}

// writeInPlace 在无法创建临时文件时直接写入目标文件（非原子）。
// 目录不可写时已存在的目标文件仍可能可写，因此该回退仍有实用价值。
//
// 覆盖前先记录目标文件是否已存在，并尽力把旧内容备份到系统临时目录：
//   - 失败且目标原本不存在：删除本次新建的半成品；
//   - 失败且目标原本已存在：用备份恢复旧内容，绝不直接删除旧文件；
//     恢复失败时保留备份并把恢复错误与下载错误一起返回，便于用户手工找回；
//   - 成功：删除备份。
func (c *Client) writeInPlace(resp *http.Response, destPath string) (retErr error) {
	// 打开（O_TRUNC 会立即截断）前记录目标状态，决定失败时能否安全删除。
	_, statErr := os.Stat(destPath)
	existed := statErr == nil
	if statErr != nil && !os.IsNotExist(statErr) {
		// 状态未知时保守处理为已存在，失败时宁可保留也不误删。
		existed = true
	}

	backupPath, backupErr := backupExistingFile(destPath, existed)
	if backupErr != nil {
		// 已有目标文件却无法备份时拒绝截断：宁可下载失败，也不破坏旧文件。
		removeBackupFile(backupPath)
		return fmt.Errorf("备份已存在的目标文件failed，拒绝直接覆盖: %w", backupErr)
	}

	file, err := os.OpenFile(destPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		removeBackupFile(backupPath)
		return fmt.Errorf("创建临时文件failed且无法直接写入目标文件: %w", err)
	}

	succeeded := false
	defer func() {
		if succeeded {
			removeBackupFile(backupPath)
			return
		}
		_ = file.Close()
		if existed {
			// 目标原本就存在：用备份恢复，绝不删除旧目标文件。
			// 仅恢复成功才删除备份；恢复失败时备份是旧内容的唯一副本，
			// 必须保留，并把恢复错误（含备份路径）与下载错误一起返回。
			if restoreErr := restoreBackupFile(backupPath, destPath); restoreErr != nil {
				logger.Warn("恢复目标文件failed，备份保留于 %s: %v", backupPath, restoreErr)
				retErr = errors.Join(retErr, fmt.Errorf("恢复目标文件失败，备份保留于 %s: %w", backupPath, restoreErr))
				return
			}
			removeBackupFile(backupPath)
			return
		}
		// 目标由本次调用新建：失败时清理半成品。
		_ = os.Remove(destPath)
	}()

	if err := c.writeResponseBody(file, resp); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("关闭目标文件failed: %w", err)
	}
	succeeded = true
	return nil
}

// backupExistingFile 把已存在的目标文件复制到系统临时目录，返回备份路径。
// 目标原本不存在时返回空路径与 nil。备份失败时返回错误：调用方必须在
// 截断目标文件之前终止，避免失败下载破坏旧文件。
func backupExistingFile(destPath string, existed bool) (string, error) {
	if !existed {
		return "", nil
	}
	src, err := os.Open(destPath)
	if err != nil {
		return "", err
	}
	defer src.Close()

	backup, err := os.CreateTemp("", "repo-download-backup-*")
	if err != nil {
		return "", err
	}
	backupPath := backup.Name()
	if _, err := io.Copy(backup, src); err != nil {
		_ = backup.Close()
		_ = os.Remove(backupPath)
		return "", err
	}
	if err := backup.Close(); err != nil {
		_ = os.Remove(backupPath)
		return "", err
	}
	return backupPath, nil
}

// removeBackupFile 删除备份文件；路径为空时不做任何事。
func removeBackupFile(backupPath string) {
	if backupPath == "" {
		return
	}
	_ = os.Remove(backupPath)
}

// restoreBackupFile 用备份内容覆盖目标文件，失败时返回错误。
// 抽成变量便于测试模拟恢复失败，验证此时唯一备份不会被删除。
var restoreBackupFile = func(backupPath, destPath string) error {
	if backupPath == "" {
		return errors.New("没有可用的目标文件备份")
	}
	backup, err := os.Open(backupPath)
	if err != nil {
		return err
	}
	defer backup.Close()

	out, err := os.OpenFile(destPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, backup); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

// checkContentLength 在服务端提供 Content-Length 时快速拒绝超限响应。
// 实际读取仍由 limitBody 兜底，因此分块传输或服务端谎报同样会被拦截。
func (c *Client) checkContentLength(contentLength int64) error {
	if c.maxBodySize > 0 && contentLength > c.maxBodySize {
		return fmt.Errorf("%w: %d bytes, 超过限制 %d bytes", ErrBodyTooLarge, contentLength, c.maxBodySize)
	}
	return nil
}

// bodyLimitReader 在底层 Reader 实际返回超过上限的字节时立即报错。
type bodyLimitReader struct {
	r         io.ReadCloser
	remaining int64
}

func (b *bodyLimitReader) Read(p []byte) (int, error) {
	if b.remaining <= 0 {
		// 已读满上限：探测是否还有数据，有则说明超限，而不是恰好结束
		var probe [1]byte
		n, err := b.r.Read(probe[:])
		if n > 0 {
			return 0, ErrBodyTooLarge
		}
		return 0, err
	}

	if int64(len(p)) > b.remaining {
		p = p[:int(b.remaining)]
	}
	n, err := b.r.Read(p)
	b.remaining -= int64(n)
	return n, err
}

func (b *bodyLimitReader) Close() error {
	return b.r.Close()
}

// limitBody 按 maxBodySize 包装响应体；maxBodySize <= 0 表示不限制。
func limitBody(body io.ReadCloser, maxBodySize int64) io.ReadCloser {
	if maxBodySize <= 0 {
		return body
	}
	return &bodyLimitReader{r: body, remaining: maxBodySize}
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

	// 发送请求
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("发送请求失败: %w", err)
	}

	// 已知长度超限时直接失败，避免调用方读取超大响应
	if err := c.checkContentLength(resp.ContentLength); err != nil {
		_ = resp.Body.Close()
		return nil, err
	}

	// 对实际读取字节限流：Content-Length 为 -1、分块传输或谎报时同样生效
	resp.Body = limitBody(resp.Body, c.maxBodySize)

	return resp, nil
}

// IsValidURL 检查URL是否有效
func IsValidURL(rawURL string) bool {
	_, err := url.ParseRequestURI(rawURL)
	return err == nil
}
