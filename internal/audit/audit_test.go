package audit_test

import (
	"slices"
	"testing"

	ledgerv1 "github.com/xJohnNascimento/ledger/api/ledger/v1"
	"github.com/xJohnNascimento/ledger/internal/audit"
)

func commandRecord(command *ledgerv1.Command) *ledgerv1.ExportAuditSnapshotResponse {
	return &ledgerv1.ExportAuditSnapshotResponse{Record: &ledgerv1.ExportAuditSnapshotResponse_Command{Command: command}}
}

func TestCompletedCommandsWithoutTheirEffectOrOfAnUnknownKindAreFindings(t *testing.T) {
	t.Parallel()
	completed := ledgerv1.CommandOutcome_COMMAND_OUTCOME_COMPLETED
	account := &ledgerv1.Account{Id: "account-1", LedgerId: "corridor"}
	cases := map[string]struct {
		command *ledgerv1.Command
		want    bool
	}{
		"controls on a known account": {
			&ledgerv1.Command{Id: "c", LedgerId: "corridor", Operation: "UPDATE_ACCOUNT_CONTROLS", Outcome: completed, AccountId: "account-1"},
			false,
		},
		"controls on a missing account": {
			&ledgerv1.Command{Id: "c", LedgerId: "corridor", Operation: "UPDATE_ACCOUNT_CONTROLS", Outcome: completed, AccountId: "account-2"},
			true,
		},
		"controls with no account": {
			&ledgerv1.Command{Id: "c", LedgerId: "corridor", Operation: "UPDATE_ACCOUNT_CONTROLS", Outcome: completed},
			true,
		},
		"unknown operation": {
			&ledgerv1.Command{Id: "c", LedgerId: "corridor", Operation: "MINT", Outcome: completed},
			true,
		},
	}
	for name, c := range cases {
		checker := audit.NewChecker()
		records := []*ledgerv1.ExportAuditSnapshotResponse{
			{Record: &ledgerv1.ExportAuditSnapshotResponse_Account{Account: account}},
			commandRecord(c.command),
		}
		for _, record := range records {
			if err := checker.Add(record); err != nil {
				t.Fatal(err)
			}
		}
		found := slices.ContainsFunc(checker.Report().Findings, func(f audit.Finding) bool { return f.Check == audit.CheckLinks })
		if found != c.want {
			t.Errorf("%s: link finding %v, want %v", name, found, c.want)
		}
	}
}
