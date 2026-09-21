package s3backend

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/infrena/infrena/pkg/backend"
)

// stateSuffix and lockSuffix sit side by side under the same prefix, so one
// listing sees both. It also means the two are told apart by their suffix
// rather than by a separate directory a user could delete half of.
const (
	stateSuffix = ".json"
	lockSuffix  = ".lock"
)

// Get reads an environment's state.
//
// NOTHING STORED IS EMPTY, NOT AN ERROR. A project that has never applied is
// the ordinary case for every command that asks, and a backend that errored
// here would make `infrena plan` on a new project look like a broken bucket.
// A bucket that is missing or unreadable is still an error, because that is a
// question answered rather than an empty answer.
func (b *Backend) Get(ctx context.Context, environment string) ([]byte, error) {
	if b.client == nil {
		return nil, errNotConfigured
	}
	key := b.config.keyFor(environment)
	body, err := b.client.get(ctx, key)
	if err != nil {
		if codeOf(err) == codeNoSuchKey {
			return nil, nil
		}
		return nil, fmt.Errorf("could not read the state for %q from %s/%s: %w", environment, b.config.Bucket, key, err)
	}
	// Returned exactly as stored. This backend has no opinion about what
	// the bytes mean and must not acquire one by re-encoding them.
	return body, nil
}

// Put writes an environment's whole state.
//
// THE LOCK IS CHECKED HERE, at the write, and the refusal wraps
// backend.ErrNotLocked. Invariant 5 is a property of what the backend allows
// rather than of callers remembering to lock first, so a caller
// that skipped Lock gets an error rather than a silent overwrite of somebody
// else's apply.
func (b *Backend) Put(ctx context.Context, environment string, state []byte) error {
	if b.client == nil {
		return errNotConfigured
	}
	if err := b.stillHeldByThisRun(ctx, environment); err != nil {
		return err
	}
	key := b.config.keyFor(environment)
	// Not conditional: the whole point of holding the lock is that this
	// run is the one allowed to overwrite.
	if err := b.client.put(ctx, key, state, false); err != nil {
		return fmt.Errorf("could not write the state for %q to %s/%s: %w", environment, b.config.Bucket, key, err)
	}
	return nil
}

// stillHeldByThisRun answers "may this run write to that environment right
// now", which is a different question from "did this run call Lock earlier".
//
// It is checked against the STORE rather than against this process's memory
// alone, because `infrena state unlock` exists: a lock that was broken and
// retaken belongs to somebody else now, and a write made on the strength of a
// stale memory would land on top of their apply.
func (b *Backend) stillHeldByThisRun(ctx context.Context, environment string) error {
	b.mu.Lock()
	ours, taken := b.held[environment]
	b.mu.Unlock()
	if !taken {
		return fmt.Errorf("%w: this run never locked %q", backend.ErrNotLocked, environment)
	}

	stored, err := b.client.get(ctx, b.config.lockKeyFor(environment))
	if err != nil {
		if codeOf(err) == codeNoSuchKey {
			b.forget(environment)
			return fmt.Errorf("%w: the lock this run took on %q has been removed since", backend.ErrNotLocked, environment)
		}
		return fmt.Errorf("could not check the lock on %q before writing its state: %w", environment, err)
	}
	if !bytes.Equal(stored, ours) {
		return fmt.Errorf("%w: the lock this run took on %q was broken and %s", backend.ErrNotLocked, environment, b.describeHolder(ctx, environment))
	}
	return nil
}

// List names every environment this backend holds state for.
//
// IT REPORTS ENVIRONMENTS, NOT OBJECTS. The lock objects sit beside the state
// objects under the same prefix, so a listing that reported keys would show
// "dev" twice or invent an environment out of a lock. Anything this backend
// did not write is somebody else's object in somebody else's bucket and is
// left alone rather than reported as an environment.
func (b *Backend) List(ctx context.Context) ([]string, error) {
	if b.client == nil {
		return nil, errNotConfigured
	}
	prefix := b.config.listPrefix()
	keys, err := b.client.list(ctx, prefix)
	if err != nil {
		return nil, fmt.Errorf("could not list the state in %s: %w", b.config.Bucket, err)
	}

	environments := make([]string, 0, len(keys))
	for _, key := range keys {
		name := strings.TrimPrefix(key, prefix)
		// Nested keys are not this backend's: every key it writes is one
		// segment under the prefix.
		if strings.Contains(name, "/") {
			continue
		}
		// Which quietly drops the lock objects, and the probe object of a
		// configure that is running right now.
		if !strings.HasSuffix(name, stateSuffix) {
			continue
		}
		environments = append(environments, strings.TrimSuffix(name, stateSuffix))
	}
	// Sorted because the contract asks for a stable order and a listing
	// people read should not reshuffle itself between runs.
	slices.Sort(environments)
	return environments, nil
}
