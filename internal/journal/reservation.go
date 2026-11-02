package journal

import (
	"bytes"
	"errors"
	"fmt"
	"math/big"
	"slices"

	"github.com/google/uuid"
)

var ErrInvalidReservation = errors.New("invalid reservation")

const MaxScope = 100

type Leg struct {
	AccountID    uuid.UUID
	MaxReduction *big.Int
}

type Reservation struct {
	Namespace string
	Reference string
	Legs      []Leg
	Scope     []uuid.UUID
}

// Normalize validates the reservation, sorts its legs by account and makes its scope the sorted
// set of the declared scope and every leg.
func (r *Reservation) Normalize() error {
	if len(r.Legs) == 0 || len(r.Legs) > MaxLines {
		return fmt.Errorf("%w: %d legs; 1 to %d are allowed", ErrInvalidReservation, len(r.Legs), MaxLines)
	}
	scope := make([]uuid.UUID, 0, len(r.Scope)+len(r.Legs))
	for _, leg := range r.Legs {
		if leg.AccountID == uuid.Nil {
			return fmt.Errorf("%w: a leg needs an account", ErrInvalidReservation)
		}
		if leg.MaxReduction == nil || leg.MaxReduction.Sign() <= 0 {
			return fmt.Errorf("%w: max reductions must be positive", ErrInvalidAmount)
		}
		scope = append(scope, leg.AccountID)
	}
	for _, account := range r.Scope {
		if account == uuid.Nil {
			return fmt.Errorf("%w: a scope entry needs an account", ErrInvalidReservation)
		}
		scope = append(scope, account)
	}
	slices.SortFunc(r.Legs, func(x, y Leg) int { return bytes.Compare(x.AccountID[:], y.AccountID[:]) })
	for i := 1; i < len(r.Legs); i++ {
		if r.Legs[i].AccountID == r.Legs[i-1].AccountID {
			return fmt.Errorf("%w: account %s has two legs", ErrInvalidReservation, r.Legs[i].AccountID)
		}
	}
	slices.SortFunc(scope, func(x, y uuid.UUID) int { return bytes.Compare(x[:], y[:]) })
	r.Scope = slices.Compact(scope)
	if len(r.Scope) > MaxScope {
		return fmt.Errorf("%w: %d scope accounts; at most %d are allowed", ErrInvalidReservation, len(r.Scope), MaxScope)
	}
	return nil
}

type canonicalLeg struct {
	Account      string `json:"account"`
	MaxReduction string `json:"max_reduction"`
}

type canonicalShape struct {
	Legs  []canonicalLeg `json:"legs"`
	Scope []string       `json:"scope"`
}

func shape(r Reservation) canonicalShape {
	legs := make([]canonicalLeg, len(r.Legs))
	for i, leg := range r.Legs {
		legs[i] = canonicalLeg{Account: leg.AccountID.String(), MaxReduction: leg.MaxReduction.String()}
	}
	scope := make([]string, len(r.Scope))
	for i, account := range r.Scope {
		scope[i] = account.String()
	}
	return canonicalShape{Legs: legs, Scope: scope}
}

// ReservationFingerprint identifies what a reservation protects, so a later admission attempt can
// adopt it only when it asks for the same legs and scope. Call it on a normalized reservation.
func ReservationFingerprint(r Reservation) [32]byte {
	return hash(struct {
		Version int            `json:"v"`
		Shape   canonicalShape `json:"shape"`
	}{HashVersion, shape(r)})
}

func ReserveFundsHash(ledgerID string, r Reservation) [32]byte {
	return hash(struct {
		Version   int            `json:"v"`
		Operation string         `json:"op"`
		Ledger    string         `json:"ledger"`
		Namespace string         `json:"namespace"`
		Reference string         `json:"reference"`
		Shape     canonicalShape `json:"shape"`
	}{HashVersion, "RESERVE_FUNDS", ledgerID, r.Namespace, r.Reference, shape(r)})
}

func CommitReservationHash(ledgerID string, reservationID uuid.UUID, j Journal) [32]byte {
	return finalizationHash("COMMIT_RESERVATION", ledgerID, reservationID, &j)
}

func ReleaseReservationHash(ledgerID string, reservationID uuid.UUID, returnJournal *Journal) [32]byte {
	return finalizationHash("RELEASE_RESERVATION", ledgerID, reservationID, returnJournal)
}

func finalizationHash(operation, ledgerID string, reservationID uuid.UUID, j *Journal) [32]byte {
	var journal *canonicalJournal
	if j != nil {
		c := canonical(*j)
		journal = &c
	}
	return hash(struct {
		Version     int               `json:"v"`
		Operation   string            `json:"op"`
		Ledger      string            `json:"ledger"`
		Reservation string            `json:"reservation"`
		Journal     *canonicalJournal `json:"journal"`
	}{HashVersion, operation, ledgerID, reservationID.String(), journal})
}
