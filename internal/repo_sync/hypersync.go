package repo_sync

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/leopardxu/repo-go/internal/project"
)

// hyperSyncAvailable 判断 HyperSync 是否具备可用的清单服务器。
// 未配置 manifest-server（清单未声明且未用 --manifest-server-url 指定）
// 或显式 --no-manifest-server 时，HyperSync 直接退化为全量同步，
// 不产生 "Using manifest server"/"cannot connect" 之类的告警噪音。
func (e *Engine) hyperSyncAvailable() bool {
	if e == nil || e.options == nil || !e.options.HyperSync {
		return false
	}
	if e.options.NoManifestServer {
		return false
	}
	return e.manifestServerURL() != ""
}

// manifestServerClient builds an authenticated manifest server URL and HTTP client.
// Shared by getHyperSyncProjects and getChangedProjectsFromServer to avoid duplication.
func (e *Engine) manifestServerClient() (string, *http.Client, error) {
	manifestServer := e.manifestServerURL()
	if manifestServer == "" {
		return "", nil, fmt.Errorf("cannot connect: manifest server not defined")
	}

	// Handle authentication
	if !strings.Contains(manifestServer, "@") {
		username := e.options.ManifestServerUsername
		password := e.options.ManifestServerPassword

		if username != "" && password != "" {
			u, err := url.Parse(manifestServer)
			if err == nil {
				u.User = url.UserPassword(username, password)
				manifestServer = u.String()
			}
		}
	}

	httpTimeout := e.options.HTTPTimeout
	if httpTimeout <= 0 {
		httpTimeout = 30 * time.Second
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

	return manifestServer, client, nil
}

// fetchChangedProjects queries the manifest server for changed projects on the given branch.
// Returns a list of project names that have changed.
func (e *Engine) fetchChangedProjects() ([]string, error) {
	manifestServer, client, err := e.manifestServerClient()
	if err != nil {
		return nil, err
	}

	branch := e.getBranch()
	requestURL := fmt.Sprintf("%s/api/GetChangedProjects?branch=%s",
		manifestServer, url.QueryEscape(branch))

	resp, err := client.Get(requestURL)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to manifest server: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("manifest server returned status %d", resp.StatusCode)
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response from server: %w", err)
	}

	var changedProjects []string
	if err := json.Unmarshal(data, &changedProjects); err != nil {
		return nil, fmt.Errorf("failed to parse server response: %w", err)
	}

	return changedProjects, nil
}

// getHyperSyncProjects returns the subset of projects that need HyperSync.
// 调用方须先用 hyperSyncAvailable 确认服务器可用；本函数不再自行打印
// "Using manifest server"，避免无服务器时先输出误导性信息再告警。
func (e *Engine) getHyperSyncProjects() ([]*project.Project, error) {
	if !e.options.HyperSync {
		return nil, nil
	}

	changedProjects, err := e.fetchChangedProjects()
	if err != nil {
		return nil, err
	}

	// Filter to only changed projects
	changedSet := make(map[string]bool, len(changedProjects))
	for _, name := range changedProjects {
		changedSet[name] = true
	}

	var hyperSyncProjects []*project.Project
	for _, p := range e.projects {
		if changedSet[p.Name] {
			hyperSyncProjects = append(hyperSyncProjects, p)
		}
	}

	if !e.options.Quiet {
		fmt.Printf("HyperSync: %d of %d projects changed\n",
			len(hyperSyncProjects), len(e.projects))
	}

	return hyperSyncProjects, nil
}
