package repo_sync

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/leopardxu/repo-go/internal/manifest"
)

// handleSmartSync 处理智能同步。
//
// 与上游 repo 一致，清单服务器使用 XML-RPC over HTTP POST 通信：
//   - --smart-sync / --smart-tag：分别调用 GetApprovedManifest(branch[,target])
//     与 GetManifest(tag)；
//   - 显式请求（-s / --smart-tag / repo smartsync）缺少 manifest-server 时
//     必须报错，不能静默退化为常规同步；
//   - 仅由清单 <default sync-smartsync="true"> 隐式启用的智能同步允许在
//     无服务器/请求失败时告警并回退（由 preSync 捕获）。
func (e *Engine) handleSmartSync() error {
	// --no-manifest-server：显式禁用清单服务器，跳过智能同步。
	if e.options != nil && e.options.NoManifestServer {
		e.logger.Warn("已通过 --no-manifest-server 禁用清单服务器，跳过智能同步，将按当前清单执行常规同步")
		return nil
	}

	manifestServer := e.manifestServerURL()
	explicit := e.explicitSmartSync()
	if manifestServer == "" {
		if explicit {
			return fmt.Errorf("cannot smart sync: no manifest server defined in manifest (or --manifest-server-url)")
		}
		e.logger.Warn("manifest-server not defined in manifest 且未通过 --manifest-server-url 指定，无法进行智能同步，将按当前清单执行常规同步")
		return nil
	}

	if !e.options.Quiet {
		fmt.Printf("使用清单服务 %s\n", manifestServer)
	}

	// 组装 RPC 方法与参数（对齐上游 positional API）。
	method := "GetApprovedManifest"
	params := []string{}
	if e.options.SmartSync {
		branch := e.getBranch()
		params = append(params, branch)
		if target := resolveSmartSyncTarget(); target != "" {
			params = append(params, target)
		}
	} else {
		if e.options.SmartTag == "" {
			return fmt.Errorf("cannot smart sync: --smart-tag value is empty")
		}
		method = "GetManifest"
		params = append(params, e.options.SmartTag)
	}

	ctx, cancel := context.WithTimeout(context.Background(), e.smartSyncHTTPTimeout())
	defer cancel()

	success, manifestStr, err := e.callManifestServerRPC(ctx, manifestServer, method, params)
	if err != nil {
		return err
	}
	if !success {
		return fmt.Errorf("manifest server RPC call failed: %s", manifestStr)
	}
	if strings.TrimSpace(manifestStr) == "" {
		return fmt.Errorf("manifest server returned an empty manifest")
	}

	// 使用内存缓存处理清单
	e.manifestCache = []byte(manifestStr)

	// 重新加载清单
	if err := e.reloadManifestFromCache(); err != nil {
		return err
	}

	// 可选：写入临时文件用于调试
	if e.options.Debug {
		smartSyncManifestPath := filepath.Join(resolveRepoMetadataDir(e.manifest), "smart-sync-manifest.xml")
		if err := os.MkdirAll(filepath.Dir(smartSyncManifestPath), 0755); err != nil {
			return fmt.Errorf("创建 .repo 目录 failed: %w", err)
		}
		if err := os.WriteFile(smartSyncManifestPath, []byte(manifestStr), 0644); err != nil {
			return fmt.Errorf("将清单写入 %s 时出错: %w", smartSyncManifestPath, err)
		}
	}

	return nil
}

// explicitSmartSync 判断智能同步是否由用户显式请求（-s/--smart-tag/
// repo smartsync），而不是清单 <default sync-smartsync> 的隐式默认。
func (e *Engine) explicitSmartSync() bool {
	if e == nil || e.options == nil || e.smartSyncImplicit {
		return false
	}
	return e.options.SmartSync || e.options.SmartTag != ""
}

// manifestServerURL 返回清单服务器 URL：命令行 --manifest-server-url
// 覆盖清单中的 <manifest-server url="...">。
func (e *Engine) manifestServerURL() string {
	if e.options != nil && strings.TrimSpace(e.options.ManifestServerURL) != "" {
		return strings.TrimSpace(e.options.ManifestServerURL)
	}
	if e.manifest != nil && e.manifest.ManifestServer != nil {
		return strings.TrimSpace(e.manifest.ManifestServer.URL)
	}
	return ""
}

// manifestServerHelper 返回清单中声明的 <manifest-server helper="...">。
func (e *Engine) manifestServerHelper() string {
	if e.manifest != nil && e.manifest.ManifestServer != nil {
		return strings.TrimSpace(e.manifest.ManifestServer.Helper)
	}
	return ""
}

// defaultSyncSmartSyncEnabled 读取清单 <default sync-smartsync="true">。
// 该属性属于未知的 default 扩展属性，由 manifest 解析器保存在
// Default.CustomAttrs 中；缺失或非 true 均视为关闭。
func defaultSyncSmartSyncEnabled(m *manifest.Manifest) bool {
	if m == nil {
		return false
	}
	value, ok := m.Default.GetCustomAttr("sync-smartsync")
	if !ok {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(value), "true")
}

