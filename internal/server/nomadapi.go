package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// nomadClient talks to the Nomad server API with the ACL token held by this
// service. It is used for drain and eligibility, which do not need the host.
type nomadClient struct {
	addr  string
	token string
	hc    *http.Client
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
