package server_test

import (
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	ledgerv1 "github.com/xJohnNascimento/ledger/api/ledger/v1"
	"github.com/xJohnNascimento/ledger/api/ledger/v1/ledgerv1connect"
	"github.com/xJohnNascimento/ledger/internal/export"
	"github.com/xJohnNascimento/ledger/internal/pgtest"
	"github.com/xJohnNascimento/ledger/internal/server"
	"github.com/xJohnNascimento/ledger/internal/store"
)

func TestMain(m *testing.M) {
	os.Exit(pgtest.Main(m))
}

type harness struct {
	t        *testing.T
	db       pgtest.Database
	srv      *httptest.Server
	worker   ledgerv1connect.LedgerServiceClient
	seed     ledgerv1connect.LedgerServiceClient
	auditor  ledgerv1connect.LedgerServiceClient
	operator ledgerv1connect.LedgerServiceClient
	stranger ledgerv1connect.LedgerServiceClient
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	return newHarnessWith(t, export.Limits{})
}

func newHarnessWith(t *testing.T, limits export.Limits) *harness {
	t.Helper()
	clients, err := server.ParseClients([]byte(clientsYAML), getenv)
	if err != nil {
		t.Fatal(err)
	}
	db := pgtest.NewDatabase(t)
	srv := httptest.NewServer(server.Handler(server.Config{
		Store:        store.New(db.App),
		AuditStore:   store.New(db.Audit),
		Clients:      clients,
		ExportLimits: limits,
	}))
	t.Cleanup(srv.Close)
	h := &harness{t: t, db: db, srv: srv}
	h.worker = h.client(secrets["WORKER_SECRET"])
	h.seed = h.client(secrets["SEED_SECRET"])
	h.auditor = h.client(secrets["AUDITOR_SECRET"])
	h.operator = h.client(secrets["OPERATOR_SECRET"])
	h.stranger = h.client(secrets["STRANGER_SECRET"])
	return h
}

func (h *harness) client(secret string) ledgerv1connect.LedgerServiceClient {
	return ledgerv1connect.NewLedgerServiceClient(h.srv.Client(), h.srv.URL, connect.WithInterceptors(bearer(secret)))
}

type bearer string

func (b bearer) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		if b != "" {
			req.Header().Set("Authorization", "Bearer "+string(b))
		}
		return next(ctx, req)
	}
}

func (b bearer) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return func(ctx context.Context, spec connect.Spec) connect.StreamingClientConn {
		conn := next(ctx, spec)
		if b != "" {
			conn.RequestHeader().Set("Authorization", "Bearer "+string(b))
		}
		return conn
	}
}

func (b bearer) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return next
}

func (h *harness) account(client ledgerv1connect.LedgerServiceClient, kind ledgerv1.OwnerKind, owner, purpose, asset string) string {
	h.t.Helper()
	resp, err := client.EnsureAccount(context.Background(), connect.NewRequest(&ledgerv1.EnsureAccountRequest{
		LedgerId: "corridor", OwnerKind: kind, OwnerId: owner, Purpose: purpose, AssetCode: asset,
	}))
	if err != nil {
		h.t.Fatal(err)
	}
	return resp.Msg.Account.Id
}

func (h *harness) post(client ledgerv1connect.LedgerServiceClient, namespace, reference string, lines ...*ledgerv1.JournalLine) (*ledgerv1.PostJournalResponse, error) {
	resp, err := client.PostJournal(context.Background(), connect.NewRequest(&ledgerv1.PostJournalRequest{
		Command:  &ledgerv1.Reference{Namespace: namespace, Reference: reference},
		LedgerId: "corridor",
		Journal:  &ledgerv1.Journal{Kind: "test", ProductReference: "test/" + reference, Lines: lines},
	}))
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}

func (h *harness) balance(account string) string {
	h.t.Helper()
	var posted string
	err := h.db.Owner.QueryRow(context.Background(),
		"select posted_balance::text from ledger.account_balance where account_id = $1", account,
	).Scan(&posted)
	if err != nil {
		h.t.Fatal(err)
	}
	return posted
}

