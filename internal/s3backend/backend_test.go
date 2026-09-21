package s3backend

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/infrena/infrena/pkg/backend"
)

// State is bytes. The backend never parses it, so a round trip must be
// byte-identical rather than merely equivalent.
func TestStateRoundTripsByteForByte(t *testing.T) {
	b := newTestBackend(t, &fakeStore{})
	ctx := context.Background()
	if _, err := b.Lock(ctx, "dev"); err != nil {
		t.Fatal(err)
	}
	raw := []byte(`{"version":1,"serial":7,"resources":{"a":{"x":1}}}`)

	if err := b.Put(ctx, "dev", raw); err != nil {
		t.Fatal(err)
	}
	got, err := b.Get(ctx, "dev")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, raw) {
		t.Errorf("Get = %s, want %s", got, raw)
	}
}

// Bytes means bytes: whitespace, key order and anything that is not JSON at
// all come back exactly as they went in. A backend that re-encoded state would
// be a second reader of it, free to disagree with the first.
func TestAnythingAtAllRoundTripsUnchanged(t *testing.T) {
	b := newTestBackend(t, &fakeStore{})
	ctx := context.Background()
	if _, err := b.Lock(ctx, "dev"); err != nil {
		t.Fatal(err)
	}
	for _, raw := range [][]byte{
		[]byte("{\n  \"b\": 2,\n  \"a\": 1\n}\n"),
		{0x00, 0x01, 0xff, 0xfe},
		{},
	} {
		if err := b.Put(ctx, "dev", raw); err != nil {
			t.Fatal(err)
		}
		got, err := b.Get(ctx, "dev")
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, raw) {
			t.Errorf("Get = %q, want %q", got, raw)
		}
	}
}

// An environment never written is empty, not an error — a project that has
// never applied is the ordinary case.
func TestGettingAnEnvironmentThatWasNeverWrittenIsEmpty(t *testing.T) {
	b := newTestBackend(t, &fakeStore{})
	got, err := b.Get(context.Background(), "never")
	if err != nil {
		t.Fatalf("Get on a missing environment errored: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("Get = %s, want empty", got)
	}
}

// Put without the lock is refused BY THE BACKEND, not by caller discipline.
// Invariant 5 is a property of what the backend allows.
func TestPutWithoutTheLockIsRefused(t *testing.T) {
	b := newTestBackend(t, &fakeStore{})
	err := b.Put(context.Background(), "dev", []byte(`{}`))
	if !errors.Is(err, backend.ErrNotLocked) {
		t.Fatalf("Put without a lock returned %v, want ErrNotLocked", err)
	}
}

// And having held the lock once is not holding it now. The check is against
// the store at the moment of the write, not against this process's memory of
// having locked earlier.
func TestPutAfterUnlockIsRefused(t *testing.T) {
	b := newTestBackend(t, &fakeStore{})
	ctx := context.Background()
	if _, err := b.Lock(ctx, "dev"); err != nil {
		t.Fatal(err)
	}
	if err := b.Unlock(ctx, "dev"); err != nil {
		t.Fatal(err)
	}
	if err := b.Put(ctx, "dev", []byte(`{}`)); !errors.Is(err, backend.ErrNotLocked) {
		t.Fatalf("Put after Unlock returned %v, want ErrNotLocked", err)
	}
}

// The case force-unlocking creates: this run took the lock, somebody broke it
// and took their own, and this run's write must not land on top of theirs.
func TestPutIsRefusedOnceTheLockHasBeenTakenBySomebodyElse(t *testing.T) {
	store := &fakeStore{}
	first := newTestBackend(t, store)
	ctx := context.Background()
	if _, err := first.Lock(ctx, "dev"); err != nil {
		t.Fatal(err)
	}

	second := newTestBackend(t, store)
	if err := second.ForceUnlock(ctx, "dev"); err != nil {
		t.Fatal(err)
	}
	if _, err := second.Lock(ctx, "dev"); err != nil {
		t.Fatal(err)
	}

	err := first.Put(ctx, "dev", []byte(`{"clobbered":true}`))
	if !errors.Is(err, backend.ErrNotLocked) {
		t.Fatalf("Put with a broken lock returned %v, want ErrNotLocked", err)
	}
}

// List reports environments, not objects: it must not report the lock files
// sitting beside them, and must be sorted.
func TestListReportsEnvironmentsAndNotLockObjects(t *testing.T) {
	b := newTestBackend(t, &fakeStore{})
	ctx := context.Background()
	for _, env := range []string{"production", "dev"} {
		if _, err := b.Lock(ctx, env); err != nil {
			t.Fatal(err)
		}
		if err := b.Put(ctx, env, []byte(`{}`)); err != nil {
			t.Fatal(err)
		}
	}

	got, err := b.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, []string{"dev", "production"}) {
		t.Errorf("List = %v, want [dev production]", got)
	}
}

// A bucket holds whatever its owner put there. Only the keys this backend
// wrote are environments, so anything else in the prefix is somebody else's
// and is not reported as one.
func TestListIgnoresObjectsThisBackendDidNotWrite(t *testing.T) {
	store := &fakeStore{}
	b := newTestBackend(t, store)
	ctx := context.Background()
	if _, err := b.Lock(ctx, "dev"); err != nil {
		t.Fatal(err)
	}
	if err := b.Put(ctx, "dev", []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{
		"infrena/notes.txt",
		"infrena/archive/old.json",
		"infrena/.infrena-probe-abcdef",
		"elsewhere/staging.json",
	} {
		if err := store.put(ctx, key, []byte("x"), false); err != nil {
			t.Fatal(err)
		}
	}

	got, err := b.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, []string{"dev"}) {
		t.Errorf("List = %v, want [dev]", got)
	}
}

// A bucket with no state in it is an empty list and no error, the same way a
// never-written environment is empty rather than missing.
func TestListingAnEmptyBucketIsNotAnError(t *testing.T) {
	b := newTestBackend(t, &fakeStore{})
	got, err := b.List(context.Background())
	if err != nil {
		t.Fatalf("List on an empty bucket errored: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("List = %v, want nothing", got)
	}
}

// State at the bucket root is the same code with no prefix to strip.
func TestListWorksWithNoPathConfigured(t *testing.T) {
	store := &fakeStore{}
	b := newTestBackend(t, store)
	b.config = Config{Bucket: "test-bucket"}
	ctx := context.Background()
	if _, err := b.Lock(ctx, "dev"); err != nil {
		t.Fatal(err)
	}
	if err := b.Put(ctx, "dev", []byte(`{}`)); err != nil {
		t.Fatal(err)
	}

	got, err := b.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, []string{"dev"}) {
		t.Errorf("List = %v, want [dev]", got)
	}
}

func TestReadingAndWritingRefuseBeforeConfigure(t *testing.T) {
	b := New()
	ctx := context.Background()
	if _, err := b.Get(ctx, "dev"); !errors.Is(err, errNotConfigured) {
		t.Errorf("Get = %v", err)
	}
	if err := b.Put(ctx, "dev", []byte(`{}`)); !errors.Is(err, errNotConfigured) {
		t.Errorf("Put = %v", err)
	}
	if _, err := b.List(ctx); !errors.Is(err, errNotConfigured) {
		t.Errorf("List = %v", err)
	}
}
