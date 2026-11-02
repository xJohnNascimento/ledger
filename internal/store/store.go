package store

import (
	"context"
	"errors"
	"fmt"
	"math/big"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Store struct {
	pool *pgxpool.Pool
}

type AccountSpec struct {
	LedgerID   string
	OwnerKind  string
	OwnerID    string
	Purpose    string
	AssetCode  string
	AccountKey string
}

type Account struct {
	AccountSpec
	ID               uuid.UUID
	NormalSide       string
	EnforceAvailable bool
	Status           string
	OverdraftLimit   *big.Int
}

func New(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// callRoutine runs a routine that returns a ledger.command row; call is the routine with its
// placeholders, such as "ledger.post_journal($1, $2)".
func (s *Store) callRoutine(ctx context.Context, call string, args ...any) (Command, error) {
	command, err := scanCommand(s.pool.QueryRow(ctx, "select "+commandColumns+" from "+call, args...))
	if err != nil {
		return Command{}, mapError(err)
	}
	return command, nil
}

func nullIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func (s *Store) EnsureAccount(ctx context.Context, spec AccountSpec) (Account, bool, error) {
	newID, err := uuid.NewV7()
	if err != nil {
		return Account{}, false, err
	}
	var (
		id      uuid.UUID
		created bool
	)
	err = s.pool.QueryRow(ctx,
		"select account_id, created from ledger.ensure_account($1, $2, $3, $4, $5, $6, $7)",
		newID, spec.LedgerID, spec.OwnerKind, spec.OwnerID, spec.Purpose, spec.AssetCode, spec.AccountKey,
	).Scan(&id, &created)
	if err != nil {
		return Account{}, false, mapError(err)
	}
	account, err := s.Account(ctx, id)
	return account, created, err
}

const accountQuery = `
	select a.id, a.ledger_id, a.owner_kind, a.owner_id, a.purpose, a.asset_code, a.account_key,
	       a.normal_side, a.enforce_available, a.status, b.overdraft_limit::text
	from ledger.account a
	join ledger.account_balance b on b.account_id = a.id`

func (s *Store) Account(ctx context.Context, id uuid.UUID) (Account, error) {
	a, err := scanAccount(s.pool.QueryRow(ctx, accountQuery+" where a.id = $1", id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Account{}, &Error{Reason: ErrNotFound.Reason, Message: fmt.Sprintf("account %s not found", id)}
	}
	return a, err
}

func scanAccount(row pgx.Row) (Account, error) {
	var (
		a         Account
		overdraft string
	)
	err := row.Scan(
		&a.ID, &a.LedgerID, &a.OwnerKind, &a.OwnerID, &a.Purpose, &a.AssetCode, &a.AccountKey,
		&a.NormalSide, &a.EnforceAvailable, &a.Status, &overdraft,
	)
	if err != nil {
		return Account{}, err
	}
	a.OverdraftLimit, err = parseStored(overdraft)
	return a, err
}
