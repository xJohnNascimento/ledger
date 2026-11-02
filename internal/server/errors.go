package server

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5/pgconn"

	ledgerv1 "github.com/xJohnNascimento/ledger/api/ledger/v1"
	"github.com/xJohnNascimento/ledger/internal/store"
)

var codeByReason = map[ledgerv1.Reason]connect.Code{
	ledgerv1.Reason_REASON_INVALID_AMOUNT:                  connect.CodeInvalidArgument,
	ledgerv1.Reason_REASON_INVALID_JOURNAL:                 connect.CodeInvalidArgument,
	ledgerv1.Reason_REASON_OUT_OF_SCOPE:                    connect.CodeInvalidArgument,
	ledgerv1.Reason_REASON_ACCOUNT_CLASSIFICATION_CONFLICT: connect.CodeInvalidArgument,
	ledgerv1.Reason_REASON_INVALID_RESERVATION:             connect.CodeInvalidArgument,
	ledgerv1.Reason_REASON_REFERENCE_CONFLICT:              connect.CodeAlreadyExists,
	ledgerv1.Reason_REASON_INSUFFICIENT_FUNDS:              connect.CodeFailedPrecondition,
	ledgerv1.Reason_REASON_ACCOUNT_BLOCKED:                 connect.CodeFailedPrecondition,
	ledgerv1.Reason_REASON_CEILING_EXCEEDED:                connect.CodeFailedPrecondition,
	ledgerv1.Reason_REASON_RESERVATION_FINAL:               connect.CodeFailedPrecondition,
	ledgerv1.Reason_REASON_NOT_FOUND:                       connect.CodeNotFound,
	ledgerv1.Reason_REASON_NAMESPACE_NOT_ALLOWED:           connect.CodePermissionDenied,
	ledgerv1.Reason_REASON_PURPOSE_NOT_ALLOWED:             connect.CodePermissionDenied,
	ledgerv1.Reason_REASON_SCOPE_NOT_ALLOWED:               connect.CodePermissionDenied,
}

func reasonError(reason ledgerv1.Reason, message string, detail *ledgerv1.ErrorDetail) *connect.Error {
	code, ok := codeByReason[reason]
	if !ok {
		code = connect.CodeInternal
	}
	err := connect.NewError(code, errors.New(message))
	if detail == nil {
		detail = &ledgerv1.ErrorDetail{}
	}
	detail.Reason = reason
	if d, detailErr := connect.NewErrorDetail(detail); detailErr == nil {
		err.AddDetail(d)
	}
	return err
}

func invalidArgument(message string) *connect.Error {
	return connect.NewError(connect.CodeInvalidArgument, errors.New(message))
}

func refusal(command store.Command) *connect.Error {
	return reasonError(reasonOf(command.Reason), "refused: "+command.Reason, detailOf(command.Reason, command.Detail))
}

func storeError(ctx context.Context, err error) error {
	var storeErr *store.Error
	if errors.As(err, &storeErr) {
		return reasonError(reasonOf(storeErr.Reason), storeErr.Message, detailOf(storeErr.Reason, storeErr.Detail))
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return connect.NewError(connect.CodeDeadlineExceeded, err)
	case errors.Is(err, context.Canceled):
		return connect.NewError(connect.CodeCanceled, err)
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgError(ctx, pgErr)
	}
	if isTransient(err) {
		slog.WarnContext(ctx, "transient failure", "error", err)
		return unavailable()
	}
	slog.ErrorContext(ctx, "unexpected failure", "error", err)
	return connect.NewError(connect.CodeInternal, errors.New("internal error"))
}

// lock_timeout expiry; the transaction rolled back, so a retry is safe.
const lockNotAvailable = "55P03"

func pgError(ctx context.Context, pgErr *pgconn.PgError) error {
	if pgErr.Code == lockNotAvailable {
		return connect.NewError(connect.CodeAborted, errors.New("lock wait timed out; retry with the same reference and body"))
	}
	switch sqlStateClass(pgErr.Code) {
	case "40":
		return connect.NewError(connect.CodeAborted, errors.New("transaction aborted; retry with the same reference and body"))
	case "08", "53", "57":
		return unavailable()
	}
	slog.ErrorContext(ctx, "unexpected database error", "sqlstate", pgErr.Code, "message", pgErr.Message)
	return connect.NewError(connect.CodeInternal, errors.New("internal error"))
}

func sqlStateClass(code string) string {
	if len(code) < 2 {
		return ""
	}
	return code[:2]
}

// Only a failure of the connection or the transport is worth a retry; anything else, such as a
// corrupt stored value, would fail the same way every time.
func isTransient(err error) bool {
	var netErr net.Error
	return errors.As(err, &netErr) || errors.Is(err, io.ErrUnexpectedEOF) || pgconn.SafeToRetry(err)
}

func unavailable() *connect.Error {
	return connect.NewError(connect.CodeUnavailable, errors.New("temporarily unavailable; retry with the same reference and body"))
}

func reasonOf(name string) ledgerv1.Reason {
	return ledgerv1.Reason(ledgerv1.Reason_value["REASON_"+name])
}

// The routines report the ceiling of a CEILING_EXCEEDED refusal under the key "available".
func detailOf(reason string, fields map[string]string) *ledgerv1.ErrorDetail {
	if fields == nil {
		return nil
	}
	available, ceiling := fields["available"], ""
	if reason == "CEILING_EXCEEDED" {
		available, ceiling = "", available
	}
	return &ledgerv1.ErrorDetail{
		AccountId:         fields["account_id"],
		Available:         available,
		Ceiling:           ceiling,
		Required:          fields["required"],
		FinalState:        ledgerv1.ReservationState(ledgerv1.ReservationState_value["RESERVATION_STATE_"+fields["final_state"]]),
		FinalCommandId:    fields["final_command_id"],
		FinalFingerprint:  fields["final_fingerprint"],
		ExistingCommandId: fields["existing_command_id"],
	}
}
