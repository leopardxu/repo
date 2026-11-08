package network

import (
	"errors"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// roundTripFunc 允许测试用内存响应替换真实网络请求
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

// newTestClient 返回一个不重试、响应由 fn 提供的客户端
func newTestClient(fn roundTripFunc, options ...ClientOption) *Client {
	options = append([]ClientOption{WithRetry(0, 0)}, options...)
	c := NewClient(options...)
	c.httpClient.Transport = fn
	return c
}

// fakeResponse 构造内存响应
func fakeResponse(body io.ReadCloser, contentLength int64) *http.Response {
	return &http.Response{
		StatusCode:    http.StatusOK,
		Status:        "200 OK",
		Body:          body,
		ContentLength: contentLength,
	}
}

// assertNoTempFiles 断言下载目录中没有残留临时文件
func assertNoTempFiles(t *testing.T, dir string) {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, ".repo-download-*"))
	if err != nil {
		t.Fatalf("glob temp files: %v", err)
	}
	if len(matches) != 0 {
		t.Errorf("残留临时文件: %v", matches)
	}
}

func TestNewClientTimeouts(t *testing.T) {
	c := NewClient()
	if c.httpClient.Timeout != defaultRequestTimeout {
		t.Errorf("默认总超时 = %v, want %v", c.httpClient.Timeout, defaultRequestTimeout)
	}
	transport, ok := c.httpClient.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("Transport 类型 = %T, want *http.Transport", c.httpClient.Transport)
	}
	if transport.ResponseHeaderTimeout != defaultResponseHeaderTimeout {
		t.Errorf("ResponseHeaderTimeout = %v, want %v",
			transport.ResponseHeaderTimeout, defaultResponseHeaderTimeout)
	}

	custom := NewClient(WithTimeout(5 * time.Second))
	if custom.httpClient.Timeout != 5*time.Second {
		t.Errorf("WithTimeout 后总超时 = %v, want 5s", custom.httpClient.Timeout)
	}
}

func TestDownloadChunkedBodyTooLarge(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "artifact.bin")
	c := newTestClient(func(*http.Request) (*http.Response, error) {
		// ContentLength = -1 模拟分块传输
		return fakeResponse(io.NopCloser(strings.NewReader(strings.Repeat("a", 32))), -1), nil
	}, WithMaxBodySize(16))

	err := c.Download("http://example.invalid/artifact", dest)
	if !errors.Is(err, ErrBodyTooLarge) {
		t.Fatalf("Download() error = %v, want ErrBodyTooLarge", err)
	}
	if _, statErr := os.Stat(dest); !os.IsNotExist(statErr) {
		t.Errorf("失败下载不应留下目标文件, stat err = %v", statErr)
	}
	assertNoTempFiles(t, dir)
}

func TestDownloadLiedContentLength(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "artifact.bin")
	c := newTestClient(func(*http.Request) (*http.Response, error) {
		// 声称只有 4 字节，实际写 32 字节
		return fakeResponse(io.NopCloser(strings.NewReader(strings.Repeat("a", 32))), 4), nil
	}, WithMaxBodySize(16))

	err := c.Download("http://example.invalid/artifact", dest)
	if !errors.Is(err, ErrBodyTooLarge) {
		t.Fatalf("Download() error = %v, want ErrBodyTooLarge", err)
	}
	if _, statErr := os.Stat(dest); !os.IsNotExist(statErr) {
		t.Errorf("失败下载不应留下目标文件, stat err = %v", statErr)
	}
	assertNoTempFiles(t, dir)
}

func TestDownloadKeepsExistingFileOnFailure(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "artifact.bin")
	if err := os.WriteFile(dest, []byte("previous-good-content"), 0644); err != nil {
		t.Fatalf("write existing file: %v", err)
	}

	c := newTestClient(func(*http.Request) (*http.Response, error) {
		return fakeResponse(io.NopCloser(strings.NewReader(strings.Repeat("a", 64))), -1), nil
	}, WithMaxBodySize(8))

	if err := c.Download("http://example.invalid/artifact", dest); err == nil {
		t.Fatal("Download() 期望失败")
	}

	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read existing file: %v", err)
	}
	if string(got) != "previous-good-content" {
		t.Errorf("已有文件被破坏, got %q", string(got))
	}
	assertNoTempFiles(t, dir)
}

