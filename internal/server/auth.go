package server

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"connectrpc.com/connect"

	ledgerv1 "github.com/xJohnNascimento/ledger/api/ledger/v1"
	"github.com/xJohnNascimento/ledger/api/ledger/v1/ledgerv1connect"
)

var requiredScope = map[string]Scope{
	ledgerv1connect.LedgerServiceEnsureAccountProcedure:         ScopeProvision,
	ledgerv1connect.LedgerServicePostJournalProcedure:           ScopeMoney,
	ledgerv1connect.LedgerServiceReserveFundsProcedure:          ScopeMoney,
	ledgerv1connect.LedgerServiceCommitReservationProcedure:     ScopeMoney,
	ledgerv1connect.LedgerServiceReleaseReservationProcedure:    ScopeMoney,
	ledgerv1connect.LedgerServiceGetReservationProcedure:        ScopeRead,
	ledgerv1connect.LedgerServiceGetCommandProcedure:            ScopeRead,
	ledgerv1connect.LedgerServiceGetBalanceProcedure:            ScopeRead,
	ledgerv1connect.LedgerServiceListPostingsProcedure:          ScopeRead,
	ledgerv1connect.LedgerServiceUpdateAccountControlsProcedure: ScopeOperator,
	ledgerv1connect.LedgerServicePostCorrectionProcedure:        ScopeOperator,
	ledgerv1connect.LedgerServiceExportAuditSnapshotProcedure:   ScopeAudit,
}

type clientKey struct{}

func clientFrom(ctx context.Context) *Client {
	return ctx.Value(clientKey{}).(*Client)
}

type authInterceptor struct {
	clients *Clients
}

func (i authInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		client, err := i.authorize(req.Spec().Procedure, req.Header())
		if err != nil {
			return nil, err
		}
		return next(context.WithValue(ctx, clientKey{}, client), req)
	}
}

func (i authInterceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

func (i authInterceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		client, err := i.authorize(conn.Spec().Procedure, conn.RequestHeader())
		if err != nil {
			return err
		}
		return next(context.WithValue(ctx, clientKey{}, client), conn)
	}
}

func (i authInterceptor) authorize(procedure string, header http.Header) (*Client, error) {
	scheme, secret, ok := strings.Cut(header.Get("Authorization"), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("a bearer credential is required"))
	}
	client, ok := i.clients.Authenticate(secret)
	if !ok {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("unknown credential"))
	}
	scope, ok := requiredScope[procedure]
	if !ok || !client.Has(scope) {
		return nil, reasonError(ledgerv1.Reason_REASON_SCOPE_NOT_ALLOWED, "the client lacks the "+string(scope)+" scope", nil)
	}
	return client, nil
}
