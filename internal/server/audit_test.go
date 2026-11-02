package server_test

import (
	"context"
	"fmt"
	"strconv"
	"testing"
	"time"

	"connectrpc.com/connect"
	"pgregory.net/rapid"

	ledgerv1 "github.com/xJohnNascimento/ledger/api/ledger/v1"
	"github.com/xJohnNascimento/ledger/api/ledger/v1/ledgerv1connect"
	"github.com/xJohnNascimento/ledger/internal/audit"
	"github.com/xJohnNascimento/ledger/internal/export"
)

func (h *harness) export(client ledgerv1connect.LedgerServiceClient) ([]*ledgerv1.ExportAuditSnapshotResponse, error) {
	return h.exportOf(client, "corridor")
}

func (h *harness) exportOf(client ledgerv1connect.LedgerServiceClient, ledgerID string) ([]*ledgerv1.ExportAuditSnapshotResponse, error) {
	stream, err := client.ExportAuditSnapshot(context.Background(), connect.NewRequest(&ledgerv1.ExportAuditSnapshotRequest{LedgerId: ledgerID}))
	if err != nil {
		return nil, err
	}
	var records []*ledgerv1.ExportAuditSnapshotResponse
	for stream.Receive() {
		records = append(records, stream.Msg())
	}
	return records, stream.Err()
}

func (h *harness) audit() audit.Report {
	h.t.Helper()
	records, err := h.export(h.auditor)
	if err != nil {
		h.t.Fatal(err)
	}
	checker := audit.NewChecker()
	for _, record := range records {
		if err := checker.Add(record); err != nil {
			h.t.Fatal(err)
		}
	}
	return checker.Report()
}

func (h *harness) getBalance(account string) *ledgerv1.Balance {
	h.t.Helper()
	resp, err := h.worker.GetBalance(context.Background(), connect.NewRequest(&ledgerv1.GetBalanceRequest{AccountId: account}))
	if err != nil {
		h.t.Fatal(err)
	}
	return resp.Msg.Balance
}

