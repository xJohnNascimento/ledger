package server_test

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	ledgerv1 "github.com/xJohnNascimento/ledger/api/ledger/v1"
	"github.com/xJohnNascimento/ledger/api/ledger/v1/ledgerv1connect"
	"github.com/xJohnNascimento/ledger/internal/audit"
)

func (h *harness) controls(command, account string, status ledgerv1.AccountStatus, overdraft string) (*ledgerv1.UpdateAccountControlsResponse, error) {
	req := &ledgerv1.UpdateAccountControlsRequest{
		Command:   &ledgerv1.Reference{Namespace: "operator.controls", Reference: command},
		AccountId: account,
	}
	if status != ledgerv1.AccountStatus_ACCOUNT_STATUS_UNSPECIFIED {
		req.Status = &status
	}
	if overdraft != "" {
		req.OverdraftLimit = &overdraft
	}
	resp, err := h.operator.UpdateAccountControls(context.Background(), connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}

func (h *harness) correct(command, corrects, reason string, lines ...*ledgerv1.JournalLine) (*ledgerv1.PostCorrectionResponse, error) {
	resp, err := h.operator.PostCorrection(context.Background(), connect.NewRequest(&ledgerv1.PostCorrectionRequest{
		Command:         &ledgerv1.Reference{Namespace: "operator.corrections", Reference: command},
		LedgerId:        "corridor",
		Journal:         &ledgerv1.Journal{Kind: "correction", ProductReference: "correction/" + command, Lines: lines},
		CorrectsEntryId: corrects,
		Reason:          reason,
	}))
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}

func TestOperatorBlocksAndUnblocksAnAccount(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	funds, revenue := h.fundedCustomer("100")
	existing, err := h.reserve("ops/1", "ops", []*ledgerv1.ReservationLeg{leg(funds, "60")}, revenue)
	if err != nil {
		t.Fatal(err)
	}

	blocked, err := h.controls("block-1", funds, ledgerv1.AccountStatus_ACCOUNT_STATUS_BLOCKED, "")
	if err != nil {
		t.Fatal(err)
	}
	if blocked.Account.Status != ledgerv1.AccountStatus_ACCOUNT_STATUS_BLOCKED {
		t.Fatalf("status %v, want BLOCKED", blocked.Account.Status)
	}
	again, err := h.controls("block-1", funds, ledgerv1.AccountStatus_ACCOUNT_STATUS_BLOCKED, "")
	if err != nil || again.CommandId != blocked.CommandId {
		t.Fatalf("replayed control: %v, %v", again, err)
	}

	_, err = h.reserve("ops-new/1", "ops-new", []*ledgerv1.ReservationLeg{leg(funds, "10")}, revenue)
	requireReason(t, err, connect.CodeFailedPrecondition, ledgerv1.Reason_REASON_ACCOUNT_BLOCKED)
	if _, err := h.commit("ops-settle", existing.Reservation.Id, debit(funds, "60"), credit(revenue, "60")); err != nil {
		t.Fatalf("a blocked account must settle its existing reservation: %v", err)
	}

	if _, err := h.controls("unblock-1", funds, ledgerv1.AccountStatus_ACCOUNT_STATUS_ACTIVE, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := h.reserve("ops-new/2", "ops-new", []*ledgerv1.ReservationLeg{leg(funds, "10")}, revenue); err != nil {
		t.Fatalf("an unblocked account must accept reservations: %v", err)
	}
}

func TestOverdraftExtendsAvailabilityButNeverStrandsAnAccount(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	funds, revenue := h.fundedCustomer("100")

	if _, err := h.controls("od-1", funds, ledgerv1.AccountStatus_ACCOUNT_STATUS_UNSPECIFIED, "50"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.post(h.worker, "corridor.fees", "od-debit", debit(funds, "140"), credit(revenue, "140")); err != nil {
		t.Fatalf("a debit within the overdraft was refused: %v", err)
	}
	for range 2 {
		_, err := h.controls("od-2", funds, ledgerv1.AccountStatus_ACCOUNT_STATUS_UNSPECIFIED, "30")
		detail := requireReason(t, err, connect.CodeFailedPrecondition, ledgerv1.Reason_REASON_INSUFFICIENT_FUNDS)
		if detail.Available != "10" || detail.Required != "20" {
			t.Fatalf("refusal %+v, want available 10 and required 20", detail)
		}
	}
	if _, err := h.controls("od-3", funds, ledgerv1.AccountStatus_ACCOUNT_STATUS_UNSPECIFIED, "40"); err != nil {
		t.Fatal(err)
	}
	if b := h.getBalance(funds); b.Posted != "-40" || b.Available != "0" || b.OverdraftLimit != "40" {
		t.Fatalf("balance %+v, want posted -40, available 0, overdraft 40", b)
	}

	_, err := h.operator.UpdateAccountControls(context.Background(), connect.NewRequest(&ledgerv1.UpdateAccountControlsRequest{
		Command: &ledgerv1.Reference{Namespace: "operator.controls", Reference: "od-empty"}, AccountId: funds,
	}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("a control without a change: %v", err)
	}
}

func TestCorrectionsNameTheEntryTheyCorrect(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	funds, revenue := h.fundedCustomer("100")
	original, err := h.post(h.worker, "corridor.fees", "double-charge", debit(funds, "30"), credit(revenue, "30"))
	if err != nil {
		t.Fatal(err)
	}

	fixed, err := h.correct("c-1", original.Entry.Id, "fee charged twice", debit(revenue, "30"), credit(funds, "30"))
	if err != nil {
		t.Fatal(err)
	}
	if fixed.Entry.CorrectsEntryId != original.Entry.Id || fixed.Entry.Reason != "fee charged twice" {
		t.Fatalf("correction entry %+v, want it to name %s with its reason", fixed.Entry, original.Entry.Id)
	}
	if h.balance(funds) != "100" {
		t.Fatalf("funds %s after the correction, want 100", h.balance(funds))
	}

	_, err = h.correct("c-2", original.Entry.Id, "", debit(revenue, "1"), credit(funds, "1"))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("a correction without a reason: %v", err)
	}
	_, err = h.correct("c-3", "01900000-0000-7000-8000-000000000000", "no such entry", debit(revenue, "1"), credit(funds, "1"))
	requireReason(t, err, connect.CodeNotFound, ledgerv1.Reason_REASON_NOT_FOUND)
	if report := h.audit(); !report.OK() {
		t.Fatalf("audit: %v", report.Findings)
	}
}

func TestMoneyAndOperatorScopesStaySeparate(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	funds, revenue := h.fundedCustomer("100")
	entry, err := h.post(h.worker, "corridor.fees", "scoped", debit(funds, "1"), credit(revenue, "1"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	blocked := ledgerv1.AccountStatus_ACCOUNT_STATUS_BLOCKED

	_, err = h.worker.UpdateAccountControls(ctx, connect.NewRequest(&ledgerv1.UpdateAccountControlsRequest{
		Command: &ledgerv1.Reference{Namespace: "corridor.controls", Reference: "x"}, AccountId: funds, Status: &blocked,
	}))
	requireReason(t, err, connect.CodePermissionDenied, ledgerv1.Reason_REASON_SCOPE_NOT_ALLOWED)
	_, err = h.worker.PostCorrection(ctx, connect.NewRequest(&ledgerv1.PostCorrectionRequest{
		Command: &ledgerv1.Reference{Namespace: "corridor.corrections", Reference: "x"}, LedgerId: "corridor",
		CorrectsEntryId: entry.Entry.Id, Reason: "x",
		Journal: &ledgerv1.Journal{Kind: "correction", ProductReference: "x", Lines: []*ledgerv1.JournalLine{debit(revenue, "1"), credit(funds, "1")}},
	}))
	requireReason(t, err, connect.CodePermissionDenied, ledgerv1.Reason_REASON_SCOPE_NOT_ALLOWED)
	_, err = h.export(h.worker)
	requireReason(t, err, connect.CodePermissionDenied, ledgerv1.Reason_REASON_SCOPE_NOT_ALLOWED)

	_, err = h.post(h.operator, "operator.corrections", "x", debit(revenue, "1"), credit(funds, "1"))
	requireReason(t, err, connect.CodePermissionDenied, ledgerv1.Reason_REASON_SCOPE_NOT_ALLOWED)
	_, err = h.operator.EnsureAccount(ctx, connect.NewRequest(&ledgerv1.EnsureAccountRequest{
		LedgerId: "corridor", OwnerKind: ledgerv1.OwnerKind_OWNER_KIND_HOUSE, OwnerId: "house", Purpose: "fee_revenue", AssetCode: "USD",
	}))
	requireReason(t, err, connect.CodePermissionDenied, ledgerv1.Reason_REASON_SCOPE_NOT_ALLOWED)
}

func TestLedgersDoNotSeeEachOther(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := context.Background()
	if _, err := h.db.Owner.Exec(ctx, "insert into ledger.ledger (id, legal_entity_key, name) values ('other', 'other', 'Other')"); err != nil {
		t.Fatal(err)
	}
	funds, revenue := h.fundedCustomer("100")
	r, err := h.reserve("visible/1", "visible", []*ledgerv1.ReservationLeg{leg(funds, "10")}, revenue)
	if err != nil {
		t.Fatal(err)
	}

	otherFunds := h.ensureIn(h.stranger, "other", ledgerv1.OwnerKind_OWNER_KIND_CUSTOMER, "c-1", "funds")
	otherEquity := h.ensureIn(h.stranger, "other", ledgerv1.OwnerKind_OWNER_KIND_HOUSE, "house", "funding_equity")
	_, err = h.stranger.PostJournal(ctx, connect.NewRequest(&ledgerv1.PostJournalRequest{
		Command:  &ledgerv1.Reference{Namespace: "other.funding", Reference: "c-1"},
		LedgerId: "other",
		Journal:  &ledgerv1.Journal{Kind: "funding", ProductReference: "c-1", Lines: []*ledgerv1.JournalLine{debit(otherEquity, "5"), credit(otherFunds, "5")}},
	}))
	if err != nil {
		t.Fatal(err)
	}

	calls := map[string]func() error{
		"GetBalance": func() error {
			_, err := h.stranger.GetBalance(ctx, connect.NewRequest(&ledgerv1.GetBalanceRequest{AccountId: funds}))
			return err
		},
		"ListPostings": func() error {
			_, err := h.stranger.ListPostings(ctx, connect.NewRequest(&ledgerv1.ListPostingsRequest{AccountId: funds}))
			return err
		},
		"GetReservation": func() error {
			_, err := h.stranger.GetReservation(ctx, connect.NewRequest(&ledgerv1.GetReservationRequest{
				Lookup: &ledgerv1.GetReservationRequest_ReservationId{ReservationId: r.Reservation.Id},
			}))
			return err
		},
		"CommitReservation": func() error {
			_, err := h.stranger.CommitReservation(ctx, connect.NewRequest(&ledgerv1.CommitReservationRequest{
				Command:       &ledgerv1.Reference{Namespace: "other.commit", Reference: "steal"},
				ReservationId: r.Reservation.Id,
				Journal:       &ledgerv1.Journal{Kind: "steal", ProductReference: "steal", Lines: []*ledgerv1.JournalLine{debit(funds, "10"), credit(revenue, "10")}},
			}))
			return err
		},
		"PostJournal on foreign accounts": func() error {
			_, err := h.stranger.PostJournal(ctx, connect.NewRequest(&ledgerv1.PostJournalRequest{
				Command:  &ledgerv1.Reference{Namespace: "other.steal", Reference: "1"},
				LedgerId: "other",
				Journal:  &ledgerv1.Journal{Kind: "steal", ProductReference: "steal", Lines: []*ledgerv1.JournalLine{debit(funds, "10"), credit(otherFunds, "10")}},
			}))
			return err
		},
		"EnsureAccount in a foreign ledger": func() error {
			_, err := h.stranger.EnsureAccount(ctx, connect.NewRequest(&ledgerv1.EnsureAccountRequest{
				LedgerId: "corridor", OwnerKind: ledgerv1.OwnerKind_OWNER_KIND_HOUSE, OwnerId: "house", Purpose: "fee_revenue", AssetCode: "CAD",
			}))
			return err
		},
		"ExportAuditSnapshot of a foreign ledger": func() error {
			stream, err := h.stranger.ExportAuditSnapshot(ctx, connect.NewRequest(&ledgerv1.ExportAuditSnapshotRequest{LedgerId: "corridor"}))
			if err != nil {
				return err
			}
			for stream.Receive() {
			}
			return stream.Err()
		},
	}
	for name, call := range calls {
		if code, reason := reasonOfError(call()); code != connect.CodeNotFound || reason != ledgerv1.Reason_REASON_NOT_FOUND {
			t.Errorf("%s: got %v %v, want not_found NOT_FOUND", name, code, reason)
		}
	}
	if state := h.reservation(r.Reservation.Id).State; state != ledgerv1.ReservationState_RESERVATION_STATE_ACTIVE {
		t.Fatalf("the foreign commit changed the reservation to %v", state)
	}

	records, err := h.exportOf(h.stranger, "other")
	if err != nil {
		t.Fatal(err)
	}
	checker := audit.NewChecker()
	for _, record := range records {
		if err := checker.Add(record); err != nil {
			t.Fatal(err)
		}
	}
	report := checker.Report()
	if !report.OK() || report.Counts["account"] != 2 || report.Counts["entry"] != 1 {
		t.Fatalf("other ledger audit: counts %v findings %v", report.Counts, report.Findings)
	}
	if report := h.audit(); !report.OK() {
		t.Fatalf("corridor audit: %v", report.Findings)
	}
}

func (h *harness) ensureIn(client ledgerv1connect.LedgerServiceClient, ledgerID string, kind ledgerv1.OwnerKind, owner, purpose string) string {
	h.t.Helper()
	resp, err := client.EnsureAccount(context.Background(), connect.NewRequest(&ledgerv1.EnsureAccountRequest{
		LedgerId: ledgerID, OwnerKind: kind, OwnerId: owner, Purpose: purpose, AssetCode: "CAD",
	}))
	if err != nil {
		h.t.Fatal(err)
	}
	return resp.Msg.Account.Id
}
