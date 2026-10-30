package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Config is the agent's on-disk configuration. Everything the agent is allowed
// to execute is defined here, never taken from the server.
type Config struct {
	Server      string            `json:"server"`    // https://host:8443
	CAPin       string            `json:"ca_sha256"` // pinned SHA-256 of the server CA, hex
	Hostname    string            `json:"hostname"`  // defaults to os hostname
	Labels      map[string]string `json:"labels"`
	DataDir     string            `json:"data_dir"`    // holds the key, cert and CA
	NomadAddr   string            `json:"nomad_addr"`  // local Nomad API
	NomadToken  string            `json:"nomad_token"` // optional, node:read
	NomadBin    string            `json:"nomad_bin"`
	NomadConfig string            `json:"nomad_config"` // path of the Nomad client config file
	TartBin     string            `json:"tart_bin"`
	DiskPath    string            `json:"disk_path"` // volume that holds Tart images
	// Commands used to manage the Nomad client process. launchctl by default.
	NomadRestartCmd []string `json:"nomad_restart_cmd"`
	NomadStopCmd    []string `json:"nomad_stop_cmd"`
	NomadStartCmd   []string `json:"nomad_start_cmd"`
	MetricsEverySec int      `json:"metrics_every_sec"`
	InventoryEvery  int      `json:"inventory_every_sec"`
}

func (c *Config) defaults() {
	if c.NomadAddr == "" {
		c.NomadAddr = "http://127.0.0.1:4646"
	}
	if c.NomadBin == "" {
		c.NomadBin = "nomad"
	}
	if c.TartBin == "" {
		c.TartBin = "tart"
	}
	home, _ := os.UserHomeDir()
	if c.DataDir == "" {
		c.DataDir = filepath.Join(home, ".macfleet")
	}
	if c.DiskPath == "" {
		c.DiskPath = home
	}
	if c.NomadConfig == "" {
		c.NomadConfig = filepath.Join(home, "nomad", "client.hcl")
	}
	uid := os.Getuid()
	label := "io.harness.nomad"
	if len(c.NomadRestartCmd) == 0 {
		c.NomadRestartCmd = []string{"launchctl", "kickstart", "-k", fmt.Sprintf("gui/%d/%s", uid, label)}
	}
	if len(c.NomadStopCmd) == 0 {
		c.NomadStopCmd = []string{"launchctl", "kill", "SIGTERM", fmt.Sprintf("gui/%d/%s", uid, label)}
	}
	if len(c.NomadStartCmd) == 0 {
		c.NomadStartCmd = []string{"launchctl", "kickstart", fmt.Sprintf("gui/%d/%s", uid, label)}
	}
	if c.MetricsEverySec <= 0 {
		c.MetricsEverySec = 15
	}
	if c.InventoryEvery <= 0 {
		c.InventoryEvery = 60
	}
	if c.Hostname == "" {
		c.Hostname, _ = os.Hostname()
	}
}

func LoadConfig(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	c.defaults()
	return &c, nil
}

func (c *Config) Save(path string) error {
	b, _ := json.MarshalIndent(c, "", "  ")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}

func (c *Config) Defaults() { c.defaults() }