func (e *Engine) smartSyncHTTPTimeout() time.Duration {
	if e.options != nil && e.options.HTTPTimeout > 0 {
		return e.options.HTTPTimeout
	}
	// 默认 60 秒，避免 0 表示无超时导致永久挂起
	return 60 * time.Second
}

// smartSyncHTTPClient 构造清单服务器 HTTP 客户端。
// helper 存在时按上游语义执行 helper 取得代理地址，并把 XML-RPC 请求
// 通过该代理转发；否则沿用 HTTP_PROXY/HTTPS_PROXY 环境配置。
func (e *Engine) smartSyncHTTPClient(ctx context.Context, serverURL string) (*http.Client, error) {
	transport := &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		MaxIdleConns:        10,
		IdleConnTimeout:     30 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second,
		TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
		},
	}

	if helper := e.manifestServerHelper(); helper != "" {
		proxyURL, err := runManifestServerHelper(ctx, helper, serverURL)
		if err != nil {
			return nil, err
		}
		parsed, err := url.Parse(proxyURL)
		if err != nil {
			return nil, fmt.Errorf("manifest server helper %q returned invalid proxy URL %q: %w", helper, proxyURL, err)
		}
		transport.Proxy = http.ProxyURL(parsed)
	}

	return &http.Client{
		Timeout:   e.smartSyncHTTPTimeout(),
		Transport: transport,
	}, nil
}

// runManifestServerHelper 执行 <manifest-server helper>：
//   - helper 必须位于 PATH 中；
//   - 调用形式 `<helper> <manifest-server-url>`；
//   - 10 秒内输出一行 JSON：{"status":"ok","message":"<proxy-url>"}。
func runManifestServerHelper(ctx context.Context, helper, serverURL string) (string, error) {
	helperPath, err := exec.LookPath(helper)
	if err != nil {
		return "", fmt.Errorf("helper binary %q declared in manifest was not found in PATH: %w", helper, err)
	}

	helperCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	cmd := exec.CommandContext(helperCtx, helperPath, serverURL)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	runErr := cmd.Run()
	if helperCtx.Err() == context.DeadlineExceeded {
		return "", fmt.Errorf("helper %s timed out after 10 seconds (stderr: %s)", helper, strings.TrimSpace(stderr.String()))
	}
	if runErr != nil {
		return "", fmt.Errorf("helper %s failed: %w (stderr: %s)", helper, runErr, strings.TrimSpace(stderr.String()))
	}

	var result struct {
		Status  string `json:"status"`
		Message string `json:"message"`
	}
	output := strings.TrimSpace(stdout.String())
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		return "", fmt.Errorf("failed to parse JSON from helper %s: %w (output: %s)", helper, err, output)
	}
	if result.Status != "ok" {
		return "", fmt.Errorf("helper %s returned error: %s", helper, result.Message)
	}
	if strings.TrimSpace(result.Message) == "" {
		return "", fmt.Errorf("helper %s returned empty proxy URL", helper)
	}
	return result.Message, nil
}

// XML-RPC 请求/响应结构（仅覆盖清单服务器使用的字符串参数与
// [boolean, string] 返回值形态）。
type xmlRPCRequest struct {
	XMLName    xml.Name             `xml:"methodCall"`
	MethodName string               `xml:"methodName"`
	Params     []xmlRPCRequestParam `xml:"params>param"`
}

type xmlRPCRequestParam struct {
	Value xmlRPCRequestValue `xml:"value"`
}

type xmlRPCRequestValue struct {
	String string `xml:"string"`
}

type xmlRPCResponse struct {
	XMLName xml.Name      `xml:"methodResponse"`
	Params  []xmlRPCParam `xml:"params>param"`
	Fault   *xmlRPCFault  `xml:"fault"`
}

type xmlRPCParam struct {
	Value xmlRPCValue `xml:"value"`
}

type xmlRPCFault struct {
	Value xmlRPCValue `xml:"value"`
}

type xmlRPCValue struct {
	String  string        `xml:"string"`
	Boolean string        `xml:"boolean"`
	Int     string        `xml:"int"`
	Array   *xmlRPCArray  `xml:"array"`
	Struct  *xmlRPCStruct `xml:"struct"`
}

type xmlRPCArray struct {
	Values []xmlRPCValue `xml:"data>value"`
}

type xmlRPCStruct struct {
	Members []xmlRPCMember `xml:"member"`
}

type xmlRPCMember struct {
	Name  string      `xml:"name"`
	Value xmlRPCValue `xml:"value"`
}

