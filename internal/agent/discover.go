package agent

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Under launchd the PATH is minimal, so tools installed by Homebrew or by hand
// are not found. These directories are searched as a fallback.
func extraPathDirs() []string {
	home, _ := os.UserHomeDir()
	return []string{"/opt/homebrew/bin", "/usr/local/bin", filepath.Join(home, "bin"), "/usr/bin", "/bin"}
}

// resolveBin finds an executable by name, falling back to common install dirs.
func resolveBin(name string) string {
	if strings.Contains(name, "/") {
		return name
	}
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	for _, d := range extraPathDirs() {
		p := filepath.Join(d, name)
		if st, err := os.Stat(p); err == nil && !st.IsDir() && st.Mode()&0o111 != 0 {
			return p
		}
	}
	return name
}

// childEnv is the environment for child processes, with the fallback dirs
// appended to PATH.
func childEnv() []string {
	path := os.Getenv("PATH")
	for _, d := range extraPathDirs() {
		if !strings.Contains(":"+path+":", ":"+d+":") {
			path += ":" + d
		}
	}
	env := make([]string, 0, len(os.Environ())+1)
	for _, e := range os.Environ() {
		if !strings.HasPrefix(e, "PATH=") {
			env = append(env, e)
		}
	}
	return append(env, "PATH="+strings.TrimPrefix(path, ":"))
}

// localIPs lists the non-loopback IPv4 addresses of this machine.
func localIPs() []string {
	var out []string
	ifs, err := net.Interfaces()
	if err != nil {
		return out
	}
	for _, ifc := range ifs {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip := ipn.IP.To4()
			if ip == nil || ip.IsLinkLocalUnicast() {
				continue
			}
			out = append(out, ip.String())
		}
	}
	sort.Strings(out)
	return out
}

// ---- Nomad process and config discovery ----

type nomadProc struct {
	Pid  int
	Bin  string
	Args []string // everything after the binary
	Path string   // PATH of the process, when it could be read
}

// configArgs returns the -config values of the command line.
func (p *nomadProc) configArgs() []string {
	var out []string
	for i := 0; i < len(p.Args); i++ {
		a := p.Args[i]
		for _, pre := range []string{"-config", "--config"} {
			if a == pre && i+1 < len(p.Args) {
				out = append(out, p.Args[i+1])
				i++
			} else if strings.HasPrefix(a, pre+"=") {
				out = append(out, strings.TrimPrefix(a, pre+"="))
			}
		}
	}
	return out
}

// parsePS finds a running `nomad agent` in `ps -axo pid=,args=` output.
func parsePS(out string) *nomadProc {
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 3 {
			continue
		}
		pid, err := strconv.Atoi(f[0])
		if err != nil || filepath.Base(f[1]) != "nomad" || f[2] != "agent" {
			continue
		}
		return &nomadProc{Pid: pid, Bin: f[1], Args: f[2:]}
	}
	return nil
}

var pathEnvRe = regexp.MustCompile(`(?:^|\s)PATH=(\S+)`)

func findNomadProc(ctx context.Context) *nomadProc {
	out, err := exec.CommandContext(ctx, "ps", "-axo", "pid=,args=").Output()
	if err != nil {
		return nil
	}
	p := parsePS(string(out))
	if p == nil {
		return nil
	}
	// Best effort: the environment is visible for our own processes.
	if env, err := exec.CommandContext(ctx, "ps", "eww", "-p", strconv.Itoa(p.Pid), "-o", "command=").Output(); err == nil {
		if m := pathEnvRe.FindStringSubmatch(string(env)); m != nil {
			p.Path = m[1]
		}
	}
	return p
}

// ---- minimal HCL reading (only what the agent needs) ----

var hclCommentRe = regexp.MustCompile(`(?s)/\*.*?\*/`)

func stripHCLComments(s string) string {
	s = hclCommentRe.ReplaceAllString(s, "")
	var b strings.Builder
	for _, line := range strings.Split(s, "\n") {
		inStr := false
		cut := len(line)
		for i := 0; i < len(line); i++ {
			ch := line[i]
			if ch == '"' && (i == 0 || line[i-1] != '\\') {
				inStr = !inStr
			}
			if !inStr && (ch == '#' || (ch == '/' && i+1 < len(line) && line[i+1] == '/')) {
				cut = i
				break
			}
		}
		b.WriteString(line[:cut])
		b.WriteByte('\n')
	}
	return b.String()
}

// hclBlock returns the body of the first `name { ... }` block.
func hclBlock(src, name string) (string, bool) {
	re := regexp.MustCompile(`(?m)(?:^|[\s{;])` + regexp.QuoteMeta(name) + `\s*=?\s*\{`)
	loc := re.FindStringIndex(src)
	if loc == nil {
		return "", false
	}
	depth, inStr := 0, false
	start := loc[1]
	for i := start; i < len(src); i++ {
		switch src[i] {
		case '"':
			if i == 0 || src[i-1] != '\\' {
				inStr = !inStr
			}
		case '{':
			if !inStr {
				depth++
			}
		case '}':
			if !inStr {
				if depth == 0 {
					return src[start:i], true
				}
				depth--
			}
		}
	}
	return "", false
}

// hclValue reads `key = value` from the top level of a block body.
func hclValue(body, key string) string {
	re := regexp.MustCompile(`(?m)^\s*` + regexp.QuoteMeta(key) + `\s*=\s*("([^"]*)"|[^\s#]+)`)
	m := re.FindStringSubmatch(body)
	if m == nil {
		return ""
	}
	if m[2] != "" {
		return m[2]
	}
	return strings.Trim(m[1], `"`)
}