func (h *harness) fundedCustomer(amount string) (funds, revenue string) {
	h.t.Helper()
	customer := fmt.Sprintf("customer-%p", h)
	funds = h.account(h.seed, ledgerv1.OwnerKind_OWNER_KIND_CUSTOMER, customer, "funds", "CAD")
	equity := h.account(h.seed, ledgerv1.OwnerKind_OWNER_KIND_HOUSE, "house", "funding_equity", "CAD")
	revenue = h.account(h.worker, ledgerv1.OwnerKind_OWNER_KIND_HOUSE, "house", "fee_revenue", "CAD")
	if _, err := h.post(h.seed, "seed.funding", customer, debit(equity, amount), credit(funds, amount)); err != nil {
		h.t.Fatal(err)
	}
	return funds, revenue
}

func debit(account, amount string) *ledgerv1.JournalLine {
	return &ledgerv1.JournalLine{AccountId: account, Side: ledgerv1.Side_SIDE_DEBIT, AssetCode: "CAD", Amount: amount}
}

func credit(account, amount string) *ledgerv1.JournalLine {
	return &ledgerv1.JournalLine{AccountId: account, Side: ledgerv1.Side_SIDE_CREDIT, AssetCode: "CAD", Amount: amount}
}

func reasonOfError(err error) (connect.Code, ledgerv1.Reason) {
	var connectErr *connect.Error
	if !errors.As(err, &connectErr) {
		return connect.CodeUnknown, ledgerv1.Reason_REASON_UNSPECIFIED
	}
	for _, d := range connectErr.Details() {
		if detail, ok := d.Value(); ok == nil {
			if typed, isDetail := detail.(*ledgerv1.ErrorDetail); isDetail {
				return connectErr.Code(), typed.Reason
			}
		}
	}
	return connectErr.Code(), ledgerv1.Reason_REASON_UNSPECIFIED
}

func reasonOf(t *testing.T, err error) (connect.Code, *ledgerv1.ErrorDetail) {
	t.Helper()
	var connectErr *connect.Error
	if !errors.As(err, &connectErr) {
		t.Fatalf("want a Connect error, got %v", err)
	}
	for _, d := range connectErr.Details() {
		value, valueErr := d.Value()
		if detail, ok := value.(*ledgerv1.ErrorDetail); ok && valueErr == nil {
			return connectErr.Code(), detail
		}
	}
	return connectErr.Code(), &ledgerv1.ErrorDetail{}
}

func requireReason(t *testing.T, err error, code connect.Code, reason ledgerv1.Reason) *ledgerv1.ErrorDetail {
	t.Helper()
	if err == nil {
		t.Fatalf("want %v %v, got success", code, reason)
	}
	gotCode, detail := reasonOf(t, err)
	if gotCode != code || detail.Reason != reason {
		t.Fatalf("want %v %v, got %v %v (%v)", code, reason, gotCode, detail.Reason, err)
	}
	return detail
}

func TestCallsNeedAKnownCredential(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	for name, client := range map[string]ledgerv1connect.LedgerServiceClient{
		"no credential":    h.client(""),
		"wrong credential": h.client("worker-0123456789abcdef0123456789abcdeX"),
	} {
		_, err := client.GetCommand(context.Background(), connect.NewRequest(&ledgerv1.GetCommandRequest{LedgerId: "corridor"}))
		if connect.CodeOf(err) != connect.CodeUnauthenticated {
			t.Errorf("%s: got %v, want unauthenticated", name, err)
		}
	}
}

func TestTheBearerSchemeIsCaseInsensitive(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	for _, scheme := range []string{"bearer", "BEARER"} {
		req := connect.NewRequest(&ledgerv1.GetCommandRequest{LedgerId: "corridor", Command: &ledgerv1.Reference{Namespace: "corridor.fees", Reference: "none"}})
		req.Header().Set("Authorization", scheme+" "+secrets["WORKER_SECRET"])
		_, err := ledgerv1connect.NewLedgerServiceClient(h.srv.Client(), h.srv.URL).GetCommand(context.Background(), req)
		requireReason(t, err, connect.CodeNotFound, ledgerv1.Reason_REASON_NOT_FOUND)
	}
}

