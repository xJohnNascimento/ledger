package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/xJohnNascimento/ledger/internal/amount"
	"github.com/xJohnNascimento/ledger/internal/journal"
)

const (
	OutcomeCompleted = "COMPLETED"
	OutcomeRefused   = "REFUSED"
)

type CommandRef struct {
	LedgerID  string
	Namespace string
	Reference string
}

type Command struct {
	CommandRef
	ID            uuid.UUID
	Operation     string
	RequestHash   []byte
	Outcome       string
	Reason        string
	Detail        map[string]string
	EntryID       uuid.NullUUID
	ReservationID uuid.NullUUID
	AccountID     uuid.NullUUID
	CommittedAt   time.Time
}

type PostJournalParams struct {
	Ref             CommandRef
	RequestHash     [32]byte
	AllowedPurposes []string
	AccountingDate  string
	Journal         journal.Journal
}

type Entry struct {
	ID               uuid.UUID
	LedgerID         string
	CommandID        uuid.UUID
	Kind             string
	ProductReference string
	RecordedAt       time.Time
	AccountingDate   string
	CorrectsEntryID  uuid.NullUUID
	Reason           string
	Postings         []Posting
}

type Posting struct {
	EntryID         uuid.UUID
	LineNo          int32
	AccountID       uuid.UUID
	AssetCode       string
	Side            string
	Amount          *big.Int
	AccountSequence int64
}

const commandColumns = `id, ledger_id, namespace, reference, operation, request_hash, outcome,
	coalesce(reason, ''), detail, entry_id, reservation_id, account_id, committed_at`

func (s *Store) PostJournal(ctx context.Context, p PostJournalParams) (Command, error) {
	ids, err := newIDs(2)
	if err != nil {
		return Command{}, err
	}
	accounts, sides, assets, amounts := journalArrays(p.Journal)
	return s.callRoutine(ctx, `ledger.post_journal(
		$1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11::date, $12::text[]::uuid[], $13, $14, $15::text[]::numeric[])`,
		ids[0], p.Ref.LedgerID, p.Ref.Namespace, p.Ref.Reference, p.RequestHash[:], journal.HashVersion,
		p.AllowedPurposes, ids[1], p.Journal.Kind, p.Journal.ProductReference, nullIfEmpty(p.AccountingDate),
		accounts, sides, assets, amounts,
	)
}

func (s *Store) Command(ctx context.Context, ref CommandRef) (Command, error) {
	row := s.pool.QueryRow(ctx, "select "+commandColumns+`
		from ledger.command where ledger_id = $1 and namespace = $2 and reference = $3`,
		ref.LedgerID, ref.Namespace, ref.Reference,
	)
	command, err := scanCommand(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Command{}, &Error{Reason: ErrNotFound.Reason, Message: fmt.Sprintf("command %s/%s not found", ref.Namespace, ref.Reference)}
	}
	return command, err
}

const entryQuery = `
	select e.id, e.ledger_id, e.command_id, e.kind, e.product_reference, e.recorded_at, e.accounting_date::text,
	       e.corrects_entry_id, coalesce(e.reason, ''),
	       coalesce((
	           select json_agg(json_build_object(
	               'line_no', p.line_no, 'account_id', p.account_id, 'asset_code', p.asset_code,
	               'side', p.side, 'amount', p.amount::text, 'account_sequence', p.account_sequence
	           ) order by p.line_no)
	           from ledger.posting p where p.entry_id = e.id
	       ), '[]')
	from ledger.journal_entry e`

func (s *Store) Entry(ctx context.Context, id uuid.UUID) (Entry, error) {
	e, err := scanEntry(s.pool.QueryRow(ctx, entryQuery+" where e.id = $1", id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Entry{}, &Error{Reason: ErrNotFound.Reason, Message: fmt.Sprintf("entry %s not found", id)}
	}
	return e, err
}

func scanEntry(row pgx.Row) (Entry, error) {
	var (
		e        Entry
		postings []byte
	)
	err := row.Scan(&e.ID, &e.LedgerID, &e.CommandID, &e.Kind, &e.ProductReference, &e.RecordedAt, &e.AccountingDate,
		&e.CorrectsEntryID, &e.Reason, &postings)
	if err != nil {
		return Entry{}, err
	}
	var lines []struct {
		LineNo          int32     `json:"line_no"`
		AccountID       uuid.UUID `json:"account_id"`
		AssetCode       string    `json:"asset_code"`
		Side            string    `json:"side"`
		Amount          string    `json:"amount"`
		AccountSequence int64     `json:"account_sequence"`
	}
	if err := json.Unmarshal(postings, &lines); err != nil {
		return Entry{}, err
	}
	e.Postings = make([]Posting, len(lines))
	for i, l := range lines {
		value, err := parseStored(l.Amount)
		if err != nil {
			return Entry{}, err
		}
		e.Postings[i] = Posting{
			EntryID: e.ID, LineNo: l.LineNo, AccountID: l.AccountID, AssetCode: l.AssetCode,
			Side: l.Side, Amount: value, AccountSequence: l.AccountSequence,
		}
	}
	return e, nil
}

func scanCommand(row pgx.Row) (Command, error) {
	var c Command
	err := row.Scan(&c.ID, &c.LedgerID, &c.Namespace, &c.Reference, &c.Operation, &c.RequestHash, &c.Outcome,
		&c.Reason, &c.Detail, &c.EntryID, &c.ReservationID, &c.AccountID, &c.CommittedAt)
	return c, err
}

func scanPosting(row pgx.CollectableRow) (Posting, error) {
	var (
		p      Posting
		amount string
	)
	if err := row.Scan(&p.EntryID, &p.LineNo, &p.AccountID, &p.AssetCode, &p.Side, &amount, &p.AccountSequence); err != nil {
		return Posting{}, err
	}
	var err error
	p.Amount, err = parseStored(amount)
	return p, err
}

func journalArrays(j journal.Journal) (accounts, sides, assets, amounts []string) {
	for _, l := range j.Lines {
		accounts = append(accounts, l.AccountID.String())
		sides = append(sides, string(l.Side))
		assets = append(assets, l.AssetCode)
		amounts = append(amounts, l.Amount.String())
	}
	return accounts, sides, assets, amounts
}

func newIDs(n int) ([]uuid.UUID, error) {
	ids := make([]uuid.UUID, n)
	for i := range ids {
		id, err := uuid.NewV7()
		if err != nil {
			return nil, err
		}
		ids[i] = id
	}
	return ids, nil
}

func parseStored(s string) (*big.Int, error) {
	v, err := amount.ParseSigned(s)
	if err != nil {
		return nil, fmt.Errorf("stored amount %q: %w", s, err)
	}
	return v, nil
}
