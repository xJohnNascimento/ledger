package migrations_test

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"os"
	"slices"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"pgregory.net/rapid"

	"github.com/xJohnNascimento/ledger/internal/amount"
	"github.com/xJohnNascimento/ledger/internal/pgtest"
)

const (
	checkViolation      = "23514"
	foreignKeyViolation = "23503"
	insufficientPriv    = "42501"
	entryLegs           = "LD201"
	entryUnbalanced     = "LD202"
	entryClosed         = "LD203"
	appendOnly          = "LD204"
	commandWithoutEnd   = "LD205"
)

func TestMain(m *testing.M) {
	os.Exit(pgtest.Main(m))
}

func TestAmountDomainsAgreeWithTheGoParser(t *testing.T) {
	t.Parallel()
	db := pgtest.NewDatabase(t)
	ctx := context.Background()
	span := new(big.Int).Lsh(big.NewInt(1), 129)
	low := new(big.Int).Neg(new(big.Int).Lsh(big.NewInt(1), 128))

	rapid.Check(t, func(t *rapid.T) {
		v := new(big.Int).SetBytes(rapid.SliceOfN(rapid.Byte(), 17, 17).Draw(t, "bytes"))
		v.Mod(v, span).Add(v, low)
		if rapid.Bool().Draw(t, "nearBound") {
			v.Rsh(v, 1)
		}
		s := v.String()

		_, signedErr := amount.ParseSigned(s)
		_, err := db.Owner.Exec(ctx, "select $1::numeric::ledger.signed_amount", s)
		if (signedErr == nil) != (err == nil) {
			t.Fatalf("signed %s: Go %v, PostgreSQL %v", s, signedErr, err)
		}
		_, unsignedErr := amount.Parse(s)
		_, err = db.Owner.Exec(ctx, "select $1::numeric::ledger.amount", s)
		if (unsignedErr == nil) != (err == nil) {
			t.Fatalf("amount %s: Go %v, PostgreSQL %v", s, unsignedErr, err)
		}
	})
}

func TestAmountDomainsRefuseFractionsAndSpecialValues(t *testing.T) {
	t.Parallel()
	db := pgtest.NewDatabase(t)
	ctx := context.Background()

	for _, value := range []string{"1.5", "1.0", "0.1", "-0.0", "NaN", "Infinity", "-Infinity"} {
		for _, domain := range []string{"ledger.amount", "ledger.signed_amount"} {
			_, err := db.Owner.Exec(ctx, fmt.Sprintf("select $1::numeric::%s", domain), value)
			requireState(t, err, checkViolation)
		}
	}
	_, err := db.Owner.Exec(ctx, "select '-1'::numeric::ledger.amount")
	requireState(t, err, checkViolation)
}

func TestEntryNeedsTwoToFiftyPostings(t *testing.T) {
	t.Parallel()
	db := pgtest.NewDatabase(t)
	usd := newAccount(t, db, "HOUSE", "fee_revenue", "USD")
	usd2 := newAccount(t, db, "HOUSE", "funding_equity", "USD")

	err := asOwner(t, db, func(tx pgx.Tx) error {
		return writeEntry(tx)
	})
	requireState(t, err, entryLegs)

	err = asOwner(t, db, func(tx pgx.Tx) error {
		return writeEntry(tx, line{usd, "DEBIT", "USD", "0"})
	})
	requireState(t, err, checkViolation)

	err = asOwner(t, db, func(tx pgx.Tx) error {
		return writeEntry(tx, line{usd, "DEBIT", "USD", "100"})
	})
	requireState(t, err, entryLegs)

	lines := make([]line, 0, 51)
	for range 25 {
		lines = append(lines, line{usd, "DEBIT", "USD", "1"}, line{usd2, "CREDIT", "USD", "1"})
	}
	err = asOwner(t, db, func(tx pgx.Tx) error {
		return writeEntry(tx, lines...)
	})
	requireNoError(t, err)

	lines = append(lines, line{usd, "DEBIT", "USD", "1"})
	err = asOwner(t, db, func(tx pgx.Tx) error {
		return writeEntry(tx, lines...)
	})
	requireState(t, err, checkViolation)
}