func TestScopesNamespacesPurposesAndLedgersAreEnforced(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := context.Background()
	equity := h.account(h.seed, ledgerv1.OwnerKind_OWNER_KIND_HOUSE, "house", "funding_equity", "CAD")
	revenue := h.account(h.worker, ledgerv1.OwnerKind_OWNER_KIND_HOUSE, "house", "fee_revenue", "CAD")

	_, err := h.auditor.EnsureAccount(ctx, connect.NewRequest(&ledgerv1.EnsureAccountRequest{
		LedgerId: "corridor", OwnerKind: ledgerv1.OwnerKind_OWNER_KIND_HOUSE, OwnerId: "house", Purpose: "fee_revenue", AssetCode: "CAD",
	}))
	requireReason(t, err, connect.CodePermissionDenied, ledgerv1.Reason_REASON_SCOPE_NOT_ALLOWED)

	_, err = h.seed.EnsureAccount(ctx, connect.NewRequest(&ledgerv1.EnsureAccountRequest{
		LedgerId: "corridor", OwnerKind: ledgerv1.OwnerKind_OWNER_KIND_PAYMENT, OwnerId: "p-1", Purpose: "clearing", AssetCode: "CAD",
	}))
	requireReason(t, err, connect.CodePermissionDenied, ledgerv1.Reason_REASON_PURPOSE_NOT_ALLOWED)

	_, err = h.post(h.seed, "corridor.conversion", "x", debit(equity, "1"), credit(revenue, "1"))
	requireReason(t, err, connect.CodePermissionDenied, ledgerv1.Reason_REASON_NAMESPACE_NOT_ALLOWED)

	_, err = h.post(h.worker, "corridor.fees", "x", debit(equity, "1"), credit(revenue, "1"))
	requireReason(t, err, connect.CodePermissionDenied, ledgerv1.Reason_REASON_PURPOSE_NOT_ALLOWED)

	_, err = h.worker.PostJournal(ctx, connect.NewRequest(&ledgerv1.PostJournalRequest{
		Command:  &ledgerv1.Reference{Namespace: "corridor.fees", Reference: "y"},
		LedgerId: "other",
		Journal:  &ledgerv1.Journal{Kind: "test", ProductReference: "test", Lines: []*ledgerv1.JournalLine{debit(revenue, "1"), credit(revenue, "1")}},
	}))
	requireReason(t, err, connect.CodeNotFound, ledgerv1.Reason_REASON_NOT_FOUND)

	_, err = h.auditor.PostJournal(ctx, connect.NewRequest(&ledgerv1.PostJournalRequest{}))
	requireReason(t, err, connect.CodePermissionDenied, ledgerv1.Reason_REASON_SCOPE_NOT_ALLOWED)
}

func TestInvalidRequestsAreRefusedWithoutBeingStored(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	funds, revenue := h.fundedCustomer("100")
	usd := h.account(h.worker, ledgerv1.OwnerKind_OWNER_KIND_HOUSE, "house", "fee_revenue", "USD")

	cases := map[string]struct {
		lines  []*ledgerv1.JournalLine
		code   connect.Code
		reason ledgerv1.Reason
	}{
		"fractional amount": {[]*ledgerv1.JournalLine{debit(funds, "1.5"), credit(revenue, "1.5")}, connect.CodeInvalidArgument, ledgerv1.Reason_REASON_INVALID_AMOUNT},
		"zero amount":       {[]*ledgerv1.JournalLine{debit(funds, "0"), credit(revenue, "0")}, connect.CodeInvalidArgument, ledgerv1.Reason_REASON_INVALID_AMOUNT},
		"leading zero":      {[]*ledgerv1.JournalLine{debit(funds, "010"), credit(revenue, "010")}, connect.CodeInvalidArgument, ledgerv1.Reason_REASON_INVALID_AMOUNT},
		"unbalanced":        {[]*ledgerv1.JournalLine{debit(funds, "10"), credit(revenue, "9")}, connect.CodeInvalidArgument, ledgerv1.Reason_REASON_INVALID_JOURNAL},
		"single line":       {[]*ledgerv1.JournalLine{debit(funds, "10")}, connect.CodeInvalidArgument, ledgerv1.Reason_REASON_INVALID_JOURNAL},
		"wrong asset":       {[]*ledgerv1.JournalLine{debit(funds, "10"), credit(usd, "10")}, connect.CodeInvalidArgument, ledgerv1.Reason_REASON_INVALID_JOURNAL},
		"unknown account":   {[]*ledgerv1.JournalLine{debit(funds, "10"), credit("01900000-0000-7000-8000-000000000000", "10")}, connect.CodeNotFound, ledgerv1.Reason_REASON_NOT_FOUND},
	}
	for name, c := range cases {
		_, err := h.post(h.worker, "corridor.fees", "invalid", c.lines...)
		if code, detail := reasonOf(t, err); code != c.code || detail.Reason != c.reason {
			t.Errorf("%s: got %v %v, want %v %v", name, code, detail.Reason, c.code, c.reason)
		}
	}

	_, err := h.worker.GetCommand(context.Background(), connect.NewRequest(&ledgerv1.GetCommandRequest{
		LedgerId: "corridor", Command: &ledgerv1.Reference{Namespace: "corridor.fees", Reference: "invalid"},
	}))
	requireReason(t, err, connect.CodeNotFound, ledgerv1.Reason_REASON_NOT_FOUND)
	if _, err := h.post(h.worker, "corridor.fees", "invalid", debit(funds, "10"), credit(revenue, "10")); err != nil {
		t.Fatalf("the reference must still be free after refused requests: %v", err)
	}
}

