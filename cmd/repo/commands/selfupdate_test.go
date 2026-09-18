package commands

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/leopardxu/repo-go/internal/logger"
)

// TestValidateUpdateURL 验证更新源协议约束：https 放行，
// http 仅允许 loopback（localhost/127.0.0.1/::1），其余一律拒绝。
func TestValidateUpdateURL(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		wantErr bool
	}{
		{name: "https放行", raw: "https://example.com/repo-linux"},
		{name: "http本机127.0.0.1放行", raw: "http://127.0.0.1:8080/repo"},
		{name: "http本机localhost放行", raw: "http://localhost/repo"},
		{name: "http本机IPv6放行", raw: "http://[::1]:9000/repo"},
		{name: "http非本机拒绝", raw: "http://example.com/repo-linux", wantErr: true},
		{name: "http公网IP拒绝", raw: "http://93.184.216.34/repo", wantErr: true},
		{name: "其他协议拒绝", raw: "ftp://example.com/repo", wantErr: true},
		{name: "无协议拒绝", raw: "example.com/repo", wantErr: true},
		{name: "空地址拒绝", raw: "", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateUpdateURL(tt.raw)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("期望拒绝 %q，实际通过", tt.raw)
				}
				return
			}
			if err != nil {
				t.Fatalf("期望放行 %q，实际得到错误: %v", tt.raw, err)
			}
		})
	}
}

// TestNormalizeChecksum 验证校验和归一化：合法/非法/缺省（空）边界。
func TestNormalizeChecksum(t *testing.T) {
	const validLower = "b94d27b9934d3e08a52e52d7da7dabfac484efe37a5380ee9088f7ace2efcde9"
	const validUpper = "B94D27B9934D3E08A52E52D7DA7DABFAC484EFE37A5380EE9088F7ACE2EFCDE9"

	tests := []struct {
		name     string
		input    string
		want     string
		wantErr  bool
		errParts []string // 期望错误信息包含的片段
	}{
		{name: "合法小写", input: validLower, want: validLower},
		{name: "合法大写归一为小写", input: validUpper, want: validLower},
		{name: "首尾空白被裁剪", input: "  " + validLower + "\n", want: validLower},
		{name: "空输入表示未提供", input: "", want: ""},
		{name: "纯空白输入表示未提供", input: "   ", want: ""},
		{
			name:     "长度不足63位拒绝",
			input:    validLower[:63],
			wantErr:  true,
			errParts: []string{"64 位十六进制", "63 位"},
		},
		{
			name:     "长度超出65位拒绝",
			input:    validLower + "a",
			wantErr:  true,
			errParts: []string{"64 位十六进制", "65 位"},
		},
		{
			name:     "含非十六进制字符拒绝",
			input:    strings.Repeat("z", 64),
			wantErr:  true,
			errParts: []string{"非法十六进制"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := normalizeChecksum(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("期望输入 %q 报错，实际归一化为 %q", tt.input, got)
				}
				for _, part := range tt.errParts {
					if !strings.Contains(err.Error(), part) {
						t.Errorf("错误信息应包含 %q，实际: %v", part, err)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("期望输入 %q 合法，实际得到错误: %v", tt.input, err)
			}
			if got != tt.want {
				t.Errorf("normalizeChecksum(%q) = %q, 期望 %q", tt.input, got, tt.want)
			}
		})
	}
}

// TestParseChecksumFile 验证 <url>.sha256 响应体解析：
// 裸哈希、sha256sum 输出格式、空白与非法内容边界。
func TestParseChecksumFile(t *testing.T) {
	const hash = "b94d27b9934d3e08a52e52d7da7dabfac484efe37a5380ee9088f7ace2efcde9"

	tests := []struct {
		name    string
		body    string
		want    string
		wantErr bool
	}{
		{name: "裸哈希", body: hash, want: hash},
		{name: "sha256sum输出格式", body: hash + "  repo-linux", want: hash},
		{name: "带换行", body: hash + "\n", want: hash},
		{name: "大写归一为小写", body: strings.ToUpper(hash), want: hash},
		{name: "空内容拒绝", body: "", wantErr: true},
		{name: "纯空白拒绝", body: "  \n\t ", wantErr: true},
		{name: "非法哈希拒绝", body: "not-a-hash", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseChecksumFile([]byte(tt.body))
			if tt.wantErr {
				if err == nil {
					t.Fatalf("期望解析 %q 报错，实际得到 %q", tt.body, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("期望解析 %q 成功，实际得到错误: %v", tt.body, err)
			}
			if got != tt.want {
				t.Errorf("parseChecksumFile(%q) = %q, 期望 %q", tt.body, got, tt.want)
			}
		})
	}
}

// TestCopyAndHash 验证下载内容复制、sha256 计算与体积封顶：
// 内容正确哈希、恰好等于上限放行、超过上限报错。
func TestCopyAndHash(t *testing.T) {
	// "hello world" 的 sha256
	const helloSum = "b94d27b9934d3e08a52e52d7da7dabfac484efe37a5380ee9088f7ace2efcde9"

	tests := []struct {
		name     string
		content  []byte
		limit    int64
		wantSum  string
		wantData []byte
		wantErr  bool
	}{
		{
			name:     "小内容正确哈希并完整写入",
			content:  []byte("hello world"),
			limit:    1 << 20,
			wantSum:  helloSum,
			wantData: []byte("hello world"),
		},
		{
			name:     "恰好等于上限放行",
			content:  bytes.Repeat([]byte{0xAB}, 16),
			limit:    16,
			wantData: bytes.Repeat([]byte{0xAB}, 16),
		},
		{
			name:    "超过上限报错",
			content: bytes.Repeat([]byte{0xAB}, 17),
			limit:   16,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var dst bytes.Buffer
			sum, err := copyAndHash(&dst, bytes.NewReader(tt.content), tt.limit)

			if tt.wantErr {
				if err == nil {
					t.Fatalf("期望超限报错，实际成功，sum=%s", sum)
				}
				return
			}
			if err != nil {
				t.Fatalf("期望成功，实际得到错误: %v", err)
			}
			if !bytes.Equal(dst.Bytes(), tt.wantData) {
				t.Errorf("写入内容不一致：得到 %d 字节，期望 %d 字节", dst.Len(), len(tt.wantData))
			}
			if tt.wantSum != "" && sum != tt.wantSum {
				t.Errorf("sha256 = %s, 期望 %s", sum, tt.wantSum)
			}
			if len(tt.wantData) > 0 {
				// 用标准库交叉验证哈希正确性
				h := sha256.Sum256(tt.wantData)
				if want := hex.EncodeToString(h[:]); sum != want {
					t.Errorf("sha256 = %s, 期望 %s（标准库计算值）", sum, want)
				}
			}
		})
	}
}

// TestFetchRemoteChecksum 验证从 <url>.sha256 获取校验和：
// 200+合法内容返回归一化哈希；404、网络错误、非法内容均返回错误。
func TestFetchRemoteChecksum(t *testing.T) {
	const hash = "b94d27b9934d3e08a52e52d7da7dabfac484efe37a5380ee9088f7ace2efcde9"

	tests := []struct {
		name       string
		status     int
		body       string
		want       string
		wantErr    bool
		wantErrSub string
	}{
		{name: "200裸哈希", status: http.StatusOK, body: hash, want: hash},
		{name: "200sha256sum格式", status: http.StatusOK, body: hash + "  repo-linux\n", want: hash},
		{name: "404返回错误", status: http.StatusNotFound, body: "not found", wantErr: true, wantErrSub: "HTTP 404"},
		{name: "500返回错误", status: http.StatusInternalServerError, body: "oops", wantErr: true, wantErrSub: "HTTP 500"},
		{name: "200但内容非法返回错误", status: http.StatusOK, body: "<html>error page</html>", wantErr: true, wantErrSub: "解析sha256校验文件failed"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer srv.Close()

			got, err := fetchRemoteChecksum(srv.Client(), srv.URL)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("期望报错，实际得到 %q", got)
				}
				if !strings.Contains(err.Error(), tt.wantErrSub) {
					t.Errorf("错误信息应包含 %q，实际: %v", tt.wantErrSub, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("期望成功，实际得到错误: %v", err)
			}
			if got != tt.want {
				t.Errorf("fetchRemoteChecksum = %q, 期望 %q", got, tt.want)
			}
		})
	}
}