func TestDownloadDeclaredSizeTooLarge(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "artifact.bin")
	if err := os.WriteFile(dest, []byte("keep-me"), 0644); err != nil {
		t.Fatalf("write existing file: %v", err)
	}

	c := newTestClient(func(*http.Request) (*http.Response, error) {
		return fakeResponse(io.NopCloser(strings.NewReader("small")), 100), nil
	}, WithMaxBodySize(16))

	err := c.Download("http://example.invalid/artifact", dest)
	if !errors.Is(err, ErrBodyTooLarge) {
		t.Fatalf("Download() error = %v, want ErrBodyTooLarge", err)
	}
	got, readErr := os.ReadFile(dest)
	if readErr != nil {
		t.Fatalf("read existing file: %v", readErr)
	}
	if string(got) != "keep-me" {
		t.Errorf("已有文件被破坏, got %q", string(got))
	}
	assertNoTempFiles(t, dir)
}

func TestDownloadIncompleteBody(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "artifact.bin")
	c := newTestClient(func(*http.Request) (*http.Response, error) {
		// 声明 100 字节但只返回 5 字节
		return fakeResponse(io.NopCloser(strings.NewReader("short")), 100), nil
	}, WithMaxBodySize(1024))

	err := c.Download("http://example.invalid/artifact", dest)
	if err == nil {
		t.Fatal("Download() 期望失败")
	}
	if !strings.Contains(err.Error(), "下载不完整") {
		t.Errorf("error = %v, want 包含下载不完整", err)
	}
	if _, statErr := os.Stat(dest); !os.IsNotExist(statErr) {
		t.Errorf("失败下载不应留下目标文件, stat err = %v", statErr)
	}
	assertNoTempFiles(t, dir)
}

func TestDownloadSuccessReplacesFile(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "artifact.bin")
	if err := os.WriteFile(dest, []byte("old"), 0644); err != nil {
		t.Fatalf("write existing file: %v", err)
	}

	want := "new-content"
	c := newTestClient(func(*http.Request) (*http.Response, error) {
		return fakeResponse(io.NopCloser(strings.NewReader(want)), int64(len(want))), nil
	}, WithMaxBodySize(1024))

	if err := c.Download("http://example.invalid/artifact", dest); err != nil {
		t.Fatalf("Download(): %v", err)
	}

	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read dest: %v", err)
	}
	if string(got) != want {
		t.Errorf("dest 内容 = %q, want %q", string(got), want)
	}
	info, err := os.Stat(dest)
	if err != nil {
		t.Fatalf("stat dest: %v", err)
	}
	if info.Mode().Perm() != 0644 {
		t.Errorf("下载文件权限 = %v, want 0644", info.Mode().Perm())
	}
	assertNoTempFiles(t, dir)
}

func TestDownloadUnknownLengthSuccess(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "artifact.bin")

	want := "chunked-content"
	c := newTestClient(func(*http.Request) (*http.Response, error) {
		return fakeResponse(io.NopCloser(strings.NewReader(want)), -1), nil
	}, WithMaxBodySize(1024))

	if err := c.Download("http://example.invalid/artifact", dest); err != nil {
		t.Fatalf("Download(): %v", err)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read dest: %v", err)
	}
	if string(got) != want {
		t.Errorf("dest 内容 = %q, want %q", string(got), want)
	}
	assertNoTempFiles(t, dir)
}

func TestGetBodyTooLargeUnknownLength(t *testing.T) {
	c := newTestClient(func(*http.Request) (*http.Response, error) {
		return fakeResponse(io.NopCloser(strings.NewReader(strings.Repeat("b", 32))), -1), nil
	}, WithMaxBodySize(16))

	resp, err := c.Get("http://example.invalid/manifest", nil)
	if err != nil {
		t.Fatalf("Get(): %v", err)
	}
	defer resp.Body.Close()

	_, readErr := io.ReadAll(resp.Body)
	if !errors.Is(readErr, ErrBodyTooLarge) {
		t.Fatalf("读取响应 error = %v, want ErrBodyTooLarge", readErr)
	}
}

