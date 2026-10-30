package agent

import (
	"context"
	"crypto/x509/pkix"
	"encoding/json"
	"fmt"
	"net/http"
	"os/exec"
	"path/filepath"
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
	h := wire.Hello{Hostname: c.Hostname, Version: version, OS: runtime.GOOS + "/" + runtime.GOARCH, Cores: runtime.NumCPU(), Labels: c.Labels, IPs: localIPs()}
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
	// Short-lived VMs can start and stop between inventory refreshes, so also
	// count the `tart run` processes that exist right now.
	if n := countTartRuns(ctx); n > s.VMsRunning {
		s.VMsRunning = n
	}
	s.Nomad = nomadState(ctx, c)
	return s
}

func round1(f float64) float64 { return float64(int(f*10+0.5)) / 10 }

// nomadState asks the local Nomad client API for this node's status. The
// address, TLS settings and config path come from the running process when the
// agent config does not say.
func nomadState(ctx context.Context, c *Config) wire.NomadState {
	info := discoverNomad(ctx, c)
	st := wire.NomadState{Addr: info.Addr, ConfigPath: info.ConfigPath, ManagedBy: "none"}
	if info.Proc != nil {
		st.Pid = info.Proc.Pid
		st.ManagedBy = "shell"
	}
	if len(c.NomadRestartCmd) == 0 && launchdLoaded(ctx, c) {
		st.ManagedBy = "launchd"
	}
	var self struct {
		Stats map[string]map[string]string `json:"stats"`
	}
	if err := nomadGet(ctx, c, "/v1/agent/self", &self); err != nil {
		// The node details need agent:read; the health endpoint may still answer.
		var health map[string]any
		if herr := nomadGet(ctx, c, "/v1/agent/health", &health); herr == nil {
			st.Reachable = true
			st.Error = "node status unavailable: " + err.Error()
			return st
		}
		st.Error = err.Error()
		if info.Proc == nil {
			st.Error = "no nomad process found; " + st.Error
		}
		return st
	}
	st.Reachable = true
	id := self.Stats["client"]["node_id"]
	if id == "" {
		st.Error = "agent is not running as a client"
		return st
	}
	var node struct {
		Status                string `json:"Status"`
		SchedulingEligibility string `json:"SchedulingEligibility"`
		Drain                 bool   `json:"Drain"`
	}
	if err := nomadGet(ctx, c, "/v1/node/"+id, &node); err != nil {
		st.NodeID = id
		st.Error = err.Error()
		return st
	}
	st.NodeID, st.Status, st.Eligibility, st.Draining = id, node.Status, node.SchedulingEligibility, node.Drain
	return st
}

func nomadGet(ctx context.Context, c *Config, path string, out any) error {
	info := discoverNomad(ctx, c)
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(info.Addr, "/")+path, nil)
	if err != nil {
		return err
	}
	if c.NomadToken != "" {
		req.Header.Set("X-Nomad-Token", c.NomadToken)
	}
	resp, err := info.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("nomad %s: %s", path, resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// parseTartRuns counts `tart run` commands in `ps -axo args=` output.
func parseTartRuns(out string) int {
	n := 0
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 && filepath.Base(f[0]) == "tart" && f[1] == "run" {
			n++
		}
	}
	return n
}

func countTartRuns(ctx context.Context) int {
	out, err := exec.CommandContext(ctx, "ps", "-axo", "args=").Output()
	if err != nil {
		return 0
	}
	return parseTartRuns(string(out))
}