func TestConcurrentDuplicatesProduceOneEntry(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	funds, revenue := h.fundedCustomer("1000")

	const callers = 16
	var (
		wg      sync.WaitGroup
		start   = make(chan struct{})
		entries = make([]string, callers)
		errs    = make([]error, callers)
	)
	for i := range callers {
		wg.Go(func() {
			<-start
			resp, err := h.post(h.worker, "corridor.fees", "fee-1", debit(funds, "70"), credit(revenue, "70"))
			errs[i] = err
			if err == nil {
				entries[i] = resp.Entry.Id
			}
		})
	}
	close(start)
	wg.Wait()

	for i := range callers {
		if errs[i] != nil {
			t.Fatalf("caller %d: %v", i, errs[i])
		}
		if entries[i] != entries[0] {
			t.Fatalf("caller %d got entry %s, caller 0 got %s", i, entries[i], entries[0])
		}
	}
	if got := h.balance(funds); got != "930" {
		t.Fatalf("funds = %s, want 930", got)
	}

	original, err := h.worker.GetCommand(context.Background(), connect.NewRequest(&ledgerv1.GetCommandRequest{
		LedgerId: "corridor", Command: &ledgerv1.Reference{Namespace: "corridor.fees", Reference: "fee-1"},
	}))
	if err != nil {
		t.Fatal(err)
	}
	_, err = h.post(h.worker, "corridor.fees", "fee-1", debit(funds, "71"), credit(revenue, "71"))
	detail := requireReason(t, err, connect.CodeAlreadyExists, ledgerv1.Reason_REASON_REFERENCE_CONFLICT)
	if detail.ExistingCommandId != original.Msg.Command.Id {
		t.Fatalf("conflict names command %q, want %q", detail.ExistingCommandId, original.Msg.Command.Id)
	}
}