func TestEntryBalancesPerAsset(t *testing.T) {
	t.Parallel()
	db := pgtest.NewDatabase(t)
	usdFee := newAccount(t, db, "HOUSE", "fee_revenue", "USD")
	usdEquity := newAccount(t, db, "HOUSE", "funding_equity", "USD")
	cadFee := newAccount(t, db, "HOUSE", "fee_revenue", "CAD")
	cadEquity := newAccount(t, db, "HOUSE", "funding_equity", "CAD")

	cases := map[string][]line{
		"short credit": {
			{usdFee, "DEBIT", "USD", "100"},
			{usdEquity, "CREDIT", "USD", "60"},
		},
		"balanced only across assets": {
			{usdFee, "DEBIT", "USD", "100"},
			{cadEquity, "CREDIT", "CAD", "100"},
		},
		"one asset balanced, the other not": {
			{usdFee, "DEBIT", "USD", "100"},
			{usdEquity, "CREDIT", "USD", "100"},
			{cadFee, "DEBIT", "CAD", "135"},
			{cadEquity, "CREDIT", "CAD", "134"},
		},
	}
	for name, lines := range cases {
		err := asOwner(t, db, func(tx pgx.Tx) error {
			return writeEntry(tx, lines...)
		})
		if !hasState(err, entryUnbalanced) {
			t.Errorf("%s: want SQLSTATE %s, got %v", name, entryUnbalanced, err)
		}
	}

	err := asOwner(t, db, func(tx pgx.Tx) error {
		return writeEntry(tx,
			line{usdFee, "DEBIT", "USD", "100"},
			line{usdEquity, "CREDIT", "USD", "100"},
			line{cadFee, "DEBIT", "CAD", "170141183460469231731687303715884105727"},
			line{cadEquity, "CREDIT", "CAD", "170141183460469231731687303715884105727"},
		)
	})
	requireNoError(t, err)
	requireCount(t, db, "select count(*) from ledger.posting", 4)
}

func TestPostingsCannotJoinACommittedEntry(t *testing.T) {
	t.Parallel()
	db := pgtest.NewDatabase(t)
	fee := newAccount(t, db, "HOUSE", "fee_revenue", "USD")
	equity := newAccount(t, db, "HOUSE", "funding_equity", "USD")
	entry := committedEntry(t, db, fee, equity)

	err := asOwner(t, db, func(tx pgx.Tx) error {
		return insertPosting(tx, entry, 3, line{fee, "DEBIT", "USD", "1"})
	})
	requireState(t, err, entryClosed)
}

func TestJournalIsAppendOnly(t *testing.T) {
	t.Parallel()
	db := pgtest.NewDatabase(t)
	fee := newAccount(t, db, "HOUSE", "fee_revenue", "USD")
	equity := newAccount(t, db, "HOUSE", "funding_equity", "USD")
	entry := committedEntry(t, db, fee, equity)

	for _, statement := range []string{
		"update ledger.posting set amount = 1 where entry_id = $1",
		"delete from ledger.posting where entry_id = $1",
		"update ledger.journal_entry set kind = 'edited' where id = $1",
		"delete from ledger.journal_entry where id = $1",
		"update ledger.command set reason = 'edited' where id = (select command_id from ledger.journal_entry where id = $1)",
		"delete from ledger.command where id = (select command_id from ledger.journal_entry where id = $1)",
	} {
		err := asOwner(t, db, func(tx pgx.Tx) error {
			_, err := tx.Exec(context.Background(), statement, entry)
			return err
		})
		if !hasState(err, appendOnly) {
			t.Errorf("%s: want SQLSTATE %s, got %v", statement, appendOnly, err)
		}
	}
	for _, table := range []string{"posting", "journal_entry", "command", "asset", "purpose", "ledger", "account", "account_balance"} {
		_, err := db.Owner.Exec(context.Background(), "truncate ledger."+table+" cascade")
		if !hasState(err, appendOnly) {
			t.Errorf("truncate %s: want SQLSTATE %s, got %v", table, appendOnly, err)
		}
	}
	for _, statement := range []string{
		"update ledger.asset set scale = 3 where code = 'USD'",
		"delete from ledger.asset where code = 'USD'",
		"update ledger.purpose set enforce_available = false where code = 'funds'",
		"update ledger.ledger set legal_entity_key = 'other' where id = 'corridor'",
	} {
		_, err := db.Owner.Exec(context.Background(), statement)
		if !hasState(err, appendOnly) {
			t.Errorf("%s: want SQLSTATE %s, got %v", statement, appendOnly, err)
		}
	}
}

