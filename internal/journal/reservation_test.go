package journal_test

import (
	"errors"
	"math/big"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/xJohnNascimento/ledger/internal/journal"
)

func reservation() journal.Reservation {
	return journal.Reservation{
		Namespace: "corridor.conversion",
		Reference: "payment-1",
		Legs: []journal.Leg{
			{AccountID: c, MaxReduction: big.NewInt(500)},
			{AccountID: a, MaxReduction: big.NewInt(1000)},
		},
		Scope: []uuid.UUID{d, b, d},
	}
}

func TestNormalizeSortsLegsAndAddsThemToTheScope(t *testing.T) {
	r := reservation()
	if err := r.Normalize(); err != nil {
		t.Fatal(err)
	}
	if r.Legs[0].AccountID != a || r.Legs[1].AccountID != c {
		t.Fatalf("legs not sorted by account: %v", r.Legs)
	}
	if want := []uuid.UUID{a, b, c, d}; !slices.Equal(r.Scope, want) {
		t.Fatalf("scope %v, want %v", r.Scope, want)
	}
}

func TestNormalizeRefusesBrokenReservations(t *testing.T) {
	cases := map[string]struct {
		edit func(*journal.Reservation)
		want error
	}{
		"no legs":         {func(r *journal.Reservation) { r.Legs = nil }, journal.ErrInvalidReservation},
		"duplicate leg":   {func(r *journal.Reservation) { r.Legs[1].AccountID = c }, journal.ErrInvalidReservation},
		"nil account":     {func(r *journal.Reservation) { r.Legs[0].AccountID = uuid.Nil }, journal.ErrInvalidReservation},
		"nil scope":       {func(r *journal.Reservation) { r.Scope[0] = uuid.Nil }, journal.ErrInvalidReservation},
		"zero ceiling":    {func(r *journal.Reservation) { r.Legs[0].MaxReduction = big.NewInt(0) }, journal.ErrInvalidAmount},
		"missing ceiling": {func(r *journal.Reservation) { r.Legs[0].MaxReduction = nil }, journal.ErrInvalidAmount},
		"too many legs": {func(r *journal.Reservation) {
			r.Legs = nil
			for range journal.MaxLines + 1 {
				r.Legs = append(r.Legs, journal.Leg{AccountID: uuid.New(), MaxReduction: big.NewInt(1)})
			}
		}, journal.ErrInvalidReservation},
	}
	for name, tc := range cases {
		r := reservation()
		tc.edit(&r)
		if err := r.Normalize(); !errors.Is(err, tc.want) {
			t.Errorf("%s: got %v, want %v", name, err, tc.want)
		}
	}
}

func TestReservationHashesIgnoreOrderButNotContent(t *testing.T) {
	first := reservation()
	second := reservation()
	second.Legs[0], second.Legs[1] = second.Legs[1], second.Legs[0]
	second.Scope = []uuid.UUID{b, d}
	for _, r := range []*journal.Reservation{&first, &second} {
		if err := r.Normalize(); err != nil {
			t.Fatal(err)
		}
	}
	if journal.ReservationFingerprint(first) != journal.ReservationFingerprint(second) {
		t.Fatal("the same legs and scope in another order changed the fingerprint")
	}
	if journal.ReserveFundsHash("corridor", first) != journal.ReserveFundsHash("corridor", second) {
		t.Fatal("the same request in another order changed the hash")
	}

	third := reservation()
	third.Legs[0].MaxReduction = big.NewInt(501)
	if err := third.Normalize(); err != nil {
		t.Fatal(err)
	}
	if journal.ReservationFingerprint(first) == journal.ReservationFingerprint(third) {
		t.Fatal("another ceiling kept the fingerprint")
	}

	renamed := first
	renamed.Reference = "payment-2"
	if journal.ReservationFingerprint(first) != journal.ReservationFingerprint(renamed) {
		t.Fatal("the fingerprint must cover legs and scope only")
	}
	if journal.ReserveFundsHash("corridor", first) == journal.ReserveFundsHash("corridor", renamed) {
		t.Fatal("the request hash must cover the reservation reference")
	}
}

func TestFinalizationHashesDifferByOperationAndJournal(t *testing.T) {
	id := uuid.MustParse("01900000-0000-7000-8000-0000000000ff")
	j := valid()
	commit := journal.CommitReservationHash("corridor", id, j)
	release := journal.ReleaseReservationHash("corridor", id, &j)
	bare := journal.ReleaseReservationHash("corridor", id, nil)
	if commit == release || release == bare || commit == bare {
		t.Fatal("commit, release with a journal and a bare release must hash differently")
	}
	if journal.ReleaseReservationHash("corridor", uuid.New(), nil) == bare {
		t.Fatal("the reservation id must be part of the hash")
	}
}
