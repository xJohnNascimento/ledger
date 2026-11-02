package export

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"hash"
	"log/slog"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	ledgerv1 "github.com/xJohnNascimento/ledger/api/ledger/v1"
	"github.com/xJohnNascimento/ledger/internal/store"
	"github.com/xJohnNascimento/ledger/internal/wire"
)

const SchemaVersion = 1

type Limits struct {
	MaxRecords  int64
	MaxDuration time.Duration
}

var DefaultLimits = Limits{MaxRecords: 5_000_000, MaxDuration: 5 * time.Minute}

// OrDefaults replaces each zero field with its default, so a partial value never means "no time".
func (l Limits) OrDefaults() Limits {
	return Limits{
		MaxRecords:  cmp.Or(l.MaxRecords, DefaultLimits.MaxRecords),
		MaxDuration: cmp.Or(l.MaxDuration, DefaultLimits.MaxDuration),
	}
}

type Send func(*ledgerv1.ExportAuditSnapshotResponse) error

var errLimit = errors.New("the export reached its record limit")

type sendError struct{ err error }

func (e sendError) Error() string { return e.err.Error() }
func (e sendError) Unwrap() error { return e.err }

// Stream sends one snapshot of a ledger. A database failure, a timeout or a reached limit still
// ends the stream with an INCOMPLETE trailer; only a failed send ends it without one.
func Stream(ctx context.Context, s *store.Store, ledgerID string, limits Limits, send Send) error {
	limits = limits.OrDefaults()
	ctx, cancel := context.WithTimeout(ctx, limits.MaxDuration)
	defer cancel()
	w := &writer{
		send:     send,
		ledgerID: ledgerID,
		limit:    limits.MaxRecords,
		hash:     sha256.New(),
		counts:   make(map[string]int64),
	}
	status := ledgerv1.SnapshotStatus_SNAPSHOT_STATUS_COMPLETE
	if err := s.Snapshot(ctx, ledgerID, w); err != nil {
		var failedSend sendError
		if errors.As(err, &failedSend) {
			return failedSend.err
		}
		slog.WarnContext(ctx, "audit export interrupted", "ledger", ledgerID, "records", w.records, "error", err)
		status = ledgerv1.SnapshotStatus_SNAPSHOT_STATUS_INCOMPLETE
	}
	return send(&ledgerv1.ExportAuditSnapshotResponse{Record: &ledgerv1.ExportAuditSnapshotResponse_Trailer{
		Trailer: &ledgerv1.SnapshotTrailer{Status: status, Counts: w.counts, ContentHash: hex.EncodeToString(w.hash.Sum(nil))},
	}})
}

type writer struct {
	send     Send
	ledgerID string
	limit    int64
	records  int64
	hash     hash.Hash
	counts   map[string]int64
}

func (w *writer) Header(capturedAt time.Time) error {
	return w.emit("", &ledgerv1.ExportAuditSnapshotResponse{Record: &ledgerv1.ExportAuditSnapshotResponse_Header{
		Header: &ledgerv1.SnapshotHeader{
			SchemaVersion: SchemaVersion,
			SourceId:      "ledger",
			DatasetId:     w.ledgerID,
			CapturedAt:    timestamppb.New(capturedAt),
		},
	}})
}

func (w *writer) Account(a store.Account) error {
	return w.emit("account", &ledgerv1.ExportAuditSnapshotResponse{Record: &ledgerv1.ExportAuditSnapshotResponse_Account{Account: wire.Account(a)}})
}

func (w *writer) Balance(b store.Balance) error {
	return w.emit("balance", &ledgerv1.ExportAuditSnapshotResponse{Record: &ledgerv1.ExportAuditSnapshotResponse_Balance{Balance: wire.Balance(b)}})
}

func (w *writer) Entry(e store.Entry) error {
	return w.emit("entry", &ledgerv1.ExportAuditSnapshotResponse{Record: &ledgerv1.ExportAuditSnapshotResponse_Entry{Entry: wire.Entry(e)}})
}

func (w *writer) Reservation(r store.Reservation) error {
	return w.emit("reservation", &ledgerv1.ExportAuditSnapshotResponse{Record: &ledgerv1.ExportAuditSnapshotResponse_Reservation{Reservation: wire.Reservation(r)}})
}

func (w *writer) Command(c store.Command) error {
	return w.emit("command", &ledgerv1.ExportAuditSnapshotResponse{Record: &ledgerv1.ExportAuditSnapshotResponse_Command{Command: wire.Command(c)}})
}

func (w *writer) emit(kind string, record *ledgerv1.ExportAuditSnapshotResponse) error {
	if kind != "" {
		if w.records >= w.limit {
			return errLimit
		}
		w.records++
		w.counts[kind]++
	}
	if err := digest(w.hash, record); err != nil {
		return err
	}
	if err := w.send(record); err != nil {
		return sendError{err}
	}
	return nil
}

func digest(h hash.Hash, record *ledgerv1.ExportAuditSnapshotResponse) error {
	encoded, err := proto.MarshalOptions{Deterministic: true}.Marshal(record)
	if err != nil {
		return err
	}
	var length [4]byte
	binary.BigEndian.PutUint32(length[:], uint32(len(encoded))) //nolint:gosec // a marshaled protobuf message is capped at 2 GiB
	h.Write(length[:])
	h.Write(encoded)
	return nil
}