func TestGetDeclaredSizeTooLarge(t *testing.T) {
	c := newTestClient(func(*http.Request) (*http.Response, error) {
		return fakeResponse(io.NopCloser(strings.NewReader("payload")), 4096), nil
	}, WithMaxBodySize(16))

	resp, err := c.Get("http://example.invalid/manifest", nil)
	if resp != nil {
		_ = resp.Body.Close()
	}
	if !errors.Is(err, ErrBodyTooLarge) {
		t.Fatalf("Get() error = %v, want ErrBodyTooLarge", err)
	}
}

func TestLimitBodyAllowsExactSize(t *testing.T) {
	body := limitBody(io.NopCloser(strings.NewReader("12345678")), 8)
	defer body.Close()

	got, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("读取恰好等于上限的内容 error = %v", err)
	}
	if string(got) != "12345678" {
		t.Errorf("got %q, want %q", string(got), "12345678")
	}
}

func TestNewClientDialerHasConnectTimeout(t *testing.T) {
	dialer := newDialer()
	if dialer.Timeout != defaultDialTimeout {
		t.Errorf("拨号超时 = %v, want %v", dialer.Timeout, defaultDialTimeout)
	}
	if dialer.Timeout != 30*time.Second {
		t.Errorf("拨号超时 = %v, 验收要求 30s", dialer.Timeout)
	}
	if dialer.KeepAlive != defaultTCPKeepAlive {
		t.Errorf("keepalive = %v, want %v", dialer.KeepAlive, defaultTCPKeepAlive)
	}

	c := NewClient()
	transport, ok := c.httpClient.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("Transport 类型 = %T, want *http.Transport", c.httpClient.Transport)
	}
	if transport.DialContext == nil {
		t.Fatal("Transport.DialContext 未设置：黑洞地址建连将没有上限")
	}
}

func TestDownloadFallsBackToDirectWriteWhenTempFileFails(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root 可绕过目录权限限制，无法触发 CreateTemp 失败")
	}

	dir := t.TempDir()
	dest := filepath.Join(dir, "artifact.bin")
	if err := os.WriteFile(dest, []byte("stale"), 0644); err != nil {
		t.Fatalf("准备目标文件: %v", err)
	}

	// 目录不可写但目标文件本身可写：CreateTemp 失败，直接写目标仍应成功
	if err := os.Chmod(dir, 0555); err != nil {
		t.Fatalf("chmod 目录: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0755) })

	payload := "fallback-payload"
	c := newTestClient(func(*http.Request) (*http.Response, error) {
		return fakeResponse(io.NopCloser(strings.NewReader(payload)), int64(len(payload))), nil
	})

	if err := c.Download("http://example.invalid/artifact", dest); err != nil {
		t.Fatalf("回退直接写入失败: %v", err)
	}

	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("读取目标文件: %v", err)
	}
	if string(got) != payload {
		t.Errorf("目标文件内容 = %q, want %q", got, payload)
	}
	assertNoTempFiles(t, dir)
}

func TestWriteInPlaceSuccess(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "artifact.bin")
	if err := os.WriteFile(dest, []byte("stale"), 0644); err != nil {
		t.Fatalf("准备目标文件: %v", err)
	}

	payload := "direct-write"
	c := NewClient()
	resp := fakeResponse(io.NopCloser(strings.NewReader(payload)), int64(len(payload)))
	if err := c.writeInPlace(resp, dest); err != nil {
		t.Fatalf("writeInPlace() error: %v", err)
	}

	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("读取目标文件: %v", err)
	}
	if string(got) != payload {
		t.Errorf("目标文件内容 = %q, want %q", got, payload)
	}
}

