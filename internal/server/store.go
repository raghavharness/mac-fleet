package server

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed schema.sql
var schemaSQL string

type Store struct {
	pool *pgxpool.Pool
	key  []byte
}

func openStore(ctx context.Context, cfg *Config) (*Store, error) {
	pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		return nil, err
	}
	s := &Store{pool: pool, key: cfg.MasterKey}
	if _, err := pool.Exec(ctx, schemaSQL); err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return s, nil
}

// ---- settings ----

func (s *Store) getSetting(ctx context.Context, key string) ([]byte, error) {
	var v []byte
	err := s.pool.QueryRow(ctx, `SELECT value FROM settings WHERE key=$1`, key).Scan(&v)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	return v, err
}

func (s *Store) putSetting(ctx context.Context, key string, v []byte) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO settings(key,value) VALUES($1,$2)
		ON CONFLICT (key) DO UPDATE SET value=EXCLUDED.value, updated_at=now()`, key, v)
	return err
}

func (s *Store) getSecret(ctx context.Context, key string) (string, error) {
	v, err := s.getSetting(ctx, key)
	if err != nil || v == nil {
		return "", err
	}
	p, err := open(s.key, v)
	return string(p), err
}

func (s *Store) putSecret(ctx context.Context, key, plain string) error {
	b, err := seal(s.key, []byte(plain))
	if err != nil {
		return err
	}
	return s.putSetting(ctx, key, b)
}

// Settings that operators edit from the dashboard.
type Settings struct {
	NomadAddr     string `json:"nomad_addr"`
	NomadTokenSet bool   `json:"nomad_token_set"`
	NomadTokenAt  string `json:"nomad_token_updated,omitempty"`
	DiskPct       int    `json:"disk_pct"`
	MemPct        int    `json:"mem_pct"`
	OfflineMin    int    `json:"offline_min"`
	MaxOutPct     int    `json:"max_out_pct"`
	TargetVersion string `json:"target_agent_version"`
}

func defaultSettings() Settings {
	return Settings{DiskPct: 85, MemPct: 90, OfflineMin: 2, MaxOutPct: 10}
}

func (s *Store) loadSettings(ctx context.Context) Settings {
	st := defaultSettings()
	if b, _ := s.getSetting(ctx, "ui"); b != nil {
		json.Unmarshal(b, &st)
	}
	var at time.Time
	if err := s.pool.QueryRow(ctx, `SELECT updated_at FROM settings WHERE key='nomad_token'`).Scan(&at); err == nil {
		st.NomadTokenSet, st.NomadTokenAt = true, at.Format(time.RFC3339)
	}
	return st
}

func (s *Store) saveSettings(ctx context.Context, st Settings) error {
	st.NomadTokenSet, st.NomadTokenAt = false, ""
	b, _ := json.Marshal(st)
	return s.putSetting(ctx, "ui", b)
}

// ---- audit ----

func (s *Store) audit(ctx context.Context, actor, action, target string, detail any) {
	b, _ := json.Marshal(detail)
	if detail == nil {
		b = []byte("{}")
	}
	s.pool.Exec(ctx, `INSERT INTO audit_log(actor,action,target,detail) VALUES($1,$2,$3,$4)`, actor, action, target, b)
}

// ---- partitions and retention ----

var partRe = regexp.MustCompile(`^(metrics_raw|metrics_5m)_(\d{8})$`)

func (s *Store) ensurePartitions(ctx context.Context, daysAhead int) error {
	day := time.Now().UTC().Truncate(24 * time.Hour)
	for _, tbl := range []string{"metrics_raw", "metrics_5m"} {
		for i := -1; i <= daysAhead; i++ {
			d := day.AddDate(0, 0, i)
			name := tbl + "_" + d.Format("20060102")
			q := fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s PARTITION OF %s FOR VALUES FROM ('%s') TO ('%s')`,
				name, tbl, d.Format(time.RFC3339), d.AddDate(0, 0, 1).Format(time.RFC3339))
			if _, err := s.pool.Exec(ctx, q); err != nil {
				return err
			}
			s.pool.Exec(ctx, fmt.Sprintf(`CREATE INDEX IF NOT EXISTS %s_idx ON %s (host_id, ts)`, name, name))
		}
	}
	return nil
}

