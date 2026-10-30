package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/raghavharness/mac-fleet/internal/wire"
)

func runArgv(ctx context.Context, argv []string) error {
	if len(argv) == 0 {
		return errors.New("command is not configured")
	}
	var out bytes.Buffer
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = childEnv()
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s: %w: %s", argv[0], err, strings.TrimSpace(out.String()))
	}
	return nil
}

// waitReady polls the local Nomad API until the node reports ready.
func waitReady(ctx context.Context, c *Config, wait time.Duration, progress func(string)) error {
	deadline := time.Now().Add(wait)
	// Give the process a moment to go down before the first check.
	time.Sleep(3 * time.Second)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		invalidateNomadInfo()
		st := nomadState(ctx, c)
		if st.Reachable && st.Status == "ready" {
			return nil
		}
		progress("waiting for node to rejoin as ready...")
		time.Sleep(5 * time.Second)
	}
	return fmt.Errorf("node did not become ready within %s", wait)
}

// ---- launchd ----

func launchdTarget(c *Config) string { return fmt.Sprintf("gui/%d/%s", os.Getuid(), c.NomadLabel) }
func launchdDomain() string          { return fmt.Sprintf("gui/%d", os.Getuid()) }

func plistPath(c *Config) string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "LaunchAgents", c.NomadLabel+".plist")
}

// launchdLoaded reports whether the Nomad job is loaded in the user's domain.
func launchdLoaded(ctx context.Context, c *Config) bool {
	return exec.CommandContext(ctx, "launchctl", "print", launchdTarget(c)).Run() == nil
}

const errNotLaunchd = "nomad is not managed by launchd (it was started from a shell); use \"Manage with launchd\" first"

func nomadRestart(ctx context.Context, c *Config, p wire.NomadRestartParams, progress func(string)) error {
	progress("restarting nomad client")
	if len(c.NomadRestartCmd) > 0 {
		if err := runArgv(ctx, c.NomadRestartCmd); err != nil {
			return err
		}
	} else {
		if !launchdLoaded(ctx, c) {
			return errors.New(errNotLaunchd)
		}
		if err := runArgv(ctx, []string{"launchctl", "kickstart", "-k", launchdTarget(c)}); err != nil {
			return err
		}
	}
	wait := time.Duration(p.WaitReadySec) * time.Second
	if wait <= 0 {
		wait = 120 * time.Second
	}
	return waitReady(ctx, c, wait, progress)
}

func nomadStop(ctx context.Context, c *Config, progress func(string)) error {
	progress("stopping nomad client")
	if len(c.NomadStopCmd) > 0 {
		return runArgv(ctx, c.NomadStopCmd)
	}
	if !launchdLoaded(ctx, c) {
		return errors.New(errNotLaunchd)
	}
	// bootout unloads the job, so KeepAlive does not bring it back.
	return runArgv(ctx, []string{"launchctl", "bootout", launchdTarget(c)})
}

func nomadStart(ctx context.Context, c *Config, progress func(string)) error {
	progress("starting nomad client")
	if len(c.NomadStartCmd) > 0 {
		if err := runArgv(ctx, c.NomadStartCmd); err != nil {
			return err
		}
	} else if launchdLoaded(ctx, c) {
		if err := runArgv(ctx, []string{"launchctl", "kickstart", launchdTarget(c)}); err != nil {
			return err
		}
	} else {
		if _, err := os.Stat(plistPath(c)); err != nil {
			return errors.New(errNotLaunchd)
		}
		if err := runArgv(ctx, []string{"launchctl", "bootstrap", launchdDomain(), plistPath(c)}); err != nil {
			return err
		}
	}
	return waitReady(ctx, c, 120*time.Second, progress)
}

func xmlEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

func buildPlist(label, bin string, args []string, path, logPath string) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>` + xmlEscape(label) + `</string>
  <key>ProgramArguments</key>
  <array>
    <string>` + xmlEscape(bin) + `</string>
`)
	for _, a := range args {
		b.WriteString("    <string>" + xmlEscape(a) + "</string>\n")
	}
	b.WriteString(`  </array>
  <key>EnvironmentVariables</key>
  <dict><key>PATH</key><string>` + xmlEscape(path) + `</string></dict>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>StandardOutPath</key><string>` + xmlEscape(logPath) + `</string>
  <key>StandardErrorPath</key><string>` + xmlEscape(logPath) + `</string>
