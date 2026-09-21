//go:build live

// The live suite runs this backend against a REAL S3-compatible store.
//
// IT EXISTS BECAUSE A FAKE CANNOT PROVE WHAT A STORE DOES. Every test beside
// it talks to fakeStore, which proves this package does what its author
// thought; only these prove the store agrees. The clearest case is
// TestOnlyOneOfManyConcurrentLocksSucceedsAgainstARealStore: fakeStore's map is
// guarded by a mutex, so ten goroutines racing through it are not racing at
// all, and the one thing invariant 5 rests on is the one thing a double cannot
// be asked.
//
// Behind a build tag because it needs a store:
//
//	docker compose up -d
//	go test -tags live -count=1 ./internal/s3backend/
//
// It SKIPS when the store is unreachable, saying how to start one, and
// REQUIRE_LIVE_STORE=1 turns that skip into a failure. That is infrena's own
// INFRENA_REQUIRE_PLUGIN idea and it is not decoration: a suite that silently
// skips reports green for tests that never executed, and the day the store
// stops being reachable is the day nobody notices. CI sets it; a contributor
// with no Docker still gets a skip, because that is a setup problem rather
// than a defect.
package s3backend

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/infrena/infrena/pkg/backend"
	"github.com/infrena/infrena/pkg/backendtest"
)

// The store the suite talks to, overridable so that Task 8 can point the same
// tests at something that is not MinIO without editing them.
const (
	liveEndpointEnv = "INFRENA_LIVE_S3_ENDPOINT"
	liveAccessEnv   = "INFRENA_LIVE_S3_ACCESS_KEY"
	liveSecretEnv   = "INFRENA_LIVE_S3_SECRET_KEY"
	liveRegionEnv   = "INFRENA_LIVE_S3_REGION"

	// requireEnv turns a skip into a FAILURE. Mirrors infrena's
	// INFRENA_REQUIRE_PLUGIN, for the same reason.
	requireEnv = "REQUIRE_LIVE_STORE"
)

// The defaults are docker-compose.yml's.
const (
	liveDefaultEndpoint = "http://127.0.0.1:9000"
	liveDefaultKey      = "minioadmin"
	liveDefaultRegion   = "us-east-1"
)

// livePath is where the suite puts state in every bucket it makes. A prefix
// rather than the bucket root, because the key layout is part of what is under
// test and a root-level layout would hide a mistake in it.
const livePath = "/infrena/"

var (
	liveEndpoint string
	liveRegion   string
	// admin talks to the store DIRECTLY, beside the backend rather than
	// through it. Bucket lifecycle needs it, and so does every assertion
	// about what is actually in the bucket: a backend asked whether its own
	// write worked is the wrong witness.
	admin *minio.Client
	// liveSkip is why the suite cannot run, or empty.
	liveSkip string
)

func TestMain(m *testing.M) {
	liveEndpoint = envOr(liveEndpointEnv, liveDefaultEndpoint)
	liveRegion = envOr(liveRegionEnv, liveDefaultRegion)
	access, secret := envOr(liveAccessEnv, liveDefaultKey), envOr(liveSecretEnv, liveDefaultKey)

	// NOTHING FROM THE DEVELOPER'S MACHINE REACHES THE BACKEND. newClient
	// resolves credentials through a chain whose first link is the shared
	// AWS credentials file, so a developer with real AWS credentials on
	// disk would have this suite sign its requests with them and get a
	// puzzling 403 from MinIO. The files are pointed at nothing and the
	// store's credentials are put in the environment, which is the link
	// below.
	missing := filepath.Join(os.TempDir(), "infrena-live-no-such-credentials")
	for k, v := range map[string]string{
		"AWS_SHARED_CREDENTIALS_FILE": missing,
		"AWS_CONFIG_FILE":             missing,
		"AWS_PROFILE":                 "",
		"AWS_SESSION_TOKEN":           "",
		"AWS_ACCESS_KEY_ID":           access,
		"AWS_SECRET_ACCESS_KEY":       secret,
	} {
		if err := os.Setenv(k, v); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}

	if err := dialStore(access, secret); err != nil {
		liveSkip = fmt.Sprintf("no S3-compatible store at %s: %v\n"+
			"Start one with `docker compose up -d`, or point %s at another store.",
			liveEndpoint, err, liveEndpointEnv)
	}
	os.Exit(m.Run())
}