// callManifestServerRPC 以 XML-RPC POST 调用清单服务器并解析
// methodResponse。网络错误最多重试 3 次（与既有行为一致）。
func (e *Engine) callManifestServerRPC(ctx context.Context, serverURL, method string, params []string) (bool, string, error) {
	requestURL := strings.TrimPrefix(serverURL, "persistent-")
	if e.options != nil && !strings.Contains(requestURL, "@") {
		username, password := e.options.ManifestServerUsername, e.options.ManifestServerPassword
		if username != "" && password != "" {
			if parsed, err := url.Parse(requestURL); err == nil {
				parsed.User = url.UserPassword(username, password)
				requestURL = parsed.String()
			}
		}
	}

	request := xmlRPCRequest{
		MethodName: method,
		Params:     make([]xmlRPCRequestParam, 0, len(params)),
	}
	for _, param := range params {
		request.Params = append(request.Params, xmlRPCRequestParam{
			Value: xmlRPCRequestValue{String: param},
		})
	}
	body, err := xml.Marshal(request)
	if err != nil {
		return false, "", fmt.Errorf("failed to encode XML-RPC request: %w", err)
	}
	body = append([]byte(xml.Header), body...)

	client, err := e.smartSyncHTTPClient(ctx, serverURL)
	if err != nil {
		return false, "", err
	}

	const maxAttempts = 3
	var lastErr error
	for attempt := range maxAttempts {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return false, "", ctx.Err()
			case <-time.After(time.Duration(attempt) * time.Second):
			}
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, requestURL, bytes.NewReader(body))
		if err != nil {
			return false, "", fmt.Errorf("failed to build XML-RPC request: %w", err)
		}
		req.Header.Set("Content-Type", "text/xml")

		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		responseBody, readErr := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return false, "", fmt.Errorf("manifest server returned status code %d", resp.StatusCode)
		}
		if readErr != nil {
			return false, "", fmt.Errorf("failed to read manifest server response: %w", readErr)
		}
		return parseXMLRPCManifestResponse(responseBody)
	}

	return false, "", fmt.Errorf("after %d attempts could not connect to manifest server: %w", maxAttempts, lastErr)
}

// parseXMLRPCManifestResponse 解析 <methodResponse>：
//   - fault → 错误；
//   - params[0] 必须是 [boolean, string] 数组，返回 (success, manifest/错误文本)。
func parseXMLRPCManifestResponse(data []byte) (bool, string, error) {
	var response xmlRPCResponse
	if err := xml.Unmarshal(data, &response); err != nil {
		return false, "", fmt.Errorf("failed to parse XML-RPC response: %w", err)
	}
	if response.Fault != nil {
		code, message := "0", ""
		if response.Fault.Value.Struct != nil {
			for _, member := range response.Fault.Value.Struct.Members {
				switch member.Name {
				case "faultCode":
					code = firstNonEmpty(member.Value.Int, member.Value.String)
				case "faultString":
					message = member.Value.String
				}
			}
		}
		return false, "", fmt.Errorf("manifest server RPC fault %s: %s", code, message)
	}
	if len(response.Params) == 0 || response.Params[0].Value.Array == nil {
		return false, "", fmt.Errorf("invalid XML-RPC response: expected a [boolean, string] array")
	}
	values := response.Params[0].Value.Array.Values
	if len(values) < 2 {
		return false, "", fmt.Errorf("invalid XML-RPC response: expected at least 2 array values, got %d", len(values))
	}
	success := values[0].Boolean == "1" || strings.EqualFold(values[0].Boolean, "true")
	return success, values[1].String, nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

// resolveSmartSyncTarget 决定智能同步请求的目标标识（GetApprovedManifest
// 的 target 参数）。环境变量契约：
//  1. 优先读取本仓约定的 REPO_SYNC_TARGET（新增环境变量一律使用 REPO_ 前缀）；
//  2. 为空时回退 AOSP 构建环境惯例变量，保持上游 repo smartsync 语义兼容：
//     先取 SYNC_TARGET；再由 TARGET_PRODUCT + TARGET_RELEASE +
//     TARGET_BUILD_VARIANT 拼接为 "<product>-<release>-<variant>"；
//     缺少 TARGET_RELEASE 时退化为 "<product>-<variant>"（三者均非空才生效）；
//  3. 都未设置时返回空字符串，请求将不带 target 参数。
func resolveSmartSyncTarget() string {
	if target := os.Getenv("REPO_SYNC_TARGET"); target != "" {
		return target
	}
	if target := os.Getenv("SYNC_TARGET"); target != "" {
		return target
	}
	product := os.Getenv("TARGET_PRODUCT")
	release := os.Getenv("TARGET_RELEASE")
	variant := os.Getenv("TARGET_BUILD_VARIANT")
	if product != "" && release != "" && variant != "" {
		return fmt.Sprintf("%s-%s-%s", product, release, variant)
	}
	if product != "" && variant != "" {
		return fmt.Sprintf("%s-%s", product, variant)
	}
	return ""
}

// getBranch 获取发送给清单服务器的分支名称。
// 优先 SYNC_BRANCH（上游 smartsync 约定，空值视为未设置），其次
// config.ManifestBranch；仍为空时回退清单 default revision。
// 不再读取永远为 nil 的 ManifestProject。
func (e *Engine) getBranch() string {
	branch := strings.TrimSpace(os.Getenv("SYNC_BRANCH"))
	if branch == "" && e.config != nil {
		branch = strings.TrimSpace(e.config.ManifestBranch)
	}
	if branch == "" && e.manifest != nil {
		branch = strings.TrimSpace(e.manifest.Default.Revision)
	}
	return strings.TrimPrefix(branch, "refs/heads/")
}
