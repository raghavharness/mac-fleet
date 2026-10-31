package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/raghavharness/mac-fleet/internal/wire"
)

// The server usually cannot reach a cluster's Nomad API (it sits behind a VPN),
// but the Macs can. Nomad API operations therefore run on an agent: the
// target's own when it is online, otherwise any online host in the same
// cluster. Credentials stay on the server and travel with the command only.

// clusterOf returns the host's cluster, or the only cluster if there is one.
func (s *Server) clusterOf(ctx context.Context, hostID string) (string, error) {
	var cid string
	if err := s.st.pool.QueryRow(ctx, `SELECT cluster_id FROM hosts WHERE id=$1`, hostID).Scan(&cid); err != nil {
		return "", err
	}
	if cid == "" {
		cid = s.onlyCluster(ctx)
	}
	if cid == "" {
		return "", errors.New("this host has no Nomad cluster; assign one on the host page or add a cluster in Settings")
	}
	return cid, nil
}

type executor struct {
	ID, Name string
}

// pickExecutor prefers the preferred host, then any other connected host in
// the cluster.
func (s *Server) pickExecutor(ctx context.Context, clusterID, prefer string) (*executor, error) {
	rows, err := s.st.pool.Query(ctx, `SELECT id, name FROM hosts
		WHERE connected AND NOT revoked AND ($1='' OR cluster_id=$1 OR id=$2)
		ORDER BY (id=$2) DESC, last_seen DESC NULLS LAST`, clusterID, prefer)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var e executor
		if rows.Scan(&e.ID, &e.Name) != nil {
			continue
		}
		if s.hub.get(e.ID) != nil {
			return &e, nil
		}
	}
	return nil, errors.New("no online Mac in this cluster can run it")
}

func (s *Server) runNomadOp(ctx context.Context, c *Command) {
	fail := func(err error) {
		s.addEvent(ctx, c.ID, "error: "+err.Error())
		s.setCommandState(ctx, c.ID, wire.StateFailed, err.Error())
	}
	cid, err := s.clusterOf(ctx, c.HostID)
	if err != nil {
		fail(err)
		return
	}
	token, err := s.st.getSecret(ctx, secretKey(cid, "token"))
	if err != nil {
		fail(err)
		return
	}
	var name string
	var sample []byte
	if err := s.st.pool.QueryRow(ctx, `SELECT name, last_sample FROM hosts WHERE id=$1`, c.HostID).Scan(&name, &sample); err != nil {
		fail(err)
		return
	}
	var p wire.NomadAPIParams
	_ = json.Unmarshal(c.Params, &p)
	p.Token, p.NodeName = token, name
	if sample != nil {
		var sm wire.Sample
		if json.Unmarshal(sample, &sm) == nil {
			p.NodeID = sm.Nomad.NodeID
		}
	}
	ex, err := s.pickExecutor(ctx, cid, c.HostID)
	if err != nil {
		fail(err)
		return
	}
	if ex.ID != c.HostID {
		s.addEvent(ctx, c.ID, fmt.Sprintf("%s is not connected; running through %s in the same cluster", name, ex.Name))
	} else {
		s.addEvent(ctx, c.ID, "running on "+ex.Name)
	}
	payload, _ := json.Marshal(p)
	s.setCommandState(ctx, c.ID, wire.StateRunning, "")
	if !s.sendToAgent(ex.ID, wire.Command{ID: c.ID, Type: c.Type, Params: payload}) {
		fail(errors.New("could not reach the agent on " + ex.Name))
	}
}

// pingViaHost checks a Nomad connection from a Mac. The check is not kept in
// the command history.
func (s *Server) pingViaHost(ctx context.Context, hostID, hostName, token string) (string, error) {
	id := randID()
	if _, err := s.st.pool.Exec(ctx, `INSERT INTO commands(id,type,host_id,params,state,requested_by,expires_at)
		VALUES($1,$2,$3,'{}','running','connection test',now()+interval '2 minutes')`, id, wire.CmdNomadPing, hostID); err != nil {
		return "", err
	}
	defer func() {
		s.st.pool.Exec(context.Background(), `DELETE FROM command_events WHERE command_id=$1`, id)
		s.st.pool.Exec(context.Background(), `DELETE FROM commands WHERE id=$1`, id)
	}()
	payload, _ := json.Marshal(wire.NomadAPIParams{Token: token})
	if !s.sendToAgent(hostID, wire.Command{ID: id, Type: wire.CmdNomadPing, Params: payload}) {
		return "", errors.New("could not reach the agent on " + hostName)
	}
	end := time.Now().Add(25 * time.Second)
	for time.Now().Before(end) {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(400 * time.Millisecond):
		}
		var state, msg string
		if s.st.pool.QueryRow(ctx, `SELECT state, message FROM commands WHERE id=$1`, id).Scan(&state, &msg) != nil {
			continue
		}
		switch state {
		case wire.StateSucceeded:
			return msg, nil
		case wire.StateFailed:
			return "", errors.New(msg)
		}
	}
	return "", errors.New("timed out waiting for " + hostName)
}

// pushSettings sends the host's cluster token to its agent, which keeps it in
// memory to read node state.
func (s *Server) pushSettings(ctx context.Context, hostID string) {
	ac := s.hub.get(hostID)
	if ac == nil {
		return
	}
	var tok string
	if cid, err := s.clusterOf(ctx, hostID); err == nil {
		tok, _ = s.st.getSecret(ctx, secretKey(cid, "token"))
	}
	b, err := wire.Marshal(wire.TypeSettings, "", wire.Settings{NomadToken: tok})
	if err != nil {
		return
	}
	select {
	case ac.send <- b:
	default:
	}
}

// pushClusterSettings refreshes every connected host of a cluster.
func (s *Server) pushClusterSettings(ctx context.Context, clusterID string) {
	rows, err := s.st.pool.Query(ctx, `SELECT id FROM hosts WHERE connected AND NOT revoked AND (cluster_id=$1 OR cluster_id='')`, clusterID)
	if err != nil {
		return
	}
	var ids []string
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	for _, id := range ids {
		s.pushSettings(ctx, id)
	}
}