func dialStore(access, secret string) error {
	u, err := url.Parse(liveEndpoint)
	if err != nil {
		return err
	}
	admin, err = minio.New(u.Host, &minio.Options{
		Creds:        credentials.NewStaticV4(access, secret, ""),
		Secure:       u.Scheme == "https",
		Region:       liveRegion,
		BucketLookup: minio.BucketLookupPath,
	})
	if err != nil {
		return err
	}
	// A call, not a constructor: minio.New contacts nothing, so a store
	// that is not listening is only discovered by asking it something.
	_, err = admin.ListBuckets(context.Background())
	return err
}

// live is every live test's first line. It skips when there is no store, or
// fails when REQUIRE_LIVE_STORE says a skip is not acceptable.
func live(t *testing.T) {
	t.Helper()
	if liveSkip == "" {
		return
	}
	if os.Getenv(requireEnv) != "" {
		t.Fatalf("%s\n%s is set, so this is a failure rather than a skip.", liveSkip, requireEnv)
	}
	t.Skip(liveSkip)
}

// liveBucket makes a bucket for one test and takes it away afterwards.
//
// A BUCKET PER TEST, not a prefix per test: two tests sharing a bucket share
// a listing, and List is one of the things under test.
func liveBucket(t *testing.T) string {
	t.Helper()
	live(t)
	ctx := context.Background()
	bucket := "infrena-live-" + randomSuffix()
	if err := admin.MakeBucket(ctx, bucket, minio.MakeBucketOptions{Region: liveRegion}); err != nil {
		t.Fatalf("creating the bucket %q on %s: %v", bucket, liveEndpoint, err)
	}
	t.Cleanup(func() {
		// A bucket only goes away empty, and a test that failed
		// halfway has left objects in it.
		for _, key := range keysIn(t, bucket) {
			if err := admin.RemoveObject(ctx, bucket, key, minio.RemoveObjectOptions{}); err != nil {
				t.Errorf("removing %s/%s: %v", bucket, key, err)
			}
		}
		if err := admin.RemoveBucket(ctx, bucket); err != nil {
			t.Errorf("removing the bucket %q: %v", bucket, err)
		}
	})
	return bucket
}

// backendOn opens a configured backend over an existing bucket. Configure runs
// the conditional-write proof, so every live test has already proved MinIO
// honours If-None-Match before its first assertion.
func backendOn(t *testing.T, bucket string) *Backend {
	t.Helper()
	b := New()
	if err := b.Configure(context.Background(), map[string]any{
		"bucket":   bucket,
		"endpoint": liveEndpoint,
		"region":   liveRegion,
		"path":     livePath,
	}); err != nil {
		t.Fatalf("configuring the backend against %s/%s: %v", liveEndpoint, bucket, err)
	}
	return b
}

// liveBackend is a fresh bucket and one backend over it, which is what most
// tests want.
func liveBackend(t *testing.T) *Backend {
	t.Helper()
	return backendOn(t, liveBucket(t))
}

// keysIn lists a bucket DIRECTLY, so an assertion about what the backend left
// behind does not go through the backend that left it.
func keysIn(t *testing.T, bucket string) []string {
	t.Helper()
	var keys []string
	for info := range admin.ListObjects(context.Background(), bucket, minio.ListObjectsOptions{Recursive: true}) {
		if info.Err != nil {
			t.Fatalf("listing %s: %v", bucket, info.Err)
		}
		keys = append(keys, info.Key)
	}
	return keys
}

