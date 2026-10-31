package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/raghavharness/mac-fleet/internal/wire"
)

// Agent keeps one outbound WebSocket to the server, sends metrics and
// inventory, and executes allowlisted commands one at a time.
type Agent struct {
	cfg     *Config
	cfgPath string
	version string

	mu     sync.Mutex // guards images
	images []wire.Image
}

func New(cfg *Config, cfgPath, version string) *Agent {
	return &Agent{cfg: cfg, cfgPath: cfgPath, version: version}
}

// Run connects and reconnects until ctx is cancelled.
func (a *Agent) Run(ctx context.Context) error {
	a.startUpgradeWatch()
	go a.certRenewLoop(ctx)
	backoff := time.Second
	for ctx.Err() == nil {
		start := time.Now()
		err := a.session(ctx)
		if ctx.Err() != nil {
			return nil
		}
		log.Printf("disconnected: %v", err)
		if time.Since(start) > time.Minute {
			backoff = time.Second
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff):
		}
		if backoff < time.Minute {
			backoff *= 2
		}
	}
	return nil
}

func (a *Agent) certRenewLoop(ctx context.Context) {
	t := time.NewTicker(6 * time.Hour)
	defer t.Stop()
	for {
		if exp, err := CertExpiry(a.cfg); err == nil && time.Until(exp) < 30*24*time.Hour {
			if err := Renew(a.cfg); err != nil {
				log.Printf("certificate renewal failed: %v", err)
			} else {
				log.Printf("certificate renewed, takes effect on next reconnect")
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (a *Agent) session(ctx context.Context) error {
	tc, err := TLSConfig(a.cfg)
	if err != nil {
		return err
	}
	u, err := url.Parse(a.cfg.Server)
	if err != nil {
		return err
	}
	u.Scheme = "wss"
	u.Path = "/v1/agent"
	dialer := websocket.Dialer{TLSClientConfig: tc, HandshakeTimeout: 15 * time.Second}
	conn, _, err := dialer.DialContext(ctx, u.String(), http.Header{})
	if err != nil {
		return err
	}
	defer conn.Close()
	log.Printf("connected to %s", u.Host)

	var wmu sync.Mutex
	send := func(typ, id string, payload any) error {
		b, err := wire.Marshal(typ, id, payload)
		if err != nil {
			return err
		}
		wmu.Lock()
		defer wmu.Unlock()
		conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
		return conn.WriteMessage(websocket.TextMessage, b)
	}
	if err := send(wire.TypeHello, "", hello(a.cfg, a.version)); err != nil {
		return err
	}
	a.upgradeOK()

	sctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Sample loop.
	go func() {
		a.sendInventory(sctx, send)
		a.sendMetrics(sctx, send)
		mt := time.NewTicker(time.Duration(a.cfg.MetricsEverySec) * time.Second)
		it := time.NewTicker(time.Duration(a.cfg.InventoryEvery) * time.Second)
		defer mt.Stop()
		defer it.Stop()
		for {
			select {
			case <-sctx.Done():
				return
			case <-mt.C:
				a.sendMetrics(sctx, send)
			case <-it.C:
				a.sendInventory(sctx, send)
			}
		}
	}()

	// Command worker: strictly one command at a time.
	cmds := make(chan wire.Command, 32)
	go func() {
		for {
			select {
			case <-sctx.Done():
				return
			case cmd := <-cmds:
				a.execute(sctx, cmd, send)
			}
		}
	}()

	// Server pings keep the connection alive; we also bound reads.
	conn.SetReadDeadline(time.Now().Add(90 * time.Second))
	conn.SetPongHandler(func(string) error { conn.SetReadDeadline(time.Now().Add(90 * time.Second)); return nil })
	conn.SetPingHandler(func(d string) error {
		conn.SetReadDeadline(time.Now().Add(90 * time.Second))
		wmu.Lock()
		defer wmu.Unlock()
		return conn.WriteControl(websocket.PongMessage, []byte(d), time.Now().Add(5*time.Second))
	})
	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			return err
		}
		conn.SetReadDeadline(time.Now().Add(90 * time.Second))
		var env wire.Envelope
		if json.Unmarshal(data, &env) != nil {
			continue
		}
		if env.Type == wire.TypeSettings {
			var st wire.Settings
			if json.Unmarshal(env.Payload, &st) == nil {
				setRuntimeToken(st.NomadToken)
			}
			continue
		}
		if env.Type == wire.TypeCommand {
			var cmd wire.Command
			if json.Unmarshal(env.Payload, &cmd) != nil {
				continue
			}
			select {
			case cmds <- cmd:
			default:
				send(wire.TypeResult, cmd.ID, wire.Result{CommandID: cmd.ID, State: wire.StateFailed, Message: "agent command queue is full"})
			}
		}
	}
}

type sender func(typ, id string, payload any) error

func (a *Agent) sendInventory(ctx context.Context, send sender) {
	imgs, err := listImages(ctx, a.cfg)
	if err != nil {
		log.Printf("inventory: %v", err)
		send(wire.TypeInventory, "", wire.Inventory{Error: err.Error()})
		return
	}
	a.mu.Lock()
	a.images = imgs
	a.mu.Unlock()
	send(wire.TypeInventory, "", wire.Inventory{Images: imgs})
}

func (a *Agent) sendMetrics(ctx context.Context, send sender) {
	a.mu.Lock()
	imgs := a.images
	a.mu.Unlock()
	send(wire.TypeMetrics, "", collect(ctx, a.cfg, imgs))
}

func (a *Agent) execute(ctx context.Context, cmd wire.Command, send sender) {
	progress := func(line string) {
		send(wire.TypeProgress, cmd.ID, wire.Progress{CommandID: cmd.ID, Line: line})
	}
	send(wire.TypeResult, cmd.ID, wire.Result{CommandID: cmd.ID, State: wire.StateRunning})

	cctx, cancel := context.WithTimeout(ctx, 2*time.Hour)
	defer cancel()
	msg, data, err := a.dispatch(cctx, cmd, progress)
	res := wire.Result{CommandID: cmd.ID, State: wire.StateSucceeded, Message: msg, Data: data}
	if err != nil {
		res.State, res.Message = wire.StateFailed, err.Error()
	}
	send(wire.TypeResult, cmd.ID, res)
	if strings.HasPrefix(cmd.Type, "tart.") {
		a.sendInventory(ctx, send)
	}
}

func (a *Agent) dispatch(ctx context.Context, cmd wire.Command, progress func(string)) (string, json.RawMessage, error) {
	plain := func(msg string, err error) (string, json.RawMessage, error) { return msg, nil, err }
	switch cmd.Type {
	case wire.CmdNomadRestart:
		var p wire.NomadRestartParams
		_ = json.Unmarshal(cmd.Params, &p)
		return plain("restarted", nomadRestart(ctx, a.cfg, p, progress))
	case wire.CmdNomadStop:
		return plain("stopped", nomadStop(ctx, a.cfg, progress))
	case wire.CmdNomadStart:
		return plain("started", nomadStart(ctx, a.cfg, progress))
	case wire.CmdNomadDrain, wire.CmdNomadEligibility, wire.CmdNomadPing:
		var p wire.NomadAPIParams
		if err := json.Unmarshal(cmd.Params, &p); err != nil {
			return "", nil, err
		}
		switch cmd.Type {
		case wire.CmdNomadDrain:
			return plain(nomadDrain(ctx, a.cfg, p, progress))
		case wire.CmdNomadEligibility:
			return plain(nomadEligibility(ctx, a.cfg, p))
		}
		return plain(nomadPing(ctx, a.cfg, p))
	case wire.CmdAgentUpgrade:
		var p wire.AgentUpgradeParams
		if err := json.Unmarshal(cmd.Params, &p); err != nil {
			return "", nil, err
		}
		return plain(a.upgrade(ctx, p, progress))
	case wire.CmdNomadAdopt:
		return plain("nomad is now managed by launchd", nomadAdopt(ctx, a.cfg, progress))
	case wire.CmdNomadConfigRead:
		d, err := nomadConfigRead(ctx, a.cfg)
		if err != nil {
			return "", nil, err
		}
		b, _ := json.Marshal(d)
		return "read " + d.Path, b, nil
	case wire.CmdNomadConfigApply:
		var p wire.NomadConfigParams
		if err := json.Unmarshal(cmd.Params, &p); err != nil {
			return "", nil, err
		}
		return nomadConfigApply(ctx, a.cfg, p, progress)
	case wire.CmdTartList:
		imgs, err := listImages(ctx, a.cfg)
		return plain(fmt.Sprintf("%d images", len(imgs)), err)
	case wire.CmdTartPull:
		var p wire.TartImageParams
		if err := json.Unmarshal(cmd.Params, &p); err != nil {
			return "", nil, err
		}
		return plain("pulled "+p.Image, tartPull(ctx, a.cfg, p.Image, progress))
	case wire.CmdTartDelete:
		var p wire.TartImageParams
		if err := json.Unmarshal(cmd.Params, &p); err != nil {
			return "", nil, err
		}
		return plain("deleted "+p.Image, tartDelete(ctx, a.cfg, p.Image))
	case wire.CmdTartPrune:
		var p wire.TartPruneParams
		if err := json.Unmarshal(cmd.Params, &p); err != nil {
			return "", nil, err
		}
		return plain(tartPrune(ctx, a.cfg, p, progress))
	}
	return "", nil, errors.New("unsupported command type " + cmd.Type)
}
