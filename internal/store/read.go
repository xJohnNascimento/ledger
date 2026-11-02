package store

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type Balance struct {
	AccountID           uuid.UUID
	LedgerID            string
	AssetCode           string
	Posted              *big.Int
	Reserved            *big.Int
	OverdraftLimit      *big.Int
	Version             int64
	LastPostingSequence int64
}

func (b Balance) Available() *big.Int {
	available := new(big.Int).Sub(b.Posted, b.Reserved)
	return available.Add(available, b.OverdraftLimit)
}

type SnapshotVisitor interface {
	Header(capturedAt time.Time) error
	Account(Account) error
	Balance(Balance) error
	Entry(Entry) error
	Reservation(Reservation) error
	Command(Command) error
}

const balanceQuery = `
	select b.account_id, a.ledger_id, a.asset_code, b.posted_balance::text, b.reserved_amount::text,
	       b.overdraft_limit::text, b.version, b.last_posting_sequence
	from ledger.account_balance b
	join ledger.account a on a.id = b.account_id`

func (s *Store) Balance(ctx context.Context, accountID uuid.UUID) (Balance, error) {
	b, err := scanBalance(s.pool.QueryRow(ctx, balanceQuery+" where b.account_id = $1", accountID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Balance{}, &Error{Reason: ErrNotFound.Reason, Message: fmt.Sprintf("account %s not found", accountID)}
	}
	return b, err
}

func (s *Store) Postings(ctx context.Context, accountID uuid.UUID, afterSequence int64, limit int) ([]Posting, error) {
	rows, err := s.pool.Query(ctx, `
		select entry_id, line_no, account_id, asset_code, side, amount::text, account_sequence
		from ledger.posting
		where account_id = $1 and account_sequence > $2
		order by account_sequence
		limit $3`,
		accountID, afterSequence, limit,
	)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, scanPosting)
}

// Snapshot reads one repeatable-read, read-only view of a ledger and hands every row to the
// visitor in a fixed order: accounts, balances, entries, reservations and commands, each by id.
func (s *Store) Snapshot(ctx context.Context, ledgerID string, v SnapshotVisitor) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var capturedAt time.Time
	if err := tx.QueryRow(ctx, "select now()").Scan(&capturedAt); err != nil {
		return err
	}
	if err := v.Header(capturedAt); err != nil {
		return err
	}
	if err := visit(ctx, tx, accountQuery+" where a.ledger_id = $1 order by a.id", ledgerID, scanAccount, v.Account); err != nil {
		return err
	}
	if err := visit(ctx, tx, balanceQuery+" where a.ledger_id = $1 order by b.account_id", ledgerID, scanBalance, v.Balance); err != nil {
		return err
	}
	if err := visit(ctx, tx, entryQuery+" where e.ledger_id = $1 order by e.id", ledgerID, scanEntry, v.Entry); err != nil {
		return err
	}
	if err := visit(ctx, tx, reservationQuery+" where r.ledger_id = $1 order by r.id", ledgerID, scanReservation, v.Reservation); err != nil {
		return err
	}
	if err := visit(ctx, tx, "select "+commandColumns+" from ledger.command where ledger_id = $1 order by id", ledgerID, scanCommand, v.Command); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func visit[T any](ctx context.Context, tx pgx.Tx, query, ledgerID string, scan func(pgx.Row) (T, error), fn func(T) error) error {
	rows, err := tx.Query(ctx, query, ledgerID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		item, err := scan(rows)
		if err != nil {
			return err
		}
		if err := fn(item); err != nil {
			return err
		}
	}
	return rows.Err()
}

func scanBalance(row pgx.Row) (Balance, error) {
	var (
		b                           Balance
		posted, reserved, overdraft string
	)
	err := row.Scan(&b.AccountID, &b.LedgerID, &b.AssetCode, &posted, &reserved, &overdraft, &b.Version, &b.LastPostingSequence)
	if err != nil {
		return Balance{}, err
	}
	for _, field := range []struct {
		dst **big.Int
		src string
	}{{&b.Posted, posted}, {&b.Reserved, reserved}, {&b.OverdraftLimit, overdraft}} {
		if *field.dst, err = parseStored(field.src); err != nil {
			return Balance{}, err
		}
	}
	return b, nil
}
