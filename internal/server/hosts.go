package server

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/raghavharness/mac-fleet/internal/wire"
)

type Alert struct {
	HostID   string `json:"host_id"`
	Host     string `json:"host"`
	Severity string `json:"severity"` // critical, warning
	Text     string `json:"text"`
}

type HostView struct {
	ID           string            `json:"id"`
	Name         string            `json:"name"`
	Labels       map[string]string `json:"labels"`
	AgentVersion string            `json:"agent_version"`
	OS           string            `json:"os"`
	Cores        int               `json:"cores"`
	MemGB        float64           `json:"mem_gb"`
	LastSeen     *time.Time        `json:"last_seen"`
	Connected    bool              `json:"connected"`
	Status       string            `json:"status"` // online, offline
	Revoked      bool              `json:"revoked"`
	Sample       *wire.Sample      `json:"sample"`
	SampleAt     *time.Time        `json:"sample_at"`
	Inventory    []wire.Image      `json:"inventory"`
	InventoryErr string            `json:"inventory_error,omitempty"`
	IPs          []string          `json:"ips"`
	RemoteIP     string            `json:"remote_ip"`
	ClusterID    string            `json:"cluster_id"`
	Alerts       []Alert           `json:"alerts"`
}

func (s *Server) listHosts(ctx context.Context) ([]HostView, error) {
	st := s.st.loadSettings(ctx)
	rows, err := s.st.pool.Query(ctx, `SELECT id,name,labels,agent_version,os,cores,mem_gb,last_seen,connected,revoked,last_sample,last_sample_at,inventory,ips,remote_ip,inventory_error,cluster_id
		FROM hosts ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []HostView{}
	for rows.Next() {
		var h HostView
		var labels, sample, inv, ips []byte
		if err := rows.Scan(&h.ID, &h.Name, &labels, &h.AgentVersion, &h.OS, &h.Cores, &h.MemGB, &h.LastSeen, &h.Connected, &h.Revoked, &sample, &h.SampleAt, &inv, &ips, &h.RemoteIP, &h.InventoryErr, &h.ClusterID); err != nil {
			return nil, err
		}
		json.Unmarshal(labels, &h.Labels)
		json.Unmarshal(inv, &h.Inventory)
		json.Unmarshal(ips, &h.IPs)
		if h.IPs == nil {
			h.IPs = []string{}
		}
		if sample != nil {
			var sm wire.Sample
			if json.Unmarshal(sample, &sm) == nil {
				h.Sample = &sm
			}
		}
		if h.Inventory == nil {
			h.Inventory = []wire.Image{}
		}
		h.Status = "offline"
		if h.Connected && h.LastSeen != nil && time.Since(*h.LastSeen) < time.Duration(st.OfflineMin)*time.Minute {
			h.Status = "online"
		}
		h.Alerts = alertsFor(h, st)
		if h.Alerts == nil {
			h.Alerts = []Alert{}
		}
		if h.Labels == nil {
			h.Labels = map[string]string{}
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

func alertsFor(h HostView, st Settings) []Alert {
	var a []Alert
	add := func(sev, text string) { a = append(a, Alert{HostID: h.ID, Host: h.Name, Severity: sev, Text: text}) }
	if h.Revoked {
		return nil
	}
	if h.Status == "offline" {
		add("critical", "agent offline")
		return a
	}
	if h.Sample == nil {
		return a
	}
	sm := h.Sample
	if sm.DiskUsedPct >= float64(st.DiskPct) {
		sev := "warning"
		if sm.DiskUsedPct >= 95 {
			sev = "critical"
		}
		add(sev, fmt.Sprintf("disk %.0f%% used", sm.DiskUsedPct))
	}
	if sm.MemPct >= float64(st.MemPct) {
		add("warning", fmt.Sprintf("memory at %.0f%%", sm.MemPct))
	}
	if !sm.Nomad.Reachable {
		add("warning", "Nomad client not reachable from the agent")
	} else if sm.Nomad.Status == "down" {
		add("critical", "Nomad client is down")
	}
	sort.SliceStable(a, func(i, j int) bool { return a[i].Severity == "critical" && a[j].Severity != "critical" })
	return a
}

// publishHost tells dashboards that a host changed and includes the new view,
// so the browser can patch the page without another request. Revoked or
// missing hosts are sent as a bare event.
func (s *Server) publishHost(id string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	h, err := s.hostByID(ctx, id)
	if err != nil || h.Revoked {
		s.bus.publish("host", id)
		return
	}
	b, err := json.Marshal(map[string]any{"kind": "host", "id": id, "host": h})
	if err != nil {
		s.bus.publish("host", id)
		return
	}
	s.bus.publishRaw(string(b))
}
