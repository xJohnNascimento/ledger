package server

import (
	"net/http"

	"connectrpc.com/connect"
	"connectrpc.com/grpchealth"

	"github.com/xJohnNascimento/ledger/api/ledger/v1/ledgerv1connect"
	"github.com/xJohnNascimento/ledger/internal/export"
	"github.com/xJohnNascimento/ledger/internal/store"
)

const maxRequestBytes = 1 << 20

type Config struct {
	Store        *store.Store
	AuditStore   *store.Store
	Clients      *Clients
	ExportLimits export.Limits
}

func Handler(cfg Config) http.Handler {
	mux := http.NewServeMux()
	mux.Handle(ledgerv1connect.NewLedgerServiceHandler(
		&ledgerService{store: cfg.Store, auditStore: cfg.AuditStore, exportLimits: cfg.ExportLimits.OrDefaults()},
		connect.WithInterceptors(authInterceptor{clients: cfg.Clients}),
		connect.WithReadMaxBytes(maxRequestBytes),
	))
	mux.Handle(grpchealth.NewHandler(grpchealth.NewStaticChecker(ledgerv1connect.LedgerServiceName)))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok\n"))
	})
	return mux
}
