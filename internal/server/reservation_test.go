package server_test

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"connectrpc.com/connect"

	ledgerv1 "github.com/xJohnNascimento/ledger/api/ledger/v1"
)

func leg(account, maxReduction string) *ledgerv1.ReservationLeg {
	return &ledgerv1.ReservationLeg{AccountId: account, MaxReduction: maxReduction}
}

func (h *harness) reserve(command, logical string, legs []*ledgerv1.ReservationLeg, scope ...string) (*ledgerv1.ReserveFundsResponse, error) {
	resp, err := h.worker.ReserveFunds(context.Background(), connect.NewRequest(&ledgerv1.ReserveFundsRequest{
		Command:         &ledgerv1.Reference{Namespace: "corridor.reserve", Reference: command},
		LedgerId:        "corridor",
		ReservationRef:  &ledgerv1.Reference{Namespace: "corridor.reservation", Reference: logical},
		Legs:            legs,
		ScopeAccountIds: scope,
	}))
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}

func (h *harness) commit(command, reservation string, lines ...*ledgerv1.JournalLine) (*ledgerv1.CommitReservationResponse, error) {
	resp, err := h.worker.CommitReservation(context.Background(), connect.NewRequest(&ledgerv1.CommitReservationRequest{
		Command:       &ledgerv1.Reference{Namespace: "corridor.commit", Reference: command},
		ReservationId: reservation,
		Journal:       &ledgerv1.Journal{Kind: "test.commit", ProductReference: "test/" + command, Lines: lines},
	}))
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}

