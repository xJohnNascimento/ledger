package server

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"

	ledgerv1 "github.com/xJohnNascimento/ledger/api/ledger/v1"
	"github.com/xJohnNascimento/ledger/api/ledger/v1/ledgerv1connect"
	"github.com/xJohnNascimento/ledger/internal/amount"
	"github.com/xJohnNascimento/ledger/internal/export"
	"github.com/xJohnNascimento/ledger/internal/journal"
	"github.com/xJohnNascimento/ledger/internal/store"
	"github.com/xJohnNascimento/ledger/internal/wire"
)

var (
	ownerIDPattern    = regexp.MustCompile(`^[\x21-\x7e]{1,128}$`)
	accountKeyPattern = regexp.MustCompile(`^[\x21-\x7e]{0,128}$`)
)

type ledgerService struct {
	ledgerv1connect.UnimplementedLedgerServiceHandler
	store        *store.Store
	auditStore   *store.Store
	exportLimits export.Limits
}

func (h *ledgerService) EnsureAccount(
	ctx context.Context,
	req *connect.Request[ledgerv1.EnsureAccountRequest],
) (*connect.Response[ledgerv1.EnsureAccountResponse], error) {
	client := clientFrom(ctx)
	m := req.Msg
	if err := checkLedger(client, m.LedgerId); err != nil {
		return nil, err
	}
	if m.OwnerKind == ledgerv1.OwnerKind_OWNER_KIND_UNSPECIFIED {
		return nil, invalidArgument("owner_kind is required")
	}
	if !ownerIDPattern.MatchString(m.OwnerId) || !accountKeyPattern.MatchString(m.AccountKey) || !journal.ValidAsset(m.AssetCode) {
		return nil, invalidArgument("owner_id needs 1 to 128 printable ASCII characters, account_key up to 128, asset_code three capital letters")
	}
	if !client.AllowsPurpose(m.Purpose) {
		return nil, reasonError(ledgerv1.Reason_REASON_PURPOSE_NOT_ALLOWED, fmt.Sprintf("purpose %q is outside the client's scope", m.Purpose), nil)
	}
	account, created, err := h.store.EnsureAccount(ctx, store.AccountSpec{
		LedgerID:   m.LedgerId,
		OwnerKind:  strings.TrimPrefix(m.OwnerKind.String(), "OWNER_KIND_"),
		OwnerID:    m.OwnerId,
		Purpose:    m.Purpose,
		AssetCode:  m.AssetCode,
		AccountKey: m.AccountKey,
	})
	if err != nil {
		return nil, storeError(ctx, err)
	}
	return connect.NewResponse(&ledgerv1.EnsureAccountResponse{Account: wire.Account(account), Created: created}), nil
}

func (h *ledgerService) PostJournal(
	ctx context.Context,
	req *connect.Request[ledgerv1.PostJournalRequest],
) (*connect.Response[ledgerv1.PostJournalResponse], error) {
	client := clientFrom(ctx)
	m := req.Msg
	ref, err := commandRef(client, m.LedgerId, m.Command)
	if err != nil {
		return nil, err
	}
	j, err := parseJournal(m.Journal)
	if err != nil {
		return nil, err
	}
	if err := parseAccountingDate(m.AccountingDate); err != nil {
		return nil, err
	}
	command, err := h.store.PostJournal(ctx, store.PostJournalParams{
		Ref:             ref,
		RequestHash:     journal.PostJournalHash(m.LedgerId, m.AccountingDate, j),
		AllowedPurposes: client.Purposes,
		AccountingDate:  m.AccountingDate,
		Journal:         j,
	})
	entry, err := h.completedEntry(ctx, command, err)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&ledgerv1.PostJournalResponse{CommandId: command.ID.String(), Entry: entry}), nil
}

