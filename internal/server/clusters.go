package server

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// A Nomad cluster the fleet's hosts belong to. Credentials are stored
// encrypted in settings under cluster:<id>:<token|ca|cert|key>.
type Cluster struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Addr          string `json:"addr"`
	TLSServerName string `json:"tls_server_name"`
	Hosts         int    `json:"hosts"`
	HasToken      bool   `json:"has_token"`
	HasCA         bool   `json:"has_ca"`
	HasCert       bool   `json:"has_cert"`
	HasKey        bool   `json:"has_key"`
}

var clusterNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 ._-]{0,62}$`)

func secretKey(id, part string) string { return "cluster:" + id + ":" + part }

var secretParts = []string{"token", "ca", "cert", "key"}

func (s *Server) listClusters(ctx context.Context) ([]Cluster, error) {
	rows, err := s.st.pool.Query(ctx, `SELECT c.id, c.name, c.addr, c.tls_server_name,
		(SELECT count(*) FROM hosts h WHERE h.cluster_id=c.id AND NOT h.revoked) FROM clusters c ORDER BY c.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Cluster{}
	for rows.Next() {
		var c Cluster
		if err := rows.Scan(&c.ID, &c.Name, &c.Addr, &c.TLSServerName, &c.Hosts); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	have := map[string]bool{}
	kr, err := s.st.pool.Query(ctx, `SELECT key FROM settings WHERE key LIKE 'cluster:%'`)
	if err != nil {
		return nil, err
	}
	defer kr.Close()
	for kr.Next() {
		var k string
		kr.Scan(&k)
		have[k] = true
	}
	for i := range out {
		c := &out[i]
		c.HasToken, c.HasCA = have[secretKey(c.ID, "token")], have[secretKey(c.ID, "ca")]
		c.HasCert, c.HasKey = have[secretKey(c.ID, "cert")], have[secretKey(c.ID, "key")]
	}
	return out, nil
}

// newNomadClient builds a client for a cluster. CA, cert and key are optional
// PEM strings; cert and key must be given together.
func newNomadClient(addr, serverName, token, ca, cert, key string) (*nomadClient, error) {
	addr = strings.TrimRight(strings.TrimSpace(addr), "/")
	if !strings.HasPrefix(addr, "http://") && !strings.HasPrefix(addr, "https://") {
		return nil, errors.New("address must start with http:// or https://")
	}
	tr := &http.Transport{}
	if strings.HasPrefix(addr, "https://") {
		cfg := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: serverName}
		if ca != "" {
			pool := x509.NewCertPool()
			if !pool.AppendCertsFromPEM([]byte(ca)) {
				return nil, errors.New("the CA certificate is not valid PEM")
			}
			cfg.RootCAs = pool
		}
		if (cert == "") != (key == "") {
			return nil, errors.New("the client certificate and key must be given together")
		}
		if cert != "" {
			kp, err := tls.X509KeyPair([]byte(cert), []byte(key))
			if err != nil {
				return nil, fmt.Errorf("client certificate and key: %w", err)
			}
			cfg.Certificates = []tls.Certificate{kp}
		}
		tr.TLSClientConfig = cfg
	} else if ca != "" || cert != "" || key != "" {
		return nil, errors.New("certificates need an https:// address")
	}
	return &nomadClient{addr: addr, token: token, hc: &http.Client{Timeout: 20 * time.Second, Transport: tr}}, nil
}

func (s *Server) nomadFor(ctx context.Context, clusterID string) (*nomadClient, error) {
	var addr, sn string
	if err := s.st.pool.QueryRow(ctx, `SELECT addr, tls_server_name FROM clusters WHERE id=$1`, clusterID).Scan(&addr, &sn); err != nil {
		return nil, errors.New("Nomad cluster not found")
	}
	var v [4]string
	for i, p := range secretParts {
		x, err := s.st.getSecret(ctx, secretKey(clusterID, p))
		if err != nil {
			return nil, err
		}
		v[i] = x
	}
	return newNomadClient(addr, sn, v[0], v[1], v[2], v[3])
}

// nomadForHost uses the host's cluster, or the only cluster if there is one.
func (s *Server) nomadForHost(ctx context.Context, hostID string) (*nomadClient, error) {
	var cid string
	if err := s.st.pool.QueryRow(ctx, `SELECT cluster_id FROM hosts WHERE id=$1`, hostID).Scan(&cid); err != nil {
		return nil, err
	}
	if cid == "" {
		cid = s.onlyCluster(ctx)
	}
	if cid == "" {
		return nil, errors.New("this host has no Nomad cluster; assign one on the host page or add a cluster in Settings")
	}
	return s.nomadFor(ctx, cid)
}

func (s *Server) onlyCluster(ctx context.Context) string {
	rows, err := s.st.pool.Query(ctx, `SELECT id FROM clusters LIMIT 2`)
	if err != nil {
		return ""
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		rows.Scan(&id)
		ids = append(ids, id)
	}
	if len(ids) == 1 {
		return ids[0]
	}
	return ""
}

