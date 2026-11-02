package store

import (
	"errors"

	"github.com/jackc/pgx/v5/pgconn"
)

type Error struct {
	Reason  string
	Message string
	Detail  map[string]string
}

func (e *Error) Error() string {
	if e.Message == "" {
		return e.Reason
	}
	return e.Message
}

func (e *Error) Is(target error) bool {
	t, ok := target.(*Error)
	return ok && t.Reason == e.Reason
}

var (
	ErrNotFound               = &Error{Reason: "NOT_FOUND"}
	ErrClassificationConflict = &Error{Reason: "ACCOUNT_CLASSIFICATION_CONFLICT"}
	ErrReferenceConflict      = &Error{Reason: "REFERENCE_CONFLICT"}
	ErrInvalidJournal         = &Error{Reason: "INVALID_JOURNAL"}
	ErrInvalidAmount          = &Error{Reason: "INVALID_AMOUNT"}
	ErrPurposeNotAllowed      = &Error{Reason: "PURPOSE_NOT_ALLOWED"}
	ErrOutOfScope             = &Error{Reason: "OUT_OF_SCOPE"}
	ErrInvalidReservation     = &Error{Reason: "INVALID_RESERVATION"}
)

var reasonBySQLState = map[string]string{
	"LD101": ErrClassificationConflict.Reason,
	"LD201": ErrInvalidJournal.Reason,
	"LD202": ErrInvalidJournal.Reason,
	"LD400": ErrInvalidAmount.Reason,
	"LD403": ErrPurposeNotAllowed.Reason,
	"LD404": ErrNotFound.Reason,
	"LD409": ErrReferenceConflict.Reason,
	"LD422": ErrInvalidJournal.Reason,
	"LD423": ErrOutOfScope.Reason,
	"LD424": ErrInvalidReservation.Reason,
}

func mapError(err error) error {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return err
	}
	reason, ok := reasonBySQLState[pgErr.Code]
	if !ok {
		return err
	}
	mapped := &Error{Reason: reason, Message: pgErr.Message}
	if pgErr.Code == "LD409" {
		mapped.Detail = map[string]string{"existing_command_id": pgErr.Detail}
	}
	return mapped
}
