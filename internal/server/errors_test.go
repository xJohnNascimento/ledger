package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"testing"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestStoreErrorSeparatesTransientFailuresFromBugs(t *testing.T) {
	t.Parallel()
	jsonErr := json.Unmarshal([]byte("{"), new(any))

	cases := map[string]struct {
		err  error
		want connect.Code
	}{
		"serialization failure":        {&pgconn.PgError{Code: "40001"}, connect.CodeAborted},
		"lock timeout":                 {&pgconn.PgError{Code: "55P03"}, connect.CodeAborted},
		"connection failure":           {&pgconn.PgError{Code: "08006"}, connect.CodeUnavailable},
		"admin shutdown":               {&pgconn.PgError{Code: "57P01"}, connect.CodeUnavailable},
		"routine error with no reason": {&pgconn.PgError{Code: "LD204"}, connect.CodeInternal},
		"network error":                {&net.OpError{Op: "read", Err: errors.New("connection reset")}, connect.CodeUnavailable},
		"wrapped unexpected eof":       {fmt.Errorf("scan: %w", io.ErrUnexpectedEOF), connect.CodeUnavailable},
		"deadline":                     {fmt.Errorf("query: %w", context.DeadlineExceeded), connect.CodeDeadlineExceeded},
		"canceled":                     {context.Canceled, connect.CodeCanceled},
		"corrupt stored amount":        {fmt.Errorf("stored amount %q: %w", "01", errors.New("not canonical")), connect.CodeInternal},
		"undecodable json":             {fmt.Errorf("decode: %w", jsonErr), connect.CodeInternal},
		"plain error":                  {errors.New("boom"), connect.CodeInternal},
	}
	for name, c := range cases {
		got := connect.CodeOf(storeError(context.Background(), c.err))
		if got != c.want {
			t.Errorf("%s: got %v, want %v", name, got, c.want)
		}
	}
}
