package server

import (
	"context"
	"errors"
	"fmt"

	"connectrpc.com/connect"
	"github.com/google/uuid"

	ledgerv1 "github.com/xJohnNascimento/ledger/api/ledger/v1"
	"github.com/xJohnNascimento/ledger/internal/amount"
	"github.com/xJohnNascimento/ledger/internal/journal"
	"github.com/xJohnNascimento/ledger/internal/store"
	"github.com/xJohnNascimento/ledger/internal/wire"
)

func (h *ledgerService) ReserveFunds(
	ctx context.Context,
	req *connect.Request[ledgerv1.ReserveFundsRequest],
) (*connect.Response[ledgerv1.ReserveFundsResponse], error) {
	client := clientFrom(ctx)
	m := req.Msg
	ref, err := commandRef(client, m.LedgerId, m.Command)
	if err != nil {
		return nil, err
	}
	r, err := parseReservation(client, m)
	if err != nil {
		return nil, err
	}
	command, err := h.store.ReserveFunds(ctx, store.ReserveParams{
		Ref:             ref,
		RequestHash:     journal.ReserveFundsHash(m.LedgerId, r),
		AllowedPurposes: client.Purposes,
		Reservation:     r,
	})
	if err != nil {
		return nil, storeError(ctx, err)
	}
	if command.Outcome == store.OutcomeRefused {
		return nil, refusal(command)
	}
	reservation, err := h.store.Reservation(ctx, command.ReservationID.UUID)
	if err != nil {
		return nil, storeError(ctx, err)
	}
	return connect.NewResponse(&ledgerv1.ReserveFundsResponse{
		CommandId:   command.ID.String(),
		Reservation: wire.Reservation(reservation),
	}), nil
}

func (h *ledgerService) CommitReservation(
	ctx context.Context,
	req *connect.Request[ledgerv1.CommitReservationRequest],
) (*connect.Response[ledgerv1.CommitReservationResponse], error) {
	client := clientFrom(ctx)
	m := req.Msg
	ref, err := commandRef(client, client.LedgerID, m.Command)
	if err != nil {
		return nil, err
	}
	id, err := parseReservationID(m.ReservationId)
	if err != nil {
		return nil, err
	}
	j, err := parseJournal(m.Journal)
	if err != nil {
		return nil, err
	}
	command, err := h.store.CommitReservation(ctx, store.FinalizeParams{
		Ref:             ref,
		RequestHash:     journal.CommitReservationHash(client.LedgerID, id, j),
		AllowedPurposes: client.Purposes,
		ReservationID:   id,
		Journal:         &j,
	})
	reservation, entry, err := h.finalized(ctx, command, err)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&ledgerv1.CommitReservationResponse{
		CommandId:   command.ID.String(),
		Reservation: reservation,
		Entry:       entry,
	}), nil
}

func (h *ledgerService) ReleaseReservation(
	ctx context.Context,
	req *connect.Request[ledgerv1.ReleaseReservationRequest],
) (*connect.Response[ledgerv1.ReleaseReservationResponse], error) {
	client := clientFrom(ctx)
	m := req.Msg
	ref, err := commandRef(client, client.LedgerID, m.Command)
	if err != nil {
		return nil, err
	}
	id, err := parseReservationID(m.ReservationId)
	if err != nil {
		return nil, err
	}
	var returnJournal *journal.Journal
	if m.ReturnJournal != nil {
		j, err := parseJournal(m.ReturnJournal)
		if err != nil {
			return nil, err
		}
		returnJournal = &j
	}
	command, err := h.store.ReleaseReservation(ctx, store.FinalizeParams{
		Ref:             ref,
		RequestHash:     journal.ReleaseReservationHash(client.LedgerID, id, returnJournal),
		AllowedPurposes: client.Purposes,
		ReservationID:   id,
		Journal:         returnJournal,
	})
	reservation, entry, err := h.finalized(ctx, command, err)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&ledgerv1.ReleaseReservationResponse{
		CommandId:   command.ID.String(),
		Reservation: reservation,
		Entry:       entry,
	}), nil
}