func (h *ledgerService) GetCommand(
	ctx context.Context,
	req *connect.Request[ledgerv1.GetCommandRequest],
) (*connect.Response[ledgerv1.GetCommandResponse], error) {
	m := req.Msg
	if err := checkLedger(clientFrom(ctx), m.LedgerId); err != nil {
		return nil, err
	}
	if m.Command == nil {
		return nil, invalidArgument("command is required")
	}
	command, err := h.store.Command(ctx, store.CommandRef{LedgerID: m.LedgerId, Namespace: m.Command.Namespace, Reference: m.Command.Reference})
	if err != nil {
		return nil, storeError(ctx, err)
	}
	return connect.NewResponse(&ledgerv1.GetCommandResponse{Command: wire.Command(command)}), nil
}

func checkLedger(client *Client, ledgerID string) error {
	if ledgerID != client.LedgerID {
		return reasonError(ledgerv1.Reason_REASON_NOT_FOUND, fmt.Sprintf("ledger %q not found", ledgerID), nil)
	}
	return nil
}

func commandRef(client *Client, ledgerID string, ref *ledgerv1.Reference) (store.CommandRef, error) {
	if err := checkLedger(client, ledgerID); err != nil {
		return store.CommandRef{}, err
	}
	if err := parseRef(client, ref, "command"); err != nil {
		return store.CommandRef{}, err
	}
	return store.CommandRef{LedgerID: ledgerID, Namespace: ref.Namespace, Reference: ref.Reference}, nil
}

func parseRef(client *Client, ref *ledgerv1.Reference, field string) error {
	if ref == nil || !namespacePattern.MatchString(ref.Namespace) || len(ref.Namespace) > 128 || !journal.Printable(ref.Reference, 256) {
		return invalidArgument(field + " needs a dotted lowercase namespace and a reference of 1 to 256 bytes")
	}
	if !client.AllowsNamespace(ref.Namespace) {
		return reasonError(ledgerv1.Reason_REASON_NAMESPACE_NOT_ALLOWED,
			fmt.Sprintf("namespace %q is outside the client's scope", ref.Namespace), nil)
	}
	return nil
}

func parseAccountingDate(date string) error {
	if date == "" {
		return nil
	}
	if _, err := time.Parse(time.DateOnly, date); err != nil {
		return invalidArgument("accounting_date must be YYYY-MM-DD")
	}
	return nil
}

func parseJournal(m *ledgerv1.Journal) (journal.Journal, error) {
	if m == nil {
		return journal.Journal{}, reasonError(ledgerv1.Reason_REASON_INVALID_JOURNAL, "journal is required", nil)
	}
	j := journal.Journal{Kind: m.Kind, ProductReference: m.ProductReference, Lines: make([]journal.Line, 0, len(m.Lines))}
	for i, l := range m.Lines {
		accountID, err := uuid.Parse(l.AccountId)
		if err != nil {
			return journal.Journal{}, reasonError(ledgerv1.Reason_REASON_INVALID_JOURNAL, fmt.Sprintf("line %d: account_id is not a UUID", i+1), nil)
		}
		value, err := amount.Parse(l.Amount)
		if err != nil {
			return journal.Journal{}, reasonError(ledgerv1.Reason_REASON_INVALID_AMOUNT, fmt.Sprintf("line %d: %v", i+1, err), nil)
		}
		j.Lines = append(j.Lines, journal.Line{
			AccountID: accountID,
			Side:      journal.Side(strings.TrimPrefix(l.Side.String(), "SIDE_")),
			AssetCode: l.AssetCode,
			Amount:    value,
		})
	}
	if err := j.Validate(); err != nil {
		reason := ledgerv1.Reason_REASON_INVALID_JOURNAL
		if errors.Is(err, journal.ErrInvalidAmount) {
			reason = ledgerv1.Reason_REASON_INVALID_AMOUNT
		}
		return journal.Journal{}, reasonError(reason, err.Error(), nil)
	}
	return j, nil
}

func (h *ledgerService) completedEntry(ctx context.Context, command store.Command, err error) (*ledgerv1.JournalEntry, error) {
	if err != nil {
		return nil, storeError(ctx, err)
	}
	if command.Outcome == store.OutcomeRefused {
		return nil, refusal(command)
	}
	entry, err := h.store.Entry(ctx, command.EntryID.UUID)
	if err != nil {
		return nil, storeError(ctx, err)
	}
	return wire.Entry(entry), nil
}