func TestBalancesAndPostingsAreReadFromTheWriter(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	funds, revenue := h.fundedCustomer("100")
	for i := range 3 {
		if _, err := h.post(h.worker, "corridor.fees", fmt.Sprintf("read-%d", i), debit(funds, "10"), credit(revenue, "10")); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := h.reserve("read/1", "read", []*ledgerv1.ReservationLeg{leg(funds, "20")}, revenue); err != nil {
		t.Fatal(err)
	}

	b := h.getBalance(funds)
	if b.Posted != "70" || b.Reserved != "20" || b.Available != "50" || b.LastPostingSequence != 4 || b.Version != 5 {
		t.Fatalf("balance %+v, want posted 70, reserved 20, available 50, sequence 4, version 5", b)
	}

	var sequences []int64
	after := int64(0)
	for {
		resp, err := h.worker.ListPostings(context.Background(), connect.NewRequest(&ledgerv1.ListPostingsRequest{
			AccountId: funds, AfterSequence: after, Limit: 3,
		}))
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range resp.Msg.Postings {
			sequences = append(sequences, p.AccountSequence)
		}
		if resp.Msg.NextAfterSequence == 0 {
			break
		}
		after = resp.Msg.NextAfterSequence
	}
	if fmt.Sprint(sequences) != "[1 2 3 4]" {
		t.Fatalf("paged sequences %v, want [1 2 3 4]", sequences)
	}

	_, err := h.worker.GetBalance(context.Background(), connect.NewRequest(&ledgerv1.GetBalanceRequest{AccountId: "01900000-0000-7000-8000-000000000000"}))
	requireReason(t, err, connect.CodeNotFound, ledgerv1.Reason_REASON_NOT_FOUND)
	_, err = h.worker.ListPostings(context.Background(), connect.NewRequest(&ledgerv1.ListPostingsRequest{AccountId: funds, Limit: 501}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("limit 501: %v", err)
	}
}

func TestOnlyTheAuditScopeExports(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.fundedCustomer("100")

	_, err := h.export(h.worker)
	requireReason(t, err, connect.CodePermissionDenied, ledgerv1.Reason_REASON_SCOPE_NOT_ALLOWED)
	_, err = h.export(h.seed)
	requireReason(t, err, connect.CodePermissionDenied, ledgerv1.Reason_REASON_SCOPE_NOT_ALLOWED)
	if report := h.audit(); !report.OK() {
		t.Fatalf("audit findings: %v", report.Findings)
	}
}

func TestAnInterruptedExportEndsIncomplete(t *testing.T) {
	t.Parallel()
	h := newHarnessWith(t, export.Limits{MaxRecords: 3, MaxDuration: time.Minute})
	h.fundedCustomer("100")

	records, err := h.export(h.auditor)
	if err != nil {
		t.Fatal(err)
	}
	trailer := records[len(records)-1].GetTrailer()
	if trailer == nil || trailer.Status != ledgerv1.SnapshotStatus_SNAPSHOT_STATUS_INCOMPLETE {
		t.Fatalf("last record %v, want an INCOMPLETE trailer", records[len(records)-1])
	}
	if len(records) != 5 {
		t.Fatalf("%d records, want the header, 3 records and the trailer", len(records))
	}
	report := h.audit()
	if report.OK() || report.Findings[0].Check != audit.CheckSnapshot {
		t.Fatalf("an incomplete snapshot must fail the audit: %v", report.Findings)
	}
}

func TestAuditFindsTamperingThatBypassesTheRoutines(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	funds, revenue := h.fundedCustomer("100")
	entry, err := h.post(h.worker, "corridor.fees", "tamper", debit(funds, "30"), credit(revenue, "30"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.reserve("tamper/1", "tamper", []*ledgerv1.ReservationLeg{leg(funds, "20")}, revenue); err != nil {
		t.Fatal(err)
	}
	if report := h.audit(); !report.OK() {
		t.Fatalf("clean ledger: %v", report.Findings)
	}

	ctx := context.Background()
	tx, err := h.db.Owner.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		"set local session_replication_role = replica",
		"update ledger.posting set amount = 31 where entry_id = '" + entry.Entry.Id + "' and line_no = 2",
		"update ledger.account_balance set reserved_amount = 0 where account_id = '" + funds + "'",
	} {
		if _, err := tx.Exec(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	checks := map[string]bool{}
	for _, f := range h.audit().Findings {
		checks[f.Check] = true
	}
	for _, want := range []string{audit.CheckV1, audit.CheckV2, audit.CheckV3} {
		if !checks[want] {
			t.Errorf("tampering not caught by %s", want)
		}
	}
}

type modelReservation struct {
	id       string
	customer int
	max      int64
	active   bool
}

type replay struct {
	call   func() (string, error)
	entry  string
	reason ledgerv1.Reason
}

func TestRandomHistoriesMatchAModelAndTheAudit(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	revenue := h.account(h.worker, ledgerv1.OwnerKind_OWNER_KIND_HOUSE, "house", "fee_revenue", "CAD")
	run := 0

	rapid.Check(t, func(rt *rapid.T) {
		run++
		accounts := make([]string, 3)
		posted := make([]int64, 3)
		reserved := make([]int64, 3)
		for i := range accounts {
			posted[i] = int64(rapid.IntRange(0, 300).Draw(rt, "funding"))
			accounts[i] = h.account(h.seed, ledgerv1.OwnerKind_OWNER_KIND_CUSTOMER, fmt.Sprintf("model-%d-%d", run, i), "funds", "CAD")
			if posted[i] > 0 {
				h.topUp(accounts[i], strconv.FormatInt(posted[i], 10), fmt.Sprintf("model-%d-%d", run, i))
			}
		}
		var (
			reservations []*modelReservation
			replays      []replay
		)
		reasonOf := func(err error) ledgerv1.Reason {
			if err == nil {
				return ledgerv1.Reason_REASON_UNSPECIFIED
			}
			_, detail := reasonOfError(err)
			return detail
		}
		expect := func(op string, err error, want ledgerv1.Reason) {
			if got := reasonOf(err); got != want {
				rt.Fatalf("%s: got %v (%v), want %v", op, got, err, want)
			}
		}

		steps := rapid.IntRange(1, 20).Draw(rt, "steps")
		for step := range steps {
			ref := fmt.Sprintf("model-%d-%d", run, step)
			switch rapid.IntRange(0, 4).Draw(rt, "op") {
			case 0:
				i := rapid.IntRange(0, 2).Draw(rt, "customer")
				amount := int64(rapid.IntRange(1, 150).Draw(rt, "amount"))
				want := ledgerv1.Reason_REASON_INSUFFICIENT_FUNDS
				if posted[i]-reserved[i] >= amount {
					want = ledgerv1.Reason_REASON_UNSPECIFIED
					posted[i] -= amount
				}
				call := func() (string, error) {
					resp, err := h.post(h.worker, "corridor.model", ref, debit(accounts[i], strconv.FormatInt(amount, 10)), credit(revenue, strconv.FormatInt(amount, 10)))
					if err != nil {
						return "", err
					}
					return resp.Entry.Id, nil
				}
				entry, err := call()
				expect("debit", err, want)
				replays = append(replays, replay{call, entry, want})
			case 1:
				i := rapid.IntRange(0, 2).Draw(rt, "customer")
				ceiling := int64(rapid.IntRange(1, 150).Draw(rt, "ceiling"))
				resp, err := h.reserve(ref+"/1", ref, []*ledgerv1.ReservationLeg{leg(accounts[i], strconv.FormatInt(ceiling, 10))}, revenue)
				if posted[i]-reserved[i] >= ceiling {
					expect("reserve", err, ledgerv1.Reason_REASON_UNSPECIFIED)
					reserved[i] += ceiling
					reservations = append(reservations, &modelReservation{resp.Reservation.Id, i, ceiling, true})
					continue
				}
				expect("reserve", err, ledgerv1.Reason_REASON_INSUFFICIENT_FUNDS)
			case 2, 3:
				if len(reservations) == 0 {
					continue
				}
				r := reservations[rapid.IntRange(0, len(reservations)-1).Draw(rt, "reservation")]
				amount := int64(rapid.IntRange(1, int(r.max)+1).Draw(rt, "settled"))
				var err error
				commit := rapid.Bool().Draw(rt, "commit")
				if commit {
					_, err = h.commit(ref, r.id, debit(accounts[r.customer], strconv.FormatInt(amount, 10)), credit(revenue, strconv.FormatInt(amount, 10)))
				} else {
					_, err = h.release(ref, r.id)
				}
				switch {
				case !r.active:
					expect("finalize", err, ledgerv1.Reason_REASON_RESERVATION_FINAL)
				case commit && amount > r.max:
					expect("commit", err, ledgerv1.Reason_REASON_CEILING_EXCEEDED)
				default:
					expect("finalize", err, ledgerv1.Reason_REASON_UNSPECIFIED)
					r.active = false
					reserved[r.customer] -= r.max
					if commit {
						posted[r.customer] -= amount
					}
				}
			case 4:
				if len(replays) == 0 {
					continue
				}
				previous := replays[rapid.IntRange(0, len(replays)-1).Draw(rt, "replay")]
				entry, err := previous.call()
				expect("replay", err, previous.reason)
				if entry != previous.entry {
					rt.Fatalf("replay answered entry %q, want %q", entry, previous.entry)
				}
			}
		}

		for i, account := range accounts {
			b := h.getBalance(account)
			if b.Posted != strconv.FormatInt(posted[i], 10) || b.Reserved != strconv.FormatInt(reserved[i], 10) {
				rt.Fatalf("customer %d: posted %s reserved %s, model says %d and %d", i, b.Posted, b.Reserved, posted[i], reserved[i])
			}
		}
	})

	if report := h.audit(); !report.OK() {
		t.Fatalf("audit after %d random histories: %v", run, report.Findings)
	}
}
