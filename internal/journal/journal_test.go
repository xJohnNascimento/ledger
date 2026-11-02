package journal_test

import (
	"errors"
	"math/big"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/xJohnNascimento/ledger/internal/journal"
)

var (
	a = uuid.MustParse("01900000-0000-7000-8000-00000000000a")
	b = uuid.MustParse("01900000-0000-7000-8000-00000000000b")
	c = uuid.MustParse("01900000-0000-7000-8000-00000000000c")
	d = uuid.MustParse("01900000-0000-7000-8000-00000000000d")
)

func line(account uuid.UUID, side journal.Side, asset string, amount int64) journal.Line {
	return journal.Line{AccountID: account, Side: side, AssetCode: asset, Amount: big.NewInt(amount)}
}

func valid() journal.Journal {
	return journal.Journal{
		Kind:             "fx.conversion",
		ProductReference: "payment/42",
		Lines: []journal.Line{
			line(a, journal.Debit, "CAD", 1000000),
			line(b, journal.Credit, "CAD", 1000000),
			line(c, journal.Debit, "USD", 731000),
			line(d, journal.Credit, "USD", 731000),
		},
	}
}

func TestValidateAcceptsAJournalBalancedPerAsset(t *testing.T) {
	if err := valid().Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestValidateRefusesBrokenJournals(t *testing.T) {
	huge, _ := new(big.Int).SetString("170141183460469231731687303715884105727", 10)
	cases := map[string]struct {
		edit func(*journal.Journal)
		want error
	}{
		"one line":          {func(j *journal.Journal) { j.Lines = j.Lines[:1] }, journal.ErrInvalid},
		"no lines":          {func(j *journal.Journal) { j.Lines = nil }, journal.ErrInvalid},
		"fifty-one lines":   {func(j *journal.Journal) { j.Lines = repeat(51) }, journal.ErrInvalid},
		"unbalanced":        {func(j *journal.Journal) { j.Lines[1].Amount = big.NewInt(999999) }, journal.ErrInvalid},
		"balanced in total": {func(j *journal.Journal) { j.Lines[1].AssetCode, j.Lines[3].AssetCode = "USD", "CAD" }, journal.ErrInvalid},
		"unknown side":      {func(j *journal.Journal) { j.Lines[0].Side = "SIDEWAYS" }, journal.ErrInvalid},
		"lowercase asset":   {func(j *journal.Journal) { j.Lines[0].AssetCode = "cad" }, journal.ErrInvalid},
		"missing account":   {func(j *journal.Journal) { j.Lines[0].AccountID = uuid.Nil }, journal.ErrInvalid},
		"bad kind":          {func(j *journal.Journal) { j.Kind = "FX Conversion" }, journal.ErrInvalid},
		"empty reference":   {func(j *journal.Journal) { j.ProductReference = "" }, journal.ErrInvalid},
		"long reference":    {func(j *journal.Journal) { j.ProductReference = strings.Repeat("r", 257) }, journal.ErrInvalid},
		"zero amount":       {func(j *journal.Journal) { j.Lines[0].Amount = big.NewInt(0) }, journal.ErrInvalidAmount},
		"negative amount":   {func(j *journal.Journal) { j.Lines[0].Amount = big.NewInt(-1) }, journal.ErrInvalidAmount},
		"missing amount":    {func(j *journal.Journal) { j.Lines[0].Amount = nil }, journal.ErrInvalidAmount},
		"account movement over 128 bits": {func(j *journal.Journal) {
			j.Lines = []journal.Line{
				{AccountID: a, Side: journal.Debit, AssetCode: "CAD", Amount: huge},
				{AccountID: a, Side: journal.Debit, AssetCode: "CAD", Amount: huge},
				{AccountID: b, Side: journal.Credit, AssetCode: "CAD", Amount: huge},
				{AccountID: b, Side: journal.Credit, AssetCode: "CAD", Amount: huge},
			}
		}, journal.ErrInvalidAmount},
	}
	for name, c := range cases {
		j := valid()
		c.edit(&j)
		if err := j.Validate(); !errors.Is(err, c.want) {
			t.Errorf("%s: got %v, want %v", name, err, c.want)
		}
	}
	if err := (journal.Journal{Kind: "k", ProductReference: "r", Lines: repeat(50)}).Validate(); err != nil {
		t.Errorf("fifty lines: %v", err)
	}
}

func TestHashIsStableAndCoversEveryField(t *testing.T) {
	base := journal.PostJournalHash("corridor", "", valid())
	if base != journal.PostJournalHash("corridor", "", valid()) {
		t.Fatal("the same request hashed twice differs")
	}
	variants := map[string][32]byte{
		"ledger":          journal.PostJournalHash("other", "", valid()),
		"accounting date": journal.PostJournalHash("corridor", "2026-11-02", valid()),
	}
	edits := map[string]func(*journal.Journal){
		"kind":       func(j *journal.Journal) { j.Kind = "fx.fee" },
		"reference":  func(j *journal.Journal) { j.ProductReference = "payment/43" },
		"amount":     func(j *journal.Journal) { j.Lines[0].Amount = big.NewInt(1000001) },
		"side":       func(j *journal.Journal) { j.Lines[0].Side = journal.Credit },
		"asset":      func(j *journal.Journal) { j.Lines[0].AssetCode = "EUR" },
		"account":    func(j *journal.Journal) { j.Lines[0].AccountID = d },
		"line order": func(j *journal.Journal) { j.Lines[0], j.Lines[1] = j.Lines[1], j.Lines[0] },
	}
	for name, edit := range edits {
		j := valid()
		edit(&j)
		variants[name] = journal.PostJournalHash("corridor", "", j)
	}
	for name, h := range variants {
		if h == base {
			t.Errorf("changing the %s does not change the hash", name)
		}
	}
}

func repeat(n int) []journal.Line {
	lines := make([]journal.Line, 0, n)
	for i := range n {
		side := journal.Debit
		if i%2 == 1 {
			side = journal.Credit
		}
		lines = append(lines, line(a, side, "CAD", 1))
	}
	return lines
}
