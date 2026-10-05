package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Marker for an unreachable database
type connectError struct{ err error }

func (e *connectError) Error() string { return connectReason(e.err) }

// Read from the first reachable database
func readHostRows(ctx context.Context, primary DatabaseConfig, backups []DatabaseConfig) ([]HostRow, error) {
	var failures []string
	for _, target := range append([]DatabaseConfig{primary}, backups...) {
		rows, err := readHostRowsFrom(ctx, target)
		var unreachable *connectError
		if !errors.As(err, &unreachable) {
			return rows, err
		}
		failures = append(failures, fmt.Sprintf("%s:%d: %v", target.Host, target.Port, unreachable))
	}
	return nil, fmt.Errorf("PostgreSQL connection failed for all configured databases: %s",
		strings.Join(failures, "; "))
}

// Validate the columns and load every row, read-only
func readHostRowsFrom(ctx context.Context, config DatabaseConfig) ([]HostRow, error) {
	conn, err := pgx.Connect(ctx, fmt.Sprintf("host=%s port=%d dbname=%s user=%s password=%s connect_timeout=10",
		dsnValue(config.Host), config.Port, dsnValue(config.Name), dsnValue(config.User), dsnValue(config.Password)))
	if err != nil {
		return nil, &connectError{err}
	}
	defer func() {
		// Bounded close, even after cancellation
		closing, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		conn.Close(closing)
	}()

	tx, err := conn.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, readFailed(err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	if err := validateColumns(ctx, tx); err != nil {
		return nil, err
	}
	// Priority as text keeps every numeric type exact
	query := fmt.Sprintf("SELECT status, ip, address, priority::text FROM %s", pgx.Identifier{dbSchema, dbTable}.Sanitize())
	rows, err := tx.Query(ctx, query)
	if err != nil {
		return nil, readFailed(err)
	}
	records, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (HostRow, error) {
		values, err := row.Values()
		if err != nil {
			return HostRow{}, err
		}
		return HostRow{Status: values[0], IP: values[1], Address: values[2], Priority: values[3]}, nil
	})
	if err != nil {
		return nil, readFailed(err)
	}
	return records, nil
}

func validateColumns(ctx context.Context, tx pgx.Tx) error {
	rows, err := tx.Query(ctx, `
		SELECT column_name::text
		FROM information_schema.columns
		WHERE table_schema = $1 AND table_name = $2`, dbSchema, dbTable)
	if err != nil {
		return readFailed(err)
	}
	available, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return readFailed(err)
	}
	missing := map[string]bool{"status": true, "ip": true, "address": true, "priority": true}
	for _, name := range available {
		delete(missing, name)
	}
	if len(missing) == 0 {
		return nil
	}
	names := make([]string, 0, len(missing))
	for name := range missing {
		names = append(names, "'"+name+"'")
	}
	sort.Strings(names)
	return fmt.Errorf("required PostgreSQL columns are missing: %s", strings.Join(names, ", "))
}

// Quote a keyword/value connection string value
func dsnValue(value string) string {
	return "'" + strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(value) + "'"
}

// Server message when available, generic text otherwise
func readFailed(err error) error {
	var server *pgconn.PgError
	if errors.As(err, &server) {
		return errors.New(server.Message)
	}
	return errors.New("PostgreSQL read failed")
}

// Short cause of a failed connection attempt
func connectReason(err error) string {
	var server *pgconn.PgError
	if errors.As(err, &server) {
		return server.Message
	}
	var network *net.OpError
	if errors.As(err, &network) {
		return network.Error()
	}
	return strings.Join(strings.Fields(err.Error()), " ")
}