// TestRunSelfUpdate_RejectsPlainHTTPRemote 验证整体流程：
// 非 loopback 的 http 地址在任何网络动作之前即被拒绝。
func TestRunSelfUpdate_RejectsPlainHTTPRemote(t *testing.T) {
	opts := &SelfUpdateOptions{URL: "http://example.com/repo-linux"}
	err := runSelfUpdate(opts, VersionInfo{Version: "test"}, logger.NewDefaultLogger())
	if err == nil {
		t.Fatal("期望拒绝明文 http 更新源，实际成功")
	}
	if !strings.Contains(err.Error(), "明文 HTTP") {
		t.Errorf("错误信息应说明拒绝明文 HTTP，实际: %v", err)
	}
}

// TestRunSelfUpdate_RejectsInvalidChecksumFlag 验证 --checksum 参数非法时
// 在任何网络动作之前即报错（URL 为合法 https，但校验和格式错误）。
func TestRunSelfUpdate_RejectsInvalidChecksumFlag(t *testing.T) {
	opts := &SelfUpdateOptions{URL: "https://example.invalid/repo-linux", Checksum: "xyz"}
	err := runSelfUpdate(opts, VersionInfo{Version: "test"}, logger.NewDefaultLogger())
	if err == nil {
		t.Fatal("期望非法 --checksum 报错，实际成功")
	}
	if !strings.Contains(err.Error(), "--checksum 参数非法") {
		t.Errorf("错误信息应指出 --checksum 参数非法，实际: %v", err)
	}
}
