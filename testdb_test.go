package stats

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

// openTestDB is a database of this test's own, opened as the README
// tells a host to (WAL and a busy timeout: Run's keeper writes beside
// the collector). The package itself never registers a driver — the
// host opens the database and hands it over — so the driver is a
// dependency of these tests alone.
func openTestDB(t *testing.T) *DB {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "t.db")+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	d, err := Open(db)
	if err != nil {
		t.Fatal(err)
	}
	return d
}
