package pg

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
)

// IsRetryable reports whether err is a transient failure after which a stage may reconnect and
// resume from its persisted position: network problems, server restarts, deadlocks and a replication
// slot that the server still considers active.
//
// Everything else (authentication, syntax, constraint and data errors, cancelled contexts) is
// permanent: retrying would loop forever, so the caller reports it and stops.
func IsRetryable(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) {
		return false
	}

	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch {
		case strings.HasPrefix(pgErr.Code, "08"): // connection exception
			return true
		case strings.HasPrefix(pgErr.Code, "40"): // serialization failure, deadlock
			return true
		case strings.HasPrefix(pgErr.Code, "53"): // insufficient resources
			return true
		case pgErr.Code == "57P01", pgErr.Code == "57P02", pgErr.Code == "57P03": // shutdown, crash, not accepting
			return true
		case pgErr.Code == "55006": // object_in_use: the slot is still active for a dead connection
			return true
		}
		return false
	}

	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, net.ErrClosed) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	var connErr *pgconn.ConnectError
	return errors.As(err, &connErr)
}

// SQLState returns the SQLSTATE code carried by err, or "" when err is not a server error.
func SQLState(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}
