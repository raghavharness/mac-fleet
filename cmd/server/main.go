// macfleet-server is the control plane: dashboard, REST API and the mTLS
// listener that agents connect to.
package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/raghavharness/mac-fleet/internal/server"
)

func main() {
	cfg, err := server.LoadConfig()
	if err != nil {
		log.Fatal(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	s, err := server.New(ctx, cfg)
	if err != nil {
		log.Fatal(err)
	}
	if err := s.Run(ctx); err != nil {
		log.Fatal(err)
	}
}