func (s *Store) dropOldPartitions(ctx context.Context, rawDays, keepDays int) error {
	rows, err := s.pool.Query(ctx, `SELECT c.relname FROM pg_inherits i
		JOIN pg_class c ON c.oid=i.inhrelid JOIN pg_class p ON p.oid=i.inhparent
		WHERE p.relname IN ('metrics_raw','metrics_5m')`)
	if err != nil {
		return err
	}
	var names []string
	for rows.Next() {
		var n string
		rows.Scan(&n)
		names = append(names, n)
	}
	rows.Close()
	for _, n := range names {
		m := partRe.FindStringSubmatch(n)
		if m == nil {
			continue
		}
		d, err := time.Parse("20060102", m[2])
		if err != nil {
			continue
		}
		keep := keepDays
		if m[1] == "metrics_raw" {
			keep = rawDays
		}
		// A partition covers a whole day, drop it once its day ended before the cutoff.
		if d.AddDate(0, 0, 1).Before(time.Now().UTC().AddDate(0, 0, -keep)) {
			if _, err := s.pool.Exec(ctx, "DROP TABLE IF EXISTS "+pgx.Identifier{n}.Sanitize()); err != nil {
				return err
			}
		}
	}
	return nil
}

// rollup aggregates raw samples into 5 minute buckets, only for buckets that
// are complete. The last processed boundary is kept in settings.
func (s *Store) rollup(ctx context.Context) error {
	end := time.Now().UTC().Truncate(5 * time.Minute)
	var start time.Time
	if b, _ := s.getSetting(ctx, "rollup_upto"); b != nil {
		start, _ = time.Parse(time.RFC3339, string(b))
	}
	if start.IsZero() {
		start = end.Add(-time.Hour)
	}
	if !start.Before(end) {
		return nil
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO metrics_5m(host_id, ts, cpu, cpu_max, mem, mem_max, disk, vms)
		SELECT host_id, date_bin('5 minutes', ts, TIMESTAMPTZ '2000-01-01'),
		       avg(cpu), max(cpu), avg(mem), max(mem), avg(disk), avg(vms)
		FROM metrics_raw WHERE ts >= $1 AND ts < $2 GROUP BY 1,2`, start, end)
	if err != nil {
		return err
	}
	return s.putSetting(ctx, "rollup_upto", []byte(end.Format(time.RFC3339)))
}

type Point struct {
	T    time.Time `json:"t"`
	CPU  float64   `json:"cpu"`
	Mem  float64   `json:"mem"`
	Disk float64   `json:"disk"`
	VMs  float64   `json:"vms"`
}

// metricsFor returns chart points for one host. Short ranges read raw samples,
// long ranges read the 5 minute rollups, always bucketed to keep payloads small.
func (s *Store) metricsFor(ctx context.Context, hostID, rng string) ([]Point, error) {
	var since time.Duration
	var table, bucket string
	switch rng {
	case "1h":
		since, table, bucket = time.Hour, "metrics_raw", "30 seconds"
	case "7d":
		since, table, bucket = 7*24*time.Hour, "metrics_5m", "30 minutes"
	case "30d":
		since, table, bucket = 30*24*time.Hour, "metrics_5m", "2 hours"
	default:
		since, table, bucket = 24*time.Hour, "metrics_raw", "5 minutes"
	}
	q := fmt.Sprintf(`SELECT date_bin('%s', ts, TIMESTAMPTZ '2000-01-01') AS t,
		avg(cpu), avg(mem), avg(disk), avg(vms) FROM %s
		WHERE host_id=$1 AND ts >= $2 GROUP BY 1 ORDER BY 1`, bucket, table)
	rows, err := s.pool.Query(ctx, q, hostID, time.Now().Add(-since))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Point{}
	for rows.Next() {
		var p Point
		var cpu, mem, disk, vms *float64
		if err := rows.Scan(&p.T, &cpu, &mem, &disk, &vms); err != nil {
			return nil, err
		}
		p.CPU, p.Mem, p.Disk, p.VMs = deref(cpu), deref(mem), deref(disk), deref(vms)
		out = append(out, p)
	}
	return out, rows.Err()
}

func deref(f *float64) float64 {
	if f == nil {
		return 0
	}
	return *f
}

func hostNameOK(n string) bool { return n != "" && len(n) < 128 && !strings.ContainsAny(n, " /\\") }
