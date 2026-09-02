package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"doal/config"
)

func TestFetchPublicIPFromValidatesStatusAndAddress(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/valid":
			_, _ = w.Write([]byte("203.0.113.8\n"))
		case "/status":
			http.Error(w, "198.51.100.4", http.StatusBadGateway)
		default:
			_, _ = w.Write([]byte("not-an-ip"))
		}
	}))
	defer server.Close()

	if got := fetchPublicIPFrom(context.Background(), server.Client(), []string{server.URL + "/status", server.URL + "/invalid"}); got != "" {
		t.Fatalf("invalid provider responses produced %q", got)
	}
	for i := 0; i < 100; i++ {
		if got := fetchPublicIPFrom(context.Background(), server.Client(), []string{server.URL + "/valid"}); got != "203.0.113.8" {
			t.Fatalf("iteration %d: valid provider response produced %q", i, got)
		}
	}
}

func TestGetConfigReturnsIndependentSnapshot(t *testing.T) {
	engine := &Engine{cfg: &config.Config{Client: "original.client", DHTBootstrapNodes: []string{"node.example:6881"}}}
	snapshot := engine.GetConfig()
	snapshot.Client = "changed.client"
	snapshot.DHTBootstrapNodes[0] = "changed.example:6881"

	if engine.cfg.Client != "original.client" || engine.cfg.DHTBootstrapNodes[0] != "node.example:6881" {
		t.Fatalf("GetConfig exposed mutable engine state: %+v", engine.cfg)
	}
}

func TestSelectExplicitResumeEvictionIsDeterministic(t *testing.T) {
	t.Parallel()

	active := []string{"bbbb", "aaaa"}
	if got := selectExplicitResumeEviction(active, 2); got != "bbbb" {
		t.Fatalf("eviction = %q, want lexicographically last active hash", got)
	}
	if got := selectExplicitResumeEviction(active, 3); got != "" {
		t.Fatalf("eviction below capacity = %q, want empty", got)
	}
}

func TestRestartBoundConfigDetectsNetworkPolicyChanges(t *testing.T) {
	t.Parallel()

	current := &config.Config{Client: "client.client"}
	next := cloneConfig(current)
	next.AllowPrivateNetworks = true
	if !restartBoundConfigChanged(current, next) {
		t.Fatal("private-network policy change was not marked restart-bound")
	}
	next = cloneConfig(current)
	next.MaxUploadRate = 500
	if restartBoundConfigChanged(current, next) {
		t.Fatal("live bandwidth-only change was incorrectly marked restart-bound")
	}
}
