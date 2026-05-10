package db

import (
	"sync"
	"testing"
)

// FLEET-155 — N concurrent EnqueueCommand inserts must all succeed.
// Before the fix the DSN _busy_timeout only stuck on the first pool
// connection, so 7+ of 9 concurrent inserts failed with SQLITE_BUSY in
// 1-9ms (Update-all-agents fan-out). The DSN now uses
// ?_pragma=busy_timeout(5000), which modernc.org/sqlite applies to
// every new pooled connection.
func TestEnqueueCommand_ConcurrentInsertsAllSucceed(t *testing.T) {
	store := newTestStore(t)

	const n = 16
	var wg sync.WaitGroup
	errs := make([]error, n)
	wg.Add(n)
	for i := 0; i < n; i++ {
		i := i
		go func() {
			defer wg.Done()
			_, err := store.EnqueueCommand("dsc0", "agent.update", map[string]any{"i": i}, nil)
			errs[i] = err
		}()
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("EnqueueCommand[%d]: %v", i, err)
		}
	}

	var got int
	if err := store.DB.QueryRow(`SELECT COUNT(*) FROM host_commands WHERE host = 'dsc0'`).Scan(&got); err != nil {
		t.Fatalf("count: %v", err)
	}
	if got != n {
		t.Errorf("rows inserted = %d, want %d", got, n)
	}
}
