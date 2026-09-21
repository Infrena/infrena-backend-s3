package s3backend

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/infrena/infrena/pkg/backend"
)

// A store that does not implement conditional writes does not always say so.
// It may ignore the header and overwrite, and report success — which is
// exactly the shape of a lock that never locks. So support is proven by
// observing a refusal, never by asking.
//
// The probe runs at configure time rather than at the first Lock, because a
// backend that cannot lock must be refused when it loads, not when
// an apply is already under way.
//
// Measured against Backblaze B2 on 2026-09-17, the loud case turns out to be
// the common one: B2 refuses the FIRST conditional write with NotImplemented
// and never gets as far as overwriting anything. Both are refusals here, and
// they are told apart only so the message can quote B2 rather than describe a
// silent overwrite that did not happen.
func (b *Backend) proveConditionalWrites(ctx context.Context) error {
	key := b.config.prefixed(".infrena-probe-" + randomSuffix())
	body := []byte("infrena conditional write probe: safe to delete\n")

	// Write one. A store that cannot do this at all refuses here.
	if err := b.client.put(ctx, key, body, true); err != nil {
		return fmt.Errorf(
			"bucket %q cannot be used for state: its store refused a conditional write, which is how this backend locks. Every backend must lock and there is no unsafe fallback, so the bucket is refused rather than run without one. The store's own answer: %w",
			b.config.Bucket, err)
	}
	// From here the object exists whatever the verdict, so it is removed on
	// every path out. A refusal is no reason to leave rubbish in the
	// user's bucket.
	defer func() { _ = b.client.remove(context.WithoutCancel(ctx), key) }()

	// Write two, identical. The store must refuse it.
	err := b.client.put(ctx, key, body, true)
	switch {
	case err == nil:
		return fmt.Errorf(
			"bucket %q cannot be used for state: its store accepted two conditional writes of the same object, so it is ignoring the If-None-Match header rather than honouring it. A lock there would never lock — two applies could take the same one and neither would see an error — so the bucket is refused rather than run unsafely",
			b.config.Bucket)
	case codeOf(err) == codePreconditionFailed:
		// The refusal this backend's locking is built on.
		return nil
	default:
		return fmt.Errorf(
			"bucket %q cannot be used for state: its store refused the second conditional write with something other than PreconditionFailed, so what it does with If-None-Match is not clear enough to lock with. The store's own answer: %w",
			b.config.Bucket, err)
	}
}

