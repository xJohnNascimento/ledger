package server_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/xJohnNascimento/ledger/api/ledger/v1/ledgerv1connect"
	"github.com/xJohnNascimento/ledger/internal/server"
)

func TestHealthzReturnsOK(t *testing.T) {
	srv := httptest.NewServer(server.Handler(server.Config{}))
	t.Cleanup(srv.Close)

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+"/healthz", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
}

func TestGRPCHealthReportsLedgerServiceServing(t *testing.T) {
	srv := httptest.NewServer(server.Handler(server.Config{}))
	t.Cleanup(srv.Close)

	body := strings.NewReader(`{"service":"` + ledgerv1connect.LedgerServiceName + `"}`)
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL+"/grpc.health.v1.Health/Check", body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "SERVING") {
		t.Fatalf("health check = %s, want SERVING", got)
	}
}
