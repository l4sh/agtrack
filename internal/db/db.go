// Package db opens the SQLite database, sets it up the way the Python build
// did, and migrates the schema.
//
// One process holds one *sql.Conn for its whole life. database/sql is a pool
// and SQLite is not (go spec §7.1): two pooled connections in one process
// deadlock each other's write transactions. Transactions are explicit
// `BEGIN IMMEDIATE` statements on that connection, as in the Python build.
package db

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/l4sh/agtrack/internal/errs"
	"github.com/l4sh/agtrack/internal/refs"
	_ "modernc.org/sqlite"
)

// DefaultPath resolves $AGTRACK_DB, else $XDG_DATA_HOME/agtrack/agtrack.db,
// else ~/.local/share/agtrack/agtrack.db.
func DefaultPath() string {
	if env := os.Getenv("AGTRACK_DB"); env != "" {
		return expand(env)
	}
	base := os.Getenv("XDG_DATA_HOME")
	if base != "" {
		base = expand(base)
	} else {
		home, _ := os.UserHomeDir()
		base = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(base, "agtrack", "agtrack.db")
}

// ResolvePath is the path a connection would use, without opening anything.
func ResolvePath(path string) string {
	if path != "" {
		return expand(path)
	}
	return DefaultPath()
}

func expand(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, p[1:])
	}
	return p
}

// DB is an open database: the pool (capped at one connection) and the one
// connection every query runs on.
type DB struct {
	Path    string
	pool    *sql.DB
	Conn    *sql.Conn
	ctx     context.Context
	txDepth int
}

// DSN builds the modernc DSN with the per-connection pragmas (§7.1: pragmas
// are per connection, so they go in the DSN, not a one-off Exec).
func DSN(path string) string {
	q := url.Values{}
	q.Add("_pragma", "foreign_keys(1)")
	q.Add("_pragma", "busy_timeout(5000)")
	return "file:" + (&url.URL{Path: path}).EscapedPath() + "?" + q.Encode()
}

// Open opens (creating if needed) the database and brings the schema up to date.
func Open(path string) (*DB, error) {
	resolved := ResolvePath(path)
	if err := os.MkdirAll(filepath.Dir(resolved), 0o755); err != nil {
		return nil, err
	}
	pool, err := sql.Open("sqlite", DSN(resolved))
	if err != nil {
		return nil, err
	}
	pool.SetMaxOpenConns(1)
	pool.SetMaxIdleConns(1)
	pool.SetConnMaxLifetime(0)
	ctx := context.Background()
	conn, err := pool.Conn(ctx)
	if err != nil {
		pool.Close()
		return nil, err
	}
	d := &DB{Path: resolved, pool: pool, Conn: conn, ctx: ctx}
	// Some filesystems refuse WAL. Not fatal, as in the Python build.
	_, _ = conn.ExecContext(ctx, "PRAGMA journal_mode = WAL")
	var fk int
	if err := conn.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&fk); err != nil {
		d.Close()
		return nil, err
	}
	if fk != 1 {
		d.Close()
		return nil, fmt.Errorf("foreign_keys pragma did not take")
	}
	if err := d.migrate(); err != nil {
		d.Close()
		return nil, err
	}
	return d, nil
}

// Close releases the connection and the pool.
func (d *DB) Close() error {
	if d.Conn != nil {
		d.Conn.Close()
	}
	return d.pool.Close()
}

// Ctx is the context queries run under.
func (d *DB) Ctx() context.Context { return d.ctx }

// Exec runs a statement on the connection.
func (d *DB) Exec(query string, args ...any) (sql.Result, error) {
	return d.Conn.ExecContext(d.ctx, query, args...)
}

// Query runs a query on the connection.
func (d *DB) Query(query string, args ...any) (*sql.Rows, error) {
	return d.Conn.QueryContext(d.ctx, query, args...)
}

// QueryRow runs a single-row query on the connection.
func (d *DB) QueryRow(query string, args ...any) *sql.Row {
	return d.Conn.QueryRowContext(d.ctx, query, args...)
}

// Tx runs fn inside a BEGIN IMMEDIATE transaction, committing on nil and
// rolling back on error or panic. Nested calls join the outer transaction,
// so a store method can compose others atomically (D5, D9).
func (d *DB) Tx(fn func() error) (err error) {
	if d.txDepth > 0 {
		d.txDepth++
		defer func() { d.txDepth-- }()
		return fn()
	}
	if _, err := d.Exec("BEGIN IMMEDIATE"); err != nil {
		return err
	}
	d.txDepth = 1
	defer func() {
		d.txDepth = 0
		if p := recover(); p != nil {
			_, _ = d.Exec("ROLLBACK")
			panic(p)
		}
		if err != nil {
			_, _ = d.Exec("ROLLBACK")
			return
		}
		if _, cerr := d.Exec("COMMIT"); cerr != nil {
			_, _ = d.Exec("ROLLBACK")
			err = cerr
		}
	}()
	return fn()
}

