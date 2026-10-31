package store

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func withMigrations(t *testing.T, m []string) {
	t.Helper()
	old := migrations
	migrations = m
	t.Cleanup(func() { migrations = old })
}

func TestMigrationsRunOnceInOrder(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "barpay.db")
	withMigrations(t, []string{
		"CREATE TABLE a (x INTEGER)",
		"INSERT INTO a VALUES (1)",
	})
	for range 2 {
		s, err := Open(ctx, path)
		if err != nil {
			t.Fatal(err)
		}
		_ = s.Close()
	}

	withMigrations(t, append(migrations, "INSERT INTO a VALUES (2)"))
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var n, sum, version int
	if err := s.Read().QueryRowContext(ctx, "SELECT count(*), sum(x) FROM a").Scan(&n, &sum); err != nil {
		t.Fatal(err)
	}
	if n != 2 || sum != 3 {
		t.Errorf("rows = %d, sum = %d; want each step applied once (2 rows, sum 3)", n, sum)
	}
	if err := s.Read().QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil || version != 3 {
		t.Errorf("user_version = %d, %v; want 3", version, err)
	}
}

func TestNewerSchemaIsRefused(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "barpay.db")
	withMigrations(t, []string{"CREATE TABLE a (x INTEGER)", "CREATE TABLE b (x INTEGER)"})
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Close()

	withMigrations(t, migrations[:1])
	if _, err := Open(ctx, path); err == nil || !strings.Contains(err.Error(), "newer") {
		t.Errorf("Open with an older BarPay: err = %v, want a newer-schema error", err)
	}
}

// Settings saved before the history existed become its first change.
func TestSettingsChangesStartFromSavedSettings(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "barpay.db")
	all := migrations
	i := slices.IndexFunc(all, func(m string) bool { return strings.Contains(m, "CREATE TABLE settings_changes") })
	withMigrations(t, all[:i])
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Read().ExecContext(ctx, "INSERT INTO settings VALUES (1, 'USD', 600, 2400, 10, 30, 3000, 50, 42)")
	_ = s.Close()
	if err != nil {
		t.Fatal(err)
	}

	withMigrations(t, all)
	s, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var n, floor, at int
	if err := s.Read().QueryRowContext(ctx, "SELECT count(*), floor, at FROM settings_changes").Scan(&n, &floor, &at); err != nil {
		t.Fatal(err)
	}
	if n != 1 || floor != 600 || at != 42 {
		t.Errorf("settings_changes = %d rows, floor %d, at %d; want the saved settings at 42", n, floor, at)
	}
}
