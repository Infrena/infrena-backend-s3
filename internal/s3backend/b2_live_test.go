//go:build live

// Backblaze B2, against the real service.
//
// THIS IS THE MOST VALUABLE TEST IN THE REPOSITORY, and the only one that
// proves the refusal path against something other than a fake with a boolean
// switch. Every other test of proveConditionalWrites either uses a store that
// honours If-None-Match (MinIO, live_test.go) or a double told to misbehave
// (fakeStore, lock_test.go). B2 is a real, widely used, S3-compatible store
// that genuinely cannot do a conditional write, so it is the one place the
// refusal is exercised by reality rather than by a flag this package set.
//
// A PASSING CONFIGURE HERE WOULD MEAN THE PROBE IS BROKEN, not that B2
// improved. The assertion is that the backend REFUSES, and that its refusal
// quotes what B2 said - "A header you provided implies functionality that is
// not implemented", error code NotImplemented - because B2's own explanation
// is more use to a user than anything this plugin would invent.
//
// CREDENTIALS COME FROM THE ENVIRONMENT AND NOWHERE ELSE. B2 application keys
// are long lived and this file is committed, so there is no default, no
// fixture and no fallback: absent variables skip, saying how to supply them.
// The test must not depend on any particular key existing, because the ones it
// was written against expire.
//
//	B2_KEY_ID=... B2_APP_KEY=... go test -tags live -run B2 ./internal/s3backend/ -v
package s3backend

import (
	"context"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// The credentials, read from the environment only. Never a literal in this
// file: an application key in a committed test is a published secret.
const (
	b2KeyIDEnv  = "B2_KEY_ID"
	b2AppKeyEnv = "B2_APP_KEY"
)

// Where the bucket is. Not secret - a bucket name and an endpoint are the same
// kind of thing as `backend.bucket` in a project's infrena.yml, which is
// committed - and overridable so another B2 account can run this.
const (
	b2BucketEnv   = "B2_BUCKET"
	b2EndpointEnv = "B2_ENDPOINT"
	b2RegionEnv   = "B2_REGION"

	b2DefaultBucket   = "infrena"
	b2DefaultEndpoint = "https://s3.us-west-000.backblazeb2.com"
	b2DefaultRegion   = "us-west-000"
)

// b2 reports the credentials, or skips saying how to supply them.
//
// A PLAIN SKIP, not the live(t) skip REQUIRE_LIVE_STORE turns into a failure.
// CI runs the live suite against a MinIO it starts itself, which it can insist
// on; it has no B2 account and must not be made to have one. These credentials
// belong to a person, and a suite that fails without them is a suite that
// fails for everyone who is not that person.
func b2(t *testing.T) (keyID, appKey string) {
	t.Helper()
	keyID, appKey = os.Getenv(b2KeyIDEnv), os.Getenv(b2AppKeyEnv)
	if keyID == "" || appKey == "" {
		t.Skipf("no Backblaze B2 credentials: set %s and %s to a B2 application key with access to the %q bucket.\n"+
			"They are read from the environment only and are never stored in this repository:\n"+
			"\t%s=... %s=... go test -tags live -run B2 ./internal/s3backend/ -v",
			b2KeyIDEnv, b2AppKeyEnv, envOr(b2BucketEnv, b2DefaultBucket), b2KeyIDEnv, b2AppKeyEnv)
	}
	// The backend resolves credentials through a chain whose file link is
	// pointed at nothing by TestMain, so the environment is what it reads.
	// Set here rather than in TestMain so that the MinIO suite and this one
	// do not have to agree about whose credentials are in the environment.
	t.Setenv("AWS_ACCESS_KEY_ID", keyID)
	t.Setenv("AWS_SECRET_ACCESS_KEY", appKey)
	t.Setenv("AWS_SESSION_TOKEN", "")
	return keyID, appKey
}

// TestConfigureRefusesBackblazeB2 is the refusal path, against a real store
// that really cannot lock.
//
// B2 fails the FIRST conditional write rather than silently accepting both,
// which is the friendlier of the two failures the probe covers. The verdict is
// the same either way - the bucket is refused - but the message differs, and
// this is the one that can quote the store.
func TestConfigureRefusesBackblazeB2(t *testing.T) {
	b2(t)
	bucket := envOr(b2BucketEnv, b2DefaultBucket)

	b := New()
	err := b.Configure(context.Background(), map[string]any{
		"bucket":   bucket,
		"endpoint": envOr(b2EndpointEnv, b2DefaultEndpoint),
		"region":   envOr(b2RegionEnv, b2DefaultRegion),
		"path":     "/infrena/",
	})
	if err == nil {
		t.Fatal("Backblaze B2 was ACCEPTED as a state store. B2 does not implement conditional writes, so either the probe " +
			"is no longer running at configure time or it is no longer reading the store's answer. This is a bug in the " +
			"probe, not a change in B2: a backend that cannot lock must be refused when it loads.")
	}
	// The whole point, and worth printing whether or not the assertions
	// below hold: this is what a B2 user sees.
	t.Logf("B2 refused, and the backend refused the bucket:\n\t%v", err)

	// The store's own words, not this plugin's summary of them.
	for _, want := range []string{"NotImplemented", "not implemented"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not quote what B2 said (%q missing): %v", want, err)
		}
	}
	// And the bucket, so a user with several knows which one was refused.
	if !strings.Contains(err.Error(), bucket) {
		t.Errorf("the refusal does not name the bucket %q: %v", bucket, err)
	}
	// A refused bucket leaves the backend unusable rather than
	// half-configured.
	if _, lockErr := b.Lock(context.Background(), "dev"); lockErr == nil {
		t.Error("a backend whose bucket was refused still granted a lock")
	}
}

// TestTheProbeLeavesNothingInABackblazeB2Bucket. The refusal happens on the
// first write, so there should be nothing to clean up - but a probe that had
// managed to write something and then failed to remove it would leave rubbish
// in a user's bucket on every configure, which is a worse first impression
// than the refusal itself.
func TestTheProbeLeavesNothingInABackblazeB2Bucket(t *testing.T) {
	keyID, appKey := b2(t)
	bucket := envOr(b2BucketEnv, b2DefaultBucket)
	endpoint := envOr(b2EndpointEnv, b2DefaultEndpoint)

	// A client beside the backend rather than through it: a backend asked
	// whether it cleaned up after itself is the wrong witness.
	u, err := url.Parse(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	client, err := minio.New(u.Host, &minio.Options{
		Creds:        credentials.NewStaticV4(keyID, appKey, ""),
		Secure:       u.Scheme == "https",
		Region:       envOr(b2RegionEnv, b2DefaultRegion),
		BucketLookup: minio.BucketLookupPath,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Configure and ignore the verdict; the refusal is the test above.
	_ = New().Configure(context.Background(), map[string]any{
		"bucket":   bucket,
		"endpoint": endpoint,
		"region":   envOr(b2RegionEnv, b2DefaultRegion),
		"path":     "/infrena/",
	})

	for info := range client.ListObjects(context.Background(), bucket, minio.ListObjectsOptions{
		Prefix:    "infrena/.infrena-probe-",
		Recursive: true,
	}) {
		if info.Err != nil {
			t.Fatalf("listing %s to check for probe leftovers: %v", bucket, info.Err)
		}
		t.Errorf("the probe left %s behind in the B2 bucket", info.Key)
	}
}
