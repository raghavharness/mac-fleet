package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"regexp"
	"time"

	"github.com/raghavharness/mac-fleet/internal/wire"
)

type Command struct {
	ID          string          `json:"id"`
	Type        string          `json:"type"`
	HostID      string          `json:"host_id"`
	HostName    string          `json:"host_name,omitempty"`
	Params      json.RawMessage `json:"params"`
	State       string          `json:"state"`
	Message     string          `json:"message"`
	Result      json.RawMessage `json:"result,omitempty"`
	RequestedBy string          `json:"requested_by"`
	BatchID     string          `json:"batch_id,omitempty"`
	CreatedAt   time.Time       `json:"created_at"`
	UpdatedAt   time.Time       `json:"updated_at"`
}

var imageNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/@-]{0,255}$`)

func agentCommand(t string) bool {
	switch t {
	case wire.CmdNomadRestart, wire.CmdNomadStop, wire.CmdNomadStart, wire.CmdNomadConfigApply, wire.CmdNomadConfigRead, wire.CmdNomadAdopt,
		wire.CmdTartList, wire.CmdTartPull, wire.CmdTartDelete, wire.CmdTartPrune:
		return true
	}
	return false
}

func serverCommand(t string) bool { return t == wire.CmdNomadDrain || t == wire.CmdNomadEligibility }

func knownCommand(t string) bool { return agentCommand(t) || serverCommand(t) }

// takesNodeOut lists commands that reduce schedulable capacity, which are
// subject to the blast-radius limit.
func takesNodeOut(t string, params json.RawMessage) bool {
	switch t {
	case wire.CmdNomadDrain, wire.CmdNomadStop, wire.CmdNomadRestart, wire.CmdNomadAdopt:
		return true
	case wire.CmdNomadConfigApply:
		var p wire.NomadConfigParams
		_ = json.Unmarshal(params, &p)
		return p.Restart && !p.ValidateOnly
	case wire.CmdNomadEligibility:
		var p struct {
			Eligible bool `json:"eligible"`
		}
		_ = json.Unmarshal(params, &p)
		return !p.Eligible
	}
	return false
}

func validateParams(t string, params json.RawMessage) error {
	switch t {
	case wire.CmdTartPull, wire.CmdTartDelete:
		var p wire.TartImageParams
		if json.Unmarshal(params, &p) != nil || p.Image == "" {
			return errors.New("params.image is required")
		}
		if !imageNameRe.MatchString(p.Image) {
			return fmt.Errorf("invalid image name %q", p.Image)
		}
	case wire.CmdTartPrune:
		var p wire.TartPruneParams
		if json.Unmarshal(params, &p) != nil || p.OlderThanDays < 1 {
			return errors.New("params.older_than_days must be at least 1")
		}
	case wire.CmdNomadConfigApply:
		var p wire.NomadConfigParams
		if json.Unmarshal(params, &p) != nil || p.Content == "" {
			return errors.New("params.content is required")
		}
	}
	return nil
}

// checkBlast refuses an action that would take more than max_out_pct of the
// fleet out of service at once, unless the caller overrides it.
func (s *Server) checkBlast(ctx context.Context, targets []string) error {
	st := s.st.loadSettings(ctx)
	hosts, err := s.listHosts(ctx)
	if err != nil {
		return err
	}
	total, out := 0, 0
	isOut := map[string]bool{}
	for _, h := range hosts {
		if h.Revoked {
			continue
		}
		total++
		if h.Status != "online" || h.Sample == nil || h.Sample.Nomad.Draining || h.Sample.Nomad.Eligibility == "ineligible" || h.Sample.Nomad.Status == "down" {
			out++
			isOut[h.ID] = true
		}
	}
	add := 0
	for _, id := range targets {
		if !isOut[id] {
			add++
		}
	}
	if total > 0 && (out+add)*100 > total*st.MaxOutPct {
		return fmt.Errorf("this would leave %d of %d hosts out of service (limit %d%%); pass override_limit to proceed", out+add, total, st.MaxOutPct)
	}
	return nil
}

type CreateOpts struct {
	Actor    string
	BatchID  string
	Override bool
	// SkipBlast is set by batches, which check each wave themselves.
	SkipBlast bool
}

func (s *Server) createCommand(ctx context.Context, hostID, typ string, params json.RawMessage, o CreateOpts) (*Command, error) {
	if !knownCommand(typ) {
		return nil, fmt.Errorf("unknown command type %q", typ)
	}
	if len(params) == 0 {
		params = json.RawMessage("{}")
	}
	if err := validateParams(typ, params); err != nil {
		return nil, err
	}
	var name string
	var revoked bool
	if err := s.st.pool.QueryRow(ctx, `SELECT name, revoked FROM hosts WHERE id=$1`, hostID).Scan(&name, &revoked); err != nil {
		return nil, errors.New("host not found")
	}
	if revoked {
		return nil, errors.New("host is revoked")
	}
	if !o.SkipBlast && !o.Override && takesNodeOut(typ, params) {
		if err := s.checkBlast(ctx, []string{hostID}); err != nil {
			return nil, err
		}
	}
	c := &Command{ID: randID(), Type: typ, HostID: hostID, HostName: name, Params: params, State: wire.StateQueued, RequestedBy: o.Actor, BatchID: o.BatchID, CreatedAt: time.Now(), UpdatedAt: time.Now()}
	ttl := time.Hour
	if _, err := s.st.pool.Exec(ctx, `INSERT INTO commands(id,type,host_id,params,state,requested_by,batch_id,expires_at)
		VALUES($1,$2,$3,$4,$5,$6,NULLIF($7,''),now()+$8::interval)`,
		c.ID, typ, hostID, params, c.State, o.Actor, o.BatchID, fmt.Sprintf("%d seconds", int(ttl.Seconds()))); err != nil {
		return nil, err
	}
	s.st.audit(ctx, o.Actor, "command.create", name, map[string]any{"command_id": c.ID, "type": typ})
	s.bus.publish("command", c.ID)
	if agentCommand(typ) {
		s.sendToAgent(hostID, wire.Command{ID: c.ID, Type: typ, Params: params})
	} else {
		go s.runServerCommand(context.Background(), c)
	}
	return c, nil
}

func terminal(state string) bool {
	return state == wire.StateSucceeded || state == wire.StateFailed || state == wire.StateExpired
}

func (s *Server) setCommandState(ctx context.Context, id, state, msg string, data ...json.RawMessage) {
	var res any
	if len(data) > 0 && len(data[0]) > 0 {
		res = []byte(data[0])
	}
	// Never move a command out of a terminal state.
	_, err := s.st.pool.Exec(ctx, `UPDATE commands SET state=$2, message=CASE WHEN $3='' THEN message ELSE $3 END,
		result=coalesce($4::jsonb, result), updated_at=now()
		WHERE id=$1 AND state NOT IN ('succeeded','failed','expired')`, id, state, msg, res)
	if err != nil {
		log.Printf("update command %s: %v", id, err)
		return
	}
	s.bus.publish("command", id)
}

func (s *Server) addEvent(ctx context.Context, id, line string) {
	if len(line) > 2000 {
		line = line[:2000]
	}
	s.st.pool.Exec(ctx, `INSERT INTO command_events(command_id, line) VALUES($1,$2)`, id, line)
	s.bus.publish("command", id)
}

// runServerCommand executes commands that use the Nomad API instead of an agent.
func (s *Server) runServerCommand(ctx context.Context, c *Command) {
	s.setCommandState(ctx, c.ID, wire.StateRunning, "")
	msg, err := s.execNomadCommand(ctx, c)
	if err != nil {
		s.addEvent(ctx, c.ID, "error: "+err.Error())
		s.setCommandState(ctx, c.ID, wire.StateFailed, err.Error())
		return
	}
	s.setCommandState(ctx, c.ID, wire.StateSucceeded, msg)
}

func (s *Server) nodeIDFor(ctx context.Context, nc *nomadClient, hostID string) (string, error) {
	var sample []byte
	var name string
	if err := s.st.pool.QueryRow(ctx, `SELECT name, last_sample FROM hosts WHERE id=$1`, hostID).Scan(&name, &sample); err != nil {
		return "", err
	}
	if sample != nil {
		var sm wire.Sample
		if json.Unmarshal(sample, &sm) == nil && sm.Nomad.NodeID != "" {
			return sm.Nomad.NodeID, nil
		}
	}
	return nc.findNodeByName(ctx, name)
}

func (s *Server) execNomadCommand(ctx context.Context, c *Command) (string, error) {
	nc, err := s.nomad(ctx)
	if err != nil {
		return "", err
	}
	id, err := s.nodeIDFor(ctx, nc, c.HostID)
	if err != nil {
		return "", err
	}
	switch c.Type {
	case wire.CmdNomadEligibility:
		var p struct {
			Eligible bool `json:"eligible"`
		}
		_ = json.Unmarshal(c.Params, &p)
		if err := nc.setEligibility(ctx, id, p.Eligible); err != nil {
			return "", err
		}
		if p.Eligible {
			return "node set eligible", nil
		}
		return "node set ineligible", nil
	case wire.CmdNomadDrain:
		var p struct {
			DeadlineMinutes int  `json:"deadline_minutes"`
			Force           bool `json:"force"`
		}
		_ = json.Unmarshal(c.Params, &p)
		deadline := time.Duration(p.DeadlineMinutes) * time.Minute
		if err := nc.drain(ctx, id, deadline, p.Force); err != nil {
			return "", err
		}
		s.addEvent(ctx, c.ID, "drain started")
		limit := deadline + 15*time.Minute
		if deadline == 0 {
			limit = 2 * time.Hour
		}
		return s.waitDrain(ctx, c.ID, nc, id, limit)
	}
	return "", errors.New("unsupported command")
}

func (s *Server) waitDrain(ctx context.Context, cmdID string, nc *nomadClient, nodeID string, limit time.Duration) (string, error) {
	end := time.Now().Add(limit)
	last := -1
	for time.Now().Before(end) {
		time.Sleep(5 * time.Second)
		nd, err := nc.node(ctx, nodeID)
		if err != nil {
			continue
		}
		if n, err := nc.runningAllocs(ctx, nodeID); err == nil && n != last {
			last = n
			s.addEvent(ctx, cmdID, fmt.Sprintf("allocations remaining: %d", n))
		}
		if !nd.Drain {
			return "drain complete", nil
		}
	}
	return "", errors.New("drain did not finish in time")
}

// janitor expires commands nobody picked up.
func (s *Server) janitor(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.st.pool.Exec(ctx, `UPDATE commands SET state='expired', message='host did not pick it up in time', updated_at=now()
				WHERE state='queued' AND expires_at < now()`)
			s.st.pool.Exec(ctx, `UPDATE commands SET state='failed', message='timed out', updated_at=now()
				WHERE state='running' AND updated_at < now() - interval '3 hours'`)
			// Hosts whose connection silently died.
			s.st.pool.Exec(ctx, `UPDATE hosts SET connected=false WHERE connected AND last_seen < now() - interval '3 minutes'`)
		}
	}
}