func TestConcurrentDebitsNeverOverdraw(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	funds, revenue := h.fundedCustomer("100")

	const callers = 10
	var (
		wg      sync.WaitGroup
		start   = make(chan struct{})
		errs    = make([]error, callers)
		mu      sync.Mutex
		refused []int
	)
	for i := range callers {
		wg.Go(func() {
			<-start
			_, errs[i] = h.post(h.worker, "corridor.fees", fmt.Sprintf("debit-%d", i), debit(funds, "30"), credit(revenue, "30"))
			if errs[i] != nil {
				mu.Lock()
				refused = append(refused, i)
				mu.Unlock()
			}
		})
	}
	close(start)
	wg.Wait()

	if len(refused) != callers-3 {
		t.Fatalf("%d debits of 30 refused against 100, want %d", len(refused), callers-3)
	}
	for _, i := range refused {
		detail := requireReason(t, errs[i], connect.CodeFailedPrecondition, ledgerv1.Reason_REASON_INSUFFICIENT_FUNDS)
		if detail.AccountId != funds || detail.Available != "10" || detail.Required != "30" {
			t.Fatalf("refusal detail %+v, want account %s, available 10, required 30", detail, funds)
		}
	}
	if got := h.balance(funds); got != "10" {
		t.Fatalf("funds = %s, want 10", got)
	}

	equity := h.account(h.seed, ledgerv1.OwnerKind_OWNER_KIND_HOUSE, "house", "funding_equity", "CAD")
	if _, err := h.post(h.seed, "seed.funding", "top-up", debit(equity, "500"), credit(funds, "500")); err != nil {
		t.Fatal(err)
	}
	_, err := h.post(h.worker, "corridor.fees", fmt.Sprintf("debit-%d", refused[0]), debit(funds, "30"), credit(revenue, "30"))
	requireReason(t, err, connect.CodeFailedPrecondition, ledgerv1.Reason_REASON_INSUFFICIENT_FUNDS)
	if got := h.balance(funds); got != "510" {
		t.Fatalf("a stored refusal was executed on replay: funds = %s, want 510", got)
	}
}