</dict>
</plist>
`)
	return b.String()
}

func pidAlive(pid int) bool { return syscall.Kill(pid, 0) == nil }

// nomadAdopt moves a Nomad that was started from a shell under launchd, with
// the same command line, so restart/stop/start work through launchctl. The old
// process is stopped gracefully (never killed); if the new job does not come up
// ready, the original command is started again.
func nomadAdopt(ctx context.Context, c *Config, progress func(string)) error {
	if len(c.NomadRestartCmd) > 0 {
		return errors.New("custom nomad commands are configured; launchd is not used")
	}
	if launchdLoaded(ctx, c) {
		return errors.New("nomad is already managed by launchd")
	}
	invalidateNomadInfo()
	info := discoverNomad(ctx, c)
	p := info.Proc
	if p == nil {
		return errors.New("no running `nomad agent` process found to take over")
	}
	progress(fmt.Sprintf("found nomad pid %d: %s %s", p.Pid, p.Bin, strings.Join(p.Args, " ")))
	if _, err := os.Stat("/usr/bin/plutil"); err != nil {
		return errors.New("this needs macOS (plutil not found)")
	}
	path := p.Path
	if path == "" {
		path = strings.Join(extraPathDirs(), ":")
	}
	if err := os.MkdirAll(c.DataDir, 0o700); err != nil {
		return err
	}
	logPath := filepath.Join(c.DataDir, "nomad-launchd.log")
	plist := buildPlist(c.NomadLabel, p.Bin, p.Args, path, logPath)
	pp := plistPath(c)
	if err := os.MkdirAll(filepath.Dir(pp), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(pp, []byte(plist), 0o644); err != nil {
		return err
	}
	if err := runArgv(ctx, []string{"plutil", "-lint", pp}); err != nil {
		os.Remove(pp)
		return err
	}
	progress("wrote " + pp)

	progress(fmt.Sprintf("stopping pid %d gracefully", p.Pid))
	if err := syscall.Kill(p.Pid, syscall.SIGTERM); err != nil {
		os.Remove(pp)
		return fmt.Errorf("could not signal pid %d: %w", p.Pid, err)
	}
	deadline := time.Now().Add(60 * time.Second)
	for pidAlive(p.Pid) {
		if time.Now().After(deadline) {
			os.Remove(pp)
			return fmt.Errorf("pid %d did not exit within 60s; left running and unchanged", p.Pid)
		}
		select {
		case <-ctx.Done():
			os.Remove(pp)
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}

	rollback := func(cause error) error {
		progress("rolling back: " + cause.Error())
		_ = runArgv(ctx, []string{"launchctl", "bootout", launchdTarget(c)})
		os.Remove(pp)
		cmd := exec.Command(p.Bin, p.Args...)
		cmd.Env = append(os.Environ(), "PATH="+path)
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if err := cmd.Start(); err != nil {
			return fmt.Errorf("%w; restarting the original command also failed: %v", cause, err)
		}
		go cmd.Wait()
		if err := waitReady(ctx, c, 120*time.Second, progress); err != nil {
			return fmt.Errorf("%w; original command restarted but node is not ready: %v", cause, err)
		}
		return fmt.Errorf("%w; restored the original shell-started nomad", cause)
	}

	progress("loading launchd job " + c.NomadLabel)
	if err := runArgv(ctx, []string{"launchctl", "bootstrap", launchdDomain(), pp}); err != nil {
		return rollback(err)
	}
	if err := waitReady(ctx, c, 120*time.Second, progress); err != nil {
		return rollback(err)
	}
	progress("nomad is now managed by launchd")
	return nil
}

// ---- config ----

// nomadBinary prefers the binary of the running Nomad, so validation uses the
// same version, then the configured name searched on PATH and common dirs.
func nomadBinary(ctx context.Context, c *Config) string {
	if c.NomadBin != "" && c.NomadBin != "nomad" {
		return c.NomadBin
	}
	if p := discoverNomad(ctx, c).Proc; p != nil && p.Bin != "" {
		return p.Bin
	}
	return resolveBin("nomad")
}

func resolveNomadConfig(ctx context.Context, c *Config) (string, error) {
	invalidateNomadInfo()
	info := discoverNomad(ctx, c)
	if info.ConfigPath == "" {
		return "", errors.New("could not find the nomad config file; set nomad_config in the agent config")
	}
	return info.ConfigPath, nil
}

func writable(path string) bool {
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return false
	}
	f.Close()
	return true
}

func nomadConfigRead(ctx context.Context, c *Config) (wire.NomadConfigData, error) {
	path, err := resolveNomadConfig(ctx, c)
	if err != nil {
		return wire.NomadConfigData{}, err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return wire.NomadConfigData{}, err
	}
	d := wire.NomadConfigData{Path: path, Content: string(b), Writable: writable(path)}
	if st, err := os.Stat(path); err == nil {
		d.ModTime = st.ModTime().UTC().Format(time.RFC3339)
	}
	return d, nil
}

// backupConfig copies the current file into the agent data dir and keeps the
// newest ten.
func backupConfig(c *Config, content []byte) (string, error) {
	dir := filepath.Join(c.DataDir, "backups")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	name := filepath.Join(dir, "nomad-"+time.Now().UTC().Format("20060102-150405")+".hcl")
	if err := os.WriteFile(name, content, 0o600); err != nil {
		return "", err
	}
	ents, _ := filepath.Glob(filepath.Join(dir, "nomad-*.hcl"))
	sort.Strings(ents)
	for len(ents) > 10 {
		os.Remove(ents[0])
		ents = ents[1:]
	}
	return name, nil
}

// installConfig writes content over path. It writes in place when it can, so
// ownership and mode (often set by a provisioning tool) are kept, and falls
// back to an atomic replace when only the directory is writable.
func installConfig(path string, content []byte) error {
	if writable(path) {
		return os.WriteFile(path, content, 0o644)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".nomad-config-*.hcl")
	if err != nil {
		return fmt.Errorf("%s is not writable by this user; change its permissions or edit it with your provisioning tool: %w", path, err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(content); err != nil {
		tmp.Close()
		return err
	}
	tmp.Close()
	if st, err := os.Stat(path); err == nil {
		os.Chmod(tmp.Name(), st.Mode().Perm())
	}
	return os.Rename(tmp.Name(), path)
}

// nomadConfigApply validates the new config, keeps a backup, installs it,
// optionally restarts through launchd, and rolls back if the node does not
// come back ready.
func nomadConfigApply(ctx context.Context, c *Config, p wire.NomadConfigParams, progress func(string)) (string, json.RawMessage, error) {
	if strings.TrimSpace(p.Content) == "" {
		return "", nil, errors.New("config content is empty")
	}
	path, err := resolveNomadConfig(ctx, c)
	if err != nil {
		return "", nil, err
	}
	if err := os.MkdirAll(c.DataDir, 0o700); err != nil {
		return "", nil, err
	}
	tmp, err := os.CreateTemp(c.DataDir, ".nomad-validate-*.hcl")
	if err != nil {
		return "", nil, err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(p.Content); err != nil {
		tmp.Close()
		return "", nil, err
	}
	tmp.Close()

	progress("validating config")
	if err := runArgv(ctx, []string{nomadBinary(ctx, c), "config", "validate", tmp.Name()}); err != nil {
		return "", nil, fmt.Errorf("validation failed: %w", err)
	}
	if p.ValidateOnly {
		return "config is valid", nil, nil
	}

	prev, readErr := os.ReadFile(path)
	if readErr == nil {
		bk, err := backupConfig(c, prev)
		if err != nil {
			return "", nil, fmt.Errorf("write backup: %w", err)
		}
		progress("previous config saved to " + bk)
	}
	if err := installConfig(path, []byte(p.Content)); err != nil {
		return "", nil, err
	}
	progress("config written to " + path)
	data, _ := json.Marshal(wire.NomadConfigData{Path: path, Content: p.Content, Writable: writable(path)})
	if !p.Restart {
		return "config saved", data, nil
	}
	if len(c.NomadRestartCmd) == 0 && !launchdLoaded(ctx, c) {
		progress("restart skipped: " + errNotLaunchd)
		return "config saved; restart skipped because nomad is not managed by launchd", data, nil
	}

	err = nomadRestart(ctx, c, wire.NomadRestartParams{WaitReadySec: p.WaitReadySec}, progress)
	if err == nil {
		return "config applied and nomad restarted", data, nil
	}
	if readErr != nil {
		return "", nil, fmt.Errorf("restart failed and there was no previous config to restore: %w", err)
	}
	progress("restart failed, rolling back: " + err.Error())
	if werr := installConfig(path, prev); werr != nil {
		return "", nil, fmt.Errorf("rollback write failed: %w (original error: %v)", werr, err)
	}
	if rerr := nomadRestart(ctx, c, wire.NomadRestartParams{WaitReadySec: p.WaitReadySec}, progress); rerr != nil {
		return "", nil, fmt.Errorf("rolled back but node still not ready: %w", rerr)
	}
	return "", nil, fmt.Errorf("new config failed, rolled back to previous: %w", err)
}
