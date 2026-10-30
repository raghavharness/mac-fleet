package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"time"

	"github.com/gorilla/websocket"
	"github.com/jackc/pgx/v5"
	"github.com/raghavharness/mac-fleet/internal/wire"
)

type enrollReq struct {
	Token    string `json:"token"`
	Hostname string `json:"hostname"`
	CSR      string `json:"csr"`
}

type enrollResp struct {
	Cert string `json:"cert"`
	CA   string `json:"ca"`
}

func hashToken(t string) string {
	sum := sha256.Sum256([]byte(t))
	return hex.EncodeToString(sum[:])
}

// agentMux is served on the agent listener only.
func (s *Server) agentMux() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/enroll", s.handleEnroll)
	mux.HandleFunc("POST /v1/renew", s.handleRenew)
	mux.HandleFunc("GET /v1/agent", s.handleAgentWS)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	return mux
}

func (s *Server) handleEnroll(w http.ResponseWriter, r *http.Request) {
	var req enrollReq
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil || req.Token == "" || !hostNameOK(req.Hostname) {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	var labels []byte
	err := s.st.pool.QueryRow(ctx, `UPDATE enroll_tokens SET used_at=now()
		WHERE token_hash=$1 AND used_at IS NULL AND expires_at > now() RETURNING labels`, hashToken(req.Token)).Scan(&labels)
	if err != nil {
		http.Error(w, "invalid or expired token", http.StatusUnauthorized)
		return
	}
	id := randID()
	// Re-enrolling an existing name keeps its identity and history.
	err = s.st.pool.QueryRow(ctx, `INSERT INTO hosts(id,name,labels) VALUES($1,$2,$3)
		ON CONFLICT (name) DO UPDATE SET revoked=false, labels=EXCLUDED.labels RETURNING id`, id, req.Hostname, labels).Scan(&id)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	cert, err := s.ca.SignClient([]byte(req.CSR), id, s.cfg.CertDays)
	if err != nil {
		http.Error(w, "invalid CSR", http.StatusBadRequest)
		return
	}
	s.st.audit(ctx, "agent", "host.enroll", req.Hostname, map[string]any{"host_id": id})
	s.publishHost(id)
	writeJSON(w, http.StatusOK, enrollResp{Cert: string(cert), CA: string(s.ca.certPEM)})
}

// clientHost returns the host ID from the verified client certificate.
func (s *Server) clientHost(w http.ResponseWriter, r *http.Request) (string, bool) {
	if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
		http.Error(w, "client certificate required", http.StatusUnauthorized)
		return "", false
	}
	id := r.TLS.PeerCertificates[0].Subject.CommonName
	var revoked bool
	if err := s.st.pool.QueryRow(r.Context(), `SELECT revoked FROM hosts WHERE id=$1`, id).Scan(&revoked); err != nil || revoked {
		http.Error(w, "host not allowed", http.StatusForbidden)
		return "", false
	}
	return id, true
}

func (s *Server) handleRenew(w http.ResponseWriter, r *http.Request) {
	id, ok := s.clientHost(w, r)
	if !ok {
		return
	}
	var req enrollReq
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	cert, err := s.ca.SignClient([]byte(req.CSR), id, s.cfg.CertDays)
	if err != nil {
		http.Error(w, "invalid CSR", http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusOK, enrollResp{Cert: string(cert), CA: string(s.ca.certPEM)})
}

var upgrader = websocket.Upgrader{ReadBufferSize: 1 << 16, WriteBufferSize: 1 << 16, CheckOrigin: func(*http.Request) bool { return true }}

func (s *Server) handleAgentWS(w http.ResponseWriter, r *http.Request) {
	hostID, ok := s.clientHost(w, r)
	if !ok {
		return
	}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	conn.SetReadLimit(4 << 20)

	ac := &agentConn{hostID: hostID, send: make(chan []byte, 64), done: make(chan struct{})}
	if old := s.hub.set(ac); old != nil {
		close(old.done)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Writer: owns the socket for writes, sends pings.
	go func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for {
			select {
			case msg := <-ac.send:
				conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
				if conn.WriteMessage(websocket.TextMessage, msg) != nil {
					conn.Close()
					return
				}
			case <-t.C:
				conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
				if conn.WriteMessage(websocket.PingMessage, nil) != nil {
					conn.Close()
					return
				}
			case <-ac.done:
				conn.Close()
				return
			case <-ctx.Done():
				return
			}
		}
	}()

	conn.SetReadDeadline(time.Now().Add(90 * time.Second))
	conn.SetPongHandler(func(string) error { conn.SetReadDeadline(time.Now().Add(90 * time.Second)); return nil })

	defer func() {
		if s.hub.remove(ac) {
			s.st.pool.Exec(context.Background(), `UPDATE hosts SET connected=false WHERE id=$1`, hostID)
			s.publishHost(hostID)
		}
	}()

	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			return
		}
		conn.SetReadDeadline(time.Now().Add(90 * time.Second))
		var env wire.Envelope
		if json.Unmarshal(data, &env) != nil {
			continue
		}
		s.handleAgentMessage(ctx, ac, env)
	}
}

