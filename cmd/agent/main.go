// macfleet-agent runs on each Mac host as the same non-root user that runs
// Nomad and Tart. It dials out to the control plane and never listens.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/raghavharness/mac-fleet/internal/agent"
)

var version = "dev"

func usage() {
	fmt.Fprintf(os.Stderr, `macfleet-agent %s

usage:
  macfleet-agent enroll --server https://HOST:8443 --ca-sha256 HEX --token TOKEN [--config PATH]
  macfleet-agent run [--config PATH]
  macfleet-agent install-launchd [--config PATH]   write a LaunchAgent plist and print the load command
  macfleet-agent version
`, version)
}

func defaultConfig() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".macfleet", "agent.json")
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	cfgPath := fs.String("config", defaultConfig(), "agent config file")
	server := fs.String("server", "", "server URL (enroll)")
	pin := fs.String("ca-sha256", "", "SHA-256 of the server CA (enroll)")
	token := fs.String("token", "", "one-time enrollment token (enroll)")
	host := fs.String("hostname", "", "host name to register (enroll, defaults to the OS host name)")
	fs.Parse(args)

	switch cmd {
	case "version":
		fmt.Println(version)
	case "enroll":
		if *server == "" || *pin == "" || *token == "" {
			usage()
			os.Exit(2)
		}
		cfg := &agent.Config{Server: *server, CAPin: *pin, Hostname: *host}
		if existing, err := agent.LoadConfig(*cfgPath); err == nil {
			cfg = existing
			cfg.Server, cfg.CAPin = *server, *pin
			if *host != "" {
				cfg.Hostname = *host
			}
		}
		cfg.Defaults()
		if err := agent.Enroll(cfg, strings.TrimSpace(*token)); err != nil {
			log.Fatalf("enroll: %v", err)
		}
		if err := cfg.Save(*cfgPath); err != nil {
			log.Fatalf("save config: %v", err)
		}
		fmt.Printf("enrolled as %s; config written to %s\n", cfg.Hostname, *cfgPath)
	case "run":
		cfg, err := agent.LoadConfig(*cfgPath)
		if err != nil {
			log.Fatalf("load config: %v (run enroll first)", err)
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		log.Printf("macfleet-agent %s starting for %s", version, cfg.Hostname)
		if err := agent.New(cfg, *cfgPath, version).Run(ctx); err != nil {
			log.Fatal(err)
		}
	case "install-launchd":
		installLaunchd(*cfgPath)
	default:
		usage()
		os.Exit(2)
	}
}

func installLaunchd(cfgPath string) {
	exe, err := os.Executable()
	if err != nil {
		log.Fatal(err)
	}
	home, _ := os.UserHomeDir()
	label := "io.harness.macfleet-agent"
	plist := filepath.Join(home, "Library", "LaunchAgents", label+".plist")
	body := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
<key>Label</key><string>%s</string>
<key>ProgramArguments</key><array><string>%s</string><string>run</string><string>--config</string><string>%s</string></array>
<key>RunAtLoad</key><true/>
<key>KeepAlive</key><true/>
<key>StandardOutPath</key><string>%s/.macfleet/agent.log</string>
<key>StandardErrorPath</key><string>%s/.macfleet/agent.log</string>
</dict></plist>
`, label, exe, cfgPath, home, home)
	if err := os.MkdirAll(filepath.Dir(plist), 0o755); err != nil {
		log.Fatal(err)
	}
	if err := os.WriteFile(plist, []byte(body), 0o644); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("wrote %s\nload it with:\n  launchctl bootstrap gui/$(id -u) %s\n", plist, plist)
}
