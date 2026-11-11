package postgres

import (
	"context"
	"errors"
	"net"
	"strconv"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Limits are the ones the composition root declares (Annex C, explicit limits): a value left to the default of the
// driver is not a decision.
//
// shortcut: pgxpool has no wait limit of its own; the 5 s wait for a free connection is bounded by the deadline of the
// caller's context and ConnectTimeout bounds only the dial. Upgrade when a load test shows saves queuing for the pool.
type Limits struct {
	MaxConns         int32
	ConnectTimeout   time.Duration
	StatementTimeout time.Duration
}

// Open connects, checks the database answers and returns the pool. The errors are fixed text plus the kind of failure:
// the driver's own errors quote the connection string, and the connection string carries the password.
func Open(ctx context.Context, url string, limits Limits) (*pgxpool.Pool, error) {
	// a limit left at zero would be a connection that times out at once, no pool, or no statement timeout: refused, not defaulted
	if limits.MaxConns < 1 || limits.ConnectTimeout <= 0 || limits.StatementTimeout <= 0 {
		return nil, errors.New("the database limits must all be positive")
	}
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, errors.New("DATABASE_URL cannot be parsed")
	}
	cfg.MaxConns = limits.MaxConns
	cfg.ConnConfig.ConnectTimeout = limits.ConnectTimeout
	cfg.ConnConfig.RuntimeParams["statement_timeout"] = strconv.FormatInt(limits.StatementTimeout.Milliseconds(), 10)

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, errors.New("the database pool cannot be created")
	}
	checkCtx, cancel := context.WithTimeout(ctx, limits.ConnectTimeout)
	defer cancel()
	if err := pool.Ping(checkCtx); err != nil {
		pool.Close()
		return nil, errors.New("the database does not answer: " + kindOf(err))
	}
	return pool, nil
}

// kindOf tells an operator what failed without repeating anything the driver quoted.
func kindOf(err error) string {
	var pg *pgconn.PgError
	var network net.Error
	var errno syscall.Errno
	switch {
	case errors.As(err, &pg):
		return "SQLSTATE " + pg.Code
	// 10061 is the same refusal as Windows words it (WSAECONNREFUSED)
	case errors.As(err, &errno) && (errno == syscall.ECONNREFUSED || errno == 10061):
		return "connection refused"
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &network) && network.Timeout():
		return "timeout"
	default:
		return "unclassified"
	}
}