func (s *Server) handleAgentMessage(ctx context.Context, ac *agentConn, env wire.Envelope) {
	hostID := ac.hostID
	switch env.Type {
	case wire.TypeHello:
		var h wire.Hello
		if json.Unmarshal(env.Payload, &h) != nil {
			return
		}
		labels, _ := json.Marshal(h.Labels)
		if h.Labels == nil {
			labels = []byte("{}")
		}
		// Labels from enrollment are kept; labels from the agent config are merged on top.
		s.st.pool.Exec(ctx, `UPDATE hosts SET agent_version=$2, os=$3, cores=$4, mem_gb=$5,
			labels = labels || $6::jsonb, connected=true, last_seen=now() WHERE id=$1`,
			hostID, h.Version, h.OS, h.Cores, h.MemGB, labels)
		// Anything still running belonged to the previous agent process.
		s.st.pool.Exec(ctx, `UPDATE commands SET state='failed', message='agent restarted', updated_at=now()
			WHERE host_id=$1 AND state='running'`, hostID)
		s.publishHost(hostID)
		s.flushQueued(ctx, hostID)
	case wire.TypeMetrics:
		var sm wire.Sample
		if json.Unmarshal(env.Payload, &sm) != nil {
			return
		}
		b, _ := json.Marshal(sm)
		s.st.pool.Exec(ctx, `UPDATE hosts SET last_sample=$2, last_sample_at=now(), last_seen=now() WHERE id=$1`, hostID, b)
		if _, err := s.st.pool.Exec(ctx, `INSERT INTO metrics_raw(host_id,ts,cpu,mem,disk,disk_free,load1,vms)
			VALUES($1,now(),$2,$3,$4,$5,$6,$7)`, hostID, sm.CPUPct, sm.MemPct, sm.DiskUsedPct, sm.DiskFreeGB, sm.Load1, sm.VMsRunning); err != nil {
			log.Printf("insert metrics: %v", err)
		}
		s.publishHost(hostID)
	case wire.TypeInventory:
		var inv wire.Inventory
		if json.Unmarshal(env.Payload, &inv) != nil {
			return
		}
		b, _ := json.Marshal(inv.Images)
		s.st.pool.Exec(ctx, `UPDATE hosts SET inventory=$2, last_seen=now() WHERE id=$1`, hostID, b)
		s.publishHost(hostID)
		s.bus.publish("images", "")
	case wire.TypeProgress:
		var p wire.Progress
		if json.Unmarshal(env.Payload, &p) != nil || p.CommandID == "" {
			return
		}
		s.addEvent(ctx, p.CommandID, p.Line)
	case wire.TypeResult:
		var res wire.Result
		if json.Unmarshal(env.Payload, &res) != nil || res.CommandID == "" {
			return
		}
		s.setCommandState(ctx, res.CommandID, res.State, res.Message)
	}
}

// flushQueued sends commands that were waiting while the host was offline.
func (s *Server) flushQueued(ctx context.Context, hostID string) {
	rows, err := s.st.pool.Query(ctx, `SELECT id, type, params FROM commands
		WHERE host_id=$1 AND state='queued' AND expires_at > now() ORDER BY created_at`, hostID)
	if err != nil {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var c wire.Command
		var params []byte
		if rows.Scan(&c.ID, &c.Type, &params) != nil {
			continue
		}
		c.Params = params
		if !agentCommand(c.Type) {
			continue
		}
		s.sendToAgent(hostID, c)
	}
}

func (s *Server) sendToAgent(hostID string, c wire.Command) bool {
	ac := s.hub.get(hostID)
	if ac == nil {
		return false
	}
	b, err := wire.Marshal(wire.TypeCommand, c.ID, c)
	if err != nil {
		return false
	}
	select {
	case ac.send <- b:
		return true
	default:
		return false
	}
}

var _ = pgx.ErrNoRows
