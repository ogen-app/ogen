// Package pgtest provisions throwaway Postgres databases for tests.
//
// Each MustDB call creates a fresh, fully-migrated database (bun schema +
// seed reference data + River service tables) on the Postgres pointed at by
// TEST_DATABASE_DSN (default localhost:5432, database "postgres"). Databases
// are isolated per call, mirroring the per-Describe in-memory SQLite DBs the
// suites used before CON-87. The Makefile (and CI) provision the Postgres
// instance; tests just call MustDB.
//
// The per-call databases are not dropped individually (the standard `make
// test` / CI flow uses a throwaway container that is torn down at the end of
// the run). To keep a *persistent* TEST_DATABASE_DSN from accumulating them
// across runs, the first MustDB call in each process best-effort drops
// orphaned ogen_test_* databases left by previous runs (see sweepStale).
//
// Only test files import this package, so the production binary never links it.
package pgtest

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"

	// Registers the "pgx" database/sql driver for the admin connection.
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/infra/database"
	"github.com/ogen-app/ogen/src/jobs"
)

var (
	seq       atomic.Int64
	sweepOnce sync.Once
)

// sweepStale best-effort drops leftover ogen_test_* databases from previous
// runs so a persistent TEST_DATABASE_DSN doesn't accumulate them. Runs once
// per process, before this process creates its first database. Only databases
// whose creating process has exited are dropped: a concurrent process's
// database can be momentarily idle between its CREATE and its first connect,
// and DROP DATABASE's in-use check alone would delete it from under it.
func sweepStale(admin *sql.DB) {
	// Escape the underscores so LIKE matches them literally, not as wildcards.
	rows, err := admin.Query(`SELECT datname FROM pg_database WHERE datname LIKE 'ogen\_test\_%'`)
	if err != nil {
		return
	}
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err == nil {
			names = append(names, n)
		}
	}
	_ = rows.Close()
	for _, n := range names {
		if creatorAlive(n) {
			continue
		}
		_, _ = admin.Exec("DROP DATABASE IF EXISTS " + n) // in-use DBs error out and are skipped
	}
}

// creatorAlive reports whether the process that created the ogen_test_<pid>_<seq>
// database name is still running on this host. A name it can't parse counts
// as not alive, so the sweep falls back to DROP DATABASE's in-use check.
func creatorAlive(name string) bool {
	rest, ok := strings.CutPrefix(name, "ogen_test_")
	if !ok {
		return false
	}
	pidStr, _, ok := strings.Cut(rest, "_")
	if !ok {
		return false
	}
	pid, err := strconv.Atoi(pidStr)
	if err != nil || pid <= 0 {
		return false
	}
	if pid == os.Getpid() {
		return true
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	// Signal 0 probes for existence without delivering anything. EPERM means
	// the process exists under another user.
	err = proc.Signal(syscall.Signal(0))
	return err == nil || errors.Is(err, syscall.EPERM)
}

// BaseDSN is the Postgres the tests provision databases on. The database in
// the DSN is used only as the maintenance connection for CREATE DATABASE.
func BaseDSN() string {
	if v := os.Getenv("TEST_DATABASE_DSN"); v != "" {
		return v
	}
	return "postgres://ogen:ogen@localhost:5432/postgres?sslmode=disable"
}

// withDB returns dsn with its database name replaced.
func withDB(dsn, name string) string {
	u, err := url.Parse(dsn)
	if err != nil {
		return dsn
	}
	u.Path = "/" + name
	return u.String()
}

// MustDB creates and migrates a fresh, isolated Postgres database and returns
// a bun.DB connected to it. It panics on failure — a test cannot proceed
// without a database.
func MustDB() *bun.DB {
	ctx := context.Background()
	name := fmt.Sprintf("ogen_test_%d_%d", os.Getpid(), seq.Add(1))

	admin, err := sql.Open("pgx", BaseDSN())
	if err != nil {
		panic(fmt.Sprintf("pgtest: open admin connection: %v", err))
	}
	// Once per process, clear orphaned databases from previous runs (no-op on
	// a fresh throwaway container). Runs before this process's first CREATE.
	sweepOnce.Do(func() { sweepStale(admin) })
	// CREATE DATABASE can't run in a transaction and can't bind identifiers;
	// the name is constructed from pid+seq so it's always a safe identifier.
	_, err = admin.ExecContext(ctx, "CREATE DATABASE "+name)
	_ = admin.Close()
	if err != nil {
		panic(fmt.Sprintf("pgtest: create database %s: %v", name, err))
	}

	db, err := database.New(withDB(BaseDSN(), name), false)
	if err != nil {
		panic(fmt.Sprintf("pgtest: connect to %s: %v", name, err))
	}
	// Small per-DB pool: tests create many of these and they're never
	// closed mid-run, so keep the aggregate connection count modest.
	db.SetMaxOpenConns(5)
	db.SetMaxIdleConns(2)
	if err := database.Migrate(ctx, db); err != nil {
		panic(fmt.Sprintf("pgtest: migrate %s: %v", name, err))
	}
	if err := jobs.MigrateRiver(ctx, db.DB); err != nil {
		panic(fmt.Sprintf("pgtest: river migrate %s: %v", name, err))
	}
	return db
}
