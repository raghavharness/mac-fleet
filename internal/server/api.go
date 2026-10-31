package server

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/raghavharness/mac-fleet/internal/wire"
)

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func httpErr(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": err.Error()})
}

func actor(r *http.Request) string {
	// The dashboard has no login yet. When it sits behind an authenticating
	// proxy, the proxy's user header is recorded in the audit log.
	if u := r.Header.Get("X-Forwarded-User"); u != "" {
		return u
	}
	return "anonymous"
}

func decode(r *http.Request, v any) error {
	return json.NewDecoder(io.LimitReader(r.Body, 4<<20)).Decode(v)
}

func (s *Server) routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		if err := s.st.pool.Ping(r.Context()); err != nil {
			http.Error(w, "db unavailable", http.StatusServiceUnavailable)
			return
		}
		w.Write([]byte("ok"))
	})
	mux.HandleFunc("GET /api/hosts", s.apiHosts)
	mux.HandleFunc("GET /api/hosts/{id}", s.apiHost)
	mux.HandleFunc("GET /api/hosts/{id}/metrics", s.apiMetrics)
	mux.HandleFunc("GET /api/hosts/{id}/nomad-config", s.apiNomadConfig)
	mux.HandleFunc("POST /api/hosts/{id}/commands", s.apiCreateCommand)
	mux.HandleFunc("POST /api/hosts/{id}/revoke", s.apiRevoke)
	mux.HandleFunc("GET /api/commands", s.apiCommands)
	mux.HandleFunc("GET /api/commands/{id}", s.apiCommand)
	mux.HandleFunc("POST /api/batches", s.apiCreateBatch)
	mux.HandleFunc("GET /api/batches", s.apiBatches)
	mux.HandleFunc("POST /api/batches/{id}/cancel", s.apiCancelBatch)
	mux.HandleFunc("GET /api/images", s.apiImages)
	mux.HandleFunc("GET /api/settings", s.apiGetSettings)
	mux.HandleFunc("PUT /api/settings", s.apiPutSettings)
	mux.HandleFunc("GET /api/agent-release", s.apiAgentRelease)
	mux.HandleFunc("GET /api/clusters", s.apiClusters)
	mux.HandleFunc("POST /api/clusters", s.apiCreateCluster)
	mux.HandleFunc("POST /api/clusters/test", s.apiTestCluster)
	mux.HandleFunc("PUT /api/clusters/{id}", s.apiUpdateCluster)
	mux.HandleFunc("DELETE /api/clusters/{id}", s.apiDeleteCluster)
	mux.HandleFunc("POST /api/enroll-tokens", s.apiEnrollToken)
	mux.HandleFunc("GET /api/audit", s.apiAudit)
	mux.HandleFunc("GET /api/agent-install", s.apiAgentInstall)
	mux.HandleFunc("GET /api/events", s.apiEvents)
}

func (s *Server) apiHosts(w http.ResponseWriter, r *http.Request) {
	hosts, err := s.listHosts(r.Context())
	if err != nil {
		httpErr(w, 500, err)
		return
	}
	alerts := []Alert{}
	visible := hosts[:0:0]
	for _, h := range hosts {
		if h.Revoked {
			continue
		}
		visible = append(visible, h)
		alerts = append(alerts, h.Alerts...)
	}
	writeJSON(w, 200, map[string]any{"hosts": visible, "alerts": alerts})
}

func (s *Server) hostByID(ctx context.Context, id string) (*HostView, error) {
	hosts, err := s.listHosts(ctx)
	if err != nil {
		return nil, err
	}
	for i := range hosts {
		if hosts[i].ID == id {
			return &hosts[i], nil
		}
	}
	return nil, fmt.Errorf("host not found")
}

func (s *Server) apiHost(w http.ResponseWriter, r *http.Request) {
	h, err := s.hostByID(r.Context(), r.PathValue("id"))
	if err != nil {
		httpErr(w, 404, err)
		return
	}
	cmds, _ := s.queryCommands(r.Context(), h.ID, 20)
	writeJSON(w, 200, map[string]any{"host": h, "commands": cmds})
}