func TestLostReplyIsAnsweredWithTheOriginalEntry(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	funds, revenue := h.fundedCustomer("100")

	first, err := h.post(h.worker, "corridor.fees", "fee-a", debit(funds, "30"), credit(revenue, "30"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.post(h.worker, "corridor.fees", "fee-b", debit(funds, "50"), credit(revenue, "50")); err != nil {
		t.Fatal(err)
	}
	retry, err := h.post(h.worker, "corridor.fees", "fee-a", debit(funds, "30"), credit(revenue, "30"))
	if err != nil {
		t.Fatal(err)
	}
	if retry.CommandId != first.CommandId || retry.Entry.Id != first.Entry.Id {
		t.Fatalf("retry answered command %s entry %s, want %s and %s", retry.CommandId, retry.Entry.Id, first.CommandId, first.Entry.Id)
	}
	if got := h.balance(funds); got != "20" {
		t.Fatalf("funds = %s, want 20", got)
	}
	if postings := retry.Entry.Postings; len(postings) != 2 || postings[0].AccountSequence != 2 || postings[1].AccountSequence != 1 {
		t.Fatalf("postings %v, want the customer's second and the revenue account's first", postings)
	}
}

func TestBlockedAccountsRefuseReductionsButAcceptCredits(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	funds, revenue := h.fundedCustomer("100")
	if _, err := h.db.Owner.Exec(context.Background(), "update ledger.account set status = 'BLOCKED' where id = $1", funds); err != nil {
		t.Fatal(err)
	}

	_, err := h.post(h.worker, "corridor.fees", "blocked-debit", debit(funds, "1"), credit(revenue, "1"))
	detail := requireReason(t, err, connect.CodeFailedPrecondition, ledgerv1.Reason_REASON_ACCOUNT_BLOCKED)
	if detail.AccountId != funds {
		t.Fatalf("blocked account %q, want %q", detail.AccountId, funds)
	}
	if _, err := h.post(h.worker, "corridor.fees", "blocked-credit", debit(revenue, "5"), credit(funds, "5")); err != nil {
		t.Fatalf("a credit to a blocked account was refused: %v", err)
	}

	command, err := h.worker.GetCommand(context.Background(), connect.NewRequest(&ledgerv1.GetCommandRequest{
		LedgerId: "corridor", Command: &ledgerv1.Reference{Namespace: "corridor.fees", Reference: "blocked-debit"},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if c := command.Msg.Command; c.Outcome != ledgerv1.CommandOutcome_COMMAND_OUTCOME_REFUSED || c.Reason != ledgerv1.Reason_REASON_ACCOUNT_BLOCKED || c.EntryId != "" {
		t.Fatalf("stored command %+v, want a refusal with no entry", c)
	}
}

func TestOpposingTransfersDoNotDeadlock(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	a := h.account(h.worker, ledgerv1.OwnerKind_OWNER_KIND_HOUSE, "house", "fee_revenue", "CAD")
	b := h.account(h.worker, ledgerv1.OwnerKind_OWNER_KIND_HOUSE, "house", "spread_revenue", "CAD")

	const transfers = 40
	var wg sync.WaitGroup
	errs := make([]error, transfers)
	for i := range transfers {
		wg.Go(func() {
			from, to := a, b
			if i%2 == 1 {
				from, to = b, a
			}
			_, errs[i] = h.post(h.worker, "corridor.transfers", fmt.Sprintf("t-%d", i), debit(from, "1"), credit(to, "1"))
		})
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("transfer %d: %v", i, err)
		}
	}
	if h.balance(a) != "0" || h.balance(b) != "0" {
		t.Fatalf("balances %s and %s, want 0 and 0", h.balance(a), h.balance(b))
	}
}

func TestKilledConnectionLeavesNothingAndTheSameReferenceRetries(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	funds, revenue := h.fundedCustomer("1000")
	ctx := context.Background()

	blocker, err := h.db.Owner.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = blocker.Rollback(ctx) }()
	if _, err := blocker.Exec(ctx, "select 1 from ledger.account_balance where account_id = $1 for update", funds); err != nil {
		t.Fatal(err)
	}

	inFlight := make(chan error, 1)
	go func() {
		_, err := h.post(h.worker, "corridor.fees", "kill-1", debit(funds, "70"), credit(revenue, "70"))
		inFlight <- err
	}()
	if !waitFor(func() bool {
		var waiting bool
		err := h.db.Owner.QueryRow(ctx,
			`select exists (select 1 from pg_stat_activity
			 where usename = 'ledger_app' and datname = current_database() and wait_event_type = 'Lock')`,
		).Scan(&waiting)
		return err == nil && waiting
	}) {
		t.Fatal("the post never waited on the balance lock")
	}
	var killed int
	err = h.db.Owner.QueryRow(ctx,
		`select count(*) from pg_stat_activity, lateral pg_terminate_backend(pid)
		 where usename = 'ledger_app' and datname = current_database() and wait_event_type = 'Lock'`,
	).Scan(&killed)
	if err != nil || killed != 1 {
		t.Fatalf("terminated %d backends, err %v; want 1", killed, err)
	}
	if err := <-inFlight; connect.CodeOf(err) != connect.CodeUnavailable {
		t.Fatalf("the killed call returned %v, want unavailable", err)
	}
	if err := blocker.Rollback(ctx); err != nil {
		t.Fatal(err)
	}

	entries := func() (count int) {
		err := h.db.Owner.QueryRow(ctx,
			"select count(*) from ledger.journal_entry e join ledger.command c on c.id = e.command_id where c.reference = 'kill-1'",
		).Scan(&count)
		if err != nil {
			t.Fatal(err)
		}
		return count
	}
	if got := entries(); got != 0 {
		t.Fatalf("%d entries after the killed call, want 0", got)
	}

	first, err := h.post(h.worker, "corridor.fees", "kill-1", debit(funds, "70"), credit(revenue, "70"))
	if err != nil {
		t.Fatal(err)
	}
	again, err := h.post(h.worker, "corridor.fees", "kill-1", debit(funds, "70"), credit(revenue, "70"))
	if err != nil || again.Entry.Id != first.Entry.Id || again.CommandId != first.CommandId {
		t.Fatalf("second retry %v, err %v; want the first outcome", again, err)
	}
	if got := entries(); got != 1 {
		t.Fatalf("%d entries after the retries, want 1", got)
	}
	if got := h.balance(funds); got != "930" {
		t.Fatalf("funds = %s, want 930", got)
	}
	stored, err := h.worker.GetCommand(ctx, connect.NewRequest(&ledgerv1.GetCommandRequest{
		LedgerId: "corridor", Command: &ledgerv1.Reference{Namespace: "corridor.fees", Reference: "kill-1"},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if c := stored.Msg.Command; c.Outcome != ledgerv1.CommandOutcome_COMMAND_OUTCOME_COMPLETED || c.EntryId != first.Entry.Id {
		t.Fatalf("stored command %v, want COMPLETED with entry %s", c, first.Entry.Id)
	}
	if report := h.audit(); !report.OK() {
		t.Fatalf("audit findings: %v", report.Findings)
	}
}

func waitFor(condition func() bool) bool {
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}
