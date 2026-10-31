package server

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"embed"
	"encoding/hex"
	"errors"
	"io/fs"
	"log"
	"net/http"
	"time"
)

//go:embed web
var webFS embed.FS

type Server struct {
	cfg *Config
	st  *Store
	ca  *CA
	hub *Hub
	bus *Bus
}

func New(ctx context.Context, cfg *Config) (*Server, error) {
	st, err := openStore(ctx, cfg)
	if err != nil {
		return nil, err
	}
	s := &Server{cfg: cfg, st: st, hub: newHub(), bus: newBus()}
	if err := s.loadOrCreateCA(ctx); err != nil {
		return nil, err
	}
	if err := st.ensurePartitions(ctx, 3); err != nil {
		return nil, err
	}
	s.migrateClusters(ctx)
	// Batches that were running when the process stopped cannot resume.
	st.pool.Exec(ctx, `UPDATE batches SET state='halted', message='server restarted while running', updated_at=now() WHERE state='running'`)
	return s, nil
}

func (s *Server) loadOrCreateCA(ctx context.Context) error {
	certPEM, err := s.st.getSetting(ctx, "ca_cert")
	if err != nil {
		return err
	}
	keyPEM, err := s.st.getSecret(ctx, "ca_key")
	if err != nil {
		return err
	}
	if certPEM != nil && keyPEM != "" {
		s.ca, err = loadCA(certPEM, []byte(keyPEM))
		return err
	}
	ca, key, err := newCA()
	if err != nil {
		return err
	}
	if err := s.st.putSetting(ctx, "ca_cert", ca.certPEM); err != nil {
		return err
	}
	if err := s.st.putSecret(ctx, "ca_key", string(key)); err != nil {
		return err
	}
	s.ca = ca
	log.Printf("created a new agent CA, fingerprint %s", ca.Fingerprint())
	return nil
}

func (s *Server) Run(ctx context.Context) error {
	go s.janitor(ctx)
	go s.maintenanceLoop(ctx)

	srvCert, err := s.ca.ServerCert(s.cfg.AgentHosts)
	if err != nil {
		return err
	}
	agentSrv := &http.Server{
		Addr:    s.cfg.AgentAddr,
		Handler: s.agentMux(),
		TLSConfig: &tls.Config{
			MinVersion:   tls.VersionTLS12,
			Certificates: []tls.Certificate{srvCert},
			ClientCAs:    s.ca.Pool(),
			// Enrollment has no client certificate yet; the handlers that need one check for it.
			ClientAuth: tls.VerifyClientCertIfGiven,
		},
		ReadHeaderTimeout: 15 * time.Second,
	}
	webSrv := &http.Server{Addr: s.cfg.HTTPAddr, Handler: s.webMux(), ReadHeaderTimeout: 15 * time.Second}

	errc := make(chan error, 2)
	go func() {
		log.Printf("agent listener (mTLS) on %s", s.cfg.AgentAddr)
		errc <- agentSrv.ListenAndServeTLS("", "")
	}()
	go func() { log.Printf("dashboard on %s", s.cfg.HTTPAddr); errc <- webSrv.ListenAndServe() }()

	select {
	case <-ctx.Done():
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	}
	sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	agentSrv.Shutdown(sctx)
	webSrv.Shutdown(sctx)
	return nil
}

func (s *Server) webMux() http.Handler {
	mux := http.NewServeMux()
	s.routes(mux)
	sub, _ := fs.Sub(webFS, "web")
	mux.Handle("/", staticHandler(sub))
	return mux
}

// staticHandler serves the embedded UI with content-hash ETags so browsers
// revalidate cheaply and always pick up a new build.
func staticHandler(root fs.FS) http.Handler {
	etags := map[string]string{}
	fs.WalkDir(root, ".", func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if b, err := fs.ReadFile(root, p); err == nil {
				sum := sha256.Sum256(b)
				etags["/"+p] = `"` + hex.EncodeToString(sum[:8]) + `"`
			}
		}
		return nil
	})
	fileServer := http.FileServer(http.FS(root))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		if path == "/" {
			path = "/index.html"
		}
		if et, ok := etags[path]; ok {
			w.Header().Set("ETag", et)
			w.Header().Set("Cache-Control", "no-cache")
			if r.Header.Get("If-None-Match") == et {
				w.WriteHeader(http.StatusNotModified)
				return
			}
		}
		fileServer.ServeHTTP(w, r)
	})
}

// maintenanceLoop creates partitions, rolls raw samples up and enforces
// retention. It is idempotent, so running it on every replica is safe.
func (s *Server) maintenanceLoop(ctx context.Context) {
	run := func() {
		if err := s.st.ensurePartitions(ctx, 3); err != nil {
			log.Printf("partitions: %v", err)
		}
		if err := s.st.rollup(ctx); err != nil {
			log.Printf("rollup: %v", err)
		}
		if err := s.st.dropOldPartitions(ctx, s.cfg.RawDays, s.cfg.RetentionDays); err != nil {
			log.Printf("retention: %v", err)
		}
	}
	run()
	t := time.NewTicker(5 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			run()
		}
	}
}
