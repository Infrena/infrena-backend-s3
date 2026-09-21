// Package s3backend stores infrena state in an S3-compatible object store.
//
// It implements backend.Backend from infrena's pkg/backend and is served by
// pkg/backendsdk, so it is an ordinary Go program that speaks the backend
// protocol on stdio and is started by infrena rather than by a user.
//
// State is bytes. This package writes what it is handed and reads it back
// unchanged, and never parses it: a backend that understood state would be a
// second reader of it, free to disagree with the first.
//
// Locking is a conditional write, and a store's support for one is PROVEN at
// configure time rather than assumed. A store that does not implement
// conditional writes does not report an error for If-None-Match, it ignores
// the header and overwrites, which is exactly the shape of a lock that never
// locks.
package s3backend

import (
	"context"
	"errors"
	"sync"

	"github.com/infrena/infrena/pkg/backend"
)

// Backend is the plugin. It is unconfigured until the host sends the
// project's `backend:` block, which is the only place a bucket can come from.
type Backend struct {
	config Config
	// client is nil until Configure has succeeded AND the store has proven
	// it can do conditional writes, and is the single answer to "is this
	// backend usable". A half-configured backend — a parsed config with no
	// client, or a client whose store cannot lock — would serve calls that
	// fail one layer further down, where the diagnostic no longer names the
	// block.
	client objectStore

	// newStore builds the store Configure will use. It is a field rather
	// than a direct call so that the configure-time proof can be tested
	// against a store that misbehaves, which no real store will do on
	// request. Production never replaces it.
	newStore func(Config) (objectStore, error)

	// held records the lock objects THIS run wrote, by environment, as the
	// exact bytes. Put refuses a write for an environment that is not in
	// here, and Unlock refuses to release a lock whose object no longer
	// matches — so invariant 5 is a property of what the backend allows
	// rather than of callers remembering to lock.
	mu   sync.Mutex
	held map[string][]byte
}

// New builds an unconfigured backend. main() has nothing else to do.
func New() *Backend { return &Backend{newStore: newObjectStore} }

// Configure reads the project's `backend:` block and builds the client.
//
// It is the optional eighth method of infrena's contract, and this backend
// implements it because a bucket cannot be defaulted. A block this backend
// cannot read leaves it unconfigured rather than partly configured, so the
// next call refuses instead of failing somewhere less explicable.
func (b *Backend) Configure(ctx context.Context, raw map[string]any) error {
	config, err := ParseConfig(raw)
	if err != nil {
		return err
	}
	client, err := b.newStore(config)
	if err != nil {
		return err
	}
	b.config = config
	b.client = client

	// LAST, and part of configuring rather than a separate step: a store
	// that cannot do a conditional write cannot lock, and a backend that
	// cannot lock must be refused when it LOADS rather than when an apply
	// is already under way. Failing here puts the backend back to
	// unusable, so nothing gets served by a bucket that was refused.
	if err := b.proveConditionalWrites(ctx); err != nil {
		b.config, b.client = Config{}, nil
		return err
	}
	return nil
}

// ValidateConfig answers `infrena validate` without contacting anything.
//
// It is ParseConfig and nothing else, which is the whole point: ParseConfig is
// where this backend's offline rules already live — a bucket is required, an
// access key is refused because infrena.yml is committed — and Configure calls
// exactly the same function before it builds a client. One function, so the two
// answers cannot come to disagree about what a readable block is.
//
// WHAT IS DELIBERATELY LEFT OUT is everything after that line in Configure: the
// client, and proveConditionalWrites. Both reach the network, and validate
// promises it will not. A bucket that does not exist, credentials that are
// refused, a store that cannot do a conditional write — all still surface at
// `plan`, exactly as before.
//
// The case this closes: an `access_key_id` written into infrena.yml used to pass
// `infrena validate` and fail only at `plan`. validate is the cheap gate CI
// runs, so the check that catches a long-lived key entering a repository was
// the one not running where keys enter repositories.
func (b *Backend) ValidateConfig(raw map[string]any) error {
	_, err := ParseConfig(raw)
	return err
}

// Both are optional in infrena's contract, so the assertions are here to catch
// a signature that has drifted out of one.
var (
	_ backend.Configurable = (*Backend)(nil)
	_ backend.Validator    = (*Backend)(nil)
)

// Name is what the handshake reports and what a diagnostic calls this plugin.
// It is the short name the `backend:` block uses (`plugin: s3`), not the
// binary's, by convention.
func (b *Backend) Name() string { return PluginName }

// errNotConfigured is what every method answers before the host has sent the
// `backend:` block. It names the missing call rather than failing on a nil
// client, because a nil-pointer panic in a plugin reaches the user as a
// protocol error about a closed pipe.
var errNotConfigured = errors.New("the s3 backend has not been configured: infrena sends the project's `backend:` block before any other call")

// Get, Put and List are in backend.go; Lock, Unlock, Inspect and ForceUnlock
// are in lock.go.

// Backend implements the whole of infrena's contract, checked here so a
// missing method is a compile error in this package rather than a type
// assertion failing inside main().
var _ backend.Backend = (*Backend)(nil)
