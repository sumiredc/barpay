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

const insertUnconfirmed = `INSERT INTO alerts (id, kind, raised_at, session_id, member_id, message)
	VALUES ('alt_2', 'charge_unconfirmed', 43, 'ses_1', '', 'm')`

// Alerts raised before charge_unconfirmed was added keep their order and content when the table
// is made again, and the new kind can then be stored.
func TestAlertsKeepTheirRowsWhenChargeUnconfirmedIsAdded(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "barpay.db")
	all := migrations
	i := slices.IndexFunc(all, func(m string) bool { return strings.Contains(m, "CREATE TABLE alerts_new") })
	withMigrations(t, all[:i])
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Read().ExecContext(ctx, `INSERT INTO alerts (seq, id, kind, raised_at, session_id, member_id, message)
		VALUES (7, 'alt_1', 'payment_failed', 42, 'ses_1', '', 'm')`)
	if _, before := s.Read().ExecContext(ctx, insertUnconfirmed); before == nil {
		t.Error("charge_unconfirmed was stored before the step that adds it")
	}
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
	var seq, at int
	var id, kind string
	if err := s.Read().QueryRowContext(ctx, "SELECT seq, id, kind, raised_at FROM alerts").Scan(&seq, &id, &kind, &at); err != nil {
		t.Fatal(err)
	}
	if seq != 7 || id != "alt_1" || kind != "payment_failed" || at != 42 {
		t.Errorf("alert %d %s %s %d; want the one stored before", seq, id, kind, at)
	}
	if _, err := s.Read().ExecContext(ctx, insertUnconfirmed); err != nil {
		t.Errorf("charge_unconfirmed: %v", err)
	}
}

// A database stopped one step before staff events were removed loses the table, its rows and its
// index, and moves one version on.
func TestStaffEventsTableIsDropped(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "barpay.db")
	all := migrations
	i := slices.Index(all, "DROP TABLE staff_events")
	if i < 0 {
		t.Fatal("no step drops staff_events")
	}
	withMigrations(t, all[:i])
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Read().ExecContext(ctx, `INSERT INTO staff_events (label, phase, recorded_at) VALUES ('Live show', 'starts', 42)`)
	_ = s.Close()
	if err != nil {
		t.Fatal(err)
	}

	withMigrations(t, all[:i+1])
	s, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var n, version int
	if err := s.Read().QueryRowContext(ctx,
		`SELECT count(*) FROM sqlite_schema WHERE name IN ('staff_events', 'staff_events_label')`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("%d of staff_events and staff_events_label are left", n)
	}
	if err := s.Read().QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil || version != i+1 {
		t.Errorf("user_version = %d, %v; want %d", version, err, i+1)
	}
}
