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

// manifestServerClient builds an authenticated manifest server URL and HTTP client.
// Shared by getHyperSyncProjects and getChangedProjectsFromServer to avoid duplication.
func (e *Engine) manifestServerClient() (string, *http.Client, error) {
	if e.manifest.ManifestServer == nil {
		return "", nil, fmt.Errorf("cannot connect: manifest server not defined")
	}
	manifestServer := e.manifest.ManifestServer.URL

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
func (e *Engine) getHyperSyncProjects() ([]*project.Project, error) {
	if !e.options.HyperSync {
		return nil, nil
	}

	if !e.options.Quiet {
		fmt.Printf("Using manifest server for HyperSync\n")
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
