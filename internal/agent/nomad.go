package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/raghavharness/mac-fleet/internal/wire"
)

func runArgv(ctx context.Context, argv []string) error {
	if len(argv) == 0 {
		return errors.New("command is not configured")
	}
	var out bytes.Buffer
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
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
		st := nomadState(ctx, c)
		if st.Reachable && st.Status == "ready" {
			return nil
		}
		progress("waiting for node to rejoin as ready...")
		time.Sleep(5 * time.Second)
	}
	return fmt.Errorf("node did not become ready within %s", wait)
}

func nomadRestart(ctx context.Context, c *Config, p wire.NomadRestartParams, progress func(string)) error {
	progress("restarting nomad client")
	if err := runArgv(ctx, c.NomadRestartCmd); err != nil {
		return err
	}
	wait := time.Duration(p.WaitReadySec) * time.Second
	if wait <= 0 {
		wait = 120 * time.Second
	}
	return waitReady(ctx, c, wait, progress)
}

func nomadStop(ctx context.Context, c *Config, progress func(string)) error {
	progress("stopping nomad client")
	return runArgv(ctx, c.NomadStopCmd)
}

func nomadStart(ctx context.Context, c *Config, progress func(string)) error {
	progress("starting nomad client")
	if err := runArgv(ctx, c.NomadStartCmd); err != nil {
		return err
	}
	return waitReady(ctx, c, 120*time.Second, progress)
}

// nomadConfigApply validates the new config, swaps it in atomically, keeps the
// previous file as a backup, optionally restarts, and rolls back if the node
// does not come back ready.
func nomadConfigApply(ctx context.Context, c *Config, p wire.NomadConfigParams, progress func(string)) error {
	if strings.TrimSpace(p.Content) == "" {
		return errors.New("config content is empty")
	}
	dir := filepath.Dir(c.NomadConfig)
	tmp, err := os.CreateTemp(dir, ".nomad-config-*.hcl")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(p.Content); err != nil {
		tmp.Close()
		return err
	}
	tmp.Close()

	progress("validating config")
	if err := runArgv(ctx, []string{c.NomadBin, "config", "validate", tmp.Name()}); err != nil {
		return fmt.Errorf("validation failed: %w", err)
	}

	backup := c.NomadConfig + ".bak"
	prev, readErr := os.ReadFile(c.NomadConfig)
	if readErr == nil {
		if err := os.WriteFile(backup, prev, 0o600); err != nil {
			return fmt.Errorf("write backup: %w", err)
		}
	}
	if err := os.Rename(tmp.Name(), c.NomadConfig); err != nil {
		return fmt.Errorf("install config: %w", err)
	}
	progress("config written, previous version kept as " + filepath.Base(backup))
	if !p.Restart {
		return nil
	}

	err = nomadRestart(ctx, c, wire.NomadRestartParams{WaitReadySec: p.WaitReadySec}, progress)
	if err == nil {
		return nil
	}
	if readErr != nil {
		return fmt.Errorf("restart failed and there was no previous config to restore: %w", err)
	}
	progress("restart failed, rolling back: " + err.Error())
	if werr := os.WriteFile(c.NomadConfig, prev, 0o600); werr != nil {
		return fmt.Errorf("rollback write failed: %w (original error: %v)", werr, err)
	}
	if rerr := nomadRestart(ctx, c, wire.NomadRestartParams{WaitReadySec: p.WaitReadySec}, progress); rerr != nil {
		return fmt.Errorf("rolled back but node still not ready: %w", rerr)
	}
	return fmt.Errorf("new config failed, rolled back to previous: %w", err)
}
