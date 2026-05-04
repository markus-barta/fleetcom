package db

import (
	"errors"
	"path/filepath"
	"testing"
)

// FLEET-117 — atomic posture setter. Tests the canonical posture →
// flag-triple mapping plus the locked-without-pubkey gate.

func newTestStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	store, err := Open(filepath.Join(dir, "fleetcom-test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = store.DB.Close() })
	return store
}

func seedGateway(t *testing.T, store *Store, host string) {
	t.Helper()
	if _, err := store.UpsertGateway(host, "ws://"+host+":8090"); err != nil {
		t.Fatalf("UpsertGateway: %v", err)
	}
}

func gatewayFlags(t *testing.T, store *Store, host string) (autoApprove, oob, attest bool, pubkey string) {
	t.Helper()
	gws, err := store.AllGateways()
	if err != nil {
		t.Fatalf("AllGateways: %v", err)
	}
	for _, g := range gws {
		if g.Host == host {
			return g.AutoApproveBridges, g.OOBDeliveryEnabled, g.AttestationRequired, g.GatewayPubkeyB64
		}
	}
	t.Fatalf("gateway %q not found", host)
	return
}

func TestSetGatewayPosture_AutoPair(t *testing.T) {
	store := newTestStore(t)
	seedGateway(t, store, "dsc0")

	if err := store.SetGatewayPosture("dsc0", PostureAutoPair); err != nil {
		t.Fatalf("SetGatewayPosture: %v", err)
	}

	aa, oob, att, _ := gatewayFlags(t, store, "dsc0")
	if !aa || oob || att {
		t.Fatalf("auto-pair should be auto=ON, oob=OFF, attest=OFF; got auto=%v oob=%v attest=%v", aa, oob, att)
	}
}

func TestSetGatewayPosture_Reviewed(t *testing.T) {
	store := newTestStore(t)
	seedGateway(t, store, "dsc0")
	// Pre-seed flipped flags to confirm the posture setter overwrites
	// rather than just OR-ing in.
	_ = store.SetAutoApprove("dsc0", true)
	_ = store.SetOOBDelivery("dsc0", true)

	if err := store.SetGatewayPosture("dsc0", PostureReviewed); err != nil {
		t.Fatalf("SetGatewayPosture: %v", err)
	}

	aa, oob, att, _ := gatewayFlags(t, store, "dsc0")
	if aa || oob || !att {
		t.Fatalf("reviewed should be auto=OFF, oob=OFF, attest=ON; got auto=%v oob=%v attest=%v", aa, oob, att)
	}
}

func TestSetGatewayPosture_Hardened_LockedWithoutPubkey(t *testing.T) {
	store := newTestStore(t)
	seedGateway(t, store, "dsc0")
	// Confirm gateway has empty pubkey (Upsert default).
	beforeAA, beforeOOB, beforeAtt, pk := gatewayFlags(t, store, "dsc0")
	if pk != "" {
		t.Fatalf("expected empty pubkey on fresh gateway, got %q", pk)
	}

	err := store.SetGatewayPosture("dsc0", PostureHardened)
	if !errors.Is(err, ErrPostureLocked) {
		t.Fatalf("expected ErrPostureLocked, got %v", err)
	}

	// Critical: flags must NOT have been mutated by the failed attempt.
	// Without the pre-mutation gate, an operator selecting Hardened on a
	// gateway without a pubkey would end up with oob+attest ON but no
	// pubkey to verify against — i.e. every registration falls through
	// to attestation_skipped. The 422 is the right outcome.
	afterAA, afterOOB, afterAtt, _ := gatewayFlags(t, store, "dsc0")
	if afterAA != beforeAA || afterOOB != beforeOOB || afterAtt != beforeAtt {
		t.Fatalf("locked Hardened mutated flags: before(auto=%v oob=%v att=%v) → after(auto=%v oob=%v att=%v)",
			beforeAA, beforeOOB, beforeAtt, afterAA, afterOOB, afterAtt)
	}
}

func TestSetGatewayPosture_Hardened_WithPubkey(t *testing.T) {
	store := newTestStore(t)
	seedGateway(t, store, "dsc0")
	// 32-byte all-A's, b64url-no-padding, valid Ed25519 length.
	const pubkey = "QUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUE"
	if err := store.SetGatewayPubkey("dsc0", pubkey); err != nil {
		t.Fatalf("SetGatewayPubkey: %v", err)
	}

	if err := store.SetGatewayPosture("dsc0", PostureHardened); err != nil {
		t.Fatalf("SetGatewayPosture: %v", err)
	}

	aa, oob, att, gotPk := gatewayFlags(t, store, "dsc0")
	if aa || !oob || !att {
		t.Fatalf("hardened should be auto=OFF, oob=ON, attest=ON; got auto=%v oob=%v attest=%v", aa, oob, att)
	}
	if gotPk != pubkey {
		t.Fatalf("pubkey unexpectedly cleared: got %q want %q", gotPk, pubkey)
	}
}

func TestSetGatewayPosture_UnknownName(t *testing.T) {
	store := newTestStore(t)
	seedGateway(t, store, "dsc0")
	beforeAA, beforeOOB, beforeAtt, _ := gatewayFlags(t, store, "dsc0")

	for _, name := range []string{"", "Reviewed", "REVIEWED", "open", "casual", "strict"} {
		err := store.SetGatewayPosture("dsc0", name)
		if !errors.Is(err, ErrUnknownPosture) {
			t.Fatalf("posture %q: expected ErrUnknownPosture, got %v", name, err)
		}
	}

	// And no flags should have been mutated by any of the rejected names.
	afterAA, afterOOB, afterAtt, _ := gatewayFlags(t, store, "dsc0")
	if afterAA != beforeAA || afterOOB != beforeOOB || afterAtt != beforeAtt {
		t.Fatalf("rejected names mutated flags: before(auto=%v oob=%v att=%v) → after(auto=%v oob=%v att=%v)",
			beforeAA, beforeOOB, beforeAtt, afterAA, afterOOB, afterAtt)
	}
}

// QA-AUDIT — atomic rate-limit slot consumption. Concurrent /approve
// requests must not be able to all probe the cap; the DB-side bound on
// the UPDATE keeps brute-force at 5 even with N parallel callers.
func TestConsumeConfirmationAttempt_CapEnforced(t *testing.T) {
	store := newTestStore(t)
	if err := store.RegisterBridge("dsc0", "merlin", "abcd1234", "PEM"); err != nil {
		t.Fatalf("RegisterBridge: %v", err)
	}

	// 5 successive consumes succeed and report 1..5.
	for want := 1; want <= MaxConfirmationAttempts; want++ {
		got, ok, err := store.ConsumeConfirmationAttempt("dsc0", "merlin")
		if err != nil {
			t.Fatalf("attempt %d: err=%v", want, err)
		}
		if !ok {
			t.Fatalf("attempt %d: ok=false (cap reached too early)", want)
		}
		if got != want {
			t.Fatalf("attempt %d: counter=%d want %d", want, got, want)
		}
	}

	// 6th must report cap reached.
	n, ok, err := store.ConsumeConfirmationAttempt("dsc0", "merlin")
	if err != nil {
		t.Fatalf("6th attempt err=%v", err)
	}
	if ok {
		t.Fatalf("6th attempt: ok=true (cap not enforced); n=%d", n)
	}
	if n != 0 {
		t.Fatalf("6th attempt: expected n=0 on capped, got %d", n)
	}
}

func TestConsumeConfirmationAttempt_NoRow(t *testing.T) {
	store := newTestStore(t)
	// No RegisterBridge — row doesn't exist.
	_, ok, err := store.ConsumeConfirmationAttempt("ghost", "ocean")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if ok {
		t.Fatalf("ok=true on missing row (caller would treat as a probe)")
	}
}

func TestSetGatewayPosture_NoSuchHost(t *testing.T) {
	store := newTestStore(t)
	// No seedGateway — host has no row.

	for _, name := range []string{PostureAutoPair, PostureReviewed} {
		err := store.SetGatewayPosture("ghost-host", name)
		if err == nil {
			t.Fatalf("posture %q: expected not-found error, got nil", name)
		}
		// Specific check — must NOT be ErrUnknownPosture / ErrPostureLocked.
		if errors.Is(err, ErrUnknownPosture) || errors.Is(err, ErrPostureLocked) {
			t.Fatalf("posture %q: wrong error class: %v", name, err)
		}
	}
}

// FLEET-123 — TOFU auto-pin of the gateway pubkey from the connect
// handshake. The store-side contract is: pin only when the column is
// empty; never silently overwrite.
func TestSetGatewayPubkeyTOFU_PinsOnEmpty(t *testing.T) {
	store := newTestStore(t)
	seedGateway(t, store, "dsc0")
	const pk = "QUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUE"

	res, err := store.SetGatewayPubkeyTOFU("dsc0", pk)
	if err != nil {
		t.Fatalf("TOFU: %v", err)
	}
	if !res.Pinned || res.AlreadyMatches || res.Mismatch {
		t.Fatalf("first call should report Pinned only; got %+v", res)
	}
	_, _, _, gotPk := gatewayFlags(t, store, "dsc0")
	if gotPk != pk {
		t.Fatalf("pubkey not stored: got %q want %q", gotPk, pk)
	}
}

func TestSetGatewayPubkeyTOFU_NoOpOnSameValue(t *testing.T) {
	store := newTestStore(t)
	seedGateway(t, store, "dsc0")
	const pk = "QUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUE"
	if err := store.SetGatewayPubkey("dsc0", pk); err != nil {
		t.Fatalf("seed pubkey: %v", err)
	}

	res, err := store.SetGatewayPubkeyTOFU("dsc0", pk)
	if err != nil {
		t.Fatalf("TOFU: %v", err)
	}
	if res.Pinned || !res.AlreadyMatches || res.Mismatch {
		t.Fatalf("re-pin of same value should report AlreadyMatches only; got %+v", res)
	}
	if res.Existing != pk {
		t.Fatalf("Existing should echo stored value; got %q", res.Existing)
	}
}

func TestSetGatewayPubkeyTOFU_RefusesToOverwrite(t *testing.T) {
	store := newTestStore(t)
	seedGateway(t, store, "dsc0")
	const pkOld = "QUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUE"
	const pkNew = "QkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkI"
	if err := store.SetGatewayPubkey("dsc0", pkOld); err != nil {
		t.Fatalf("seed pubkey: %v", err)
	}

	res, err := store.SetGatewayPubkeyTOFU("dsc0", pkNew)
	if err != nil {
		t.Fatalf("TOFU: %v", err)
	}
	// Mismatch must NOT silently overwrite — the operator's manual
	// re-pin via PUT /api/gateways/{host}/pubkey is the only legitimate
	// path to swap a key on a paired gateway.
	if res.Pinned || res.AlreadyMatches || !res.Mismatch {
		t.Fatalf("conflicting value should report Mismatch only; got %+v", res)
	}
	_, _, _, stored := gatewayFlags(t, store, "dsc0")
	if stored != pkOld {
		t.Fatalf("TOFU mutated key on mismatch: got %q want %q", stored, pkOld)
	}
}

func TestSetGatewayPubkeyTOFU_NoSuchHost(t *testing.T) {
	store := newTestStore(t)
	// No seedGateway.
	_, err := store.SetGatewayPubkeyTOFU("ghost", "QUFB")
	if err == nil {
		t.Fatalf("expected not-found error, got nil")
	}
}

func TestSetGatewayPubkeyTOFU_RejectsEmpty(t *testing.T) {
	store := newTestStore(t)
	seedGateway(t, store, "dsc0")
	if _, err := store.SetGatewayPubkeyTOFU("dsc0", ""); err == nil {
		t.Fatalf("empty pubkey should be rejected")
	}
}
