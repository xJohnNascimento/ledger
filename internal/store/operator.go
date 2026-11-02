package store

import (
	"context"
	"math/big"

	"github.com/google/uuid"

	"github.com/xJohnNascimento/ledger/internal/journal"
)

type CorrectionParams struct {
	PostJournalParams
	CorrectsEntryID uuid.UUID
	Reason          string
}

type ControlsParams struct {
	Ref            CommandRef
	RequestHash    [32]byte
	AccountID      uuid.UUID
	Status         string
	OverdraftLimit *big.Int
}

func (s *Store) PostCorrection(ctx context.Context, p CorrectionParams) (Command, error) {
	ids, err := newIDs(2)
	if err != nil {
		return Command{}, err
	}
	accounts, sides, assets, amounts := journalArrays(p.Journal)
	return s.callRoutine(ctx, `ledger.post_correction(
		$1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11::date, $12::text[]::uuid[], $13, $14, $15::text[]::numeric[], $16, $17)`,
		ids[0], p.Ref.LedgerID, p.Ref.Namespace, p.Ref.Reference, p.RequestHash[:], journal.HashVersion,
		p.AllowedPurposes, ids[1], p.Journal.Kind, p.Journal.ProductReference, nullIfEmpty(p.AccountingDate),
		accounts, sides, assets, amounts, p.CorrectsEntryID, p.Reason,
	)
}

func (s *Store) UpdateAccountControls(ctx context.Context, p ControlsParams) (Command, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return Command{}, err
	}
	var overdraft *string
	if p.OverdraftLimit != nil {
		limit := p.OverdraftLimit.String()
		overdraft = &limit
	}
	return s.callRoutine(ctx, "ledger.update_account_controls($1, $2, $3, $4, $5, $6, $7, $8, $9::numeric)",
		id, p.Ref.LedgerID, p.Ref.Namespace, p.Ref.Reference, p.RequestHash[:], journal.HashVersion,
		p.AccountID, nullIfEmpty(p.Status), overdraft,
	)
}
