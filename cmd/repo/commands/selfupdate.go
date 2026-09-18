package commands

import (
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/leopardxu/repo-go/internal/logger"
	"github.com/spf13/cobra"
)

// maxBinarySize 限制自更新二进制的下载体积上限（256MB），
// 防止异常响应（如被重定向到超大文件）写满磁盘。
const maxBinarySize int64 = 256 << 20

// SelfUpdateOptions 包含 selfupdate 命令的选项
type SelfUpdateOptions struct {
	URL      string
	Checksum string
	Check    bool
	Quiet    bool
}

// SelfUpdateCmd 返回 selfupdate 命令
// repo-go 以编译二进制分发：selfupdate 从给定 URL（或 REPO_SELFUPDATE_URL 环境变量）
// 下载新二进制并原子替换当前可执行文件。未提供 URL 时打印当前版本与更新指引。
func SelfUpdateCmd(info VersionInfo) *cobra.Command {
	opts := &SelfUpdateOptions{}
	cmd := &cobra.Command{
		Use:   "selfupdate",
		Short: "Update repo to the latest version",
		Long: `Update the repo launcher to the latest version.

repo-go is distributed as a compiled binary; selfupdate downloads a new binary
from the given URL (or the REPO_SELFUPDATE_URL environment variable) and
atomically replaces the current executable. Without a URL it prints the current
version and update guidance. Use "repo version" to see the current version.`,
		RunE: func(_ *cobra.Command, _ []string) error {
			log := logger.NewDefaultLogger()
			if opts.Quiet {
				log.SetLevel(logger.LogLevelError)
			}
			return runSelfUpdate(opts, info, log)
		},
	}
	cmd.Flags().StringVar(&opts.URL, "url", "", "download URL for the new repo binary (or REPO_SELFUPDATE_URL env)")
	cmd.Flags().StringVar(&opts.Checksum, "checksum", "", "expected sha256 hex digest of the new binary; when provided, verification is mandatory")
	cmd.Flags().BoolVar(&opts.Check, "check", false, "only check reachability of the update URL, do not update")
	cmd.Flags().BoolVarP(&opts.Quiet, "quiet", "q", false, "only show errors")
	return cmd
}

// validateUpdateURL 校验更新源地址：仅允许 https，或指向 loopback
// （localhost/127.0.0.1/::1）的 http。公网明文 HTTP 下载可执行文件会被
// 中间人篡改，必须拒绝。
func validateUpdateURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("解析更新地址failed: %w", err)
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		host := u.Hostname() // 去除端口与 IPv6 方括号
		if host == "localhost" {
			return nil
		}
		if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
			return nil
		}
		return fmt.Errorf("拒绝通过明文 HTTP 从 %q 下载：仅允许 https，或 http 访问 localhost/127.0.0.1", raw)
	default:
		return fmt.Errorf("不支持的更新地址协议 %q：仅允许 http/https", u.Scheme)
	}
}

// newSelfUpdateHTTPClient 创建带超时与合理连接池参数的 HTTP 客户端。
// 默认 http.Get/Head 的 client 无超时，服务器无响应时 selfupdate 会永久挂起。
func newSelfUpdateHTTPClient() *http.Client {
	return &http.Client{
		Timeout: 60 * time.Second,
		Transport: &http.Transport{
			Proxy:               http.ProxyFromEnvironment,
			MaxIdleConns:        10,
			IdleConnTimeout:     30 * time.Second,
			TLSHandshakeTimeout: 10 * time.Second,
			TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12},
		},
	}
}

// normalizeChecksum 归一化 sha256 校验和：去首尾空白、转小写，并校验为
// 64 位十六进制字符。空输入返回空串，表示"未提供校验和"。
func normalizeChecksum(s string) (string, error) {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" {
		return "", nil
	}
	if len(s) != 2*sha256.Size {
		return "", fmt.Errorf("sha256 应为 %d 位十六进制字符，实际为 %d 位", 2*sha256.Size, len(s))
	}
	if _, err := hex.DecodeString(s); err != nil {
		return "", fmt.Errorf("sha256 含非法十六进制字符: %w", err)
	}
	return s, nil
}

// parseChecksumFile 解析 <url>.sha256 响应体并提取校验和。
// 兼容裸哈希与 sha256sum 输出格式（"<hash>  <文件名>"），取首个空白分隔字段。
func parseChecksumFile(data []byte) (string, error) {
	fields := strings.Fields(string(data))
	if len(fields) == 0 {
		return "", fmt.Errorf("sha256 校验文件内容为空")
	}
	sum, err := normalizeChecksum(fields[0])
	if err != nil {
		return "", fmt.Errorf("解析sha256校验文件failed: %w", err)
	}
	return sum, nil
}

// fetchRemoteChecksum 通过 GET shaURL 获取远端 sha256 校验和并归一化。
// 仅在 HTTP 200 且响应体可解析时返回校验和；其余情况（404、网络错误、
// 非法内容）一律返回错误，绝不伪造"校验通过"。
func fetchRemoteChecksum(client *http.Client, shaURL string) (string, error) {
	resp, err := client.Get(shaURL)
	if err != nil {
		return "", fmt.Errorf("获取sha256校验文件failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("获取sha256校验文件failed: HTTP %d", resp.StatusCode)
	}
	// 校验文件很小，封顶读取防止异常响应占满内存
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return "", fmt.Errorf("读取sha256校验文件failed: %w", err)
	}
	return parseChecksumFile(data)
}

