package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/raghavharness/mac-fleet/internal/wire"
)

type Batch struct {
	ID          string          `json:"id"`
	Type        string          `json:"type"`
	Params      json.RawMessage `json:"params"`
	HostIDs     []string        `json:"host_ids"`
	WaveSize    int             `json:"wave_size"`
	FailLimit   int             `json:"fail_limit"`
	CurrentWave int             `json:"current_wave"`
	TotalWaves  int             `json:"total_waves"`
	State       string          `json:"state"` // running, succeeded, halted, cancelled, failed
	Message     string          `json:"message"`
	RequestedBy string          `json:"requested_by"`
	CreatedAt   time.Time       `json:"created_at"`
	Succeeded   int             `json:"succeeded"`
	Failed      int             `json:"failed"`
}

type batchReq struct {
	Type          string          `json:"type"`
	HostIDs       []string        `json:"host_ids"`
	Params        json.RawMessage `json:"params"`
	WaveSize      int             `json:"wave_size"`
	FailLimit     int             `json:"fail_limit"`
	OverrideLimit bool            `json:"override_limit"`
}

// startBatch validates the request, records it and runs the waves in the
// background. A wave starts only after the previous one finished, and the
// batch halts when failures reach fail_limit.
func (s *Server) startBatch(ctx context.Context, req batchReq, actor string) (*Batch, error) {
	if !knownCommand(req.Type) {
		return nil, fmt.Errorf("unknown command type %q", req.Type)
	}
	if len(req.HostIDs) == 0 {
		return nil, errors.New("host_ids is empty")
	}
	if req.WaveSize < 1 {
		req.WaveSize = 1
	}
	if req.FailLimit < 1 {
		req.FailLimit = 2
	}
	if len(req.Params) == 0 {
		req.Params = json.RawMessage("{}")
	}
	if err := validateParams(req.Type, req.Params); err != nil {
		return nil, err
	}
	b := &Batch{
		ID: randID(), Type: req.Type, Params: req.Params, HostIDs: req.HostIDs,
		WaveSize: req.WaveSize, FailLimit: req.FailLimit, State: "running", RequestedBy: actor, CreatedAt: time.Now(),
		TotalWaves: (len(req.HostIDs) + req.WaveSize - 1) / req.WaveSize,
	}
	ids, _ := json.Marshal(b.HostIDs)
	if _, err := s.st.pool.Exec(ctx, `INSERT INTO batches(id,type,params,host_ids,wave_size,fail_limit,total_waves,state,requested_by)
		VALUES($1,$2,$3,$4,$5,$6,$7,'running',$8)`, b.ID, b.Type, b.Params, ids, b.WaveSize, b.FailLimit, b.TotalWaves, actor); err != nil {
		return nil, err
	}
	s.st.audit(ctx, actor, "batch.start", b.Type, map[string]any{"batch_id": b.ID, "hosts": len(b.HostIDs), "wave_size": b.WaveSize})
	go s.runBatch(context.Background(), b, req.OverrideLimit)
	return b, nil
}

func (s *Server) finishBatch(ctx context.Context, id, state, msg string) {
	s.st.pool.Exec(ctx, `UPDATE batches SET state=$2, message=$3, updated_at=now() WHERE id=$1 AND state='running'`, id, state, msg)
	s.bus.publish("batch", id)
}

func (s *Server) batchCancelled(ctx context.Context, id string) bool {
	var st string
	s.st.pool.QueryRow(ctx, `SELECT state FROM batches WHERE id=$1`, id).Scan(&st)
	return st == "cancelled"
}

func (s *Server) runBatch(ctx context.Context, b *Batch, override bool) {
	failed := 0
	for w := 0; w*b.WaveSize < len(b.HostIDs); w++ {
		if s.batchCancelled(ctx, b.ID) {
			return
		}
		lo, hi := w*b.WaveSize, (w+1)*b.WaveSize
		if hi > len(b.HostIDs) {
			hi = len(b.HostIDs)
		}
		wave := b.HostIDs[lo:hi]
		s.st.pool.Exec(ctx, `UPDATE batches SET current_wave=$2, updated_at=now() WHERE id=$1`, b.ID, w+1)
		s.bus.publish("batch", b.ID)

		if !override && takesNodeOut(b.Type, b.Params) {
			if err := s.checkBlast(ctx, wave); err != nil {
				s.finishBatch(ctx, b.ID, "halted", fmt.Sprintf("wave %d: %v", w+1, err))
				return
			}
		}
		var cmdIDs []string
		for _, h := range wave {
			c, err := s.createCommand(ctx, h, b.Type, b.Params, CreateOpts{Actor: b.RequestedBy, BatchID: b.ID, SkipBlast: true})
			if err != nil {
				failed++
				log.Printf("batch %s: host %s: %v", b.ID, h, err)
				continue
			}
			cmdIDs = append(cmdIDs, c.ID)
		}
		failed += s.waitCommands(ctx, b.ID, cmdIDs)
		if failed >= b.FailLimit {
			s.finishBatch(ctx, b.ID, "halted", fmt.Sprintf("halted after wave %d with %d failures", w+1, failed))
			return
		}
	}
	s.finishBatch(ctx, b.ID, "succeeded", "")
}

// waitCommands blocks until every command is terminal and returns how many failed.
func (s *Server) waitCommands(ctx context.Context, batchID string, ids []string) int {
	if len(ids) == 0 {
		return 0
	}
	deadline := time.Now().Add(3 * time.Hour)
	for time.Now().Before(deadline) {
		if s.batchCancelled(ctx, batchID) {
			return 0
		}
		var pending, bad int
		s.st.pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE state NOT IN ('succeeded','failed','expired')),
			count(*) FILTER (WHERE state IN ('failed','expired')) FROM commands WHERE id = ANY($1)`, ids).Scan(&pending, &bad)
		if pending == 0 {
			return bad
		}
		time.Sleep(3 * time.Second)
	}
	return len(ids)
}

func (s *Server) listBatches(ctx context.Context, limit int) ([]Batch, error) {
	rows, err := s.st.pool.Query(ctx, `SELECT b.id,b.type,b.params,b.host_ids,b.wave_size,b.fail_limit,b.current_wave,b.total_waves,b.state,b.message,b.requested_by,b.created_at,
		coalesce((SELECT count(*) FROM commands c WHERE c.batch_id=b.id AND c.state='succeeded'),0),
		coalesce((SELECT count(*) FROM commands c WHERE c.batch_id=b.id AND c.state IN ('failed','expired')),0)
		FROM batches b ORDER BY b.created_at DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Batch{}
	for rows.Next() {
		var b Batch
		var ids []byte
		if err := rows.Scan(&b.ID, &b.Type, &b.Params, &ids, &b.WaveSize, &b.FailLimit, &b.CurrentWave, &b.TotalWaves, &b.State, &b.Message, &b.RequestedBy, &b.CreatedAt, &b.Succeeded, &b.Failed); err != nil {
			return nil, err
		}
		json.Unmarshal(ids, &b.HostIDs)
		out = append(out, b)
	}
	return out, rows.Err()
}

var _ = wire.StateQueued