func TestPostingMustMatchItsAccountLedgerAndAsset(t *testing.T) {
	t.Parallel()
	db := pgtest.NewDatabase(t)
	ctx := context.Background()
	_, err := db.Owner.Exec(ctx, "insert into ledger.ledger (id, legal_entity_key, name) values ('other', 'other', 'Other')")
	requireNoError(t, err)
	fee := newAccount(t, db, "HOUSE", "fee_revenue", "USD")
	equity := newAccount(t, db, "HOUSE", "funding_equity", "USD")
	var foreign string
	err = db.Owner.QueryRow(ctx,
		"select account_id from ledger.ensure_account($1, 'other', 'HOUSE', 'house', 'funding_equity', 'USD', '')",
		uuid.NewString(),
	).Scan(&foreign)
	requireNoError(t, err)

	err = asOwner(t, db, func(tx pgx.Tx) error {
		return writeEntry(tx, line{fee, "DEBIT", "CAD", "100"}, line{equity, "CREDIT", "CAD", "100"})
	})
	requireState(t, err, foreignKeyViolation)

	err = asOwner(t, db, func(tx pgx.Tx) error {
		return writeEntry(tx, line{fee, "DEBIT", "USD", "100"}, line{foreign, "CREDIT", "USD", "100"})
	})
	requireState(t, err, foreignKeyViolation)
}

func TestCommandOutcomeIsWrittenOnceInItsOwnTransaction(t *testing.T) {
	t.Parallel()
	db := pgtest.NewDatabase(t)
	ctx := context.Background()

	err := asOwner(t, db, func(tx pgx.Tx) error {
		_, err := insertCommand(tx, uuid.NewString(), false)
		return err
	})
	requireState(t, err, commandWithoutEnd)

	id := uuid.NewString()
	err = asOwner(t, db, func(tx pgx.Tx) error {
		if _, err := insertCommand(tx, id, false); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, "update ledger.command set outcome = 'REFUSED', reason = 'INSUFFICIENT_FUNDS', committed_at = now() where id = $1", id)
		return err
	})
	requireNoError(t, err)

	err = asOwner(t, db, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, "update ledger.command set reason = 'ACCOUNT_BLOCKED' where id = $1", id)
		return err
	})
	requireState(t, err, appendOnly)
}

func TestAccountClassificationIsImmutable(t *testing.T) {
	t.Parallel()
	db := pgtest.NewDatabase(t)
	ctx := context.Background()
	funds := newAccount(t, db, "CUSTOMER", "funds", "USD")

	for _, statement := range []string{
		"update ledger.account set purpose = 'cash' where id = $1",
		"update ledger.account set owner_id = 'someone-else' where id = $1",
		"update ledger.account set enforce_available = false where id = $1",
		"delete from ledger.account where id = $1",
		"delete from ledger.account_balance where account_id = $1",
	} {
		_, err := db.Owner.Exec(ctx, statement, funds)
		if !hasState(err, appendOnly) {
			t.Errorf("%s: want SQLSTATE %s, got %v", statement, appendOnly, err)
		}
	}
	_, err := db.Owner.Exec(ctx, "update ledger.account set status = 'BLOCKED' where id = $1", funds)
	requireNoError(t, err)
}

