package server

import (
	"context"
	"fmt"
	"strings"

	"connectrpc.com/connect"
	"github.com/google/uuid"

	ledgerv1 "github.com/xJohnNascimento/ledger/api/ledger/v1"
	"github.com/xJohnNascimento/ledger/internal/amount"
	"github.com/xJohnNascimento/ledger/internal/journal"
	"github.com/xJohnNascimento/ledger/internal/store"
	"github.com/xJohnNascimento/ledger/internal/wire"
)

const maxReasonBytes = 1024

func (h *ledgerService) UpdateAccountControls(
	ctx context.Context,
	req *connect.Request[ledgerv1.UpdateAccountControlsRequest],
) (*connect.Response[ledgerv1.UpdateAccountControlsResponse], error) {
	client := clientFrom(ctx)
	m := req.Msg
	ref, err := commandRef(client, client.LedgerID, m.Command)
	if err != nil {
		return nil, err
	}
	accountID, err := uuid.Parse(m.AccountId)
	if err != nil {
		return nil, reasonError(ledgerv1.Reason_REASON_NOT_FOUND, fmt.Sprintf("account %q not found", m.AccountId), nil)
	}
	if m.Status == nil && m.OverdraftLimit == nil {
		return nil, invalidArgument("status or overdraft_limit is required")
	}
	params := store.ControlsParams{Ref: ref, AccountID: accountID}
	if m.Status != nil {
		if *m.Status == ledgerv1.AccountStatus_ACCOUNT_STATUS_UNSPECIFIED {
			return nil, invalidArgument("status must be ACTIVE or BLOCKED")
		}
		params.Status = strings.TrimPrefix(m.Status.String(), "ACCOUNT_STATUS_")
	}
	overdraft := ""
	if m.OverdraftLimit != nil {
		params.OverdraftLimit, err = amount.Parse(*m.OverdraftLimit)
		if err != nil {
			return nil, reasonError(ledgerv1.Reason_REASON_INVALID_AMOUNT, "overdraft_limit: "+err.Error(), nil)
		}
		overdraft = params.OverdraftLimit.String()
	}
	params.RequestHash = journal.AccountControlsHash(client.LedgerID, accountID, params.Status, overdraft)

	command, err := h.store.UpdateAccountControls(ctx, params)
	if err != nil {
		return nil, storeError(ctx, err)
	}
	if command.Outcome == store.OutcomeRefused {
		return nil, refusal(command)
	}
	account, err := h.store.Account(ctx, command.AccountID.UUID)
	if err != nil {
		return nil, storeError(ctx, err)
	}
	return connect.NewResponse(&ledgerv1.UpdateAccountControlsResponse{CommandId: command.ID.String(), Account: wire.Account(account)}), nil
}

func (h *ledgerService) PostCorrection(
	ctx context.Context,
	req *connect.Request[ledgerv1.PostCorrectionRequest],
) (*connect.Response[ledgerv1.PostCorrectionResponse], error) {
	client := clientFrom(ctx)
	m := req.Msg
	ref, err := commandRef(client, m.LedgerId, m.Command)
	if err != nil {
		return nil, err
	}
	corrects, err := uuid.Parse(m.CorrectsEntryId)
	if err != nil {
		return nil, reasonError(ledgerv1.Reason_REASON_NOT_FOUND, fmt.Sprintf("entry %q not found", m.CorrectsEntryId), nil)
	}
	if !journal.Printable(m.Reason, maxReasonBytes) {
		return nil, invalidArgument(fmt.Sprintf("reason needs 1 to %d bytes of printable UTF-8", maxReasonBytes))
	}
	j, err := parseJournal(m.Journal)
	if err != nil {
		return nil, err
	}
	if err := parseAccountingDate(m.AccountingDate); err != nil {
		return nil, err
	}
	command, err := h.store.PostCorrection(ctx, store.CorrectionParams{
		PostJournalParams: store.PostJournalParams{
			Ref:             ref,
			RequestHash:     journal.PostCorrectionHash(m.LedgerId, m.AccountingDate, j, corrects, m.Reason),
			AllowedPurposes: client.Purposes,
			AccountingDate:  m.AccountingDate,
			Journal:         j,
		},
		CorrectsEntryID: corrects,
		Reason:          m.Reason,
	})
	entry, err := h.completedEntry(ctx, command, err)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&ledgerv1.PostCorrectionResponse{CommandId: command.ID.String(), Entry: entry}), nil
}
