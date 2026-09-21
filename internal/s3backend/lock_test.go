package s3backend

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/infrena/infrena/pkg/backend"
)

// THE POINT OF THIS TASK. Two different stores fail two different ways, and
// the probe has to catch both.
//
// Backblaze B2 refuses the FIRST conditional write with NotImplemented -
// measured against the real service, not assumed. That is the loud, easy
// case.
//
// The dangerous case is a store that IGNORES the header and overwrites,
// reporting success, because that is a lock that never locks with no error
// anywhere. No store is currently known to do it, but it costs one extra
// round trip to rule out and it is the one that fails silently.
//
// If the second write succeeds, two applies can both take the same lock, and
// invariant 5 is gone with no error anywhere.
// B2's ACTUAL behaviour, measured against the real service: NotImplemented on
// the first write. The loud case.
func TestProveConditionalWritesRejectsAStoreThatRefusesTheHeader(t *testing.T) {
	store := &fakeStore{conditionalWritesNotImplemented: true}
	b := newTestBackend(t, store)

	err := b.proveConditionalWrites(context.Background())
	if err == nil {
		t.Fatal("a store that cannot do conditional writes was accepted")
	}
	// The store's own message is more use than anything invented here.
	if !strings.Contains(err.Error(), "NotImplemented") {
		t.Errorf("refusal does not quote the store: %v", err)
	}
}

// The SILENT case: no store is known to do this, but it is the one that
// produces a lock which never locks with no error anywhere.
func TestProveConditionalWritesRejectsAStoreThatIgnoresTheHeader(t *testing.T) {
	store := &fakeStore{ignoresIfNoneMatch: true}
	b := newTestBackend(t, store)

	err := b.proveConditionalWrites(context.Background())
	if err == nil {
		t.Fatal("a store that silently overwrites was accepted")
	}
	for _, want := range []string{"conditional", "lock"} {
		if !strings.Contains(strings.ToLower(err.Error()), want) {
			t.Errorf("refusal does not explain what is missing: %v", err)
		}
	}
}

func TestProveConditionalWritesAcceptsAStoreThatHonoursIt(t *testing.T) {
	b := newTestBackend(t, &fakeStore{})
	if err := b.proveConditionalWrites(context.Background()); err != nil {
		t.Fatalf("a conforming store was refused: %v", err)
	}
}

