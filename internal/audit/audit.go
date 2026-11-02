package audit

import (
	"cmp"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"maps"
	"math/big"
	"slices"

	"google.golang.org/protobuf/proto"

	ledgerv1 "github.com/xJohnNascimento/ledger/api/ledger/v1"
	"github.com/xJohnNascimento/ledger/internal/amount"
)

const (
	CheckSnapshot = "snapshot"
	CheckV1       = "V1"
	CheckV2       = "V2"
	CheckV3       = "V3"
	CheckLinks    = "links"
)

var checkTitles = []struct{ check, title string }{
	{CheckSnapshot, "the snapshot is complete and matches its trailer"},
	{CheckV1, "every entry balances per asset with valid membership"},
	{CheckV2, "every balance equals the fold of its postings, with contiguous sequences"},
	{CheckV3, "every reserved amount equals its active reservation legs"},
	{CheckLinks, "every command outcome links to the effect it created"},
}

type Finding struct {
	Check   string
	Subject string
	Detail  string
}

type Report struct {
	Counts   map[string]int64
	Findings []Finding
}

func (r Report) OK() bool {
	return len(r.Findings) == 0
}

// Checker rebuilds a ledger from its exported snapshot alone: it folds postings and reservation legs
// itself and never trusts a stored balance, a routine or a database constraint.
type Checker struct {
	hash         hash.Hash
	header       *ledgerv1.SnapshotHeader
	trailer      *ledgerv1.SnapshotTrailer
	counts       map[string]int64
	accounts     map[string]*ledgerv1.Account
	balances     map[string]*ledgerv1.Balance
	folded       map[string]*big.Int
	sequences    map[string][]int64
	reserved     map[string]*big.Int
	entries      map[string]*ledgerv1.JournalEntry
	reservations map[string]*ledgerv1.Reservation
	commands     map[string]*ledgerv1.Command
	findings     []Finding
}

func NewChecker() *Checker {
	return &Checker{
		hash:         sha256.New(),
		counts:       make(map[string]int64),
		accounts:     make(map[string]*ledgerv1.Account),
		balances:     make(map[string]*ledgerv1.Balance),
		folded:       make(map[string]*big.Int),
		sequences:    make(map[string][]int64),
		reserved:     make(map[string]*big.Int),
		entries:      make(map[string]*ledgerv1.JournalEntry),
		reservations: make(map[string]*ledgerv1.Reservation),
		commands:     make(map[string]*ledgerv1.Command),
	}
}

func (c *Checker) Add(record *ledgerv1.ExportAuditSnapshotResponse) error {
	if c.trailer != nil {
		return errors.New("a record arrived after the trailer")
	}
	if trailer, ok := record.Record.(*ledgerv1.ExportAuditSnapshotResponse_Trailer); ok {
		c.trailer = trailer.Trailer
		return nil
	}
	if err := c.digest(record); err != nil {
		return err
	}
	switch r := record.Record.(type) {
	case *ledgerv1.ExportAuditSnapshotResponse_Header:
		if c.header != nil {
			return errors.New("a second header arrived")
		}
		c.header = r.Header
	case *ledgerv1.ExportAuditSnapshotResponse_Account:
		c.counts["account"]++
		c.accounts[r.Account.Id] = r.Account
	case *ledgerv1.ExportAuditSnapshotResponse_Balance:
		c.counts["balance"]++
		c.balances[r.Balance.AccountId] = r.Balance
	case *ledgerv1.ExportAuditSnapshotResponse_Entry:
		c.counts["entry"]++
		c.entries[r.Entry.Id] = r.Entry
		c.checkEntry(r.Entry)
	case *ledgerv1.ExportAuditSnapshotResponse_Reservation:
		c.counts["reservation"]++
		c.reservations[r.Reservation.Id] = r.Reservation
		c.foldReservation(r.Reservation)
	case *ledgerv1.ExportAuditSnapshotResponse_Command:
		c.counts["command"]++
		c.commands[r.Command.Id] = r.Command
	default:
		return fmt.Errorf("unknown record %T", record.Record)
	}
	return nil
}

func (c *Checker) Report() Report {
	c.checkSnapshot()
	c.checkBalances()
	c.checkLinks()
	slices.SortStableFunc(c.findings, func(a, b Finding) int {
		return cmp.Or(cmp.Compare(a.Check, b.Check), cmp.Compare(a.Subject, b.Subject))
	})
	return Report{Counts: c.counts, Findings: c.findings}
}