func TestWriteInPlaceCleansUpOnFailure(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		declLen int64
		maxSize int64
		wantErr error
	}{
		{
			name:    "incomplete body",
			body:    "short",
			declLen: 100,
		},
		{
			name:    "body exceeds limit",
			body:    strings.Repeat("a", 64),
			declLen: -1,
			maxSize: 16,
			wantErr: ErrBodyTooLarge,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			dest := filepath.Join(dir, "artifact.bin")

			options := []ClientOption{}
			if tt.maxSize > 0 {
				options = append(options, WithMaxBodySize(tt.maxSize))
			}
			c := NewClient(options...)
			resp := fakeResponse(io.NopCloser(strings.NewReader(tt.body)), tt.declLen)

			err := c.writeInPlace(resp, dest)
			if err == nil {
				t.Fatal("writeInPlace() 应返回错误")
			}
			if tt.wantErr != nil && !errors.Is(err, tt.wantErr) {
				t.Errorf("writeInPlace() error = %v, want %v", err, tt.wantErr)
			}
			if _, statErr := os.Stat(dest); !os.IsNotExist(statErr) {
				t.Errorf("失败后不应残留目标文件, stat err = %v", statErr)
			}
		})
	}
}

// stubCreateDownloadTempFile 替换临时文件创建函数，测试结束后恢复。
func stubCreateDownloadTempFile(t *testing.T, fn func(dir, pattern string) (*os.File, error)) {
	t.Helper()
	original := createDownloadTempFile
	createDownloadTempFile = fn
	t.Cleanup(func() { createDownloadTempFile = original })
}

func TestIsPermissionLikeError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil", err: nil, want: false},
		{name: "fs.ErrPermission", err: fs.ErrPermission, want: true},
		{
			name: "wrapped permission denied",
			err:  &os.PathError{Op: "open", Path: "x", Err: fs.ErrPermission},
			want: true,
		},
		{
			name: "read-only file system",
			err:  &os.PathError{Op: "open", Path: "x", Err: errors.New("read-only file system")},
			want: true,
		},
		{
			name: "no space left on device",
			err:  &os.PathError{Op: "open", Path: "x", Err: errors.New("no space left on device")},
			want: false,
		},
		{
			name: "too many open files",
			err:  &os.PathError{Op: "open", Path: "x", Err: errors.New("too many open files")},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isPermissionLikeError(tt.err); got != tt.want {
				t.Errorf("isPermissionLikeError(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestDownloadDoesNotFallbackForNonPermissionTempErrors(t *testing.T) {
	tests := []struct {
		name    string
		tempErr error
	}{
		{
			name:    "disk full",
			tempErr: &os.PathError{Op: "open", Path: ".repo-download-x", Err: errors.New("no space left on device")},
		},
		{
			name:    "too many open files",
			tempErr: &os.PathError{Op: "open", Path: ".repo-download-x", Err: errors.New("too many open files")},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			dest := filepath.Join(dir, "artifact.bin")
			const previous = "previous-good-content"
			if err := os.WriteFile(dest, []byte(previous), 0644); err != nil {
				t.Fatalf("准备目标文件: %v", err)
			}

			stubCreateDownloadTempFile(t, func(string, string) (*os.File, error) {
				return nil, tt.tempErr
			})

			c := newTestClient(func(*http.Request) (*http.Response, error) {
				return fakeResponse(io.NopCloser(strings.NewReader("new")), -1), nil
			})

			err := c.Download("http://example.invalid/artifact", dest)
			if err == nil {
				t.Fatal("Download() 期望失败")
			}
			if !errors.Is(err, tt.tempErr) {
				t.Errorf("Download() error = %v, want 包含 %v", err, tt.tempErr)
			}
			// 非权限错误不得退化为直接写入，旧文件必须保持不变
			got, readErr := os.ReadFile(dest)
			if readErr != nil {
				t.Fatalf("读取目标文件: %v", readErr)
			}
			if string(got) != previous {
				t.Errorf("旧文件被破坏, got %q, want %q", string(got), previous)
			}
			assertNoTempFiles(t, dir)
		})
	}
}

func TestDownloadFallsBackOnPermissionTempErrors(t *testing.T) {
	tests := []struct {
		name    string
		tempErr error
	}{
		{
			name:    "permission denied",
			tempErr: &os.PathError{Op: "open", Path: ".repo-download-x", Err: fs.ErrPermission},
		},
		{
			name:    "read-only file system",
			tempErr: &os.PathError{Op: "open", Path: ".repo-download-x", Err: errors.New("read-only file system")},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			dest := filepath.Join(dir, "artifact.bin")
			if err := os.WriteFile(dest, []byte("stale"), 0644); err != nil {
				t.Fatalf("准备目标文件: %v", err)
			}

			stubCreateDownloadTempFile(t, func(string, string) (*os.File, error) {
				return nil, tt.tempErr
			})

			const payload = "fallback-payload"
			c := newTestClient(func(*http.Request) (*http.Response, error) {
				return fakeResponse(io.NopCloser(strings.NewReader(payload)), int64(len(payload))), nil
			})

			if err := c.Download("http://example.invalid/artifact", dest); err != nil {
				t.Fatalf("权限类错误应回退直接写入: %v", err)
			}

			got, err := os.ReadFile(dest)
			if err != nil {
				t.Fatalf("读取目标文件: %v", err)
			}
			if string(got) != payload {
				t.Errorf("目标文件内容 = %q, want %q", string(got), payload)
			}
			assertNoTempFiles(t, dir)
		})
	}
}