// ReadTx runs fn in a deferred (read) transaction: one consistent WAL
// snapshot that neither blocks nor is blocked by writers. Inside an open
// transaction it just runs fn.
func (d *DB) ReadTx(fn func() error) (err error) {
	if d.txDepth > 0 {
		return fn()
	}
	if _, err := d.Exec("BEGIN"); err != nil {
		return err
	}
	d.txDepth = 1
	defer func() {
		d.txDepth = 0
		if p := recover(); p != nil {
			_, _ = d.Exec("ROLLBACK")
			panic(p)
		}
		if _, cerr := d.Exec("COMMIT"); cerr != nil && err == nil {
			err = cerr
		}
	}()
	return fn()
}

// InTx reports whether a transaction is open.
func (d *DB) InTx() bool { return d.txDepth > 0 }

// UserVersion reads PRAGMA user_version.
func (d *DB) UserVersion() (int, error) {
	var v int
	err := d.QueryRow("PRAGMA user_version").Scan(&v)
	return v, err
}

func (d *DB) migrate() error {
	version, err := d.UserVersion()
	if err != nil {
		return err
	}
	if version == SchemaVersion {
		return nil
	}
	if version > SchemaVersion {
		return errs.New("database schema version %d is newer than this agtrack (%d); upgrade agtrack", version, SchemaVersion)
	}
	if version > 0 {
		// The upgrade is one-way (agtrack 1.x refuses a v3 file), so keep a
		// consistent copy of the old file first. VACUUM INTO cannot run
		// inside the migration transaction; it snapshots a committed state.
		backup := fmt.Sprintf("%s.v%d.bak", d.Path, version)
		if _, err := os.Stat(backup); os.IsNotExist(err) {
			if _, err := d.Exec("VACUUM INTO ?", backup); err != nil {
				return fmt.Errorf("could not back up the v%d database to %s before migrating: %w", version, backup, err)
			}
		}
	}
	return d.Tx(func() error {
		// D14: re-read under the write lock. A concurrent first open may have
		// migrated between the read above and BEGIN IMMEDIATE.
		version, err := d.UserVersion()
		if err != nil {
			return err
		}
		if version >= SchemaVersion {
			return nil
		}
		var stmts []string
		if version == 0 {
			stmts = append(stmts, schemaV2...)
		} else if version < 2 {
			stmts = append(stmts, migration2...)
		}
		stmts = append(stmts, migration3...)
		for _, s := range stmts {
			if _, err := d.Exec(s); err != nil {
				return fmt.Errorf("migration failed on %q: %w", firstLine(s), err)
			}
		}
		if version < 3 {
			if err := d.qualifyRepoSlugs(); err != nil {
				return err
			}
		}
		_, err = d.Exec(fmt.Sprintf("PRAGMA user_version = %d", SchemaVersion))
		return err
	})
}

// qualifyRepoSlugs is the D8 data step of v2 -> v3: a bare `owner/name`
// binding becomes `host/owner/name` only where its stored URL establishes the
// host. Bindings without a URL stay bare and keep matching any host.
func (d *DB) qualifyRepoSlugs() error {
	rs, err := d.Query("SELECT id, project_id, slug, url FROM project_repos WHERE url IS NOT NULL")
	if err != nil {
		return err
	}
	type fix struct {
		id, project int64
		slug        string
	}
	var fixes []fix
	for rs.Next() {
		var id, project int64
		var slug, u string
		if err := rs.Scan(&id, &project, &slug, &u); err != nil {
			rs.Close()
			return err
		}
		identity, err := refs.RepoIdentity(u)
		if err != nil || identity == slug || refs.RepoHost(u) == "" {
			continue
		}
		if bare, _ := refs.NormalizeRepo(u); bare != slug {
			continue // the URL does not describe this binding
		}
		fixes = append(fixes, fix{id, project, identity})
	}
	rs.Close()
	for _, f := range fixes {
		if _, err := d.Exec("UPDATE OR IGNORE project_repos SET slug = ? WHERE id = ?", f.slug, f.id); err != nil {
			return err
		}
	}
	return nil
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// SQLiteVersion reports the bundled SQLite version.
func (d *DB) SQLiteVersion() string {
	var v string
	_ = d.QueryRow("SELECT sqlite_version()").Scan(&v)
	return v
}