func (c *Checker) checkEntry(e *ledgerv1.JournalEntry) {
	if n := len(e.Postings); n < 2 || n > 50 {
		c.find(CheckV1, "entry "+e.Id, "%d postings; 2 to 50 are required", n)
	}
	perAsset := make(map[string]*big.Int)
	for _, p := range e.Postings {
		account, ok := c.accounts[p.AccountId]
		if !ok {
			c.find(CheckV1, "entry "+e.Id, "posting %d is on unknown account %s", p.LineNo, p.AccountId)
			continue
		}
		if account.LedgerId != e.LedgerId || account.AssetCode != p.AssetCode {
			c.find(CheckV1, "entry "+e.Id, "posting %d in %s/%s is on account %s in %s/%s",
				p.LineNo, e.LedgerId, p.AssetCode, account.Id, account.LedgerId, account.AssetCode)
		}
		value, err := amount.Parse(p.Amount)
		if err != nil || value.Sign() <= 0 {
			c.find(CheckV1, "entry "+e.Id, "posting %d has amount %q", p.LineNo, p.Amount)
			continue
		}
		debit := new(big.Int).Set(value)
		if p.Side != ledgerv1.Side_SIDE_DEBIT {
			debit.Neg(debit)
		}
		amount.Add(perAsset, p.AssetCode, debit)
		normal := new(big.Int).Set(value)
		if p.Side != account.NormalSide {
			normal.Neg(normal)
		}
		amount.Add(c.folded, account.Id, normal)
		c.sequences[account.Id] = append(c.sequences[account.Id], p.AccountSequence)
	}
	for _, asset := range slices.Sorted(maps.Keys(perAsset)) {
		if perAsset[asset].Sign() != 0 {
			c.find(CheckV1, "entry "+e.Id, "debits exceed credits by %s in %s", perAsset[asset], asset)
		}
	}
}

func (c *Checker) foldReservation(r *ledgerv1.Reservation) {
	if r.State != ledgerv1.ReservationState_RESERVATION_STATE_ACTIVE {
		return
	}
	for _, leg := range r.Legs {
		value, err := amount.Parse(leg.MaxReduction)
		if err != nil {
			c.find(CheckV3, "reservation "+r.Id, "leg on %s has max reduction %q", leg.AccountId, leg.MaxReduction)
			continue
		}
		amount.Add(c.reserved, leg.AccountId, value)
	}
}

func (c *Checker) checkSnapshot() {
	switch {
	case c.header == nil:
		c.find(CheckSnapshot, "header", "missing")
	case c.trailer == nil:
		c.find(CheckSnapshot, "trailer", "missing")
	case c.trailer.Status != ledgerv1.SnapshotStatus_SNAPSHOT_STATUS_COMPLETE:
		c.find(CheckSnapshot, "trailer", "status %v", c.trailer.Status)
	default:
		if got := hex.EncodeToString(c.hash.Sum(nil)); got != c.trailer.ContentHash {
			c.find(CheckSnapshot, "trailer", "content hash %s, recomputed %s", c.trailer.ContentHash, got)
		}
		if !maps.Equal(c.trailer.Counts, c.counts) {
			c.find(CheckSnapshot, "trailer", "counts %v, received %v", c.trailer.Counts, c.counts)
		}
	}
}

func (c *Checker) checkBalances() {
	for _, id := range slices.Sorted(maps.Keys(c.balances)) {
		b := c.balances[id]
		posted, postedErr := amount.ParseSigned(b.Posted)
		fold := c.folded[id]
		if fold == nil {
			fold = new(big.Int)
		}
		if postedErr != nil || posted.Cmp(fold) != 0 {
			c.find(CheckV2, "account "+id, "postings fold to %s, the balance says %s", fold, b.Posted)
		}
		sequences := c.sequences[id]
		slices.Sort(sequences)
		for i, sequence := range sequences {
			if sequence != int64(i+1) {
				c.find(CheckV2, "account "+id, "posting sequences are not 1 to %d: %v", len(sequences), sequences)
				break
			}
		}
		if int64(len(sequences)) != b.LastPostingSequence {
			c.find(CheckV2, "account "+id, "%d postings, the balance says %d", len(sequences), b.LastPostingSequence)
		}
		reserved, reservedErr := amount.Parse(b.Reserved)
		legs := c.reserved[id]
		if legs == nil {
			legs = new(big.Int)
		}
		if reservedErr != nil || reserved.Cmp(legs) != 0 {
			c.find(CheckV3, "account "+id, "active legs sum to %s, the balance says %s", legs, b.Reserved)
		}
	}
	for _, id := range slices.Sorted(maps.Keys(c.folded)) {
		if _, ok := c.balances[id]; !ok {
			c.find(CheckV2, "account "+id, "has postings but no balance")
		}
	}
}

