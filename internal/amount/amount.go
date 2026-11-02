package amount

import (
	"errors"
	"math/big"
)

var (
	ErrNotCanonical = errors.New("amount is not a canonical base-10 integer")
	ErrOutOfRange   = errors.New("amount is outside the signed 128-bit range")
)

const maxDigits = 39

var (
	maxAmount = new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 127), big.NewInt(1))
	minAmount = new(big.Int).Neg(new(big.Int).Lsh(big.NewInt(1), 127))
)

// InRange reports whether v fits the signed 128-bit range every stored amount and balance lives in.
func InRange(v *big.Int) bool {
	return v.Cmp(minAmount) >= 0 && v.Cmp(maxAmount) <= 0
}

// Add accumulates v under key in totals without aliasing v.
func Add[K comparable](totals map[K]*big.Int, key K, v *big.Int) {
	if total, ok := totals[key]; ok {
		total.Add(total, v)
		return
	}
	totals[key] = new(big.Int).Set(v)
}

// Parse accepts a non-negative amount: "0", or a nonzero digit followed by digits.
func Parse(s string) (*big.Int, error) {
	if !canonicalDigits(s) {
		return nil, ErrNotCanonical
	}
	return inRange(s)
}

// ParseSigned also accepts one leading minus sign on a nonzero amount.
func ParseSigned(s string) (*big.Int, error) {
	digits := s
	if len(s) > 0 && s[0] == '-' {
		digits = s[1:]
	}
	if !canonicalDigits(digits) || (digits != s && digits == "0") {
		return nil, ErrNotCanonical
	}
	return inRange(s)
}

func canonicalDigits(s string) bool {
	if s == "0" {
		return true
	}
	if s == "" || s[0] < '1' || s[0] > '9' {
		return false
	}
	for i := 1; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func inRange(s string) (*big.Int, error) {
	digits := len(s)
	if s[0] == '-' {
		digits--
	}
	if digits > maxDigits {
		return nil, ErrOutOfRange
	}
	v, _ := new(big.Int).SetString(s, 10)
	if !InRange(v) {
		return nil, ErrOutOfRange
	}
	return v, nil
}