func (h *ledgerService) GetReservation(
	ctx context.Context,
	req *connect.Request[ledgerv1.GetReservationRequest],
) (*connect.Response[ledgerv1.GetReservationResponse], error) {
	client := clientFrom(ctx)
	var (
		reservation store.Reservation
		err         error
	)
	switch lookup := req.Msg.Lookup.(type) {
	case *ledgerv1.GetReservationRequest_ReservationId:
		id, parseErr := parseReservationID(lookup.ReservationId)
		if parseErr != nil {
			return nil, parseErr
		}
		reservation, err = h.store.Reservation(ctx, id)
	case *ledgerv1.GetReservationRequest_ByReference:
		if checkErr := checkLedger(client, lookup.ByReference.GetLedgerId()); checkErr != nil {
			return nil, checkErr
		}
		ref := lookup.ByReference.GetReservationRef()
		reservation, err = h.store.ReservationByRef(ctx, store.CommandRef{
			LedgerID:  client.LedgerID,
			Namespace: ref.GetNamespace(),
			Reference: ref.GetReference(),
		})
	default:
		return nil, invalidArgument("reservation_id or by_reference is required")
	}
	if err != nil {
		return nil, storeError(ctx, err)
	}
	if reservation.LedgerID != client.LedgerID {
		return nil, reasonError(ledgerv1.Reason_REASON_NOT_FOUND, "reservation not found", nil)
	}
	return connect.NewResponse(&ledgerv1.GetReservationResponse{Reservation: wire.Reservation(reservation)}), nil
}

func (h *ledgerService) finalized(ctx context.Context, command store.Command, err error) (*ledgerv1.Reservation, *ledgerv1.JournalEntry, error) {
	if err != nil {
		return nil, nil, storeError(ctx, err)
	}
	if command.Outcome == store.OutcomeRefused {
		return nil, nil, refusal(command)
	}
	reservation, err := h.store.Reservation(ctx, command.ReservationID.UUID)
	if err != nil {
		return nil, nil, storeError(ctx, err)
	}
	if !command.EntryID.Valid {
		return wire.Reservation(reservation), nil, nil
	}
	entry, err := h.store.Entry(ctx, command.EntryID.UUID)
	if err != nil {
		return nil, nil, storeError(ctx, err)
	}
	return wire.Reservation(reservation), wire.Entry(entry), nil
}

func parseReservation(client *Client, m *ledgerv1.ReserveFundsRequest) (journal.Reservation, error) {
	ref := m.ReservationRef
	if err := parseRef(client, ref, "reservation_ref"); err != nil {
		return journal.Reservation{}, err
	}
	r := journal.Reservation{Namespace: ref.Namespace, Reference: ref.Reference}
	for i, leg := range m.Legs {
		account, err := uuid.Parse(leg.AccountId)
		if err != nil {
			return journal.Reservation{}, reasonError(ledgerv1.Reason_REASON_INVALID_RESERVATION, fmt.Sprintf("leg %d: account_id is not a UUID", i+1), nil)
		}
		ceiling, err := amount.Parse(leg.MaxReduction)
		if err != nil {
			return journal.Reservation{}, reasonError(ledgerv1.Reason_REASON_INVALID_AMOUNT, fmt.Sprintf("leg %d: %v", i+1, err), nil)
		}
		r.Legs = append(r.Legs, journal.Leg{AccountID: account, MaxReduction: ceiling})
	}
	for _, id := range m.ScopeAccountIds {
		account, err := uuid.Parse(id)
		if err != nil {
			return journal.Reservation{}, reasonError(ledgerv1.Reason_REASON_INVALID_RESERVATION, fmt.Sprintf("scope account %q is not a UUID", id), nil)
		}
		r.Scope = append(r.Scope, account)
	}
	if err := r.Normalize(); err != nil {
		reason := ledgerv1.Reason_REASON_INVALID_RESERVATION
		if errors.Is(err, journal.ErrInvalidAmount) {
			reason = ledgerv1.Reason_REASON_INVALID_AMOUNT
		}
		return journal.Reservation{}, reasonError(reason, err.Error(), nil)
	}
	return r, nil
}

func parseReservationID(s string) (uuid.UUID, error) {
	id, err := uuid.Parse(s)
	if err != nil {
		return uuid.Nil, reasonError(ledgerv1.Reason_REASON_NOT_FOUND, fmt.Sprintf("reservation %q not found", s), nil)
	}
	return id, nil
}
