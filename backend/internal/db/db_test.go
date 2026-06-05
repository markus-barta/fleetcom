package db

import "testing"

// FLEET-188/189 regression guard. The previous DSN set journal_mode via the
// mattn-style `_journal_mode=WAL` key, which modernc.org/sqlite silently ignores
// — so WAL was never actually enabled and the DB ran in rollback-journal mode,
// causing recurring multi-second global lock stalls. These checks fail loudly if
// anyone reverts Open() to a DSN form the driver doesn't honor.
func TestOpen_PragmasApplied(t *testing.T) {
	store := newTestStore(t)

	var journalMode string
	if err := store.DB.QueryRow(`PRAGMA journal_mode`).Scan(&journalMode); err != nil {
		t.Fatalf("query journal_mode: %v", err)
	}
	if journalMode != "wal" {
		t.Errorf("journal_mode = %q, want \"wal\" (WAL not active — DSN form likely not honored by the driver)", journalMode)
	}

	var busyTimeout int
	if err := store.DB.QueryRow(`PRAGMA busy_timeout`).Scan(&busyTimeout); err != nil {
		t.Fatalf("query busy_timeout: %v", err)
	}
	if busyTimeout != 5000 {
		t.Errorf("busy_timeout = %d, want 5000", busyTimeout)
	}

	// synchronous: 0=OFF 1=NORMAL 2=FULL 3=EXTRA. NORMAL is safe under WAL.
	var synchronous int
	if err := store.DB.QueryRow(`PRAGMA synchronous`).Scan(&synchronous); err != nil {
		t.Fatalf("query synchronous: %v", err)
	}
	if synchronous != 1 {
		t.Errorf("synchronous = %d, want 1 (NORMAL)", synchronous)
	}
}

// FLEET-189: the crash-loop COUNT (host_id, container_name, event_type='restart',
// ts>=cutoff) runs once per container inside AllHosts() — on every heartbeat and
// every container-event POST. Guard that the event_type-aware index exists and
// the redundant predecessor is gone.
func TestOpen_ContainerEventsIndex(t *testing.T) {
	store := newTestStore(t)

	indexes := map[string]bool{}
	rows, err := store.DB.Query(`SELECT name FROM sqlite_master WHERE type='index' AND tbl_name='container_events'`)
	if err != nil {
		t.Fatalf("query indexes: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan: %v", err)
		}
		indexes[name] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}

	if !indexes["idx_container_events_crashloop"] {
		t.Errorf("missing idx_container_events_crashloop; have %v", indexes)
	}
	if indexes["idx_container_events_host_name"] {
		t.Errorf("redundant idx_container_events_host_name should have been dropped; have %v", indexes)
	}
}
