package journal

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/xJohnNascimento/ledger/internal/amount"
)

type Side string

const (
	Debit  Side = "DEBIT"
	Credit Side = "CREDIT"
)

const (
	HashVersion = 1
	MinLines    = 2
	MaxLines    = 50
)

var (
	ErrInvalid       = errors.New("invalid journal")
	ErrInvalidAmount = errors.New("invalid amount")
)

var (
	kindPattern  = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,63}$`)
	assetPattern = regexp.MustCompile(`^[A-Z]{3}$`)
)

type Line struct {
	AccountID uuid.UUID
	Side      Side
	AssetCode string
	Amount    *big.Int
}

type Journal struct {
	Kind             string
	ProductReference string
	Lines            []Line
}

func (j Journal) Validate() error {
	if !kindPattern.MatchString(j.Kind) {
		return fmt.Errorf("%w: kind %q must match %s", ErrInvalid, j.Kind, kindPattern)
	}
	if !Printable(j.ProductReference, 256) {
		return fmt.Errorf("%w: product reference must have 1 to 256 bytes of printable UTF-8", ErrInvalid)
	}
	if len(j.Lines) < MinLines || len(j.Lines) > MaxLines {
		return fmt.Errorf("%w: %d lines; %d to %d are required", ErrInvalid, len(j.Lines), MinLines, MaxLines)
	}
	perAsset := make(map[string]*big.Int)
	perAccount := make(map[uuid.UUID]*big.Int)
	for i, l := range j.Lines {
		if err := l.validate(); err != nil {
			return fmt.Errorf("line %d: %w", i+1, err)
		}
		signed := new(big.Int).Set(l.Amount)
		if l.Side == Credit {
			signed.Neg(signed)
		}
		amount.Add(perAsset, l.AssetCode, signed)
		amount.Add(perAccount, l.AccountID, signed)
	}
	for asset, total := range perAsset {
		if total.Sign() != 0 {
			return fmt.Errorf("%w: debits and credits differ by %s in %s", ErrInvalid, total, asset)
		}
	}
	for account, movement := range perAccount {
		if !amount.InRange(movement) {
			return fmt.Errorf("%w: the movement on account %s exceeds the signed 128-bit range", ErrInvalidAmount, account)
		}
	}
	return nil
}

func (l Line) validate() error {
	if l.AccountID == uuid.Nil {
		return fmt.Errorf("%w: account id is required", ErrInvalid)
	}
	if l.Side != Debit && l.Side != Credit {
		return fmt.Errorf("%w: side %q", ErrInvalid, l.Side)
	}
	if !ValidAsset(l.AssetCode) {
		return fmt.Errorf("%w: asset %q", ErrInvalid, l.AssetCode)
	}
	if l.Amount == nil || l.Amount.Sign() <= 0 {
		return fmt.Errorf("%w: amounts must be positive", ErrInvalidAmount)
	}
	return nil
}

func ValidAsset(code string) bool {
	return assetPattern.MatchString(code)
}

func Printable(s string, maxBytes int) bool {
	return len(s) > 0 && len(s) <= maxBytes && utf8.ValidString(s) && !strings.ContainsFunc(s, unicode.IsControl)
}

type canonicalLine struct {
	Account string `json:"account"`
	Side    Side   `json:"side"`
	Asset   string `json:"asset"`
	Amount  string `json:"amount"`
}

type canonicalJournal struct {
	Kind             string          `json:"kind"`
	ProductReference string          `json:"product_reference"`
	Lines            []canonicalLine `json:"lines"`
}

func canonical(j Journal) canonicalJournal {
	lines := make([]canonicalLine, len(j.Lines))
	for i, l := range j.Lines {
		lines[i] = canonicalLine{Account: l.AccountID.String(), Side: l.Side, Asset: l.AssetCode, Amount: l.Amount.String()}
	}
	return canonicalJournal{Kind: j.Kind, ProductReference: j.ProductReference, Lines: lines}
}

func PostJournalHash(ledgerID, accountingDate string, j Journal) [32]byte {
	return hash(struct {
		Version        int              `json:"v"`
		Operation      string           `json:"op"`
		Ledger         string           `json:"ledger"`
		AccountingDate string           `json:"accounting_date"`
		Journal        canonicalJournal `json:"journal"`
	}{HashVersion, "POST_JOURNAL", ledgerID, accountingDate, canonical(j)})
}

// The field order of the structs passed here defines the hash. Changing a field, its tag or its
// position changes every stored request hash, so it needs a HashVersion bump.
func hash(object any) [32]byte {
	encoded, err := json.Marshal(object)
	if err != nil {
		panic(err)
	}
	return sha256.Sum256(encoded)
}

func PostCorrectionHash(ledgerID, accountingDate string, j Journal, correctsEntryID uuid.UUID, reason string) [32]byte {
	return hash(struct {
		Version        int              `json:"v"`
		Operation      string           `json:"op"`
		Ledger         string           `json:"ledger"`
		AccountingDate string           `json:"accounting_date"`
		Journal        canonicalJournal `json:"journal"`
		Corrects       string           `json:"corrects"`
		Reason         string           `json:"reason"`
	}{HashVersion, "POST_CORRECTION", ledgerID, accountingDate, canonical(j), correctsEntryID.String(), reason})
}

// AccountControlsHash takes the new status and overdraft limit as strings, empty when unchanged.
func AccountControlsHash(ledgerID string, accountID uuid.UUID, status, overdraftLimit string) [32]byte {
	return hash(struct {
		Version        int    `json:"v"`
		Operation      string `json:"op"`
		Ledger         string `json:"ledger"`
		Account        string `json:"account"`
		Status         string `json:"status"`
		OverdraftLimit string `json:"overdraft_limit"`
	}{HashVersion, "UPDATE_ACCOUNT_CONTROLS", ledgerID, accountID.String(), status, overdraftLimit})
}