func (s *Server) apiMetrics(w http.ResponseWriter, r *http.Request) {
	pts, err := s.st.metricsFor(r.Context(), r.PathValue("id"), r.URL.Query().Get("range"))
	if err != nil {
		httpErr(w, 500, err)
		return
	}
	writeJSON(w, 200, map[string]any{"points": pts})
}

type cmdReq struct {
	Type          string          `json:"type"`
	Params        json.RawMessage `json:"params"`
	OverrideLimit bool            `json:"override_limit"`
}

func (s *Server) apiCreateCommand(w http.ResponseWriter, r *http.Request) {
	var req cmdReq
	if err := decode(r, &req); err != nil {
		httpErr(w, 400, err)
		return
	}
	c, err := s.createCommand(r.Context(), r.PathValue("id"), req.Type, req.Params, CreateOpts{Actor: actor(r), Override: req.OverrideLimit})
	if err != nil {
		httpErr(w, 409, err)
		return
	}
	writeJSON(w, 202, c)
}

func (s *Server) apiRevoke(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.st.pool.Exec(r.Context(), `UPDATE hosts SET revoked=true, connected=false WHERE id=$1`, id); err != nil {
		httpErr(w, 500, err)
		return
	}
	if ac := s.hub.get(id); ac != nil {
		close(ac.done)
	}
	s.st.audit(r.Context(), actor(r), "host.revoke", id, nil)
	s.publishHost(id)
	writeJSON(w, 200, map[string]string{"status": "revoked"})
}