func (c *Checker) checkLinks() {
	for _, id := range slices.Sorted(maps.Keys(c.commands)) {
		command := c.commands[id]
		subject := fmt.Sprintf("command %s/%s", command.Ref.GetNamespace(), command.Ref.GetReference())
		switch command.Outcome {
		case ledgerv1.CommandOutcome_COMMAND_OUTCOME_REFUSED:
			if command.Reason == ledgerv1.Reason_REASON_UNSPECIFIED || command.EntryId != "" || command.ReservationId != "" || command.AccountId != "" {
				c.find(CheckLinks, subject, "a refusal must carry a reason and no effect")
			}
		case ledgerv1.CommandOutcome_COMMAND_OUTCOME_COMPLETED:
			c.checkCompleted(subject, command)
		default:
			c.find(CheckLinks, subject, "outcome %v", command.Outcome)
		}
	}
	for _, id := range slices.Sorted(maps.Keys(c.entries)) {
		entry := c.entries[id]
		command, ok := c.commands[entry.CommandId]
		if !ok || command.EntryId != entry.Id {
			c.find(CheckLinks, "entry "+id, "its command %s does not name it", entry.CommandId)
		}
	}
	for _, id := range slices.Sorted(maps.Keys(c.reservations)) {
		r := c.reservations[id]
		if creator, ok := c.commands[r.CreatedByCommandId]; !ok || creator.ReservationId != r.Id {
			c.find(CheckLinks, "reservation "+id, "its creating command %s does not name it", r.CreatedByCommandId)
		}
	}
}

func (c *Checker) checkCompleted(subject string, command *ledgerv1.Command) {
	switch command.Operation {
	case "POST_JOURNAL", "POST_CORRECTION":
		if entry, ok := c.entries[command.EntryId]; !ok || entry.CommandId != command.Id {
			c.find(CheckLinks, subject, "entry %q is missing or belongs to another command", command.EntryId)
		}
	case "RESERVE_FUNDS":
		if _, ok := c.reservations[command.ReservationId]; !ok {
			c.find(CheckLinks, subject, "reservation %q is missing", command.ReservationId)
		}
	case "COMMIT_RESERVATION", "RELEASE_RESERVATION":
		r, ok := c.reservations[command.ReservationId]
		if !ok || r.FinalCommandId != command.Id || r.FinalEntryId != command.EntryId {
			c.find(CheckLinks, subject, "reservation %q does not record this command as its final transition", command.ReservationId)
		}
	case "UPDATE_ACCOUNT_CONTROLS":
		if account, ok := c.accounts[command.AccountId]; !ok || account.LedgerId != command.LedgerId {
			c.find(CheckLinks, subject, "account %q is missing or belongs to another ledger", command.AccountId)
		}
	default:
		c.find(CheckLinks, subject, "unknown operation %q", command.Operation)
	}
}

func (c *Checker) digest(record *ledgerv1.ExportAuditSnapshotResponse) error {
	encoded, err := proto.MarshalOptions{Deterministic: true}.Marshal(record)
	if err != nil {
		return err
	}
	var length [4]byte
	binary.BigEndian.PutUint32(length[:], uint32(len(encoded))) //nolint:gosec // a marshaled protobuf message is capped at 2 GiB
	c.hash.Write(length[:])
	c.hash.Write(encoded)
	return nil
}

func (c *Checker) find(check, subject, format string, args ...any) {
	c.findings = append(c.findings, Finding{Check: check, Subject: subject, Detail: fmt.Sprintf(format, args...)})
}

func Print(w io.Writer, ledgerID string, r Report) {
	_, _ = fmt.Fprintf(w, "ledger %s: %d accounts, %d entries, %d reservations, %d commands\n",
		ledgerID, r.Counts["account"], r.Counts["entry"], r.Counts["reservation"], r.Counts["command"])
	for _, check := range checkTitles {
		status := "ok"
		for _, f := range r.Findings {
			if f.Check == check.check {
				status = "BROKEN"
				break
			}
		}
		_, _ = fmt.Fprintf(w, "%-8s %-6s %s\n", check.check, status, check.title)
	}
	for _, f := range r.Findings {
		_, _ = fmt.Fprintf(w, "  %s %s: %s\n", f.Check, f.Subject, f.Detail)
	}
}
