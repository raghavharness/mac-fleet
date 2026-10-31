package server

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/raghavharness/mac-fleet/internal/wire"
)

// Agent builds are published as GitHub releases (see `make release`). The
// server reads the release and its SHA256SUMS and hands the agent the exact
// URL and checksum to accept.

var versionRe = regexp.MustCompile(`^v\d+\.\d+\.\d+([-.][0-9A-Za-z.]+)?$`)

type releaseInfo struct {
	Version string
	Assets  map[string]wire.UpgradeAsset // keyed darwin-arm64, darwin-amd64
	At      time.Time
}

var (
	relMu    sync.Mutex
	relCache = map[string]*releaseInfo{}
)

var assetNames = map[string]string{
	"darwin-arm64": "macfleet-agent-darwin-arm64",
	"darwin-amd64": "macfleet-agent-darwin-amd64",
}

func ghGet(ctx context.Context, url string, limit int64) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == 404 {
		return nil, errors.New("release not found")
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("GitHub answered %s", resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, limit))
}

// agentRelease resolves a version (or "latest") to its downloads.
func (s *Server) agentRelease(ctx context.Context, version string) (*releaseInfo, error) {
	if s.cfg.ReleasePrivate {
		return nil, errors.New("upgrading from the dashboard needs a public release repository")
	}
	if version == "" {
		version = "latest"
	}
	if version != "latest" && !versionRe.MatchString(version) {
		return nil, fmt.Errorf("invalid version %q", version)
	}
	relMu.Lock()
	if r := relCache[version]; r != nil && time.Since(r.At) < 10*time.Minute {
		relMu.Unlock()
		return r, nil
	}
	relMu.Unlock()

	api := "https://api.github.com/repos/" + s.cfg.ReleaseRepo + "/releases/"
	if version == "latest" {
		api += "latest"
	} else {
		api += "tags/" + version
	}
	b, err := ghGet(ctx, api, 4<<20)
	if err != nil {
		return nil, err
	}
	var rel struct {
		Tag    string `json:"tag_name"`
		Assets []struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if err := json.Unmarshal(b, &rel); err != nil || rel.Tag == "" {
		return nil, errors.New("could not read the release")
	}
	urls := map[string]string{}
	for _, a := range rel.Assets {
		urls[a.Name] = a.URL
	}
	sumURL := urls["SHA256SUMS"]
	if sumURL == "" {
		return nil, fmt.Errorf("release %s has no SHA256SUMS", rel.Tag)
	}
	sb, err := ghGet(ctx, sumURL, 1<<20)
	if err != nil {
		return nil, err
	}
	sums := map[string]string{}
	sc := bufio.NewScanner(strings.NewReader(string(sb)))
	for sc.Scan() {
		if f := strings.Fields(sc.Text()); len(f) == 2 {
			sums[strings.TrimPrefix(f[1], "*")] = strings.ToLower(f[0])
		}
	}
	r := &releaseInfo{Version: rel.Tag, Assets: map[string]wire.UpgradeAsset{}, At: time.Now()}
	for key, name := range assetNames {
		if u, h := urls[name], sums[name]; u != "" && len(h) == 64 {
			r.Assets[key] = wire.UpgradeAsset{URL: u, SHA256: h}
		}
	}
	if len(r.Assets) == 0 {
		return nil, fmt.Errorf("release %s has no agent builds", rel.Tag)
	}
	relMu.Lock()
	relCache[version], relCache[rel.Tag] = r, r
	relMu.Unlock()
	return r, nil
}

// resolveUpgrade fills in the download details for an agent.upgrade command.
func (s *Server) resolveUpgrade(ctx context.Context, params json.RawMessage) (json.RawMessage, error) {
	var p wire.AgentUpgradeParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, errors.New("invalid params")
	}
	r, err := s.agentRelease(ctx, p.Version)
	if err != nil {
		return nil, err
	}
	p.Version, p.Assets = r.Version, r.Assets
	return json.Marshal(p)
}

func (s *Server) apiAgentRelease(w http.ResponseWriter, r *http.Request) {
	rel, err := s.agentRelease(r.Context(), "latest")
	if err != nil {
		writeJSON(w, 200, map[string]any{"version": "", "error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"version": rel.Version})
}