func TestEnforcedAccountsNeverGoBelowAvailable(t *testing.T) {
	t.Parallel()
	db := pgtest.NewDatabase(t)
	ctx := context.Background()
	funds := newAccount(t, db, "CUSTOMER", "funds", "USD")
	fee := newAccount(t, db, "HOUSE", "fee_revenue", "USD")

	_, err := db.Owner.Exec(ctx, "update ledger.account_balance set posted_balance = -1 where account_id = $1", funds)
	requireState(t, err, checkViolation)
	_, err = db.Owner.Exec(ctx, "update ledger.account_balance set posted_balance = 10, reserved_amount = 11 where account_id = $1", funds)
	requireState(t, err, checkViolation)
	_, err = db.Owner.Exec(ctx, "update ledger.account_balance set overdraft_limit = 10, posted_balance = -10 where account_id = $1", funds)
	requireNoError(t, err)
	_, err = db.Owner.Exec(ctx, "update ledger.account_balance set posted_balance = -1 where account_id = $1", fee)
	requireNoError(t, err)
	_, err = db.Owner.Exec(ctx, "update ledger.account_balance set enforce_available = false where account_id = $1", funds)
	requireState(t, err, foreignKeyViolation)
}

func TestRuntimeRolesCannotWriteAnyTable(t *testing.T) {
	t.Parallel()
	db := pgtest.NewDatabase(t)
	ctx := context.Background()

	for _, role := range []string{"ledger_app", "ledger_audit"} {
		var writable []string
		err := db.Owner.QueryRow(ctx, `
			select coalesce(array_agg(n.nspname || '.' || c.relname order by n.nspname, c.relname), '{}')
			from pg_class c
			join pg_namespace n on n.oid = c.relnamespace
			where c.relkind in ('r', 'p', 'v', 'm', 'f')
			  and n.nspname not in ('pg_catalog', 'information_schema')
			  and n.nspname not like 'pg_toast%'
			  and has_table_privilege($1, c.oid, 'INSERT, UPDATE, DELETE, TRUNCATE, REFERENCES, TRIGGER')`,
			role,
		).Scan(&writable)
		requireNoError(t, err)
		if len(writable) > 0 {
			t.Errorf("%s can write %v", role, writable)
		}
	}

	for role, pool := range map[string]*pgxpool.Pool{"ledger_app": db.App, "ledger_audit": db.Audit} {
		for _, statement := range []string{
			"insert into ledger.asset (code, scale) values ('JPY', 0)",
			"update ledger.account_balance set posted_balance = 0",
			"delete from ledger.posting",
			"truncate ledger.posting",
			"alter table ledger.posting disable trigger all",
			"create table ledger.shadow (id int)",
			"create table public.shadow (id int)",
		} {
			_, err := pool.Exec(ctx, statement)
			if !hasState(err, insufficientPriv) {
				t.Errorf("%s: %s: want SQLSTATE %s, got %v", role, statement, insufficientPriv, err)
			}
		}
		requireCount(t, db, "select count(*) from ledger.asset", 4)
	}
}

func TestOnlyTheAppRoleExecutesRoutines(t *testing.T) {
	t.Parallel()
	db := pgtest.NewDatabase(t)
	ctx := context.Background()

	executable := func(role string) []string {
		var names []string
		err := db.Owner.QueryRow(ctx, `
			select coalesce(array_agg(p.proname::text order by p.proname), '{}')
			from pg_proc p
			join pg_namespace n on n.oid = p.pronamespace
			where n.nspname = 'ledger' and has_function_privilege($1, p.oid, 'EXECUTE')`,
			role,
		).Scan(&names)
		requireNoError(t, err)
		return names
	}
	if got, want := executable("ledger_app"), []string{
		"commit_reservation", "ensure_account", "post_correction", "post_journal",
		"release_reservation", "reserve_funds", "update_account_controls",
	}; !slices.Equal(got, want) {
		t.Errorf("ledger_app executes %v, want %v", got, want)
	}
	if got := executable("ledger_audit"); len(got) > 0 {
		t.Errorf("ledger_audit executes %v, want nothing", got)
	}
	if got := executable("public"); len(got) > 0 {
		t.Errorf("PUBLIC executes %v, want nothing", got)
	}

	var definers []string
	err := db.Owner.QueryRow(ctx, `
		select coalesce(array_agg(p.proname::text order by p.proname), '{}')
		from pg_proc p
		join pg_namespace n on n.oid = p.pronamespace
		where n.nspname = 'ledger'
		  and p.prosecdef
		  and not coalesce(p.proconfig @> array['search_path=ledger, pg_temp'], false)`,
	).Scan(&definers)
	requireNoError(t, err)
	if len(definers) > 0 {
		t.Errorf("security definer routines without a pinned search_path: %v", definers)
	}
}

