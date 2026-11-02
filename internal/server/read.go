package server

import (
	"context"
	"fmt"

	"connectrpc.com/connect"
	"github.com/google/uuid"

	ledgerv1 "github.com/xJohnNascimento/ledger/api/ledger/v1"
	"github.com/xJohnNascimento/ledger/internal/export"
	"github.com/xJohnNascimento/ledger/internal/store"
	"github.com/xJohnNascimento/ledger/internal/wire"
)

const (
	defaultPostingsLimit = 100
	maxPostingsLimit     = 500
)

func (h *ledgerService) GetBalance(
	ctx context.Context,
	req *connect.Request[ledgerv1.GetBalanceRequest],
) (*connect.Response[ledgerv1.GetBalanceResponse], error) {
	balance, err := h.visibleBalance(ctx, req.Msg.AccountId)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&ledgerv1.GetBalanceResponse{Balance: wire.Balance(balance)}), nil
}

func (h *ledgerService) ListPostings(
	ctx context.Context,
	req *connect.Request[ledgerv1.ListPostingsRequest],
) (*connect.Response[ledgerv1.ListPostingsResponse], error) {
	m := req.Msg
	limit := int(m.Limit)
	if limit == 0 {
		limit = defaultPostingsLimit
	}
	if limit < 0 || limit > maxPostingsLimit || m.AfterSequence < 0 {
		return nil, invalidArgument(fmt.Sprintf("limit must be 1 to %d and after_sequence non-negative", maxPostingsLimit))
	}
	balance, err := h.visibleBalance(ctx, m.AccountId)
	if err != nil {
		return nil, err
	}
	postings, err := h.store.Postings(ctx, balance.AccountID, m.AfterSequence, limit)
	if err != nil {
		return nil, storeError(ctx, err)
	}
	resp := &ledgerv1.ListPostingsResponse{Postings: make([]*ledgerv1.Posting, len(postings))}
	for i, p := range postings {
		resp.Postings[i] = wire.Posting(p)
	}
	if len(postings) == limit {
		resp.NextAfterSequence = postings[len(postings)-1].AccountSequence
	}
	return connect.NewResponse(resp), nil
}

func (h *ledgerService) ExportAuditSnapshot(
	ctx context.Context,
	req *connect.Request[ledgerv1.ExportAuditSnapshotRequest],
	stream *connect.ServerStream[ledgerv1.ExportAuditSnapshotResponse],
) error {
	if err := checkLedger(clientFrom(ctx), req.Msg.LedgerId); err != nil {
		return err
	}
	return export.Stream(ctx, h.auditStore, req.Msg.LedgerId, h.exportLimits, stream.Send)
}

func (h *ledgerService) visibleBalance(ctx context.Context, accountID string) (store.Balance, error) {
	id, err := uuid.Parse(accountID)
	if err != nil {
		return store.Balance{}, reasonError(ledgerv1.Reason_REASON_NOT_FOUND, fmt.Sprintf("account %q not found", accountID), nil)
	}
	balance, err := h.store.Balance(ctx, id)
	if err != nil {
		return store.Balance{}, storeError(ctx, err)
	}
	if balance.LedgerID != clientFrom(ctx).LedgerID {
		return store.Balance{}, reasonError(ledgerv1.Reason_REASON_NOT_FOUND, fmt.Sprintf("account %q not found", accountID), nil)
	}
	return balance, nil
}
