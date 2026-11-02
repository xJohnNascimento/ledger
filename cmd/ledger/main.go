package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"

	ledgeraudit "github.com/xJohnNascimento/ledger/internal/audit"
	"github.com/xJohnNascimento/ledger/internal/export"
	"github.com/xJohnNascimento/ledger/internal/server"
	"github.com/xJohnNascimento/ledger/internal/store"
	"github.com/xJohnNascimento/ledger/migrations"
)

const usage = "usage: ledger serve | migrate | audit ledger-id | healthcheck"

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "ledger:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New(usage)
	}
	switch args[0] {
	case "serve":
		return serve(
			envOr("LEDGER_ADDR", ":9090"),
			os.Getenv("LEDGER_DATABASE_URL"),
			os.Getenv("LEDGER_AUDIT_DATABASE_URL"),
			envOr("LEDGER_CLIENTS_FILE", "/etc/ledger/clients.yaml"),
		)
	case "audit":
		if len(args) != 2 {
			return errors.New(usage)
		}
		return audit(os.Getenv("LEDGER_AUDIT_DATABASE_URL"), args[1])
	case "migrate":
		return migrate(os.Getenv("LEDGER_DATABASE_URL"))
	case "healthcheck":
		return healthcheck(envOr("LEDGER_HEALTH_URL", "http://127.0.0.1:9090/healthz"))
	}
	return fmt.Errorf("unknown command %q; %s", args[0], usage)
}

func serve(addr, databaseURL, auditDatabaseURL, clientsFile string) error {
	if databaseURL == "" || auditDatabaseURL == "" {
		return errors.New("LEDGER_DATABASE_URL and LEDGER_AUDIT_DATABASE_URL are required")
	}
	clients, err := server.LoadClients(clientsFile)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	pool, err := openPool(ctx, databaseURL, os.Getenv("LEDGER_APP_PASSWORD"), appTimeouts)
	if err != nil {
		return err
	}
	defer pool.Close()
	auditPool, err := openPool(ctx, auditDatabaseURL, os.Getenv("LEDGER_AUDIT_PASSWORD"), auditTimeouts)
	if err != nil {
		return err
	}
	defer auditPool.Close()

	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	protocols.SetUnencryptedHTTP2(true)
	srv := &http.Server{
		Addr: addr,
		Handler: server.Handler(server.Config{
			Store:      store.New(pool),
			AuditStore: store.New(auditPool),
			Clients:    clients,
		}),
		Protocols:         protocols,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}

	errs := make(chan error, 1)
	go func() {
		errs <- srv.ListenAndServe()
	}()
	slog.Info("ledger listening", "addr", addr)

	select {
	case err := <-errs:
		return err
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}

func audit(databaseURL, ledgerID string) error {
	if databaseURL == "" {
		return errors.New("LEDGER_AUDIT_DATABASE_URL is required")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	pool, err := openPool(ctx, databaseURL, os.Getenv("LEDGER_AUDIT_PASSWORD"), auditTimeouts)
	if err != nil {
		return err
	}
	defer pool.Close()

	checker := ledgeraudit.NewChecker()
	if err := export.Stream(ctx, store.New(pool), ledgerID, export.DefaultLimits, checker.Add); err != nil {
		return err
	}
	report := checker.Report()
	ledgeraudit.Print(os.Stdout, ledgerID, report)
	if !report.OK() {
		return fmt.Errorf("%d findings", len(report.Findings))
	}
	return nil
}

func migrate(databaseURL string) error {
	if databaseURL == "" {
		return errors.New("LEDGER_DATABASE_URL is required")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	db, err := sql.Open("pgx", databaseURL)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	applied, err := migrations.Up(ctx, db)
	if err != nil {
		return err
	}
	slog.Info("migrations applied", "versions", applied)
	return migrations.SetRolePasswords(ctx, db, map[string]string{
		"ledger_app":   os.Getenv("LEDGER_APP_PASSWORD"),
		"ledger_audit": os.Getenv("LEDGER_AUDIT_PASSWORD"),
	})
}

// The export streams a whole ledger through statements that run as long as the client reads, so
// the audit pool has no statement timeout; the export bounds itself with export.Limits.MaxDuration.
var (
	appTimeouts   = map[string]string{"lock_timeout": "2s", "statement_timeout": "10s"}
	auditTimeouts = map[string]string{"lock_timeout": "2s"}
)

// openPool takes the password apart from the URL, so a secret store injects only the password.
func openPool(ctx context.Context, databaseURL, password string, runtimeParams map[string]string) (*pgxpool.Pool, error) {
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, err
	}
	maps.Copy(config.ConnConfig.RuntimeParams, runtimeParams)
	if password != "" {
		config.ConnConfig.Password = password
	}
	return pgxpool.NewWithConfig(ctx, config)
}

func healthcheck(url string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("health check returned %s", resp.Status)
	}
	return nil
}

func envOr(key, fallback string) string {
	if value, ok := os.LookupEnv(key); ok {
		return value
	}
	return fallback
}
