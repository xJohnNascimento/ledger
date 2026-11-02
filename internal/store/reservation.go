package store

import (
	"context"
	"errors"
	"fmt"
	"math/big"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/xJohnNascimento/ledger/internal/journal"
)

type ReserveParams struct {
	Ref             CommandRef
	RequestHash     [32]byte
	AllowedPurposes []string
	Reservation     journal.Reservation
}

type FinalizeParams struct {
	Ref             CommandRef
	RequestHash     [32]byte
	AllowedPurposes []string
	ReservationID   uuid.UUID
	Journal         *journal.Journal
}

type Leg struct {
	AccountID    uuid.UUID
	MaxReduction *big.Int
}

type Reservation struct {
	ID                 uuid.UUID
	LedgerID           string
	Namespace          string
	Reference          string
	Legs               []Leg
	Scope              []uuid.UUID
	State              string
	CreatedByCommandID uuid.UUID
	FinalCommandID     uuid.NullUUID
	FinalEntryID       uuid.NullUUID
	FinalFingerprint   []byte
}

func (s *Store) ReserveFunds(ctx context.Context, p ReserveParams) (Command, error) {
	ids, err := newIDs(2)
	if err != nil {
		return Command{}, err
	}
	r := p.Reservation
	fingerprint := journal.ReservationFingerprint(r)
	legAccounts := make([]string, len(r.Legs))
	ceilings := make([]string, len(r.Legs))
	for i, leg := range r.Legs {
		legAccounts[i] = leg.AccountID.String()
		ceilings[i] = leg.MaxReduction.String()
	}
	scope := make([]string, len(r.Scope))
	for i, account := range r.Scope {
		scope[i] = account.String()
	}
	return s.callRoutine(ctx, `ledger.reserve_funds(
		$1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12::text[]::uuid[], $13::text[]::numeric[], $14::text[]::uuid[])`,
		ids[0], p.Ref.LedgerID, p.Ref.Namespace, p.Ref.Reference, p.RequestHash[:], journal.HashVersion,
		p.AllowedPurposes, ids[1], r.Namespace, r.Reference, fingerprint[:], legAccounts, ceilings, scope,
	)
}

func (s *Store) CommitReservation(ctx context.Context, p FinalizeParams) (Command, error) {
	return s.finalize(ctx, "commit_reservation", p)
}

func (s *Store) ReleaseReservation(ctx context.Context, p FinalizeParams) (Command, error) {
	return s.finalize(ctx, "release_reservation", p)
}

func (s *Store) finalize(ctx context.Context, routine string, p FinalizeParams) (Command, error) {
	ids, err := newIDs(2)
	if err != nil {
		return Command{}, err
	}
	var j journal.Journal
	if p.Journal != nil {
		j = *p.Journal
	}
	accounts, sides, assets, amounts := journalArrays(j)
	return s.callRoutine(ctx, "ledger."+routine+`(
		$1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12::text[]::uuid[], $13::text[], $14::text[], $15::text[]::numeric[])`,
		ids[0], p.Ref.LedgerID, p.Ref.Namespace, p.Ref.Reference, p.RequestHash[:], journal.HashVersion,
		p.AllowedPurposes, p.ReservationID, ids[1], j.Kind, j.ProductReference,
		nonNil(accounts), nonNil(sides), nonNil(assets), nonNil(amounts),
	)
}

const reservationQuery = `
	select r.id, r.ledger_id, r.namespace, r.reference, r.state, r.created_by_command_id,
	       r.final_command_id, r.final_entry_id, r.final_fingerprint,
	       array(select g.account_id from ledger.reservation_leg g where g.reservation_id = r.id order by g.account_id),
	       array(select g.max_reduction::text from ledger.reservation_leg g where g.reservation_id = r.id order by g.account_id),
	       array(select s.account_id from ledger.reservation_scope s where s.reservation_id = r.id order by s.account_id)
	from ledger.reservation r`

func (s *Store) Reservation(ctx context.Context, id uuid.UUID) (Reservation, error) {
	r, err := scanReservation(s.pool.QueryRow(ctx, reservationQuery+" where r.id = $1", id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Reservation{}, &Error{Reason: ErrNotFound.Reason, Message: fmt.Sprintf("reservation %s not found", id)}
	}
	return r, err
}

func (s *Store) ReservationByRef(ctx context.Context, ref CommandRef) (Reservation, error) {
	r, err := scanReservation(s.pool.QueryRow(ctx,
		reservationQuery+" where r.ledger_id = $1 and r.namespace = $2 and r.reference = $3",
		ref.LedgerID, ref.Namespace, ref.Reference,
	))
	if errors.Is(err, pgx.ErrNoRows) {
		return Reservation{}, &Error{Reason: ErrNotFound.Reason, Message: fmt.Sprintf("reservation %s/%s not found", ref.Namespace, ref.Reference)}
	}
	return r, err
}

func scanReservation(row pgx.Row) (Reservation, error) {
	var (
		r           Reservation
		legAccounts []uuid.UUID
		ceilings    []string
	)
	err := row.Scan(&r.ID, &r.LedgerID, &r.Namespace, &r.Reference, &r.State, &r.CreatedByCommandID,
		&r.FinalCommandID, &r.FinalEntryID, &r.FinalFingerprint, &legAccounts, &ceilings, &r.Scope)
	if err != nil {
		return Reservation{}, err
	}
	r.Legs = make([]Leg, len(legAccounts))
	for i := range legAccounts {
		ceiling, err := parseStored(ceilings[i])
		if err != nil {
			return Reservation{}, err
		}
		r.Legs[i] = Leg{AccountID: legAccounts[i], MaxReduction: ceiling}
	}
	return r, nil
}

func nonNil(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}
