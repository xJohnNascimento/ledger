package amount_test

import (
	"errors"
	"math/big"
	"regexp"
	"testing"

	"pgregory.net/rapid"

	"github.com/xJohnNascimento/ledger/internal/amount"
)

const (
	max = "170141183460469231731687303715884105727"
	min = "-170141183460469231731687303715884105728"
)

func TestParseAcceptsCanonicalNonNegativeValues(t *testing.T) {
	for _, s := range []string{"0", "1", "10", "995000", max} {
		v, err := amount.Parse(s)
		if err != nil || v.String() != s {
			t.Errorf("Parse(%q) = %v, %v; want %s", s, v, err, s)
		}
	}
}

func TestParseRefusesAnythingElse(t *testing.T) {
	cases := map[string]error{
		"":     amount.ErrNotCanonical,
		"-1":   amount.ErrNotCanonical,
		"+1":   amount.ErrNotCanonical,
		"01":   amount.ErrNotCanonical,
		"00":   amount.ErrNotCanonical,
		"1.0":  amount.ErrNotCanonical,
		"1e3":  amount.ErrNotCanonical,
		" 1":   amount.ErrNotCanonical,
		"1 ":   amount.ErrNotCanonical,
		"0x10": amount.ErrNotCanonical,
		"１":    amount.ErrNotCanonical,
		"NaN":  amount.ErrNotCanonical,
		"170141183460469231731687303715884105728":  amount.ErrOutOfRange,
		"9999999999999999999999999999999999999999": amount.ErrOutOfRange,
	}
	for s, want := range cases {
		if _, err := amount.Parse(s); !errors.Is(err, want) {
			t.Errorf("Parse(%q) error = %v, want %v", s, err, want)
		}
	}
}

func TestParseSignedAcceptsTheFullSigned128BitRange(t *testing.T) {
	for _, s := range []string{min, "-1", "0", "1", max} {
		v, err := amount.ParseSigned(s)
		if err != nil || v.String() != s {
			t.Errorf("ParseSigned(%q) = %v, %v; want %s", s, v, err, s)
		}
	}
}

func TestParseSignedRefusesAnythingElse(t *testing.T) {
	cases := map[string]error{
		"-0":  amount.ErrNotCanonical,
		"--1": amount.ErrNotCanonical,
		"-01": amount.ErrNotCanonical,
		"-":   amount.ErrNotCanonical,
		"+1":  amount.ErrNotCanonical,
		"-170141183460469231731687303715884105729": amount.ErrOutOfRange,
		"170141183460469231731687303715884105728":  amount.ErrOutOfRange,
	}
	for s, want := range cases {
		if _, err := amount.ParseSigned(s); !errors.Is(err, want) {
			t.Errorf("ParseSigned(%q) error = %v, want %v", s, err, want)
		}
	}
}

func TestParseSignedRoundTripsEveryValueInRange(t *testing.T) {
	lo, _ := new(big.Int).SetString(min, 10)
	span, _ := new(big.Int).SetString("340282366920938463463374607431768211455", 10)
	rapid.Check(t, func(t *rapid.T) {
		raw := new(big.Int).SetBytes(rapid.SliceOfN(rapid.Byte(), 16, 16).Draw(t, "bytes"))
		v := new(big.Int).Add(lo, raw.Mod(raw, new(big.Int).Add(span, big.NewInt(1))))
		got, err := amount.ParseSigned(v.String())
		if err != nil || got.Cmp(v) != 0 {
			t.Fatalf("ParseSigned(%s) = %v, %v", v, got, err)
		}
	})
}

func TestParseSignedAgreesWithAnIndependentModel(t *testing.T) {
	canonical := regexp.MustCompile(`^(0|-?[1-9][0-9]*)$`)
	lo, _ := new(big.Int).SetString(min, 10)
	hi, _ := new(big.Int).SetString(max, 10)
	rapid.Check(t, func(t *rapid.T) {
		s := rapid.StringMatching(`[-+ 0-9.e]{0,42}`).Draw(t, "s")
		_, err := amount.ParseSigned(s)
		want := error(nil)
		switch v, _ := new(big.Int).SetString(s, 10); {
		case !canonical.MatchString(s):
			want = amount.ErrNotCanonical
		case v.Cmp(lo) < 0 || v.Cmp(hi) > 0:
			want = amount.ErrOutOfRange
		}
		if !errors.Is(err, want) {
			t.Fatalf("ParseSigned(%q) error = %v, want %v", s, err, want)
		}
	})
}