func (h *harness) release(command, reservation string, lines ...*ledgerv1.JournalLine) (*ledgerv1.ReleaseReservationResponse, error) {
	req := &ledgerv1.ReleaseReservationRequest{
		Command:       &ledgerv1.Reference{Namespace: "corridor.release", Reference: command},
		ReservationId: reservation,
	}
	if len(lines) > 0 {
		req.ReturnJournal = &ledgerv1.Journal{Kind: "test.return", ProductReference: "test/" + command, Lines: lines}
	}
	resp, err := h.worker.ReleaseReservation(context.Background(), connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}

func (h *harness) reserved(account string) string {
	h.t.Helper()
	var reserved string
	err := h.db.Owner.QueryRow(context.Background(),
		"select reserved_amount::text from ledger.account_balance where account_id = $1", account,
	).Scan(&reserved)
	if err != nil {
		h.t.Fatal(err)
	}
	return reserved
}

func (h *harness) customer(owner, amount string) string {
	h.t.Helper()
	funds := h.account(h.seed, ledgerv1.OwnerKind_OWNER_KIND_CUSTOMER, owner, "funds", "CAD")
	h.topUp(funds, amount, owner)
	return funds
}

func (h *harness) topUp(funds, amount, reference string) {
	h.t.Helper()
	equity := h.account(h.seed, ledgerv1.OwnerKind_OWNER_KIND_HOUSE, "house", "funding_equity", "CAD")
	if _, err := h.post(h.seed, "seed.funding", reference, debit(equity, amount), credit(funds, amount)); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) reservation(id string) *ledgerv1.Reservation {
	h.t.Helper()
	resp, err := h.worker.GetReservation(context.Background(), connect.NewRequest(&ledgerv1.GetReservationRequest{
		Lookup: &ledgerv1.GetReservationRequest_ReservationId{ReservationId: id},
	}))
	if err != nil {
		h.t.Fatal(err)
	}
	return resp.Msg.Reservation
}

func TestReservationProtectsFundsFromOtherDebits(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	funds, revenue := h.fundedCustomer("100")

	if _, err := h.reserve("protect/1", "protect", []*ledgerv1.ReservationLeg{leg(funds, "80")}, revenue); err != nil {
		t.Fatal(err)
	}
	_, err := h.post(h.worker, "corridor.fees", "after-reserve", debit(funds, "30"), credit(revenue, "30"))
	detail := requireReason(t, err, connect.CodeFailedPrecondition, ledgerv1.Reason_REASON_INSUFFICIENT_FUNDS)
	if detail.Available != "20" {
		t.Fatalf("available %s, want 20", detail.Available)
	}
	if _, err := h.post(h.worker, "corridor.fees", "within-available", debit(funds, "20"), credit(revenue, "20")); err != nil {
		t.Fatal(err)
	}
	if h.balance(funds) != "80" || h.reserved(funds) != "80" {
		t.Fatalf("posted %s reserved %s, want 80 and 80", h.balance(funds), h.reserved(funds))
	}
}

func TestAdmissionAttemptsAdoptOneReservation(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	funds, revenue := h.fundedCustomer("50")
	legs := []*ledgerv1.ReservationLeg{leg(funds, "80")}

	_, err := h.reserve("admit/1", "admit", legs, revenue)
	detail := requireReason(t, err, connect.CodeFailedPrecondition, ledgerv1.Reason_REASON_INSUFFICIENT_FUNDS)
	if detail.Available != "50" || detail.Required != "80" {
		t.Fatalf("refusal %+v, want available 50 and required 80", detail)
	}
	h.topUp(funds, "100", "admit-top-up")
	_, err = h.reserve("admit/1", "admit", legs, revenue)
	requireReason(t, err, connect.CodeFailedPrecondition, ledgerv1.Reason_REASON_INSUFFICIENT_FUNDS)

	created, err := h.reserve("admit/2", "admit", legs, revenue)
	if err != nil {
		t.Fatal(err)
	}
	if created.Reservation.CreatedByCommandId != created.CommandId {
		t.Fatalf("reservation created by %s, want %s", created.Reservation.CreatedByCommandId, created.CommandId)
	}

	const attempts = 8
	var wg sync.WaitGroup
	adopted := make([]string, attempts)
	errs := make([]error, attempts)
	for i := range attempts {
		wg.Go(func() {
			resp, err := h.reserve(fmt.Sprintf("admit/%d", i+3), "admit", []*ledgerv1.ReservationLeg{leg(funds, "80")}, revenue)
			errs[i] = err
			if err == nil {
				adopted[i] = resp.Reservation.Id
			}
		})
	}
	wg.Wait()
	for i := range attempts {
		if errs[i] != nil || adopted[i] != created.Reservation.Id {
			t.Fatalf("attempt %d adopted %q (%v), want %s", i+3, adopted[i], errs[i], created.Reservation.Id)
		}
	}
	if h.reserved(funds) != "80" {
		t.Fatalf("reserved %s, want 80 once", h.reserved(funds))
	}

	_, err = h.reserve("admit/11", "admit", []*ledgerv1.ReservationLeg{leg(funds, "81")}, revenue)
	requireReason(t, err, connect.CodeAlreadyExists, ledgerv1.Reason_REASON_REFERENCE_CONFLICT)

	byRef, err := h.worker.GetReservation(context.Background(), connect.NewRequest(&ledgerv1.GetReservationRequest{
		Lookup: &ledgerv1.GetReservationRequest_ByReference{ByReference: &ledgerv1.ReservationLookup{
			LedgerId:       "corridor",
			ReservationRef: &ledgerv1.Reference{Namespace: "corridor.reservation", Reference: "admit"},
		}},
	}))
	if err != nil || byRef.Msg.Reservation.Id != created.Reservation.Id {
		t.Fatalf("lookup by reference: %v, %v", byRef, err)
	}

	committed, err := h.commit("admit-commit", created.Reservation.Id, debit(funds, "80"), credit(revenue, "80"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = h.reserve("admit/12", "admit", legs, revenue)
	detail = requireReason(t, err, connect.CodeFailedPrecondition, ledgerv1.Reason_REASON_RESERVATION_FINAL)
	if detail.FinalState != ledgerv1.ReservationState_RESERVATION_STATE_COMMITTED || detail.FinalCommandId != committed.CommandId {
		t.Fatalf("final detail %+v, want COMMITTED by %s", detail, committed.CommandId)
	}
}

func TestFinalizationRacesHaveExactlyOneWinner(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	funds, revenue := h.fundedCustomer("1000")
	commits := 0

	for round := range 5 {
		r, err := h.reserve(fmt.Sprintf("race-%d/1", round), fmt.Sprintf("race-%d", round), []*ledgerv1.ReservationLeg{leg(funds, "100")}, revenue)
		if err != nil {
			t.Fatal(err)
		}
		const racers = 8
		var wg sync.WaitGroup
		winners := make([]string, racers)
		errs := make([]error, racers)
		for i := range racers {
			wg.Go(func() {
				command := fmt.Sprintf("race-%d-%d", round, i)
				if i%2 == 0 {
					resp, err := h.commit(command, r.Reservation.Id, debit(funds, "100"), credit(revenue, "100"))
					if errs[i] = err; err == nil {
						winners[i] = resp.CommandId
					}
					return
				}
				resp, err := h.release(command, r.Reservation.Id)
				if errs[i] = err; err == nil {
					winners[i] = resp.CommandId
				}
			})
		}
		wg.Wait()

		winner := -1
		for i := range racers {
			if errs[i] == nil {
				if winner >= 0 {
					t.Fatalf("round %d: racers %d and %d both finalized", round, winner, i)
				}
				winner = i
			}
		}
		if winner < 0 {
			t.Fatalf("round %d: nobody finalized: %v", round, errs)
		}
		final := h.reservation(r.Reservation.Id)
		for i := range racers {
			if i == winner {
				continue
			}
			detail := requireReason(t, errs[i], connect.CodeFailedPrecondition, ledgerv1.Reason_REASON_RESERVATION_FINAL)
			if detail.FinalCommandId != winners[winner] || detail.FinalState != final.State || detail.FinalFingerprint != final.FinalFingerprint {
				t.Fatalf("round %d racer %d: detail %+v, want the winner %s in %v", round, i, detail, winners[winner], final.State)
			}
		}
		if winner%2 == 0 {
			commits++
		}
	}
	if h.reserved(funds) != "0" {
		t.Fatalf("reserved %s after every reservation ended, want 0", h.reserved(funds))
	}
	if want := fmt.Sprint(1000 - 100*commits); h.balance(funds) != want {
		t.Fatalf("funds %s after %d commits, want %s", h.balance(funds), commits, want)
	}
}

func TestProviderCashIsNeverSpentTwice(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	cash := h.account(h.seed, ledgerv1.OwnerKind_OWNER_KIND_PROVIDER, "ditto", "cash", "CAD")
	equity := h.account(h.seed, ledgerv1.OwnerKind_OWNER_KIND_HOUSE, "house", "funding_equity", "CAD")
	difference := h.account(h.worker, ledgerv1.OwnerKind_OWNER_KIND_HOUSE, "house", "fx_difference", "CAD")
	if _, err := h.post(h.seed, "seed.prefunding", "ditto-cad", debit(cash, "1000"), credit(equity, "1000")); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	responses := make([]*ledgerv1.ReserveFundsResponse, 2)
	errs := make([]error, 2)
	for i := range 2 {
		wg.Go(func() {
			responses[i], errs[i] = h.reserve(fmt.Sprintf("cash-%d/1", i), fmt.Sprintf("cash-%d", i),
				[]*ledgerv1.ReservationLeg{leg(cash, "600")}, difference)
		})
	}
	wg.Wait()
	won := 0
	if errs[0] != nil {
		won = 1
	}
	if errs[won] != nil {
		t.Fatalf("no reservation succeeded: %v", errs)
	}
	detail := requireReason(t, errs[1-won], connect.CodeFailedPrecondition, ledgerv1.Reason_REASON_INSUFFICIENT_FUNDS)
	if detail.Available != "400" {
		t.Fatalf("available %s, want 400", detail.Available)
	}

	_, err := h.commit("cash-over", responses[won].Reservation.Id, credit(cash, "601"), debit(difference, "601"))
	requireReason(t, err, connect.CodeFailedPrecondition, ledgerv1.Reason_REASON_CEILING_EXCEEDED)
	if _, err := h.commit("cash-fill", responses[won].Reservation.Id, credit(cash, "590"), debit(difference, "590")); err != nil {
		t.Fatal(err)
	}
	if h.balance(cash) != "410" || h.reserved(cash) != "0" {
		t.Fatalf("cash posted %s reserved %s, want 410 and 0", h.balance(cash), h.reserved(cash))
	}
}

func TestCommitStaysWithinScopeAndCeilings(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	funds, revenue := h.fundedCustomer("100")
	other := h.customer("other-customer", "100")
	outside := h.account(h.worker, ledgerv1.OwnerKind_OWNER_KIND_HOUSE, "house", "spread_revenue", "CAD")
	r, err := h.reserve("scope/1", "scope", []*ledgerv1.ReservationLeg{leg(funds, "50")}, revenue, other)
	if err != nil {
		t.Fatal(err)
	}

	_, err = h.commit("scope-outside", r.Reservation.Id, debit(funds, "50"), credit(outside, "50"))
	requireReason(t, err, connect.CodeInvalidArgument, ledgerv1.Reason_REASON_OUT_OF_SCOPE)
	_, err = h.commit("scope-no-leg", r.Reservation.Id, debit(other, "10"), credit(revenue, "10"))
	requireReason(t, err, connect.CodeInvalidArgument, ledgerv1.Reason_REASON_OUT_OF_SCOPE)
	for _, command := range []string{"scope-outside", "scope-no-leg"} {
		_, err = h.worker.GetCommand(context.Background(), connect.NewRequest(&ledgerv1.GetCommandRequest{
			LedgerId: "corridor", Command: &ledgerv1.Reference{Namespace: "corridor.commit", Reference: command},
		}))
		requireReason(t, err, connect.CodeNotFound, ledgerv1.Reason_REASON_NOT_FOUND)
	}

	for range 2 {
		_, err = h.commit("scope-ceiling", r.Reservation.Id, debit(funds, "60"), credit(revenue, "60"))
		detail := requireReason(t, err, connect.CodeFailedPrecondition, ledgerv1.Reason_REASON_CEILING_EXCEEDED)
		if detail.Ceiling != "50" || detail.Required != "60" || detail.Available != "" {
			t.Fatalf("ceiling detail %+v, want ceiling 50 and required 60", detail)
		}
	}
	if state := h.reservation(r.Reservation.Id).State; state != ledgerv1.ReservationState_RESERVATION_STATE_ACTIVE {
		t.Fatalf("state %v after refused commits, want ACTIVE", state)
	}

	done, err := h.commit("scope-fill", r.Reservation.Id, debit(funds, "40"), credit(revenue, "40"))
	if err != nil {
		t.Fatal(err)
	}
	if done.Reservation.State != ledgerv1.ReservationState_RESERVATION_STATE_COMMITTED || done.Reservation.FinalEntryId != done.Entry.Id {
		t.Fatalf("committed reservation %+v", done.Reservation)
	}
	if h.balance(funds) != "60" || h.reserved(funds) != "0" {
		t.Fatalf("funds posted %s reserved %s, want 60 and 0", h.balance(funds), h.reserved(funds))
	}
}

func TestReleaseCanCarryAReturnJournal(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	funds, _ := h.fundedCustomer("100")
	clearing := h.account(h.worker, ledgerv1.OwnerKind_OWNER_KIND_PAYMENT, "payout-1", "clearing", "CAD")
	if _, err := h.post(h.worker, "corridor.convert", "payout-1", debit(funds, "100"), credit(clearing, "100")); err != nil {
		t.Fatal(err)
	}
	r, err := h.reserve("payout-1/1", "payout-1", []*ledgerv1.ReservationLeg{leg(clearing, "100")}, funds)
	if err != nil {
		t.Fatal(err)
	}

	returned, err := h.release("payout-1-return", r.Reservation.Id, debit(clearing, "100"), credit(funds, "100"))
	if err != nil {
		t.Fatal(err)
	}
	if returned.Entry == nil || returned.Reservation.State != ledgerv1.ReservationState_RESERVATION_STATE_RELEASED {
		t.Fatalf("release answered %+v", returned)
	}
	again, err := h.release("payout-1-return", r.Reservation.Id, debit(clearing, "100"), credit(funds, "100"))
	if err != nil || again.Entry.Id != returned.Entry.Id {
		t.Fatalf("replayed release: %v, %v", again, err)
	}
	if h.balance(funds) != "100" || h.balance(clearing) != "0" || h.reserved(clearing) != "0" {
		t.Fatalf("funds %s clearing %s reserved %s, want 100, 0 and 0", h.balance(funds), h.balance(clearing), h.reserved(clearing))
	}
}

func TestBlockedAccountRefusesNewReservationsButSettlesExistingOnes(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	funds, revenue := h.fundedCustomer("100")
	r, err := h.reserve("blocked/1", "blocked", []*ledgerv1.ReservationLeg{leg(funds, "60")}, revenue)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.db.Owner.Exec(context.Background(), "update ledger.account set status = 'BLOCKED' where id = $1", funds); err != nil {
		t.Fatal(err)
	}

	_, err = h.reserve("blocked-new/1", "blocked-new", []*ledgerv1.ReservationLeg{leg(funds, "10")}, revenue)
	requireReason(t, err, connect.CodeFailedPrecondition, ledgerv1.Reason_REASON_ACCOUNT_BLOCKED)
	if _, err := h.commit("blocked-settle", r.Reservation.Id, debit(funds, "60"), credit(revenue, "60")); err != nil {
		t.Fatalf("an existing reservation must settle on a blocked account: %v", err)
	}
}