func (s *Server) queryCommands(ctx context.Context, hostID string, limit int) ([]Command, error) {
	q := `SELECT c.id,c.type,c.host_id,h.name,c.params,c.state,c.message,c.requested_by,coalesce(c.batch_id,''),c.created_at,c.updated_at
		FROM commands c JOIN hosts h ON h.id=c.host_id`
	args := []any{limit}
	if hostID != "" {
		q += ` WHERE c.host_id=$2`
		args = append(args, hostID)
	}
	q += ` ORDER BY c.created_at DESC LIMIT $1`
	rows, err := s.st.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Command{}
	for rows.Next() {
		var c Command
		if err := rows.Scan(&c.ID, &c.Type, &c.HostID, &c.HostName, &c.Params, &c.State, &c.Message, &c.RequestedBy, &c.BatchID, &c.CreatedAt, &c.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Server) apiCommands(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	cmds, err := s.queryCommands(r.Context(), r.URL.Query().Get("host"), limit)
	if err != nil {
		httpErr(w, 500, err)
		return
	}
	writeJSON(w, 200, map[string]any{"commands": cmds})
}

func (s *Server) apiCommand(w http.ResponseWriter, r *http.Request) {
	var c Command
	err := s.st.pool.QueryRow(r.Context(), `SELECT c.id,c.type,c.host_id,h.name,c.params,c.state,c.message,c.requested_by,coalesce(c.batch_id,''),c.created_at,c.updated_at,c.result
		FROM commands c JOIN hosts h ON h.id=c.host_id WHERE c.id=$1`, r.PathValue("id")).
		Scan(&c.ID, &c.Type, &c.HostID, &c.HostName, &c.Params, &c.State, &c.Message, &c.RequestedBy, &c.BatchID, &c.CreatedAt, &c.UpdatedAt, &c.Result)
	if err != nil {
		httpErr(w, 404, fmt.Errorf("command not found"))
		return
	}
	rows, _ := s.st.pool.Query(r.Context(), `SELECT ts, line FROM command_events WHERE command_id=$1 ORDER BY id`, c.ID)
	type ev struct {
		TS   time.Time `json:"ts"`
		Line string    `json:"line"`
	}
	events := []ev{}
	for rows.Next() {
		var e ev
		rows.Scan(&e.TS, &e.Line)
		events = append(events, e)
	}
	rows.Close()
	writeJSON(w, 200, map[string]any{"command": c, "events": events})
}

func (s *Server) apiCreateBatch(w http.ResponseWriter, r *http.Request) {
	var req batchReq
	if err := decode(r, &req); err != nil {
		httpErr(w, 400, err)
		return
	}
	b, err := s.startBatch(r.Context(), req, actor(r))
	if err != nil {
		httpErr(w, 400, err)
		return
	}
	writeJSON(w, 202, b)
}

func (s *Server) apiBatches(w http.ResponseWriter, r *http.Request) {
	bs, err := s.listBatches(r.Context(), 20)
	if err != nil {
		httpErr(w, 500, err)
		return
	}
	writeJSON(w, 200, map[string]any{"batches": bs})
}

func (s *Server) apiCancelBatch(w http.ResponseWriter, r *http.Request) {
	s.st.pool.Exec(r.Context(), `UPDATE batches SET state='cancelled', message='cancelled by '||$2, updated_at=now() WHERE id=$1 AND state='running'`, r.PathValue("id"), actor(r))
	s.st.audit(r.Context(), actor(r), "batch.cancel", r.PathValue("id"), nil)
	s.bus.publish("batch", r.PathValue("id"))
	writeJSON(w, 200, map[string]string{"status": "cancelled"})
}

type imageAgg struct {
	Name     string   `json:"name"`
	SizeGB   float64  `json:"size_gb"`
	HostIDs  []string `json:"host_ids"`
	Running  int      `json:"running_vms"`
	Accessed string   `json:"last_accessed"`
}

func (s *Server) apiImages(w http.ResponseWriter, r *http.Request) {
	hosts, err := s.listHosts(r.Context())
	if err != nil {
		httpErr(w, 500, err)
		return
	}
	agg := map[string]*imageAgg{}
	total := 0
	hostList := []map[string]any{}
	cluster := r.URL.Query().Get("cluster")
	for _, h := range hosts {
		if h.Revoked || (cluster != "" && h.ClusterID != cluster) {
			continue
		}
		total++
		hostList = append(hostList, map[string]any{"id": h.ID, "name": h.Name, "status": h.Status})
		for _, im := range h.Inventory {
			a := agg[im.Name]
			if a == nil {
				a = &imageAgg{Name: im.Name}
				agg[im.Name] = a
			}
			a.HostIDs = append(a.HostIDs, h.ID)
			if im.SizeGB > a.SizeGB {
				a.SizeGB = im.SizeGB
			}
			if im.Running {
				a.Running++
			}
			if im.Accessed > a.Accessed {
				a.Accessed = im.Accessed
			}
		}
	}
	list := make([]*imageAgg, 0, len(agg))
	for _, a := range agg {
		list = append(list, a)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
	writeJSON(w, 200, map[string]any{"images": list, "total_hosts": total, "hosts": hostList})
}

func (s *Server) apiGetSettings(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, s.st.loadSettings(r.Context()))
}

type settingsReq struct {
	Settings
	NomadToken string `json:"nomad_token"` // write-only; empty leaves the stored token unchanged
}

func (s *Server) apiPutSettings(w http.ResponseWriter, r *http.Request) {
	var req settingsReq
	if err := decode(r, &req); err != nil {
		httpErr(w, 400, err)
		return
	}
	cur := s.st.loadSettings(r.Context())
	n := req.Settings
	if n.DiskPct < 1 || n.DiskPct > 100 || n.MemPct < 1 || n.MemPct > 100 || n.OfflineMin < 1 || n.MaxOutPct < 1 || n.MaxOutPct > 100 {
		httpErr(w, 400, fmt.Errorf("thresholds out of range"))
		return
	}
	if err := s.st.saveSettings(r.Context(), n); err != nil {
		httpErr(w, 500, err)
		return
	}
	detail := map[string]any{"nomad_addr_changed": cur.NomadAddr != n.NomadAddr}
	if req.NomadToken != "" {
		if err := s.st.putSecret(r.Context(), "nomad_token", req.NomadToken); err != nil {
			httpErr(w, 500, err)
			return
		}
		detail["nomad_token_replaced"] = true
	}
	s.st.audit(r.Context(), actor(r), "settings.update", "", detail)
	writeJSON(w, 200, s.st.loadSettings(r.Context()))
}

func (s *Server) apiEnrollToken(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Labels map[string]string `json:"labels"`
	}
	_ = decode(r, &req)
	if req.Labels == nil {
		req.Labels = map[string]string{}
	}
	delete(req.Labels, "cluster")
	raw := make([]byte, 24)
	rand.Read(raw)
	token := base64.RawURLEncoding.EncodeToString(raw)
	exp := time.Now().Add(24 * time.Hour)
	labels, _ := json.Marshal(req.Labels)
	if _, err := s.st.pool.Exec(r.Context(), `INSERT INTO enroll_tokens(token_hash,labels,created_by,expires_at) VALUES($1,$2,$3,$4)`,
		hashToken(token), labels, actor(r), exp); err != nil {
		httpErr(w, 500, err)
		return
	}
	s.st.audit(r.Context(), actor(r), "enroll_token.create", "", nil)
	url := s.cfg.PublicAgentURL
	writeJSON(w, 200, map[string]any{
		"token": token, "expires_at": exp, "server": url, "ca_sha256": s.ca.Fingerprint(),
		"command":         fmt.Sprintf("macfleet-agent enroll --server %s --ca-sha256 %s --token %s", url, s.ca.Fingerprint(), token),
		"install_command": s.installCommand(url, s.ca.Fingerprint(), token),
	})
}

// installCommand is the one line to run on a Mac. The script is fetched from the
// release repository; a private repository needs GITHUB_TOKEN exported first.
func (s *Server) installCommand(server, pin, token string) string {
	repo := s.cfg.ReleaseRepo
	args := fmt.Sprintf("--server %s --ca-sha256 %s", server, pin)
	if token != "" {
		args += " --token " + token
	} else {
		args = "--upgrade"
	}
	if s.cfg.ReleasePrivate {
		return fmt.Sprintf(`curl -fsSL -H "Authorization: Bearer $GITHUB_TOKEN" -H "Accept: application/vnd.github.raw" "https://api.github.com/repos/%s/contents/scripts/install.sh?ref=main" | bash -s -- %s`, repo, args)
	}
	return fmt.Sprintf(`curl -fsSL "https://raw.githubusercontent.com/%s/main/scripts/install.sh" | bash -s -- %s`, repo, args)
}

// apiAgentInstall tells the dashboard where agent builds live and how to install
// one, so the full flow can be shown before a token is created.
func (s *Server) apiAgentInstall(w http.ResponseWriter, r *http.Request) {
	repo := s.cfg.ReleaseRepo
	rel := "https://github.com/" + repo + "/releases"
	writeJSON(w, 200, map[string]any{
		"repo": repo, "private": s.cfg.ReleasePrivate,
		"server": s.cfg.PublicAgentURL, "ca_sha256": s.ca.Fingerprint(),
		"releases_url": rel, "latest_url": rel + "/latest",
		"downloads": map[string]string{
			"darwin_arm64": rel + "/latest/download/macfleet-agent-darwin-arm64",
			"darwin_amd64": rel + "/latest/download/macfleet-agent-darwin-amd64",
			"checksums":    rel + "/latest/download/SHA256SUMS",
		},
		"install_command": s.installCommand(s.cfg.PublicAgentURL, s.ca.Fingerprint(), "<TOKEN>"),
		"upgrade_command": s.installCommand("", "", ""),
	})
}

func (s *Server) apiAudit(w http.ResponseWriter, r *http.Request) {
	rows, err := s.st.pool.Query(r.Context(), `SELECT ts, actor, action, target, detail FROM audit_log ORDER BY id DESC LIMIT 200`)
	if err != nil {
		httpErr(w, 500, err)
		return
	}
	defer rows.Close()
	type entry struct {
		TS     time.Time       `json:"ts"`
		Actor  string          `json:"actor"`
		Action string          `json:"action"`
		Target string          `json:"target"`
		Detail json.RawMessage `json:"detail"`
	}
	out := []entry{}
	for rows.Next() {
		var e entry
		rows.Scan(&e.TS, &e.Actor, &e.Action, &e.Target, &e.Detail)
		out = append(out, e)
	}
	writeJSON(w, 200, map[string]any{"entries": out})
}

// apiEvents streams change notifications; the dashboard refetches on each one.
func (s *Server) apiEvents(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", 500)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	ch := s.bus.subscribe()
	defer s.bus.unsubscribe(ch)
	fmt.Fprint(w, ": connected\n\n")
	fl.Flush()
	ping := time.NewTicker(20 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case m := <-ch:
			fmt.Fprintf(w, "data: %s\n\n", m)
			fl.Flush()
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
			fl.Flush()
		}
	}
}

var _ = wire.StateQueued
