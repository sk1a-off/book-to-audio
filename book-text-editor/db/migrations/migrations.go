// Package migrations applies the versioned PostgreSQL schema.
package migrations

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed *.up.sql
var files embed.FS

const advisoryLockID int64 = 0x5454534D565031

// Apply runs every unapplied up migration under a PostgreSQL advisory lock.
func Apply(ctx context.Context, pool *pgxpool.Pool) error {
	if pool == nil {
		return errors.New("PostgreSQL pool is required")
	}

	connection, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire migration connection: %w", err)
	}
	defer connection.Release()

	if _, err := connection.Exec(
		ctx,
		"SELECT pg_advisory_lock($1)",
		advisoryLockID,
	); err != nil {
		return fmt.Errorf("acquire migration lock: %w", err)
	}
	defer func() {
		unlockContext, cancel := context.WithTimeout(
			context.Background(),
			5*time.Second,
		)
		defer cancel()
		_, _ = connection.Exec(
			unlockContext,
			"SELECT pg_advisory_unlock($1)",
			advisoryLockID,
		)
	}()

	if _, err := connection.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version BIGINT PRIMARY KEY,
			name TEXT NOT NULL,
			applied_at TIMESTAMPTZ NOT NULL
		)
	`); err != nil {
		return fmt.Errorf("create migration ledger: %w", err)
	}

	migrationFiles, err := fs.Glob(files, "*.up.sql")
	if err != nil {
		return fmt.Errorf("list embedded migrations: %w", err)
	}
	sort.Strings(migrationFiles)
	for _, filename := range migrationFiles {
		if err := applyFile(ctx, connection, filename); err != nil {
			return err
		}
	}

	return nil
}

func applyFile(
	ctx context.Context,
	connection *pgxpool.Conn,
	filename string,
) error {
	version, err := migrationVersion(filename)
	if err != nil {
		return err
	}

	var applied bool
	if err := connection.QueryRow(
		ctx,
		"SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = $1)",
		version,
	).Scan(&applied); err != nil {
		return fmt.Errorf("check migration %s: %w", filename, err)
	}
	if applied {
		return nil
	}

	script, err := files.ReadFile(filename)
	if err != nil {
		return fmt.Errorf("read migration %s: %w", filename, err)
	}
	body, err := migrationBody(string(script))
	if err != nil {
		return fmt.Errorf("validate migration %s: %w", filename, err)
	}

	transaction, err := connection.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin migration %s: %w", filename, err)
	}
	defer func() {
		_ = transaction.Rollback(context.Background())
	}()

	if _, err := transaction.Exec(ctx, body); err != nil {
		return fmt.Errorf("execute migration %s: %w", filename, err)
	}
	if _, err := transaction.Exec(
		ctx,
		`
			INSERT INTO schema_migrations (version, name, applied_at)
			VALUES ($1, $2, $3)
		`,
		version,
		filename,
		time.Now().UTC(),
	); err != nil {
		return fmt.Errorf("record migration %s: %w", filename, err)
	}
	if err := transaction.Commit(ctx); err != nil {
		return fmt.Errorf("commit migration %s: %w", filename, err)
	}

	return nil
}

func migrationVersion(filename string) (int64, error) {
	prefix, _, found := strings.Cut(filepath.Base(filename), "_")
	if !found {
		return 0, fmt.Errorf("migration %q has no numeric prefix", filename)
	}
	version, err := strconv.ParseInt(prefix, 10, 64)
	if err != nil || version <= 0 {
		return 0, fmt.Errorf("migration %q has invalid version", filename)
	}

	return version, nil
}

func migrationBody(script string) (string, error) {
	lines := strings.Split(strings.TrimSpace(script), "\n")
	if len(lines) < 3 ||
		!strings.EqualFold(strings.TrimSpace(lines[0]), "BEGIN;") ||
		!strings.EqualFold(strings.TrimSpace(lines[len(lines)-1]), "COMMIT;") {
		return "", errors.New(
			"migration must be wrapped by standalone BEGIN; and COMMIT; lines",
		)
	}

	return strings.Join(lines[1:len(lines)-1], "\n"), nil
}
