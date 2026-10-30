package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// nomadClient talks to the Nomad server API with the ACL token held by this
// service. It is used for drain and eligibility, which do not need the host.
type nomadClient struct {
	addr  string
	token string
	hc    *http.Client
}

func (s *Server) nomad(ctx context.Context) (*nomadClient, error) {
	st := s.st.loadSettings(ctx)
	if st.NomadAddr == "" {
		return nil, errors.New("Nomad address is not configured (Settings)")
	}
	tok, err := s.st.getSecret(ctx, "nomad_token")
	if err != nil {
		return nil, err
	}
	return &nomadClient{addr: strings.TrimRight(st.NomadAddr, "/"), token: tok, hc: &http.Client{Timeout: 20 * time.Second}}, nil
}

func (n *nomadClient) do(ctx context.Context, method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, n.addr+path, rd)
	if err != nil {
		return err
	}
	if n.token != "" {
		req.Header.Set("X-Nomad-Token", n.token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := n.hc.Do(req)
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

func (n *nomadClient) ping(ctx context.Context) error {
	var leader string
	return n.do(ctx, http.MethodGet, "/v1/status/leader", nil, &leader)
}

type nomadNode struct {
	ID          string `json:"ID"`
	Name        string `json:"Name"`
	Status      string `json:"Status"`
	Eligibility string `json:"SchedulingEligibility"`
	Drain       bool   `json:"Drain"`
}

func (n *nomadClient) node(ctx context.Context, id string) (nomadNode, error) {
	var nd nomadNode
	err := n.do(ctx, http.MethodGet, "/v1/node/"+id, nil, &nd)
	return nd, err
}

// findNodeByName is the fallback when an agent has not reported a node ID yet.
func (n *nomadClient) findNodeByName(ctx context.Context, name string) (string, error) {
	var list []struct{ ID, Name string }
	if err := n.do(ctx, http.MethodGet, "/v1/nodes", nil, &list); err != nil {
		return "", err
	}
	for _, x := range list {
		if strings.EqualFold(x.Name, name) {
			return x.ID, nil
		}
	}
	return "", fmt.Errorf("no Nomad node named %q", name)
}

// drain starts a drain. deadline 0 means no deadline, force drains immediately.
func (n *nomadClient) drain(ctx context.Context, id string, deadline time.Duration, force bool) error {
	d := int64(deadline)
	if force {
		d = -1
	}
	body := map[string]any{
		"NodeID":    id,
		"DrainSpec": map[string]any{"Deadline": d, "IgnoreSystemJobs": false},
	}
	return n.do(ctx, http.MethodPost, "/v1/node/"+id+"/drain", body, nil)
}

func (n *nomadClient) setEligibility(ctx context.Context, id string, eligible bool) error {
	e := "ineligible"
	if eligible {
		e = "eligible"
	}
	return n.do(ctx, http.MethodPost, "/v1/node/"+id+"/eligibility", map[string]any{"NodeID": id, "Eligibility": e}, nil)
}

func (n *nomadClient) runningAllocs(ctx context.Context, id string) (int, error) {
	var allocs []struct {
		ClientStatus string `json:"ClientStatus"`
	}
	if err := n.do(ctx, http.MethodGet, "/v1/node/"+id+"/allocations", nil, &allocs); err != nil {
		return 0, err
	}
	c := 0
	for _, a := range allocs {
		if a.ClientStatus == "running" || a.ClientStatus == "pending" {
			c++
		}
	}
	return c, nil
}
