package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"github.com/raghavharness/mac-fleet/internal/wire"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestDownloadChecksum(t *testing.T) {
	body := []byte("binary-bytes")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(body) }))
	defer srv.Close()
	sum := sha256.Sum256(body)
	dst := filepath.Join(t.TempDir(), "x")
	if err := download(context.Background(), srv.URL, dst, hex.EncodeToString(sum[:])); err != nil {
		t.Fatal(err)
	}
	if err := download(context.Background(), srv.URL, dst, hex.EncodeToString(make([]byte, 32))); err == nil {
		t.Fatal("a wrong checksum must fail")
	}
}

func TestUpgradeRefusals(t *testing.T) {
	a := New(&Config{DataDir: t.TempDir()}, "", "v0.1.0")
	noop := func(string) {}
	if msg, err := a.upgrade(context.Background(), wireUpgrade("v0.1.0"), noop); err != nil || msg == "" {
		t.Fatalf("same version should be a no-op, got %q %v", msg, err)
	}
	if _, err := a.upgrade(context.Background(), wireUpgrade("v0.2.0"), noop); err == nil {
		t.Fatal("a missing asset must fail")
	}
}

func TestMarkerRollsBackStateFile(t *testing.T) {
	c := &Config{DataDir: t.TempDir()}
	if err := writeMarker(c, &upgradeMarker{From: "v1", To: "v2"}); err != nil {
		t.Fatal(err)
	}
	a := New(c, "", "v2")
	a.upgradeOK()
	if _, err := os.Stat(markerPath(c)); !os.IsNotExist(err) {
		t.Fatal("marker should be removed once connected")
	}
}

func wireUpgrade(v string) wire.AgentUpgradeParams { return wire.AgentUpgradeParams{Version: v} }
