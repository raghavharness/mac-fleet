package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/raghavharness/mac-fleet/internal/wire"
)

// The agent replaces its own binary, then asks launchd to restart it. The old
// binary is kept as <exe>.prev. A marker file records the pending upgrade; the
// new process removes it once it has connected to the server. If it cannot
// connect, it restores the old binary and exits so launchd starts that one.

const agentLabel = "io.harness.macfleet-agent"

type upgradeMarker struct {
	From     string `json:"from"`
	To       string `json:"to"`
	Attempts int    `json:"attempts"`
}

func markerPath(c *Config) string { return filepath.Join(c.DataDir, "upgrade.json") }

func readMarker(c *Config) *upgradeMarker {
	b, err := os.ReadFile(markerPath(c))
	if err != nil {
		return nil
	}
	var m upgradeMarker
	if json.Unmarshal(b, &m) != nil {
		return nil
	}
	return &m
}

func writeMarker(c *Config, m *upgradeMarker) error {
	b, _ := json.Marshal(m)
	return os.WriteFile(markerPath(c), b, 0o600)
}

func exePath() (string, error) {
	p, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(p)
}

func agentTarget() string { return fmt.Sprintf("gui/%d/%s", os.Getuid(), agentLabel) }

func (a *Agent) upgrade(ctx context.Context, p wire.AgentUpgradeParams, progress func(string)) (string, error) {
	if p.Version == "" {
		return "", errors.New("version is required")
	}
	if p.Version == a.version {
		return "already on " + p.Version, nil
	}
	as, ok := p.Assets[runtime.GOOS+"-"+runtime.GOARCH]
	if !ok {
		return "", fmt.Errorf("release %s has no build for %s-%s", p.Version, runtime.GOOS, runtime.GOARCH)
	}
	if !strings.HasPrefix(as.URL, "https://github.com/") || len(as.SHA256) != 64 {
		return "", errors.New("refusing a download that is not an https://github.com release with a checksum")
	}
	if exec.CommandContext(ctx, "launchctl", "print", agentTarget()).Run() != nil {
		return "", errors.New("the agent is not running under launchd, so it cannot restart itself; upgrade it with install.sh --upgrade")
	}
	exe, err := exePath()
	if err != nil {
		return "", err
	}
	tmp := exe + ".new"
	defer os.Remove(tmp)

	progress("downloading " + p.Version)
	if err := download(ctx, as.URL, tmp, as.SHA256); err != nil {
		return "", err
	}
	if err := os.Chmod(tmp, 0o755); err != nil {
		return "", err
	}
	progress("checking the new binary")
	vctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	out, err := exec.CommandContext(vctx, tmp, "version").Output()
	cancel()
	if err != nil {
		return "", fmt.Errorf("the new binary does not run: %w", err)
	}
	if got := strings.TrimSpace(string(out)); got != p.Version {
		return "", fmt.Errorf("the new binary reports %q, expected %q", got, p.Version)
	}

	prev := exe + ".prev"
	if err := copyFile(exe, prev, 0o755); err != nil {
		return "", fmt.Errorf("keeping the old binary: %w", err)
	}
	if err := writeMarker(a.cfg, &upgradeMarker{From: a.version, To: p.Version}); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, exe); err != nil {
		os.Remove(markerPath(a.cfg))
		return "", err
	}
	progress("installed " + p.Version + ", restarting")
	// The result is sent before this fires.
	time.AfterFunc(3*time.Second, func() {
		if err := exec.Command("launchctl", "kickstart", "-k", agentTarget()).Run(); err != nil {
			log.Printf("upgrade: restart failed: %v", err)
		}
	})
	return fmt.Sprintf("upgraded %s to %s; the agent is restarting", a.version, p.Version), nil
}

func download(ctx context.Context, url, dst, wantSHA string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("download failed: %s", resp.Status)
	}
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(resp.Body, 256<<20))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if n == 0 {
		return errors.New("empty download")
	}
	if got := hex.EncodeToString(h.Sum(nil)); !strings.EqualFold(got, wantSHA) {
		return fmt.Errorf("checksum mismatch: got %s, expected %s", got, wantSHA)
	}
	return nil
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}

// startUpgradeWatch runs at startup. After an upgrade it gives the new process
// a few attempts and a few minutes to connect before putting the old binary back.
func (a *Agent) startUpgradeWatch() {
	m := readMarker(a.cfg)
	if m == nil {
		return
	}
	if m.To != a.version {
		// A rolled-back or unrelated marker.
		os.Remove(markerPath(a.cfg))
		return
	}
	m.Attempts++
	_ = writeMarker(a.cfg, m)
	if m.Attempts > 3 {
		a.rollback("the new version failed to start repeatedly")
		return
	}
	time.AfterFunc(3*time.Minute, func() {
		if readMarker(a.cfg) != nil {
			a.rollback("the new version could not reach the server")
		}
	})
}

// upgradeOK is called once the agent has connected and said hello.
func (a *Agent) upgradeOK() {
	if m := readMarker(a.cfg); m != nil && m.To == a.version {
		os.Remove(markerPath(a.cfg))
		log.Printf("upgrade from %s confirmed", m.From)
	}
}

func (a *Agent) rollback(why string) {
	exe, err := exePath()
	if err != nil {
		return
	}
	log.Printf("upgrade: %s; restoring the previous binary", why)
	os.Remove(markerPath(a.cfg))
	if err := os.Rename(exe+".prev", exe); err != nil {
		log.Printf("upgrade: restore failed: %v", err)
		return
	}
	// launchd KeepAlive starts the restored binary.
	os.Exit(1)
}
