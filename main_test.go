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
