package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/raghavharness/mac-fleet/internal/wire"
)

var (
	tokMu        sync.RWMutex
	runtimeToken string
)

func setRuntimeToken(t string) { tokMu.Lock(); runtimeToken = t; tokMu.Unlock() }

// nomadToken is the token pushed by the server, else the optional local one.
func nomadToken(c *Config) string {
	tokMu.RLock()
	defer tokMu.RUnlock()
	if runtimeToken != "" {
		return runtimeToken
	}
	return c.NomadToken
}

// nomadCall talks to Nomad through this machine's own client API, which
// forwards to the servers. The token comes from the command, not from disk.
func nomadCall(ctx context.Context, c *Config, token, method, path string, body, out any) error {
	info := discoverNomad(ctx, c)
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, method, strings.TrimRight(info.Addr, "/")+path, rd)
	if err != nil {
		return err
	}
	if token == "" {
		token = nomadToken(c)
	}
	if token != "" {
		req.Header.Set("X-Nomad-Token", token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := info.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("nomad %s %s: %s: %s", method, path, resp.Status, strings.TrimSpace(string(data)))
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

func nomadNodeID(ctx context.Context, c *Config, p wire.NomadAPIParams) (string, error) {
	if p.NodeID != "" {
		return p.NodeID, nil
	}
	var list []struct{ ID, Name string }
	if err := nomadCall(ctx, c, p.Token, http.MethodGet, "/v1/nodes", nil, &list); err != nil {
		return "", err
	}
	for _, x := range list {
		if strings.EqualFold(x.Name, p.NodeName) {
			return x.ID, nil
		}
	}
	return "", fmt.Errorf("no Nomad node named %q", p.NodeName)
}

func nomadPing(ctx context.Context, c *Config, p wire.NomadAPIParams) (string, error) {
	var leader string
	if err := nomadCall(ctx, c, p.Token, http.MethodGet, "/v1/status/leader", nil, &leader); err != nil {
		return "", err
	}
	return "reachable, leader " + leader, nil
}

func nomadEligibility(ctx context.Context, c *Config, p wire.NomadAPIParams) (string, error) {
	id, err := nomadNodeID(ctx, c, p)
	if err != nil {
		return "", err
	}
	e := "ineligible"
	if p.Eligible {
		e = "eligible"
	}
	if err := nomadCall(ctx, c, p.Token, http.MethodPost, "/v1/node/"+id+"/eligibility", map[string]any{"NodeID": id, "Eligibility": e}, nil); err != nil {
		return "", err
	}
	return "node set " + e, nil
}

// nomadDrain starts a drain and waits for it to finish. A zero deadline means
// none; force drains immediately.
func nomadDrain(ctx context.Context, c *Config, p wire.NomadAPIParams, progress func(string)) (string, error) {
	id, err := nomadNodeID(ctx, c, p)
	if err != nil {
		return "", err
	}
	deadline := time.Duration(p.DeadlineMinutes) * time.Minute
	d := int64(deadline)
	if p.Force {
		d = -1
	}
	body := map[string]any{"NodeID": id, "DrainSpec": map[string]any{"Deadline": d, "IgnoreSystemJobs": false}}
	if err := nomadCall(ctx, c, p.Token, http.MethodPost, "/v1/node/"+id+"/drain", body, nil); err != nil {
		return "", err
	}
	progress("drain started")
	limit := deadline + 15*time.Minute
	if deadline == 0 {
		limit = 2 * time.Hour
	}
	end := time.Now().Add(limit)
	last := -1
	for time.Now().Before(end) {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(5 * time.Second):
		}
		var nd struct {
			Drain bool `json:"Drain"`
		}
		if err := nomadCall(ctx, c, p.Token, http.MethodGet, "/v1/node/"+id, nil, &nd); err != nil {
			continue
		}
		var allocs []struct {
			ClientStatus string `json:"ClientStatus"`
		}
		if err := nomadCall(ctx, c, p.Token, http.MethodGet, "/v1/node/"+id+"/allocations", nil, &allocs); err == nil {
			n := 0
			for _, a := range allocs {
				if a.ClientStatus == "running" || a.ClientStatus == "pending" {
					n++
				}
			}
			if n != last {
				last = n
				progress(fmt.Sprintf("allocations remaining: %d", n))
			}
		}
		if !nd.Drain {
			return "drain complete", nil
		}
	}
	return "", errors.New("drain did not finish in time")
}
