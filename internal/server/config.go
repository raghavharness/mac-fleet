package server

import (
	"errors"
	"os"
	"strconv"
	"strings"
)

// Config comes from environment variables so the same image runs anywhere.
type Config struct {
	DatabaseURL    string   // DATABASE_URL
	MasterKey      []byte   // MASTER_KEY, base64 of 32 bytes: encrypts the CA key and Nomad token
	HTTPAddr       string   // HTTP_ADDR, dashboard and API, default :8080
	AgentAddr      string   // AGENT_ADDR, agent mTLS listener, default :8443
	AgentHosts     []string // AGENT_HOSTS, DNS names or IPs the agent endpoint is reached at
	RawDays        int      // RAW_RETENTION_DAYS, default 2
	RetentionDays  int      // RETENTION_DAYS, default 30
	CertDays       int      // AGENT_CERT_DAYS, default 90
	PublicAgentURL string   // PUBLIC_AGENT_URL, what agents are told to connect to
	ReleaseRepo    string   // AGENT_RELEASE_REPO, GitHub owner/repo that hosts agent releases
	ReleasePrivate bool     // AGENT_RELEASE_PRIVATE, true when downloads need a GitHub token
}

func LoadConfig() (*Config, error) {
	c := &Config{
		DatabaseURL:   os.Getenv("DATABASE_URL"),
		HTTPAddr:      getenv("HTTP_ADDR", ":8080"),
		AgentAddr:     getenv("AGENT_ADDR", ":8443"),
		RawDays:       atoi(getenv("RAW_RETENTION_DAYS", "2")),
		RetentionDays: atoi(getenv("RETENTION_DAYS", "30")),
		CertDays:      atoi(getenv("AGENT_CERT_DAYS", "90")),
	}
	for _, h := range strings.Split(os.Getenv("AGENT_HOSTS"), ",") {
		if h = strings.TrimSpace(h); h != "" {
			c.AgentHosts = append(c.AgentHosts, h)
		}
	}
	if c.DatabaseURL == "" {
		return nil, errors.New("DATABASE_URL is required")
	}
	key, err := decodeKey(os.Getenv("MASTER_KEY"))
	if err != nil {
		return nil, err
	}
	c.MasterKey = key
	c.PublicAgentURL = getenv("PUBLIC_AGENT_URL", "")
	c.ReleaseRepo = getenv("AGENT_RELEASE_REPO", "raghavharness/mac-fleet")
	c.ReleasePrivate = getenv("AGENT_RELEASE_PRIVATE", "false") == "true"
	if len(c.AgentHosts) == 0 {
		return nil, errors.New("AGENT_HOSTS is required (comma separated DNS names or IPs agents connect to)")
	}
	if c.PublicAgentURL == "" {
		c.PublicAgentURL = "https://" + c.AgentHosts[0] + ":8443"
	}
	return c, nil
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func atoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}
