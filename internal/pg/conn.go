package pg

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Querier is the subset of pgx used by the query helpers. It is implemented by *pgxpool.Pool,
// *pgx.Conn and pgx.Tx, so the same function can run inside or outside a transaction.
type Querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Settings that every gophant connection starts with.
//
//   - client_encoding=UTF8 makes both servers translate to the same wire encoding even when the
//     two databases use different server encodings.
//   - The three timeouts are switched off because a role or database level default such as
//     statement_timeout=30s would otherwise abort a long COPY or kill the idle snapshot connection.
var sessionDefaults = map[string]string{
	"client_encoding":                     "UTF8",
	"statement_timeout":                   "0",
	"lock_timeout":                        "0",
	"idle_in_transaction_session_timeout": "0",
}

// Settings that make the walsender output values in a form that is unambiguous for any target,
// the same ones the built-in subscriber forces. Without them a cluster-level DateStyle or
// IntervalStyle on s1 would change how timestamps and intervals are rendered in the WAL stream.
var replicationDefaults = map[string]string{
	"datestyle":          "ISO",
	"intervalstyle":      "postgres",
	"extra_float_digits": "3",
}

const keepAlive = 30 * time.Second

// ParseConfig parses a connection string and applies the gophant session defaults.
func ParseConfig(dsn, appName string) (*pgx.ConnConfig, error) {
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse connection string %s: %w", Redact(dsn), err)
	}
	for k, v := range sessionDefaults {
		cfg.RuntimeParams[k] = v
	}
	if _, ok := cfg.RuntimeParams["application_name"]; !ok {
		cfg.RuntimeParams["application_name"] = appName
	}
	cfg.DialFunc = (&net.Dialer{KeepAlive: keepAlive}).DialContext
	return cfg, nil
}

// Connect opens one regular connection.
func Connect(ctx context.Context, dsn, appName string) (*pgx.Conn, error) {
	cfg, err := ParseConfig(dsn, appName)
	if err != nil {
		return nil, err
	}
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("connect to %s: %w", Redact(dsn), err)
	}
	return conn, nil
}

// PoolOption customizes a pool before it is opened.
type PoolOption func(*pgxpool.Config)

// WithAfterConnect runs fn on every new connection of the pool, for example to set session parameters.
func WithAfterConnect(fn func(context.Context, *pgx.Conn) error) PoolOption {
	return func(c *pgxpool.Config) { c.AfterConnect = fn }
}

// NewPool opens a connection pool and verifies that it can reach the server.
func NewPool(ctx context.Context, dsn, appName string, maxConns int32, opts ...PoolOption) (*pgxpool.Pool, error) {
	cc, err := ParseConfig(dsn, appName)
	if err != nil {
		return nil, err
	}
	pc, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse connection string %s: %w", Redact(dsn), err)
	}
	pc.ConnConfig = cc
	if maxConns > 0 {
		pc.MaxConns = maxConns
	}
	for _, opt := range opts {
		opt(pc)
	}
	pool, err := pgxpool.NewWithConfig(ctx, pc)
	if err != nil {
		return nil, fmt.Errorf("open pool for %s: %w", Redact(dsn), err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("connect to %s: %w", Redact(dsn), err)
	}
	return pool, nil
}

// ReplicationDSN returns the connection string for the replication protocol: the configured
// replication DSN, or the regular DSN when none is configured.
func ReplicationDSN(dsn, replicationDSN string) string {
	if replicationDSN != "" {
		return replicationDSN
	}
	return dsn
}

// ConnectReplication opens a logical replication connection (replication=database).
func ConnectReplication(ctx context.Context, dsn, appName string) (*pgconn.PgConn, error) {
	cfg, err := ParseConfig(dsn, appName)
	if err != nil {
		return nil, err
	}
	cfg.RuntimeParams["replication"] = "database"
	for k, v := range replicationDefaults {
		cfg.RuntimeParams[k] = v
	}
	conn, err := pgconn.ConnectConfig(ctx, &cfg.Config)
	if err != nil {
		return nil, fmt.Errorf("connect replication to %s: %w", Redact(dsn), err)
	}
	return conn, nil
}

// SetReplicaRole switches the session to session_replication_role = replica so that triggers and
// foreign key checks of the application do not fire while data is loaded or replayed.
func SetReplicaRole(ctx context.Context, q Querier) error {
	if _, err := q.Exec(ctx, "SET session_replication_role = 'replica'"); err != nil {
		return fmt.Errorf("set session_replication_role = replica (the role needs superuser or the SET privilege on this parameter): %w", err)
	}
	return nil
}

// Ident quotes one SQL identifier.
func Ident(name string) string { return pgx.Identifier{name}.Sanitize() }

// Qualified quotes a schema-qualified name.
func Qualified(schema, name string) string { return pgx.Identifier{schema, name}.Sanitize() }

// ServerVersionNum returns server_version_num of the connected server, e.g. 160004.
func ServerVersionNum(ctx context.Context, q Querier) (int, error) {
	var v int
	if err := q.QueryRow(ctx, "SELECT current_setting('server_version_num')::int").Scan(&v); err != nil {
		return 0, fmt.Errorf("read server_version_num: %w", err)
	}
	return v, nil
}