// randomSuffix keeps two runs probing the same bucket at the same moment from
// probing the same object, which would read as a store that refuses the first
// write.
func randomSuffix() string {
	var b [8]byte
	// crypto/rand.Read cannot fail as of Go 1.24; it panics instead.
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// Lock takes the environment's lock by writing the lock object conditionally.
// The write IS the lock: whichever process's PUT the store accepts holds it,
// and there is no window between checking and taking.
//
// The holder comes from the context rather than from this process, because a
// backend plugin is a child of the infrena run and a lock stamped with the
// plugin's PID would name a process that stops existing when the run ends.
func (b *Backend) Lock(ctx context.Context, environment string) (backend.Lock, error) {
	if b.client == nil {
		return backend.Lock{}, errNotConfigured
	}
	holder := backend.HolderFrom(ctx)
	holder.Environment = environment
	body, err := json.Marshal(holder)
	if err != nil {
		return backend.Lock{}, fmt.Errorf("could not record the lock holder: %w", err)
	}

	key := b.config.lockKeyFor(environment)
	if err := b.client.put(ctx, key, body, true); err != nil {
		if codeOf(err) == codePreconditionFailed {
			return backend.Lock{}, fmt.Errorf("%w: %s", backend.ErrLocked, b.describeHolder(ctx, environment))
		}
		return backend.Lock{}, fmt.Errorf("could not write the lock for %q to %s: %w", environment, key, err)
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	if b.held == nil {
		b.held = map[string][]byte{}
	}
	// The exact bytes written, so that Unlock and Put can tell this run's
	// lock from one somebody else took after ours was force-unlocked.
	b.held[environment] = body
	return holder, nil
}

// describeHolder reads back the lock that beat us, so the conflict names
// somebody. It is best effort on purpose: the conflict is already established
// by the store's refusal, and failing to read the holder must not turn a
// perfectly good lock conflict into a transport error the caller cannot
// recognise.
func (b *Backend) describeHolder(ctx context.Context, environment string) string {
	holder, held, err := b.Inspect(ctx, environment)
	switch {
	case err != nil:
		return fmt.Sprintf("%q is locked, and the lock object could not be read to say by whom: %v", environment, err)
	case !held:
		// The lock was released between the refusal and this read.
		return fmt.Sprintf("%q was locked when the lock was attempted, and the lock has since been released. Try again", environment)
	}
	return fmt.Sprintf("%q is held by %s on %s (pid %d), running %s since %s",
		environment, holder.User, holder.Host, holder.PID, holder.Operation, holder.At.Format("2006-01-02 15:04:05 MST"))
}

// Unlock releases a lock THIS run holds, and refuses any other.
//
// It refuses rather than deleting whatever is there because a stray Unlock
// would free an environment another apply is actively mutating, and because
// breaking somebody else's lock is ForceUnlock's job, which the user asks for
// by name after being told who holds it.
func (b *Backend) Unlock(ctx context.Context, environment string) error {
	if b.client == nil {
		return errNotConfigured
	}
	b.mu.Lock()
	ours, taken := b.held[environment]
	b.mu.Unlock()
	if !taken {
		return fmt.Errorf("this run does not hold the lock on %q, so it will not release it. `infrena state unlock %s` breaks a lock another run holds", environment, environment)
	}

	stored, err := b.client.get(ctx, b.config.lockKeyFor(environment))
	if err != nil {
		if codeOf(err) == codeNoSuchKey {
			// Already gone, most likely force-unlocked. Nothing to do,
			// but this run no longer holds anything.
			b.forget(environment)
			return fmt.Errorf("the lock on %q was already gone when this run tried to release it, so something else removed it", environment)
		}
		return fmt.Errorf("could not read the lock on %q before releasing it: %w", environment, err)
	}
	if !bytes.Equal(stored, ours) {
		return fmt.Errorf("the lock on %q is no longer this run's: %s", environment, b.describeHolder(ctx, environment))
	}

	if err := b.client.remove(ctx, b.config.lockKeyFor(environment)); err != nil {
		return fmt.Errorf("could not release the lock on %q: %w", environment, err)
	}
	b.forget(environment)
	return nil
}

// Inspect reports who holds an environment's lock.
//
// The bool separates "nothing is held" from "the lock could not be read": a
// caller that conflated them would report an unreadable lock as a free
// environment and let a second apply start.
func (b *Backend) Inspect(ctx context.Context, environment string) (backend.Lock, bool, error) {
	if b.client == nil {
		return backend.Lock{}, false, errNotConfigured
	}
	body, err := b.client.get(ctx, b.config.lockKeyFor(environment))
	if err != nil {
		if codeOf(err) == codeNoSuchKey {
			return backend.Lock{}, false, nil
		}
		return backend.Lock{}, false, fmt.Errorf("could not read the lock on %q: %w", environment, err)
	}
	var holder backend.Lock
	if err := json.Unmarshal(body, &holder); err != nil {
		// Held by something, and unreadable. Reporting "not held" here
		// would let an apply start beside whatever wrote it.
		return backend.Lock{}, false, fmt.Errorf("the lock object for %q is not a lock this backend wrote: %w", environment, err)
	}
	return holder, true, nil
}

// ForceUnlock removes a lock whoever holds it, for `infrena state unlock`
// after it has named the holder.
//
// A lock that was not there is an error rather than a silent success, so that
// a typo in an environment name does not look like it worked.
func (b *Backend) ForceUnlock(ctx context.Context, environment string) error {
	if b.client == nil {
		return errNotConfigured
	}
	if _, held, err := b.Inspect(ctx, environment); err != nil {
		return err
	} else if !held {
		return fmt.Errorf("no lock is held on %q, so there is nothing to unlock. Check the environment name", environment)
	}
	if err := b.client.remove(ctx, b.config.lockKeyFor(environment)); err != nil {
		return fmt.Errorf("could not remove the lock on %q: %w", environment, err)
	}
	b.forget(environment)
	return nil
}

func (b *Backend) forget(environment string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.held, environment)
}
