package agent

import (
	"context"
	"crypto/x509/pkix"
	"encoding/json"
	"fmt"
	"net/http"
	"runtime"
	"strings"
	"time"

	"github.com/raghavharness/mac-fleet/internal/wire"
	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/host"
	"github.com/shirou/gopsutil/v4/load"
	"github.com/shirou/gopsutil/v4/mem"
)

func pkixName(cn string) pkix.Name { return pkix.Name{CommonName: cn} }

func hello(c *Config, version string) wire.Hello {
	h := wire.Hello{Hostname: c.Hostname, Version: version, OS: runtime.GOOS + "/" + runtime.GOARCH, Cores: runtime.NumCPU(), Labels: c.Labels}
	if vm, err := mem.VirtualMemory(); err == nil {
		h.MemGB = float64(vm.Total) / (1 << 30)
	}
	return h
}

// collect gathers one metrics sample. Individual failures leave zero values
// rather than failing the whole sample.
func collect(ctx context.Context, c *Config, images []wire.Image) wire.Sample {
	var s wire.Sample
	if p, err := cpu.PercentWithContext(ctx, 500*time.Millisecond, false); err == nil && len(p) > 0 {
		s.CPUPct = round1(p[0])
	}
	if vm, err := mem.VirtualMemoryWithContext(ctx); err == nil {
		s.MemPct = round1(vm.UsedPercent)
	}
	if du, err := disk.UsageWithContext(ctx, c.DiskPath); err == nil {
		s.DiskUsedPct = round1(du.UsedPercent)
		s.DiskFreeGB = round1(float64(du.Free) / (1 << 30))
	}
	if l, err := load.AvgWithContext(ctx); err == nil {
		s.Load1 = round1(l.Load1)
	}
	if up, err := host.UptimeWithContext(ctx); err == nil {
		s.UptimeSec = up
	}
	for _, im := range images {
		if im.Running {
			s.VMsRunning++
		}
	}
	s.Nomad = nomadState(ctx, c)
	return s
}

func round1(f float64) float64 { return float64(int(f*10+0.5)) / 10 }

// nomadState asks the local Nomad client API for this node's status.
func nomadState(ctx context.Context, c *Config) wire.NomadState {
	var st wire.NomadState
	var self struct {
		Stats map[string]map[string]string `json:"stats"`
	}
	if err := nomadGet(ctx, c, "/v1/agent/self", &self); err != nil {
		return st
	}
	st.Reachable = true
	id := self.Stats["client"]["node_id"]
	if id == "" {
		return st
	}
	var node struct {
		Status                string `json:"Status"`
		SchedulingEligibility string `json:"SchedulingEligibility"`
		Drain                 bool   `json:"Drain"`
	}
	if err := nomadGet(ctx, c, "/v1/node/"+id, &node); err != nil {
		return st
	}
	st.NodeID, st.Status, st.Eligibility, st.Draining = id, node.Status, node.SchedulingEligibility, node.Drain
	return st
}

func nomadGet(ctx context.Context, c *Config, path string, out any) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(c.NomadAddr, "/")+path, nil)
	if err != nil {
		return err
	}
	if c.NomadToken != "" {
		req.Header.Set("X-Nomad-Token", c.NomadToken)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("nomad %s: %s", path, resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