// bodyOf reads one object directly.
func bodyOf(t *testing.T, bucket, key string) []byte {
	t.Helper()
	object, err := admin.GetObject(context.Background(), bucket, key, minio.GetObjectOptions{})
	if err != nil {
		t.Fatalf("reading %s/%s: %v", bucket, key, err)
	}
	defer object.Close()
	body, err := io.ReadAll(object)
	if err != nil {
		t.Fatalf("reading %s/%s: %v", bucket, key, err)
	}
	return body
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// TestTheProbeAcceptsARealStoreAndCleansUpAfterItself is Task 3's probe
// against a store that answers for itself.
//
// The fake returns PreconditionFailed because it was written to; MinIO returns
// it because that is what S3 semantics say, and the two agreeing is the only
// evidence that the code branches on a real code rather than on an invented
// one.
func TestTheProbeAcceptsARealStoreAndCleansUpAfterItself(t *testing.T) {
	bucket := liveBucket(t)
	b := backendOn(t, bucket)

	if err := b.proveConditionalWrites(context.Background()); err != nil {
		t.Fatalf("a real MinIO was refused by the conditional-write proof: %v", err)
	}
	// Configure ran the probe once and this test ran it again, so a probe
	// that leaked would have left two objects in a bucket that should be
	// empty.
	if keys := keysIn(t, bucket); len(keys) != 0 {
		t.Errorf("the probe left %v in the bucket", keys)
	}
}

// TestASecondLockFailsAndNamesTheHolderAgainstARealStore. Two locks, one
// store, and the refusal has to carry a name a user can act on.
func TestASecondLockFailsAndNamesTheHolderAgainstARealStore(t *testing.T) {
	b := liveBackend(t)
	ctx := context.Background()

	first, err := b.Lock(ctx, "dev")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Lock(ctx, "dev"); err == nil {
		t.Fatal("a second lock was granted by a real store")
	} else if !strings.Contains(err.Error(), first.User) || !strings.Contains(err.Error(), first.Host) {
		t.Errorf("conflict does not name the holder: %v", err)
	}
}

// TestAPreconditionFailureIsALockConflictAgainstARealStore. MinIO's refusal
// has to arrive as ErrLocked, not as a transport error: the host classifies
// conflicts by that error and every caller tests for it.
func TestAPreconditionFailureIsALockConflictAgainstARealStore(t *testing.T) {
	b := liveBackend(t)
	ctx := context.Background()
	if _, err := b.Lock(ctx, "dev"); err != nil {
		t.Fatal(err)
	}

	if _, err := b.Lock(ctx, "dev"); !errors.Is(err, backend.ErrLocked) {
		t.Fatalf("second Lock returned %v, want ErrLocked", err)
	}
}

// TestUnlockReleasesAndInspectAgreesAgainstARealStore, and the lock object is
// gone from the bucket rather than merely forgotten by this process.
func TestUnlockReleasesAndInspectAgreesAgainstARealStore(t *testing.T) {
	bucket := liveBucket(t)
	b := backendOn(t, bucket)
	ctx := context.Background()

	if _, err := b.Lock(ctx, "dev"); err != nil {
		t.Fatal(err)
	}
	if keys := keysIn(t, bucket); !reflect.DeepEqual(keys, []string{"infrena/dev.lock"}) {
		t.Fatalf("after Lock the bucket holds %v, want [infrena/dev.lock]", keys)
	}
	if err := b.Unlock(ctx, "dev"); err != nil {
		t.Fatal(err)
	}
	if _, held, _ := b.Inspect(ctx, "dev"); held {
		t.Error("a lock survived Unlock")
	}
	if keys := keysIn(t, bucket); len(keys) != 0 {
		t.Errorf("after Unlock the bucket holds %v", keys)
	}
}

// TestStateRoundTripsByteForByteAgainstARealStore, and the object in the
// bucket is those bytes.
//
// Read back through the store directly as well as through Get, because a
// backend that mangled state on the way in and unmangled it on the way out
// would round trip perfectly and still hand a corrupted file to anything else
// that reads the bucket.
func TestStateRoundTripsByteForByteAgainstARealStore(t *testing.T) {
	bucket := liveBucket(t)
	b := backendOn(t, bucket)
	ctx := context.Background()
	if _, err := b.Lock(ctx, "dev"); err != nil {
		t.Fatal(err)
	}
	// Not valid UTF-8, with a NUL and a trailing newline: a store or a
	// backend that treats state as text loses exactly these.
	raw := []byte("{\"version\":1,\"serial\":7,\"raw\":\"\x00\xff\xfe\"}\n")

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
	if stored := bodyOf(t, bucket, "infrena/dev.json"); !bytes.Equal(stored, raw) {
		t.Errorf("the object in the bucket is %q, want %q", stored, raw)
	}
}

// TestGettingAnEnvironmentThatWasNeverWrittenIsEmptyAgainstARealStore. MinIO's
// NoSuchKey has to read as "nothing stored" rather than as a broken bucket: a
// project that has never applied is the ordinary case.
func TestGettingAnEnvironmentThatWasNeverWrittenIsEmptyAgainstARealStore(t *testing.T) {
	b := liveBackend(t)
	got, err := b.Get(context.Background(), "never")
	if err != nil {
		t.Fatalf("Get on a missing environment errored: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("Get = %q, want empty", got)
	}
}

// TestPutWithoutTheLockIsRefusedAgainstARealStore. Invariant 5 is a property
// of what the backend allows, and nothing was written to the bucket.
func TestPutWithoutTheLockIsRefusedAgainstARealStore(t *testing.T) {
	bucket := liveBucket(t)
	b := backendOn(t, bucket)

	if err := b.Put(context.Background(), "dev", []byte(`{}`)); !errors.Is(err, backend.ErrNotLocked) {
		t.Fatalf("Put without a lock returned %v, want ErrNotLocked", err)
	}
	if keys := keysIn(t, bucket); len(keys) != 0 {
		t.Errorf("a refused Put still wrote %v", keys)
	}
}

// TestListReportsEnvironmentsAndNotLockObjectsAgainstARealStore. The bucket
// really does hold four objects; List really does report two environments.
func TestListReportsEnvironmentsAndNotLockObjectsAgainstARealStore(t *testing.T) {
	bucket := liveBucket(t)
	b := backendOn(t, bucket)
	ctx := context.Background()
	for _, env := range []string{"production", "dev"} {
		if _, err := b.Lock(ctx, env); err != nil {
			t.Fatal(err)
		}
		if err := b.Put(ctx, env, []byte(`{}`)); err != nil {
			t.Fatal(err)
		}
	}

	if keys := keysIn(t, bucket); len(keys) != 4 {
		t.Fatalf("the bucket holds %v, want two state objects and two locks", keys)
	}
	got, err := b.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, []string{"dev", "production"}) {
		t.Errorf("List = %v, want [dev production]", got)
	}
}

// TestOnlyOneOfManyConcurrentLocksSucceedsAgainstARealStore is invariant 5,
// asked of the store rather than of this package.
//
// THIS IS THE TEST THE WHOLE FILE EXISTS FOR. fakeStore holds a map behind a
// mutex, so ten goroutines through it are serialised before they ever reach
// the conditional write and exactly one winner is arithmetic rather than
// evidence. Here the ten requests are ten HTTP PUTs arriving at one store with
// no mutex of ours anywhere in the path, and the single winner is the store's
// answer.
func TestOnlyOneOfManyConcurrentLocksSucceedsAgainstARealStore(t *testing.T) {
	b := liveBackend(t)
	var won int64
	var wg sync.WaitGroup
	// Errors are collected rather than reported from the goroutines, so
	// that a refusal that is NOT a lock conflict is visible instead of
	// being counted as a loss.
	errs := make([]error, 10)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := b.Lock(context.Background(), "race"); err == nil {
				atomic.AddInt64(&won, 1)
			} else {
				errs[i] = err
			}
		}()
	}
	wg.Wait()

	if won != 1 {
		t.Fatalf("%d of 10 concurrent locks succeeded, want exactly 1", won)
	}
	for _, err := range errs {
		if err != nil && !errors.Is(err, backend.ErrLocked) {
			t.Errorf("a losing Lock failed with %v, which does not wrap ErrLocked; a race the store settled must still read as a lock conflict", err)
		}
	}
}

// TestConformanceAgainstARealStore runs infrena's own contract suite against
// this backend and a real bucket.
//
// The factory makes the bucket once per check and opens a backend over it per
// call, which is what lets the suite pose the one question a single instance
// cannot: A holds the lock, an operator forces it off, B — a different infrena
// run over the same bucket — takes it, and A's next write must be refused.
// Two instances over one bucket is two machines against one bucket, and that
// is the case remote state exists for.
func TestConformanceAgainstARealStore(t *testing.T) {
	live(t)
	backendtest.Conformance(t, func(t *testing.T) func() backend.Backend {
		bucket := liveBucket(t)
		return func() backend.Backend { return backendOn(t, bucket) }
	})
}
