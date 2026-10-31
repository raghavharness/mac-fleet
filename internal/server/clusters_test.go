package server

import (
	"testing"

	"github.com/raghavharness/mac-fleet/internal/wire"
)

func TestClusterMatches(t *testing.T) {
	qa := Cluster{Addr: "https://nomad-qa.example.com:4646", Match: "10.1.0.5, qa-region"}
	n := wire.NomadState{Servers: []string{"10.1.0.5:4647"}}
	if !clusterMatches(qa, n) {
		t.Fatal("server ip should match")
	}
	if !clusterMatches(qa, wire.NomadState{Region: "QA-Region"}) {
		t.Fatal("region should match")
	}
	if !clusterMatches(qa, wire.NomadState{Servers: []string{"nomad-qa.example.com:4647"}}) {
		t.Fatal("api host should match")
	}
	if clusterMatches(qa, wire.NomadState{Servers: []string{"10.9.9.9:4647"}, Region: "prod"}) {
		t.Fatal("should not match")
	}
}
