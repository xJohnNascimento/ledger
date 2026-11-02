package store_test

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"

	"github.com/xJohnNascimento/ledger/internal/pgtest"
	"github.com/xJohnNascimento/ledger/internal/store"
)

func TestMain(m *testing.M) {
	os.Exit(pgtest.Main(m))
}

func TestEnsureAccountCreatesOnceAndThenReturnsTheSameAccount(t *testing.T) {
	t.Parallel()
	s := store.New(pgtest.NewDatabase(t).App)
	ctx := context.Background()
	spec := store.AccountSpec{
		LedgerID:  "corridor",
		OwnerKind: "PROVIDER",
		OwnerID:   "ditto",
		Purpose:   "cash",
		AssetCode: "CAD",
	}

	first, created, err := s.EnsureAccount(ctx, spec)
	if err != nil || !created {
		t.Fatalf("first call: created %v, err %v", created, err)
	}
	if first.NormalSide != "DEBIT" || !first.EnforceAvailable || first.Status != "ACTIVE" || first.OverdraftLimit.Sign() != 0 {
		t.Fatalf("classification not taken from the catalog: %+v", first)
	}
	if first.ID.Version() != 7 {
		t.Fatalf("account id %s is not a UUIDv7", first.ID)
	}

	again, created, err := s.EnsureAccount(ctx, spec)
	if err != nil || created {
		t.Fatalf("second call: created %v, err %v", created, err)
	}
	if again.ID != first.ID {
		t.Fatalf("second call returned %s, want %s", again.ID, first.ID)
	}

	spec.AccountKey = "settlement"
	other, created, err := s.EnsureAccount(ctx, spec)
	if err != nil || !created || other.ID == first.ID {
		t.Fatalf("another account key must create another account: created %v, err %v", created, err)
	}
}

func TestEnsureAccountRefusesWhatTheCatalogDoesNotAllow(t *testing.T) {
	t.Parallel()
	s := store.New(pgtest.NewDatabase(t).App)
	ctx := context.Background()
	valid := store.AccountSpec{LedgerID: "corridor", OwnerKind: "CUSTOMER", OwnerID: "c-1", Purpose: "funds", AssetCode: "BRL"}

	cases := map[string]struct {
		edit func(*store.AccountSpec)
		want error
	}{
		"owner kind outside the purpose": {func(s *store.AccountSpec) { s.OwnerKind = "HOUSE" }, store.ErrClassificationConflict},
		"unknown purpose":                {func(s *store.AccountSpec) { s.Purpose = "loans" }, store.ErrClassificationConflict},
		"unknown ledger":                 {func(s *store.AccountSpec) { s.LedgerID = "nowhere" }, store.ErrNotFound},
		"unknown asset":                  {func(s *store.AccountSpec) { s.AssetCode = "JPY" }, store.ErrNotFound},
	}
	for name, c := range cases {
		spec := valid
		c.edit(&spec)
		if _, _, err := s.EnsureAccount(ctx, spec); !errors.Is(err, c.want) {
			t.Errorf("%s: got %v, want %v", name, err, c.want)
		}
	}
}

func TestEnsureAccountUnderConcurrencyCreatesExactlyOne(t *testing.T) {
	t.Parallel()
	db := pgtest.NewDatabase(t)
	s := store.New(db.App)
	ctx := context.Background()
	spec := store.AccountSpec{LedgerID: "corridor", OwnerKind: "PAYMENT", OwnerID: "p-1", Purpose: "clearing", AssetCode: "EUR"}

	const callers = 24
	var (
		wg      sync.WaitGroup
		start   = make(chan struct{})
		results = make([]store.Account, callers)
		created = make([]bool, callers)
		errs    = make([]error, callers)
	)
	for i := range callers {
		wg.Go(func() {
			<-start
			results[i], created[i], errs[i] = s.EnsureAccount(ctx, spec)
		})
	}
	close(start)
	wg.Wait()

	creations := 0
	for i := range callers {
		if errs[i] != nil {
			t.Fatalf("caller %d: %v", i, errs[i])
		}
		if results[i].ID != results[0].ID {
			t.Fatalf("caller %d got %s, caller 0 got %s", i, results[i].ID, results[0].ID)
		}
		if created[i] {
			creations++
		}
	}
	if creations != 1 {
		t.Fatalf("%d callers reported a creation, want 1", creations)
	}
	var accounts, balances int
	err := db.Owner.QueryRow(ctx,
		"select (select count(*) from ledger.account), (select count(*) from ledger.account_balance)",
	).Scan(&accounts, &balances)
	if err != nil || accounts != 1 || balances != 1 {
		t.Fatalf("accounts %d, balances %d, err %v; want one of each", accounts, balances, err)
	}
}
