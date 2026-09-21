package s3backend

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"
)

// fakeStore is an object store that can be told to misbehave.
//
// It exists for the two behaviours a real store will not perform on demand: a
// store that refuses the conditional-write header, and a store that accepts it
// and ignores it. MinIO always gets this right, so the refusal paths can only
// be reached through a double. Everything else here is the smallest store that
// behaves.
type fakeStore struct {
	mu      sync.Mutex
	objects map[string][]byte

	// conditionalWritesNotImplemented is Backblaze B2: it refuses the
	// FIRST conditional write, loudly. Measured against the real service on
	// 2026-09-17.
	conditionalWritesNotImplemented bool
	// ignoresIfNoneMatch is the silent case: the header is accepted,
	// ignored, and the write overwrites while reporting success. No store
	// is currently known to do this, which is exactly why it needs a fake.
	ignoresIfNoneMatch bool
}

func (s *fakeStore) put(_ context.Context, key string, body []byte, ifAbsent bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ifAbsent {
		if s.conditionalWritesNotImplemented {
			// B2's own words, quoted from the measured response.
			return storeError{
				Code:    codeNotImplemented,
				Message: "A header you provided implies functionality that is not implemented",
			}
		}
		if _, exists := s.objects[key]; exists && !s.ignoresIfNoneMatch {
			return storeError{
				Code:    codePreconditionFailed,
				Message: "At least one of the pre-conditions you specified did not hold",
			}
		}
	}
	if s.objects == nil {
		s.objects = map[string][]byte{}
	}
	s.objects[key] = append([]byte(nil), body...)
	return nil
}

func (s *fakeStore) get(_ context.Context, key string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	body, ok := s.objects[key]
	if !ok {
		return nil, storeError{Code: codeNoSuchKey, Message: "The specified key does not exist."}
	}
	return append([]byte(nil), body...), nil
}

// remove is idempotent, because a DELETE of an object that is not there is a
// success in S3 and a fake that disagreed would hide that.
func (s *fakeStore) remove(_ context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.objects, key)
	return nil
}

func (s *fakeStore) list(_ context.Context, prefix string) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var keys []string
	for k := range s.objects {
		if strings.HasPrefix(k, prefix) {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	return keys, nil
}

func (s *fakeStore) objectCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.objects)
}

// newTestBackend is a configured backend over a store that is not a store.
func newTestBackend(t *testing.T, store *fakeStore) *Backend {
	t.Helper()
	b := New()
	b.config = Config{Bucket: "test-bucket", Path: "/infrena/"}
	b.client = store
	return b
}