// migrateClusters moves the single Nomad connection from the old settings
// into a cluster, and attaches unassigned hosts when there is exactly one.
func (s *Server) migrateClusters(ctx context.Context) {
	var n int
	s.st.pool.QueryRow(ctx, `SELECT count(*) FROM clusters`).Scan(&n)
	if n == 0 {
		if st := s.st.loadSettings(ctx); st.NomadAddr != "" {
			id := randID()
			if _, err := s.st.pool.Exec(ctx, `INSERT INTO clusters(id,name,addr) VALUES($1,'default',$2)`, id, st.NomadAddr); err == nil {
				if tok, _ := s.st.getSecret(ctx, "nomad_token"); tok != "" {
					s.st.putSecret(ctx, secretKey(id, "token"), tok)
				}
			}
		}
	}
	if id := s.onlyCluster(ctx); id != "" {
		s.st.pool.Exec(ctx, `UPDATE hosts SET cluster_id=$1 WHERE cluster_id=''`, id)
	}
}

type clusterReq struct {
	Name          string   `json:"name"`
	Addr          string   `json:"addr"`
	TLSServerName string   `json:"tls_server_name"`
	Token         string   `json:"token"`
	CA            string   `json:"ca"`
	Cert          string   `json:"cert"`
	Key           string   `json:"key"`
	Clear         []string `json:"clear"` // secrets to remove: token, ca, cert, key
}

func (r *clusterReq) values() map[string]string {
	return map[string]string{"token": r.Token, "ca": r.CA, "cert": r.Cert, "key": r.Key}
}

func validPEM(kind, s string) error {
	if s == "" {
		return nil
	}
	b, _ := pem.Decode([]byte(s))
	if b == nil {
		return fmt.Errorf("the %s is not valid PEM", kind)
	}
	return nil
}

