package agent

import (
	"fmt"
	"os"
	"testing"
)

const sampleHCL = `
# managed by provisioning
bind_addr = "127.0.0.1"
data_dir  = "/data/nomad"
tls {
  http = true
  rpc  = true
  ca_file   = "/etc/tls/ca.pem"
  cert_file = "/etc/tls/nomad.pem"   # trailing comment
  key_file  = "/etc/tls/nomad-key.pem"
  verify_server_hostname = true
}
advertise {
  http = "192.0.2.10"
}
client {
  enabled = true
  servers = ["192.0.2.20"]
  options {
    "driver.allowlist" = "raw_exec"
  }
}
`

func TestParseNomadHCL(t *testing.T) {
	h := parseNomadHCL(sampleHCL)
	if !h.TLSHTTP || h.CAFile != "/etc/tls/ca.pem" || h.CertFile != "/etc/tls/nomad.pem" || h.KeyFile != "/etc/tls/nomad-key.pem" {
		t.Fatalf("tls: %+v", h)
	}
	if h.Addr != "127.0.0.1:4646" {
		t.Fatalf("addr %q", h.Addr)
	}
}

func TestParseNomadHCLPorts(t *testing.T) {
	h := parseNomadHCL("bind_addr = \"0.0.0.0\"\nports {\n http = 5000\n}\n")
	if h.TLSHTTP || h.Addr != "127.0.0.1:5000" {
		t.Fatalf("%+v", h)
	}
}

func TestParsePS(t *testing.T) {
	out := "  101 /usr/sbin/cfprefsd agent\n83057 /usr/local/bin/nomad agent -config /etc/nomad.d/nomad.hcl\n  9 grep nomad\n"
	p := parsePS(out)
	if p == nil || p.Pid != 83057 || p.Bin != "/usr/local/bin/nomad" {
		t.Fatalf("%+v", p)
	}
	if got := p.configArgs(); len(got) != 1 || got[0] != "/etc/nomad.d/nomad.hcl" {
		t.Fatalf("config args %v", got)
	}
	q := parsePS("5 /opt/nomad agent -config=/a.hcl -config /b.hcl\n")
	if got := q.configArgs(); len(got) != 2 || got[0] != "/a.hcl" || got[1] != "/b.hcl" {
		t.Fatalf("config args %v", got)
	}
	if parsePS("1 /bin/sh -c nomad agent\n") != nil {
		t.Fatal("matched a shell")
	}
}

func TestBuildPlist(t *testing.T) {
	s := buildPlist("io.example.nomad", "/usr/local/bin/nomad", []string{"agent", "-config", "/etc/a&b.hcl"}, "/usr/bin", "/tmp/l.log")
	for _, want := range []string{"<string>agent</string>", "/etc/a&amp;b.hcl", "<key>KeepAlive</key><true/>", "<key>PATH</key><string>/usr/bin</string>"} {
		if !contains(s, want) {
			t.Fatalf("missing %q in\n%s", want, s)
		}
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func TestParseTartRuns(t *testing.T) {
	out := "/opt/homebrew/bin/tart run --no-graphics vm-a\n/usr/local/bin/tart run vm-b\n/usr/local/bin/tart list\n/bin/sh -c tart run x\n"
	if n := parseTartRuns(out); n != 2 {
		t.Fatalf("got %d", n)
	}
}

func TestDropLegacyDefaults(t *testing.T) {
	c := &Config{NomadAddr: "http://127.0.0.1:4646", NomadRestartCmd: []string{"launchctl", "kickstart", "-k", fmt.Sprintf("gui/%d/io.harness.nomad", os.Getuid())}, NomadStopCmd: []string{"custom", "stop"}}
	c.dropLegacyDefaults()
	if c.NomadAddr != "" || c.NomadRestartCmd != nil || len(c.NomadStopCmd) != 2 {
		t.Fatalf("%+v", c)
	}
}

func TestHeartbeatOK(t *testing.T) {
	if !heartbeatOK(map[string]string{"known_servers": "1.2.3.4:4647", "last_heartbeat": "4.5s"}) {
		t.Fatal("recent heartbeat should be ok")
	}
	if heartbeatOK(map[string]string{"known_servers": "1.2.3.4:4647", "last_heartbeat": "5m0s"}) || heartbeatOK(map[string]string{"last_heartbeat": "1s"}) {
		t.Fatal("stale or serverless should not be ok")
	}
}

func TestParseNomadHCLJoin(t *testing.T) {
	h := parseNomadHCL(`
region = "qa-region"
datacenter = "dc1"
client {
  enabled = true
  servers = ["10.1.0.5:4647", "10.1.0.6:4647"] # comment
}
`)
	if h.Region != "qa-region" || h.Datacenter != "dc1" || len(h.Servers) != 2 || h.Servers[0] != "10.1.0.5:4647" {
		t.Fatalf("got %+v", h)
	}
}
