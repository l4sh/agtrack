package db

import (
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
