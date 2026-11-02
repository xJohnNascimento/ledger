package export_test

import (
	"testing"
	"time"

	"github.com/xJohnNascimento/ledger/internal/export"
)

func TestLimitsFallBackToTheDefaultsPerField(t *testing.T) {
	t.Parallel()
	got := export.Limits{MaxRecords: 3}.OrDefaults()
	if got.MaxRecords != 3 || got.MaxDuration != export.DefaultLimits.MaxDuration {
		t.Fatalf("got %+v, want 3 records and the default duration", got)
	}
	got = export.Limits{MaxDuration: time.Second}.OrDefaults()
	if got.MaxRecords != export.DefaultLimits.MaxRecords || got.MaxDuration != time.Second {
		t.Fatalf("got %+v, want the default records and one second", got)
	}
}