type nomadHCL struct {
	TLSHTTP  bool
	CAFile   string
	CertFile string
	KeyFile  string
	Addr     string // host:port the HTTP API listens on, for local use
}

func parseNomadHCL(content string) nomadHCL {
	src := stripHCLComments(content)
	var h nomadHCL
	if body, ok := hclBlock(src, "tls"); ok {
		h.TLSHTTP = hclValue(body, "http") == "true"
		h.CAFile = hclValue(body, "ca_file")
		h.CertFile = hclValue(body, "cert_file")
		h.KeyFile = hclValue(body, "key_file")
	}
	host := hclValue(src, "bind_addr")
	if body, ok := hclBlock(src, "addresses"); ok {
		if v := hclValue(body, "http"); v != "" {
			host = v
		}
	}
	port := "4646"
	if body, ok := hclBlock(src, "ports"); ok {
		if v := hclValue(body, "http"); v != "" {
			port = v
		}
	}
	if host == "" || host == "0.0.0.0" || host == "::" || host == "[::]" {
		host = "127.0.0.1"
	}
	h.Addr = net.JoinHostPort(host, port)
	return h
}

// ---- discovery cache and HTTP client ----

type nomadInfo struct {
	Proc       *nomadProc
	ConfigPath string
	Addr       string
	Client     *http.Client
	At         time.Time
	Key        string
}

var (
	discMu    sync.Mutex
	discCache *nomadInfo
)

func invalidateNomadInfo() {
	discMu.Lock()
	discCache = nil
	discMu.Unlock()
}

// readableConfigPath picks the config file: the explicit setting, else the
// first -config of the running process (a file, or nomad.hcl in a directory).
func configFromProc(p *nomadProc) string {
	if p == nil {
		return ""
	}
	for _, a := range p.configArgs() {
		st, err := os.Stat(a)
		if err != nil {
			continue
		}
		if st.IsDir() {
			for _, n := range []string{"nomad.hcl", "client.hcl", "nomad.json"} {
				if _, err := os.Stat(filepath.Join(a, n)); err == nil {
					return filepath.Join(a, n)
				}
			}
			continue
		}
		return a
	}
	return ""
}

func discoverNomad(ctx context.Context, c *Config) *nomadInfo {
	discMu.Lock()
	defer discMu.Unlock()
	key := c.NomadAddr + "|" + c.NomadConfig
	if discCache != nil && discCache.Key == key && time.Since(discCache.At) < 30*time.Second {
		return discCache
	}
	info := &nomadInfo{At: time.Now(), Key: key}
	info.Proc = findNomadProc(ctx)
	info.ConfigPath = c.NomadConfig
	if info.ConfigPath == "" {
		info.ConfigPath = configFromProc(info.Proc)
	}
	var hcl nomadHCL
	if info.ConfigPath != "" {
		if b, err := os.ReadFile(info.ConfigPath); err == nil {
			hcl = parseNomadHCL(string(b))
		}
	}
	scheme := "http"
	if hcl.TLSHTTP {
		scheme = "https"
	}
	hostport := hcl.Addr
	if hostport == "" {
		hostport = "127.0.0.1:4646"
	}
	info.Addr = scheme + "://" + hostport
	if c.NomadAddr != "" {
		info.Addr = strings.TrimRight(c.NomadAddr, "/")
	}
	tr := &http.Transport{DisableKeepAlives: true}
	if strings.HasPrefix(info.Addr, "https://") {
		tr.TLSClientConfig = nomadTLS(hcl)
	}
	info.Client = &http.Client{Transport: tr, Timeout: 5 * time.Second}
	discCache = info
	return info
}

// nomadTLS trusts the CA from the Nomad config and verifies the chain, but not
// the host name: the agent talks to loopback while the certificate names the
// node's advertised address.
func nomadTLS(h nomadHCL) *tls.Config {
	pool := x509.NewCertPool()
	haveCA := false
	if h.CAFile != "" {
		if b, err := os.ReadFile(h.CAFile); err == nil && pool.AppendCertsFromPEM(b) {
			haveCA = true
		}
	}
	cfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if h.CertFile != "" && h.KeyFile != "" {
		if cert, err := tls.LoadX509KeyPair(h.CertFile, h.KeyFile); err == nil {
			cfg.Certificates = []tls.Certificate{cert}
		}
	}
	if !haveCA {
		// Without the CA there is nothing to verify against.
		cfg.InsecureSkipVerify = true
		return cfg
	}
	cfg.InsecureSkipVerify = true // we verify the chain ourselves below
	cfg.VerifyPeerCertificate = func(raw [][]byte, _ [][]*x509.Certificate) error {
		if len(raw) == 0 {
			return errors.New("no server certificate")
		}
		var certs []*x509.Certificate
		for _, r := range raw {
			cert, err := x509.ParseCertificate(r)
			if err != nil {
				return err
			}
			certs = append(certs, cert)
		}
		inter := x509.NewCertPool()
		for _, ct := range certs[1:] {
			inter.AddCert(ct)
		}
		_, err := certs[0].Verify(x509.VerifyOptions{Roots: pool, Intermediates: inter, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny}})
		if err != nil {
			return fmt.Errorf("nomad certificate: %w", err)
		}
		return nil
	}
	return cfg
}
