package wire

import (
	"fmt"

	"google.golang.org/protobuf/types/known/timestamppb"

	ledgerv1 "github.com/xJohnNascimento/ledger/api/ledger/v1"
	"github.com/xJohnNascimento/ledger/internal/store"
)

func Account(a store.Account) *ledgerv1.Account {
	return &ledgerv1.Account{
		Id:               a.ID.String(),
		LedgerId:         a.LedgerID,
		OwnerKind:        ledgerv1.OwnerKind(ledgerv1.OwnerKind_value["OWNER_KIND_"+a.OwnerKind]),
		OwnerId:          a.OwnerID,
		Purpose:          a.Purpose,
		AssetCode:        a.AssetCode,
		AccountKey:       a.AccountKey,
		NormalSide:       ledgerv1.Side(ledgerv1.Side_value["SIDE_"+a.NormalSide]),
		EnforceAvailable: a.EnforceAvailable,
		Status:           ledgerv1.AccountStatus(ledgerv1.AccountStatus_value["ACCOUNT_STATUS_"+a.Status]),
		OverdraftLimit:   a.OverdraftLimit.String(),
	}
}

func Entry(e store.Entry) *ledgerv1.JournalEntry {
	postings := make([]*ledgerv1.Posting, len(e.Postings))
	for i, p := range e.Postings {
		postings[i] = Posting(p)
	}
	entry := &ledgerv1.JournalEntry{
		Id:               e.ID.String(),
		LedgerId:         e.LedgerID,
		CommandId:        e.CommandID.String(),
		Kind:             e.Kind,
		ProductReference: e.ProductReference,
		RecordedAt:       timestamppb.New(e.RecordedAt),
		AccountingDate:   e.AccountingDate,
		Reason:           e.Reason,
		Postings:         postings,
	}
	if e.CorrectsEntryID.Valid {
		entry.CorrectsEntryId = e.CorrectsEntryID.UUID.String()
	}
	return entry
}

func Command(c store.Command) *ledgerv1.Command {
	message := &ledgerv1.Command{
		Id:          c.ID.String(),
		LedgerId:    c.LedgerID,
		Ref:         &ledgerv1.Reference{Namespace: c.Namespace, Reference: c.Reference},
		Operation:   c.Operation,
		RequestHash: fmt.Sprintf("%x", c.RequestHash),
		Outcome:     ledgerv1.CommandOutcome(ledgerv1.CommandOutcome_value["COMMAND_OUTCOME_"+c.Outcome]),
		Reason:      ledgerv1.Reason(ledgerv1.Reason_value["REASON_"+c.Reason]),
		CommittedAt: timestamppb.New(c.CommittedAt),
	}
	if c.EntryID.Valid {
		message.EntryId = c.EntryID.UUID.String()
	}
	if c.ReservationID.Valid {
		message.ReservationId = c.ReservationID.UUID.String()
	}
	if c.AccountID.Valid {
		message.AccountId = c.AccountID.UUID.String()
	}
	return message
}

func Reservation(r store.Reservation) *ledgerv1.Reservation {
	legs := make([]*ledgerv1.ReservationLeg, len(r.Legs))
	for i, leg := range r.Legs {
		legs[i] = &ledgerv1.ReservationLeg{AccountId: leg.AccountID.String(), MaxReduction: leg.MaxReduction.String()}
	}
	scope := make([]string, len(r.Scope))
	for i, account := range r.Scope {
		scope[i] = account.String()
	}
	message := &ledgerv1.Reservation{
		Id:                 r.ID.String(),
		LedgerId:           r.LedgerID,
		ReservationRef:     &ledgerv1.Reference{Namespace: r.Namespace, Reference: r.Reference},
		Legs:               legs,
		ScopeAccountIds:    scope,
		State:              ledgerv1.ReservationState(ledgerv1.ReservationState_value["RESERVATION_STATE_"+r.State]),
		CreatedByCommandId: r.CreatedByCommandID.String(),
		FinalFingerprint:   fmt.Sprintf("%x", r.FinalFingerprint),
	}
	if r.FinalCommandID.Valid {
		message.FinalCommandId = r.FinalCommandID.UUID.String()
	}
	if r.FinalEntryID.Valid {
		message.FinalEntryId = r.FinalEntryID.UUID.String()
	}
	return message
}

func Balance(b store.Balance) *ledgerv1.Balance {
	return &ledgerv1.Balance{
		AccountId:           b.AccountID.String(),
		AssetCode:           b.AssetCode,
		Posted:              b.Posted.String(),
		Reserved:            b.Reserved.String(),
		Available:           b.Available().String(),
		OverdraftLimit:      b.OverdraftLimit.String(),
		Version:             b.Version,
		LastPostingSequence: b.LastPostingSequence,
	}
}

func Posting(p store.Posting) *ledgerv1.Posting {
	return &ledgerv1.Posting{
		EntryId:         p.EntryID.String(),
		LineNo:          p.LineNo,
		AccountId:       p.AccountID.String(),
		AssetCode:       p.AssetCode,
		Side:            ledgerv1.Side(ledgerv1.Side_value["SIDE_"+p.Side]),
		Amount:          p.Amount.String(),
		AccountSequence: p.AccountSequence,
	}
}