func (r *clusterReq) validate() error {
	r.Name, r.Addr = strings.TrimSpace(r.Name), strings.TrimSpace(r.Addr)
	if !clusterNameRe.MatchString(r.Name) {
		return errors.New("name must be 1 to 63 letters, digits, spaces, dots, dashes or underscores")
	}
	if !strings.HasPrefix(r.Addr, "http://") && !strings.HasPrefix(r.Addr, "https://") {
		return errors.New("address must start with http:// or https://")
	}
	for k, v := range map[string]string{"CA certificate": r.CA, "client certificate": r.Cert, "client key": r.Key} {
		if err := validPEM(k, v); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) apiClusters(w http.ResponseWriter, r *http.Request) {
	cl, err := s.listClusters(r.Context())
	if err != nil {
		httpErr(w, 500, err)
		return
	}
	writeJSON(w, 200, map[string]any{"clusters": cl})
}

func (s *Server) saveClusterSecrets(ctx context.Context, id string, req *clusterReq) error {
	for p, v := range req.values() {
		if v != "" {
			if err := s.st.putSecret(ctx, secretKey(id, p), v); err != nil {
				return err
			}
		}
	}
	for _, p := range req.Clear {
		for _, ok := range secretParts {
			if p == ok {
				s.st.pool.Exec(ctx, `DELETE FROM settings WHERE key=$1`, secretKey(id, p))
			}
		}
	}
	return nil
}

func (s *Server) apiCreateCluster(w http.ResponseWriter, r *http.Request) {
	var req clusterReq
	if err := decode(r, &req); err != nil {
		httpErr(w, 400, err)
		return
	}
	if err := req.validate(); err != nil {
		httpErr(w, 400, err)
		return
	}
	if _, err := newNomadClient(req.Addr, req.TLSServerName, req.Token, req.CA, req.Cert, req.Key); err != nil {
		httpErr(w, 400, err)
		return
	}
	id := randID()
	if _, err := s.st.pool.Exec(r.Context(), `INSERT INTO clusters(id,name,addr,tls_server_name) VALUES($1,$2,$3,$4)`, id, req.Name, req.Addr, req.TLSServerName); err != nil {
		httpErr(w, 400, errors.New("a cluster with that name already exists"))
		return
	}
	if err := s.saveClusterSecrets(r.Context(), id, &req); err != nil {
		httpErr(w, 500, err)
		return
	}
	// The first cluster adopts hosts that have none.
	s.migrateClusters(r.Context())
	s.st.audit(r.Context(), actor(r), "cluster.create", req.Name, nil)
	s.pushClusterSettings(r.Context(), id)
	s.bus.publish("clusters", id)
	writeJSON(w, 200, map[string]any{"id": id})
}

func (s *Server) apiUpdateCluster(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req clusterReq
	if err := decode(r, &req); err != nil {
		httpErr(w, 400, err)
		return
	}
	if err := req.validate(); err != nil {
		httpErr(w, 400, err)
		return
	}
	ct, err := s.st.pool.Exec(r.Context(), `UPDATE clusters SET name=$2, addr=$3, tls_server_name=$4, updated_at=now() WHERE id=$1`, id, req.Name, req.Addr, req.TLSServerName)
	if err != nil {
		httpErr(w, 400, errors.New("a cluster with that name already exists"))
		return
	}
	if ct.RowsAffected() == 0 {
		httpErr(w, 404, errors.New("cluster not found"))
		return
	}
	if err := s.saveClusterSecrets(r.Context(), id, &req); err != nil {
		httpErr(w, 500, err)
		return
	}
	s.st.audit(r.Context(), actor(r), "cluster.update", req.Name, map[string]any{"cleared": req.Clear})
	s.pushClusterSettings(r.Context(), id)
	s.bus.publish("clusters", id)
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) apiDeleteCluster(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var name string
	if err := s.st.pool.QueryRow(r.Context(), `SELECT name FROM clusters WHERE id=$1`, id).Scan(&name); err != nil {
		httpErr(w, 404, errors.New("cluster not found"))
		return
	}
	s.st.pool.Exec(r.Context(), `UPDATE hosts SET cluster_id='' WHERE cluster_id=$1`, id)
	s.st.pool.Exec(r.Context(), `DELETE FROM settings WHERE key LIKE $1`, "cluster:"+id+":%")
	s.st.pool.Exec(r.Context(), `DELETE FROM clusters WHERE id=$1`, id)
	s.st.audit(r.Context(), actor(r), "cluster.delete", name, nil)
	s.bus.publish("clusters", id)
	s.bus.publish("hosts", "")
	writeJSON(w, 200, map[string]any{"ok": true})
}

// apiTestCluster checks a connection with the values in the form; blank
// secrets fall back to the stored ones when an existing cluster is named.
func (s *Server) apiTestCluster(w http.ResponseWriter, r *http.Request) {
	var req struct {
		clusterReq
		ID string `json:"id"`
	}
	if err := decode(r, &req); err != nil {
		httpErr(w, 400, err)
		return
	}
	vals := req.values()
	if req.ID != "" {
		cleared := map[string]bool{}
		for _, c := range req.Clear {
			cleared[c] = true
		}
		for _, p := range secretParts {
			if vals[p] == "" && !cleared[p] {
				vals[p], _ = s.st.getSecret(r.Context(), secretKey(req.ID, p))
			}
		}
	}
	nc, err := newNomadClient(req.Addr, req.TLSServerName, vals["token"], vals["ca"], vals["cert"], vals["key"])
	if err == nil {
		ctx, cancel := context.WithTimeout(r.Context(), 6*time.Second)
		defer cancel()
		err = nc.ping(ctx)
	}
	if err == nil {
		writeJSON(w, 200, map[string]any{"ok": true})
		return
	}
	// The server often cannot reach Nomad; try from a Mac in the cluster. The
	// Mac uses its own Nomad API, so certificate files are not needed there.
	if ex, xerr := s.pickExecutor(r.Context(), req.ID, ""); xerr == nil {
		if msg, perr := s.pingViaHost(r.Context(), ex.ID, ex.Name, vals["token"]); perr == nil {
			writeJSON(w, 200, map[string]any{"ok": true, "via": ex.Name, "note": msg})
			return
		} else {
			writeJSON(w, 200, map[string]any{"ok": false, "error": fmt.Sprintf("from %s: %v", ex.Name, perr)})
			return
		}
	}
	writeJSON(w, 200, map[string]any{"ok": false, "error": err.Error() + " (and no online Mac could test it either)"})
}

func (s *Server) apiAssignCluster(w http.ResponseWriter, r *http.Request) {
	var req struct {
		HostIDs   []string `json:"host_ids"`
		ClusterID string   `json:"cluster_id"`
	}
	if err := decode(r, &req); err != nil || len(req.HostIDs) == 0 {
		httpErr(w, 400, errors.New("host_ids is required"))
		return
	}
	if req.ClusterID != "" {
		var x string
		if err := s.st.pool.QueryRow(r.Context(), `SELECT id FROM clusters WHERE id=$1`, req.ClusterID).Scan(&x); err != nil {
			httpErr(w, 400, errors.New("cluster not found"))
			return
		}
	}
	if _, err := s.st.pool.Exec(r.Context(), `UPDATE hosts SET cluster_id=$2 WHERE id = ANY($1)`, req.HostIDs, req.ClusterID); err != nil {
		httpErr(w, 500, err)
		return
	}
	s.st.audit(r.Context(), actor(r), "host.cluster", req.ClusterID, map[string]any{"hosts": len(req.HostIDs)})
	for _, id := range req.HostIDs {
		s.publishHost(id)
		s.pushSettings(r.Context(), id)
	}
	s.bus.publish("clusters", "")
	writeJSON(w, 200, map[string]any{"ok": true})
}