// copyAndHash 将 src 复制到 dst，同时计算内容的 sha256（十六进制）。
// 读取超过 limit 字节时报错中止，防止异常响应写满磁盘。
func copyAndHash(dst io.Writer, src io.Reader, limit int64) (string, error) {
	h := sha256.New()
	// LimitReader 封顶 limit+1：多出的 1 字节用于探测超限
	written, err := io.Copy(dst, io.TeeReader(io.LimitReader(src, limit+1), h))
	if err != nil {
		return "", fmt.Errorf("写入临时文件failed: %w", err)
	}
	if written > limit {
		return "", fmt.Errorf("下载内容超过 %d MB 上限，已中止更新", limit>>20)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// runSelfUpdate 执行 selfupdate 命令
func runSelfUpdate(opts *SelfUpdateOptions, info VersionInfo, log logger.Logger) error {
	url := opts.URL
	if url == "" {
		url = os.Getenv("REPO_SELFUPDATE_URL")
	}
	if url == "" {
		log.Info("当前版本: %s", info.Version)
		log.Info("repo-go 以编译二进制分发；自更新需指定下载 URL")
		log.Info("使用: repo selfupdate --url <binary-url>，或设置 REPO_SELFUPDATE_URL 环境变量")
		return nil
	}

	// 非 loopback 地址强制 https（--check 与下载共用此校验）
	if err := validateUpdateURL(url); err != nil {
		return err
	}

	client := newSelfUpdateHTTPClient()

	if opts.Check {
		resp, err := client.Head(url)
		if err != nil {
			return fmt.Errorf("检查更新源failed: %w", err)
		}
		defer resp.Body.Close()
		log.Info("更新源可访问: %s (HTTP %d)", url, resp.StatusCode)
		return nil
	}

	// 确定期望校验和：--checksum 优先；未提供时尝试 GET <url>.sha256，
	// 获取失败（404/网络/内容非法）仅告警并继续——绝不伪造校验通过。
	expected := ""
	if opts.Checksum != "" {
		normalized, err := normalizeChecksum(opts.Checksum)
		if err != nil {
			return fmt.Errorf("--checksum 参数非法: %w", err)
		}
		expected = normalized
	} else {
		shaURL := url + ".sha256"
		log.Debug("尝试获取校验和: %s", shaURL)
		sum, err := fetchRemoteChecksum(client, shaURL)
		if err != nil {
			log.Warn("无法校验下载内容的完整性（%v），继续下载", err)
		} else {
			expected = sum
			log.Info("已获取 sha256 校验和: %s", sum)
		}
	}

	// 当前可执行文件路径
	exePath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("failed to determine executable path: %w", err)
	}
	exePath, err = filepath.EvalSymlinks(exePath)
	if err != nil {
		return fmt.Errorf("failed to resolve executable path: %w", err)
	}

	log.Info("正在从 %s 下载新版本...", url)
	resp, err := client.Get(url)
	if err != nil {
		return fmt.Errorf("下载failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// 排空 body 以便连接复用；丢弃错误（仅影响连接复用，不影响本次失败返回）
		_, _ = io.Copy(io.Discard, resp.Body)
		return fmt.Errorf("下载failed: HTTP %d", resp.StatusCode)
	}

	// 写入与可执行文件同目录的临时文件，便于原子替换
	dir := filepath.Dir(exePath)
	tmp, err := os.CreateTemp(dir, ".repo-update-*")
	if err != nil {
		return fmt.Errorf("创建临时文件failed: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) // 失败时清理临时文件

	// 下载即计算 sha256 并封顶体积；校验和已知且不匹配时报错，
	// 临时文件由 defer 清理，正式二进制不会被替换（不落盘）
	actualSum, err := copyAndHash(tmp, resp.Body, maxBinarySize)
	if err != nil {
		tmp.Close()
		return err
	}
	tmp.Close()

	if expected != "" && actualSum != expected {
		return fmt.Errorf("sha256 校验失败：期望 %s，实际 %s，已放弃替换二进制", expected, actualSum)
	}

	if runtime.GOOS != "windows" {
		if err := os.Chmod(tmpPath, 0755); err != nil {
			return fmt.Errorf("设置可执行权限failed: %w", err)
		}
	}

	// 原子替换。Windows 上无法覆盖运行中的 exe，需先将当前 exe 重命名为 .old。
	if runtime.GOOS == "windows" {
		old := exePath + ".old"
		_ = os.Remove(old) // 清理上次更新遗留的 .old
		if err := os.Rename(exePath, old); err != nil {
			return fmt.Errorf("重命名当前二进制failed: %w", err)
		}
	}
	if err := os.Rename(tmpPath, exePath); err != nil {
		return fmt.Errorf("替换二进制failed: %w", err)
	}

	log.Info("已更新到新版本；下次运行 repo 即生效")
	return nil
}
