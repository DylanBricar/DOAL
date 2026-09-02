package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
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
	if got := fetchPublicIPFrom(context.Background(), server.Client(), []string{server.URL + "/valid"}); got != "203.0.113.8" {
		t.Fatalf("valid provider response produced %q", got)
	}
}