func TestWriteInPlacePreservesExistingFileOnFailure(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "artifact.bin")
	const previous = "keep-me"
	if err := os.WriteFile(dest, []byte(previous), 0644); err != nil {
		t.Fatalf("准备目标文件: %v", err)
	}

	// 声明 100 字节但只提供 5 字节，写入必然失败
	c := NewClient()
	resp := fakeResponse(io.NopCloser(strings.NewReader("short")), 100)
	err := c.writeInPlace(resp, dest)
	if err == nil {
		t.Fatal("writeInPlace() 期望失败")
	}

	got, readErr := os.ReadFile(dest)
	if readErr != nil {
		t.Fatalf("读取目标文件: %v", readErr)
	}
	if string(got) != previous {
		t.Errorf("失败写入破坏了旧文件, got %q, want %q", string(got), previous)
	}
}

func TestWriteInPlaceRefusesWithoutBackup(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "artifact.bin")
	const previous = "keep-me"
	if err := os.WriteFile(dest, []byte(previous), 0644); err != nil {
		t.Fatalf("准备目标文件: %v", err)
	}

	// 备份目录不可用：必须拒绝截断已有文件，保证旧内容不丢失
	t.Setenv("TMPDIR", filepath.Join(dir, "missing-backup-dir"))

	c := NewClient()
	resp := fakeResponse(io.NopCloser(strings.NewReader("new-content")), -1)
	err := c.writeInPlace(resp, dest)
	if err == nil {
		t.Fatal("备份不可用时 writeInPlace() 应失败")
	}

	got, readErr := os.ReadFile(dest)
	if readErr != nil {
		t.Fatalf("读取目标文件: %v", readErr)
	}
	if string(got) != previous {
		t.Errorf("旧文件被破坏, got %q, want %q", string(got), previous)
	}
}

func TestDownloadFallbackFailurePreservesExistingFile(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "artifact.bin")
	const previous = "keep-me"
	if err := os.WriteFile(dest, []byte(previous), 0644); err != nil {
		t.Fatalf("准备目标文件: %v", err)
	}

	// 强制走直接写入回退路径
	stubCreateDownloadTempFile(t, func(string, string) (*os.File, error) {
		return nil, &os.PathError{Op: "open", Path: ".repo-download-x", Err: fs.ErrPermission}
	})

	c := newTestClient(func(*http.Request) (*http.Response, error) {
		// 声明 100 字节但只返回 5 字节，回退写入必然失败
		return fakeResponse(io.NopCloser(strings.NewReader("short")), 100), nil
	})

	if err := c.Download("http://example.invalid/artifact", dest); err == nil {
		t.Fatal("Download() 期望失败")
	}

	got, readErr := os.ReadFile(dest)
	if readErr != nil {
		t.Fatalf("读取目标文件: %v", readErr)
	}
	if string(got) != previous {
		t.Errorf("回退失败时旧文件被破坏, got %q, want %q", string(got), previous)
	}
	assertNoTempFiles(t, dir)
}