// The probe must not leave rubbish in the user's bucket.
func TestTheProbeCleansUpAfterItself(t *testing.T) {
	store := &fakeStore{}
	b := newTestBackend(t, store)

	if err := b.proveConditionalWrites(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := store.objectCount(); n != 0 {
		t.Errorf("probe left %d objects behind", n)
	}
}

// A store that ignores the header leaves the probe object behind as well, and
// the refusal is no excuse for littering.
func TestTheProbeCleansUpAfterARefusal(t *testing.T) {
	store := &fakeStore{ignoresIfNoneMatch: true}
	b := newTestBackend(t, store)

	if err := b.proveConditionalWrites(context.Background()); err == nil {
		t.Fatal("a store that silently overwrites was accepted")
	}
	if n := store.objectCount(); n != 0 {
		t.Errorf("probe left %d objects behind", n)
	}
}

// The proof belongs at the END of Configure, not at the first Lock: a backend
// that cannot lock must be refused when it LOADS (spec section 5), not when an
// apply is already under way. A failed proof leaves the backend unusable
// rather than usable-but-unlockable.
func TestConfigureRefusesAStoreThatCannotLock(t *testing.T) {
	b := New()
	b.newStore = func(Config) (objectStore, error) {
		return &fakeStore{conditionalWritesNotImplemented: true}, nil
	}

	err := b.Configure(t.Context(), map[string]any{"bucket": "b", "endpoint": "http://127.0.0.1:9000"})
	if err == nil {
		t.Fatal("a store that cannot lock was configured")
	}
	if !strings.Contains(err.Error(), "NotImplemented") {
		t.Errorf("refusal does not quote the store: %v", err)
	}
	if _, err := b.Lock(t.Context(), "dev"); err == nil {
		t.Error("the backend served a lock after a failed proof")
	}
}

// A second lock fails, and names who holds the first — the whole reason a
// lock records a holder.
func TestASecondLockFailsAndNamesTheHolder(t *testing.T) {
	b := newTestBackend(t, &fakeStore{})
	ctx := context.Background()

	first, err := b.Lock(ctx, "dev")
	if err != nil {
		t.Fatal(err)
	}
	_, err = b.Lock(ctx, "dev")
	if err == nil {
		t.Fatal("a second lock was granted")
	}
	if !strings.Contains(err.Error(), first.User) || !strings.Contains(err.Error(), first.Host) {
		t.Errorf("conflict does not name the holder: %v", err)
	}
}

// A PreconditionFailed from the store is a lock conflict and must be
// reported as one, not as a transport error — the host maps it back to
// ErrLocked and every caller tests for that.
func TestAPreconditionFailureIsALockConflict(t *testing.T) {
	b := newTestBackend(t, &fakeStore{})
	ctx := context.Background()
	if _, err := b.Lock(ctx, "dev"); err != nil {
		t.Fatal(err)
	}

	_, err := b.Lock(ctx, "dev")
	if !errors.Is(err, backend.ErrLocked) {
		t.Fatalf("second Lock returned %v, want ErrLocked", err)
	}
}

// Unlock removes the lock; Inspect then reports nothing held.
func TestUnlockReleasesAndInspectAgrees(t *testing.T) {
	b := newTestBackend(t, &fakeStore{})
	ctx := context.Background()
	if _, err := b.Lock(ctx, "dev"); err != nil {
		t.Fatal(err)
	}
	if err := b.Unlock(ctx, "dev"); err != nil {
		t.Fatal(err)
	}
	if _, held, _ := b.Inspect(ctx, "dev"); held {
		t.Error("a lock survived Unlock")
	}
}

// Inspect reports the holder, because that is what the stale-lock diagnostic
// prints. The bool tells "nothing is held" apart from "the lock is
// unreadable", and a caller that conflated them would let a second apply run.
func TestInspectReportsTheHolder(t *testing.T) {
	b := newTestBackend(t, &fakeStore{})
	ctx := context.Background()
	taken, err := b.Lock(ctx, "dev")
	if err != nil {
		t.Fatal(err)
	}

	got, held, err := b.Inspect(ctx, "dev")
	if err != nil {
		t.Fatal(err)
	}
	if !held {
		t.Fatal("Inspect reports nothing held while a lock is held")
	}
	if got.User != taken.User || got.Host != taken.Host || got.PID != taken.PID {
		t.Errorf("Inspect = %+v, want %+v", got, taken)
	}
	if got.Environment != "dev" {
		t.Errorf("lock does not record its environment: %+v", got)
	}
}

// Unlock must refuse a lock somebody else holds, or ForceUnlock means nothing
// and a stray Unlock frees an environment another apply is mutating.
func TestUnlockRefusesALockThisRunDoesNotHold(t *testing.T) {
	store := &fakeStore{}
	held := newTestBackend(t, store)
	if _, err := held.Lock(t.Context(), "dev"); err != nil {
		t.Fatal(err)
	}

	other := newTestBackend(t, store)
	if err := other.Unlock(t.Context(), "dev"); err == nil {
		t.Fatal("a run released a lock it never took")
	}
	if _, stillHeld, _ := held.Inspect(t.Context(), "dev"); !stillHeld {
		t.Error("the lock was removed anyway")
	}
}

// ForceUnlock is `infrena state unlock` after it has named the holder, so it
// removes somebody else's lock. A lock that was not there is an error rather
// than a silent success: a typo in an environment name must not look like it
// worked.
func TestForceUnlockRemovesSomebodyElsesLockAndRefusesAnAbsentOne(t *testing.T) {
	store := &fakeStore{}
	held := newTestBackend(t, store)
	if _, err := held.Lock(t.Context(), "dev"); err != nil {
		t.Fatal(err)
	}

	other := newTestBackend(t, store)
	if err := other.ForceUnlock(t.Context(), "dev"); err != nil {
		t.Fatalf("ForceUnlock refused a lock it was meant to break: %v", err)
	}
	if _, stillHeld, _ := other.Inspect(t.Context(), "dev"); stillHeld {
		t.Error("the lock survived ForceUnlock")
	}

	err := other.ForceUnlock(t.Context(), "typoo")
	if err == nil {
		t.Fatal("unlocking an environment with no lock reported success")
	}
	if !strings.Contains(err.Error(), "typoo") {
		t.Errorf("the refusal does not name the environment: %v", err)
	}
}

// Every method refuses before Configure, and says which call is missing,
// rather than panicking on a nil client inside a plugin — which reaches the
// user as a protocol error about a closed pipe.
func TestLockingRefusesBeforeConfigure(t *testing.T) {
	b := New()
	ctx := context.Background()
	if _, err := b.Lock(ctx, "dev"); !errors.Is(err, errNotConfigured) {
		t.Errorf("Lock = %v", err)
	}
	if err := b.Unlock(ctx, "dev"); !errors.Is(err, errNotConfigured) {
		t.Errorf("Unlock = %v", err)
	}
	if _, _, err := b.Inspect(ctx, "dev"); !errors.Is(err, errNotConfigured) {
		t.Errorf("Inspect = %v", err)
	}
	if err := b.ForceUnlock(ctx, "dev"); !errors.Is(err, errNotConfigured) {
		t.Errorf("ForceUnlock = %v", err)
	}
}
