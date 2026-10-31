// Package wire defines the messages exchanged between the agent and the server
// over the agent WebSocket. Everything is JSON in an Envelope.
package wire

import "encoding/json"

// Message types.
const (
	// agent -> server
	TypeHello     = "hello"
	TypeMetrics   = "metrics"
	TypeInventory = "inventory"
	TypeProgress  = "progress"
	TypeResult    = "result"
	TypeSettings  = "settings" // server -> agent: runtime settings, kept in memory only
	// server -> agent
	TypeCommand = "command"
)

// Command types the agent understands. The agent only runs commands from this
// list; there is no arbitrary shell execution.
const (
	CmdNomadRestart     = "nomad.restart"
	CmdNomadStop        = "nomad.stop"
	CmdNomadStart       = "nomad.start"
	CmdNomadConfigApply = "nomad.config.apply"
	CmdNomadConfigRead  = "nomad.config.read"
	// CmdNomadAdopt puts a shell-started Nomad under a launchd job.
	CmdNomadAdopt = "nomad.service.install"
	CmdTartList   = "tart.list"
	CmdTartPull   = "tart.pull"
	CmdTartDelete = "tart.delete"
	CmdTartPrune  = "tart.prune"
	// Nomad API operations. The server picks an online agent in the host's
	// cluster to run them (the target's own agent when it is up), because the
	// server itself usually cannot reach Nomad.
	CmdNomadDrain       = "nomad.drain"
	CmdNomadEligibility = "nomad.eligibility"
	CmdNomadPing        = "nomad.ping"
)

// Command states.
const (
	StateQueued    = "queued"
	StateRunning   = "running"
	StateSucceeded = "succeeded"
	StateFailed    = "failed"
	StateExpired   = "expired"
)

type Envelope struct {
	Type    string          `json:"type"`
	ID      string          `json:"id,omitempty"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

type Hello struct {
	Hostname string            `json:"hostname"`
	Version  string            `json:"version"`
	OS       string            `json:"os"`
	Cores    int               `json:"cores"`
	MemGB    float64           `json:"mem_gb"`
	Labels   map[string]string `json:"labels,omitempty"`
	// IPs are the host's non-loopback IPv4 addresses.
	IPs []string `json:"ips,omitempty"`
}

type NomadState struct {
	Reachable bool `json:"reachable"`
	// ManagedBy is launchd, shell or none.
	ManagedBy   string `json:"managed_by,omitempty"`
	Pid         int    `json:"pid,omitempty"`
	Addr        string `json:"addr,omitempty"`
	ConfigPath  string `json:"config_path,omitempty"`
	Error       string `json:"error,omitempty"`
	NodeID      string `json:"node_id,omitempty"`
	Status      string `json:"status,omitempty"`      // ready, down, initializing
	Eligibility string `json:"eligibility,omitempty"` // eligible, ineligible
	Draining    bool   `json:"draining"`
}

type Sample struct {
	CPUPct      float64    `json:"cpu_pct"`
	MemPct      float64    `json:"mem_pct"`
	DiskUsedPct float64    `json:"disk_used_pct"`
	DiskFreeGB  float64    `json:"disk_free_gb"`
	Load1       float64    `json:"load1"`
	UptimeSec   uint64     `json:"uptime_sec"`
	VMsRunning  int        `json:"vms_running"`
	Nomad       NomadState `json:"nomad"`
}

type Image struct {
	Name    string  `json:"name"`
	Source  string  `json:"source,omitempty"`
	SizeGB  float64 `json:"size_gb"`
	Running bool    `json:"running"`
	// Accessed is the last access time reported by Tart, RFC3339 when known.
	Accessed string `json:"accessed,omitempty"`
}

type Inventory struct {
	Images []Image `json:"images"`
	// Error is set when the image list could not be read.
	Error string `json:"error,omitempty"`
}

type Command struct {
	ID     string          `json:"id"`
	Type   string          `json:"type"`
	Params json.RawMessage `json:"params,omitempty"`
}

type Progress struct {
	CommandID string `json:"command_id"`
	Line      string `json:"line"`
}

type Result struct {
	CommandID string `json:"command_id"`
	State     string `json:"state"`
	Message   string `json:"message,omitempty"`
	// Data carries structured output, e.g. a config file read.
	Data json.RawMessage `json:"data,omitempty"`
}

// NomadConfigData is the Data of a nomad.config.read result.
type NomadConfigData struct {
	Path     string `json:"path"`
	Content  string `json:"content"`
	Writable bool   `json:"writable"`
	ModTime  string `json:"mod_time,omitempty"`
}

// Command parameters.
type TartImageParams struct {
	Image string `json:"image"`
}

type TartPruneParams struct {
	OlderThanDays int  `json:"older_than_days"`
	DryRun        bool `json:"dry_run"`
}

type NomadConfigParams struct {
	Content string `json:"content"`
	Restart bool   `json:"restart"`
	// WaitReadySec is how long to wait for the node to rejoin after restart
	// before rolling back. Defaults to 120.
	WaitReadySec int `json:"wait_ready_sec,omitempty"`
	// ValidateOnly checks the config without saving it.
	ValidateOnly bool `json:"validate_only,omitempty"`
}

// NomadAPIParams is what the server sends for CmdNomadDrain, CmdNomadEligibility
// and CmdNomadPing. It carries the cluster's ACL token, which is added when the
// command is sent and never stored with the command.
type NomadAPIParams struct {
	Token           string `json:"token,omitempty"`
	NodeID          string `json:"node_id,omitempty"`
	NodeName        string `json:"node_name,omitempty"`
	DeadlineMinutes int    `json:"deadline_minutes,omitempty"`
	Force           bool   `json:"force,omitempty"`
	Eligible        bool   `json:"eligible,omitempty"`
}

type NomadRestartParams struct {
	WaitReadySec int `json:"wait_ready_sec,omitempty"`
}

func Marshal(typ, id string, payload any) ([]byte, error) {
	var raw json.RawMessage
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		raw = b
	}
	return json.Marshal(Envelope{Type: typ, ID: id, Payload: raw})
}

// Settings is pushed by the server after hello and when a cluster changes. The
// token is kept in memory by the agent and never written to disk.
type Settings struct {
	NomadToken string `json:"nomad_token"`
}