// stubRestoreBackupFile 替换备份恢复函数，测试结束后恢复。
func stubRestoreBackupFile(t *testing.T, fn func(backupPath, destPath string) error) {
	t.Helper()
	original := restoreBackupFile
	restoreBackupFile = fn
	t.Cleanup(func() { restoreBackupFile = original })
}

func TestWriteInPlaceRetainsBackupWhenRestoreFails(t *testing.T) {
	dir := t.TempDir()
	backupDir := t.TempDir()
	t.Setenv("TMPDIR", backupDir)

	dest := filepath.Join(dir, "artifact.bin")
	const previous = "precious-old-content"
	if err := os.WriteFile(dest, []byte(previous), 0644); err != nil {
		t.Fatalf("准备目标文件: %v", err)
	}

	restoreErr := errors.New("模拟恢复失败")
	var capturedBackup string
	stubRestoreBackupFile(t, func(backupPath, destPath string) error {
		capturedBackup = backupPath
		if backupPath == "" {
			t.Error("恢复时备份路径不应为空")
		}
		return restoreErr
	})

	// 声明 100 字节但只提供 5 字节，写入必然失败
	c := NewClient()
	resp := fakeResponse(io.NopCloser(strings.NewReader("short")), 100)
	err := c.writeInPlace(resp, dest)
	if err == nil {
		t.Fatal("writeInPlace() 应返回错误")
	}
	if !errors.Is(err, restoreErr) {
		t.Errorf("错误应包含恢复失败原因, got %v", err)
	}
	if !strings.Contains(err.Error(), "下载不完整") {
		t.Errorf("错误应同时保留下载失败原因, got %v", err)
	}
	if capturedBackup == "" {
		t.Fatal("未捕获到备份路径")
	}
	if !strings.Contains(err.Error(), capturedBackup) {
		t.Errorf("错误应包含备份路径 %q 供用户找回, got %v", capturedBackup, err)
	}

	// 恢复失败时备份是旧内容的唯一副本，必须保留且内容完好
	backupContent, readErr := os.ReadFile(capturedBackup)
	if readErr != nil {
		t.Fatalf("恢复失败后备份应保留: %v", readErr)
	}
	if string(backupContent) != previous {
		t.Errorf("备份内容 = %q, want %q", string(backupContent), previous)
	}
}

func TestWriteInPlaceRemovesBackupAfterSuccessfulRestore(t *testing.T) {
	dir := t.TempDir()
	backupDir := t.TempDir()
	t.Setenv("TMPDIR", backupDir)

	dest := filepath.Join(dir, "artifact.bin")
	const previous = "keep-me"
	if err := os.WriteFile(dest, []byte(previous), 0644); err != nil {
		t.Fatalf("准备目标文件: %v", err)
	}

	// 声明 100 字节但只提供 5 字节，写入失败后应恢复旧内容
	c := NewClient()
	resp := fakeResponse(io.NopCloser(strings.NewReader("short")), 100)
	if err := c.writeInPlace(resp, dest); err == nil {
		t.Fatal("writeInPlace() 期望失败")
	}

	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("读取目标文件: %v", err)
	}
	if string(got) != previous {
		t.Errorf("恢复后目标文件内容 = %q, want %q", string(got), previous)
	}

	// 恢复成功后备份不再需要，必须清理
	matches, err := filepath.Glob(filepath.Join(backupDir, "repo-download-backup-*"))
	if err != nil {
		t.Fatalf("glob 备份文件: %v", err)
	}
	if len(matches) != 0 {
		t.Errorf("恢复成功后不应残留备份: %v", matches)
	}
}