type line struct {
	account string
	side    string
	asset   string
	amount  string
}

func newAccount(t *testing.T, db pgtest.Database, ownerKind, purpose, asset string) string {
	t.Helper()
	var id string
	err := db.Owner.QueryRow(context.Background(),
		"select account_id from ledger.ensure_account($1, 'corridor', $2, $3, $4, $5, '')",
		uuid.NewString(), ownerKind, uuid.NewString(), purpose, asset,
	).Scan(&id)
	requireNoError(t, err)
	return id
}

func committedEntry(t *testing.T, db pgtest.Database, debit, credit string) string {
	t.Helper()
	var entry string
	err := asOwner(t, db, func(tx pgx.Tx) error {
		var err error
		entry, err = insertEntry(tx, line{debit, "DEBIT", "USD", "100"}, line{credit, "CREDIT", "USD", "100"})
		return err
	})
	requireNoError(t, err)
	return entry
}

func asOwner(t *testing.T, db pgtest.Database, write func(pgx.Tx) error) error {
	t.Helper()
	ctx := context.Background()
	tx, err := db.Owner.Begin(ctx)
	requireNoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()
	_, err = tx.Exec(ctx, "set local role ledger_owner")
	requireNoError(t, err)
	if err := write(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func writeEntry(tx pgx.Tx, lines ...line) error {
	_, err := insertEntry(tx, lines...)
	return err
}

func insertEntry(tx pgx.Tx, lines ...line) (string, error) {
	ctx := context.Background()
	commandID, err := insertCommand(tx, uuid.NewString(), true)
	if err != nil {
		return "", err
	}
	entry := uuid.NewString()
	_, err = tx.Exec(ctx,
		"insert into ledger.journal_entry (id, ledger_id, command_id, kind, product_reference) values ($1, 'corridor', $2, 'test', 'test')",
		entry, commandID,
	)
	if err != nil {
		return "", err
	}
	for i, l := range lines {
		if err := insertPosting(tx, entry, i+1, l); err != nil {
			return "", err
		}
	}
	return entry, nil
}

func insertCommand(tx pgx.Tx, id string, completed bool) (string, error) {
	outcome, committedAt := "", ""
	if completed {
		outcome, committedAt = "COMPLETED", "now"
	}
	_, err := tx.Exec(context.Background(), `
		insert into ledger.command (id, ledger_id, namespace, reference, operation, request_hash, hash_version, outcome, committed_at)
		values ($1::text::uuid, 'corridor', 'test', $1::text, 'POST_JOURNAL', sha256(convert_to($1::text, 'UTF8')), 1, nullif($2, ''), nullif($3, '')::timestamptz)`,
		id, outcome, committedAt,
	)
	return id, err
}

func insertPosting(tx pgx.Tx, entry string, lineNo int, l line) error {
	_, err := tx.Exec(context.Background(), `
		insert into ledger.posting (entry_id, line_no, ledger_id, account_id, asset_code, side, amount, account_sequence)
		values ($1, $2, 'corridor', $3, $4, $5, $6::numeric,
		        (select coalesce(max(account_sequence), 0) + 1 from ledger.posting where account_id = $3))`,
		entry, lineNo, l.account, l.asset, l.side, l.amount,
	)
	return err
}

func requireCount(t *testing.T, db pgtest.Database, query string, want int) {
	t.Helper()
	var got int
	requireNoError(t, db.Owner.QueryRow(context.Background(), query).Scan(&got))
	if got != want {
		t.Fatalf("%s = %d, want %d", query, got, want)
	}
}

func requireState(t *testing.T, err error, state string) {
	t.Helper()
	if !hasState(err, state) {
		t.Fatalf("want SQLSTATE %s, got %v", state, err)
	}
}

func hasState(err error, state string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == state
}

func requireNoError(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
