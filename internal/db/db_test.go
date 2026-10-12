package db

import (
	"database/sql"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestFreshOpenMigratesToCurrent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.db")
	d, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	v, _ := d.UserVersion()
	if v != SchemaVersion {
		t.Fatalf("user_version %d", v)
	}
	var fk, bt int
	var jm string
	d.QueryRow("PRAGMA foreign_keys").Scan(&fk)
	d.QueryRow("PRAGMA busy_timeout").Scan(&bt)
	d.QueryRow("PRAGMA journal_mode").Scan(&jm)
	if fk != 1 || bt != 5000 || jm != "wal" {
		t.Fatalf("pragmas fk=%d busy=%d jm=%s", fk, bt, jm)
	}
	var n int
	if err := d.QueryRow("SELECT COUNT(*) FROM pragma_compile_options WHERE compile_options = 'ENABLE_FTS5'").Scan(&n); err != nil || n != 1 {
		t.Fatalf("fts5 missing: %v", err)
	}
}

func TestConcurrentFirstOpenMigratesOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.db")
	var wg sync.WaitGroup
	errc := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d, err := Open(path)
			if err != nil {
				errc <- err
				return
			}
			d.Close()
		}()
	}
	wg.Wait()
	close(errc)
	for err := range errc {
		t.Fatal(err)
	}
}

func TestTxRollsBack(t *testing.T) {
	d, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	_ = d.Tx(func() error {
		d.Exec("INSERT INTO meta (key, value) VALUES ('x', '1')")
		return os.ErrInvalid
	})
	var n int
	d.QueryRow("SELECT COUNT(*) FROM meta WHERE key='x'").Scan(&n)
	if n != 0 {
		t.Fatal("not rolled back")
	}
}

func TestSavepointUndoesOnlyItself(t *testing.T) {
	d, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	err = d.Tx(func() error {
		d.Exec("INSERT INTO meta (key, value) VALUES ('a', '1')")
		if d.Savepoint(func() error {
			d.Exec("INSERT INTO meta (key, value) VALUES ('b', '1')")
			return os.ErrInvalid
		}) == nil {
			t.Error("savepoint swallowed the error")
		}
		if !d.InTx() {
			t.Error("outer transaction lost")
		}
		return d.Savepoint(func() error {
			d.Exec("INSERT INTO meta (key, value) VALUES ('c', '1')")
			return nil
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	var n int
	d.QueryRow("SELECT COUNT(*) FROM meta WHERE key IN ('a','c')").Scan(&n)
	var b int
	d.QueryRow("SELECT COUNT(*) FROM meta WHERE key='b'").Scan(&b)
	if n != 2 || b != 0 {
		t.Fatalf("a,c=%d b=%d", n, b)
	}
}

// AGTRACK_TEST_V2_DB points at a *copy* of a real v2 database.
func TestOpensRealV2Copy(t *testing.T) {
	src := os.Getenv("AGTRACK_TEST_V2_DB")
	if src == "" {
		t.Skip("AGTRACK_TEST_V2_DB not set")
	}
	d, err := Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if _, err := os.Stat(src + ".v2.bak"); err != nil {
		t.Fatalf("no pre-migration backup: %v", err)
	}
	var ok string
	d.QueryRow("PRAGMA integrity_check").Scan(&ok)
	if ok != "ok" {
		t.Fatal(ok)
	}
	var missing int
	d.QueryRow("SELECT COUNT(*) FROM events WHERE uid IS NULL").Scan(&missing)
	if missing != 0 {
		t.Fatal("uid backfill incomplete")
	}
}

// v3 -> v4 (agent-features spec "Schema v4"): the v3 file is backed up first,
// rows survive, and the new columns and tables exist.
func TestMigratesV3ToV4WithBackup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v3.db")
	raw, err := sql.Open("sqlite", DSN(path))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range append(append([]string{}, schemaV2...), migration3...) {
		if _, err := raw.Exec(s); err != nil {
			t.Fatalf("%s: %v", firstLine(s), err)
		}
	}
	if _, err := raw.Exec(`INSERT INTO contexts (name, created_at, updated_at) VALUES ('w', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec("PRAGMA user_version = 3"); err != nil {
		t.Fatal(err)
	}
	raw.Close()

	d, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if v, _ := d.UserVersion(); v != SchemaVersion || SchemaVersion != 4 {
		t.Fatalf("version %d, SchemaVersion %d", v, SchemaVersion)
	}
	if _, err := os.Stat(path + ".v3.bak"); err != nil {
		t.Errorf("no v3 backup: %v", err)
	}
	var n int
	if err := d.QueryRow("SELECT COUNT(*) FROM contexts").Scan(&n); err != nil || n != 1 {
		t.Errorf("contexts after migration: %d (%v)", n, err)
	}
	for _, q := range []string{
		"SELECT session FROM events",
		"SELECT claim_session, claim_last_seen, snoozed_until, snooze_note FROM tasks",
		"SELECT acknowledged_at, acknowledged_by FROM notes",
		"SELECT * FROM task_evidence", "SELECT * FROM checkpoints", "SELECT * FROM actors",
		"SELECT * FROM actor_aliases", "SELECT * FROM rules", "SELECT * FROM rule_events",
		"SELECT * FROM lessons", "SELECT * FROM lesson_triggers", "SELECT * FROM lessons_fts",
	} {
		if _, err := d.Exec(q); err != nil {
			t.Errorf("%s: %v", q, err)
		}
	}
}
