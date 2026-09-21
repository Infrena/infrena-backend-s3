//go:build e2e

// Package e2e drives a real infrena binary, over a real infrena-backend-s3
// binary, against a real S3-compatible store.
//
// THIS IS THE JOIN NOTHING ELSE COVERS. infrena's internal/backendhost tests
// opening a backend; internal/cli tests routing a command to one; this
// package's sibling live suite tests the backend against MinIO. Until there
// was a backend to run, nothing put a command, the host, a plugin process and
// a bucket in one line — and every one of those seams is where "it worked in
// the unit tests" stops being evidence.
//
// EVERY ASSERTION ABOUT THE BUCKET READS THE BUCKET. infrena reporting that it
// wrote state is infrena's opinion of infrena, so the state object is fetched
// with a client of this suite's own, beside the backend rather than through
// it.
//
// Not part of `go test ./...`: it builds infrena and two plugins from source.
//
//	docker compose up -d
//	go test -tags e2e -count=1 -v ./e2e/
//
// INFRENA_SRC points at the infrena checkout and INFRENA_PLUGIN_FAKE_REPO at
// the fake provider's; the defaults are the siblings ../../infrena and
// ../../infrena-provider-fake. It SKIPS when a checkout or the store is
// missing, and REQUIRE_LIVE_STORE=1 turns that skip into a failure, for the
// reason the live suite's does: a suite that silently skips reports green for
// tests that never ran.
package e2e

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

const (
	liveEndpointEnv = "INFRENA_LIVE_S3_ENDPOINT"
	liveAccessEnv   = "INFRENA_LIVE_S3_ACCESS_KEY"
	liveSecretEnv   = "INFRENA_LIVE_S3_SECRET_KEY"
	liveRegionEnv   = "INFRENA_LIVE_S3_REGION"
	requireEnv      = "REQUIRE_LIVE_STORE"

	infrenaSrcEnv = "INFRENA_SRC"
	fakeRepoEnv   = "INFRENA_PLUGIN_FAKE_REPO"
)

const (
	defaultEndpoint = "http://127.0.0.1:9000"
	defaultKey      = "minioadmin"
	defaultRegion   = "us-east-1"
)

var (
	infrenaBin string
	pluginDir  string

	endpoint, region string
	access, secret   string
	// noCredentials is a path that does not exist, so that nothing on the
	// developer's machine signs a request in this suite.
	noCredentials string

	// admin reads the bucket directly. See the package comment.
	admin *minio.Client

	// skipReason is why the suite cannot run, or empty.
	skipReason string
)

func TestMain(m *testing.M) {
	tmp, err := os.MkdirTemp("", "s3-backend-e2e-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	code := func() int {
		defer os.RemoveAll(tmp)
		if err := setUp(tmp); err != nil {
			skipReason = err.Error()
			fmt.Fprintln(os.Stderr, "E2E SKIPPED: "+skipReason)
		}
		return m.Run()
	}()
	os.Exit(code)
}

func setUp(tmp string) error {
	endpoint = envOr(liveEndpointEnv, defaultEndpoint)
	region = envOr(liveRegionEnv, defaultRegion)
	access, secret = envOr(liveAccessEnv, defaultKey), envOr(liveSecretEnv, defaultKey)
	noCredentials = filepath.Join(tmp, "no-such-credentials")

	src := envOr(infrenaSrcEnv, filepath.Join("..", "..", "infrena"))
	fakeRepo := envOr(fakeRepoEnv, filepath.Join("..", "..", "infrena-provider-fake"))
	if _, err := os.Stat(filepath.Join(src, "cmd", "infrena")); err != nil {
		return fmt.Errorf("no infrena checkout at %s (set %s): %v", src, infrenaSrcEnv, err)
	}
	if _, err := os.Stat(filepath.Join(fakeRepo, "go.mod")); err != nil {
		return fmt.Errorf("no infrena-provider-fake checkout at %s (set %s): %v", fakeRepo, fakeRepoEnv, err)
	}

	infrenaBin = filepath.Join(tmp, "infrena")
	pluginDir = filepath.Join(tmp, "plugins")
	// THE BINARY NAMES ARE HOW INFRENA FINDS THEM, so none of these
	// is arbitrary: a backend is looked up as infrena-backend-<name> for the
	// `plugin:` the block names, and a provider as infrena-plugin-<name>.
	// Each is built from ITS OWN module, because `go build` of a package
	// outside the main module is refused.
	for _, b := range []struct{ dir, out, pkg string }{
		{src, infrenaBin, "./cmd/infrena"},
		{"..", filepath.Join(pluginDir, "infrena-backend-s3"), "./cmd/infrena-backend-s3"},
		{fakeRepo, filepath.Join(pluginDir, "infrena-plugin-fake"), "./cmd/infrena-plugin-fake"},
	} {
		cmd := exec.Command("go", "build", "-o", b.out, b.pkg)
		cmd.Dir = b.dir
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("building %s in %s: %v\n%s", b.pkg, b.dir, err, out)
		}
	}

	u, err := url.Parse(endpoint)
	if err != nil {
		return err
	}
	admin, err = minio.New(u.Host, &minio.Options{
		Creds:        credentials.NewStaticV4(access, secret, ""),
		Secure:       u.Scheme == "https",
		Region:       region,
		BucketLookup: minio.BucketLookupPath,
	})
	if err != nil {
		return err
	}
	if _, err := admin.ListBuckets(context.Background()); err != nil {
		return fmt.Errorf("no S3-compatible store at %s: %v\nStart one with `docker compose up -d`, or point %s at another store",
			endpoint, err, liveEndpointEnv)
	}
	return nil
}

// live skips, or fails when REQUIRE_LIVE_STORE says a skip is not acceptable.
func live(t *testing.T) {
	t.Helper()
	if skipReason == "" {
		return
	}
	if os.Getenv(requireEnv) != "" {
		t.Fatalf("%s\n%s is set, so this is a failure rather than a skip.", skipReason, requireEnv)
	}
	t.Skip(skipReason)
}

// bucket makes a bucket for one test and takes it away afterwards.
func bucket(t *testing.T) string {
	t.Helper()
	live(t)
	ctx := context.Background()
	var suffix [8]byte
	_, _ = rand.Read(suffix[:])
	name := "infrena-e2e-" + hex.EncodeToString(suffix[:])
	if err := admin.MakeBucket(ctx, name, minio.MakeBucketOptions{Region: region}); err != nil {
		t.Fatalf("creating the bucket %q on %s: %v", name, endpoint, err)
	}
	t.Cleanup(func() {
		for _, key := range keysIn(t, name) {
			if err := admin.RemoveObject(ctx, name, key, minio.RemoveObjectOptions{}); err != nil {
				t.Errorf("removing %s/%s: %v", name, key, err)
			}
		}
		if err := admin.RemoveBucket(ctx, name); err != nil {
			t.Errorf("removing the bucket %q: %v", name, err)
		}
	})
	return name
}

// project writes the fixture into a fresh directory, pointed at one bucket.
func project(t *testing.T, bucketName string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("testdata", "basic", "infrena.yml"))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	write(t, filepath.Join(dir, "infrena.yml"), strings.NewReplacer(
		"BUCKET", bucketName, "ENDPOINT", endpoint, "REGION", region,
	).Replace(string(body)))
	return dir
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// infrena runs the CLI in a project directory.
//
// The environment is scrubbed the way the sibling live suite's is: the plugin
// directory above is the only source of plugins, and the store's credentials
// reach the backend through the environment link of its credential chain with
// the file link pointed at nothing. A developer with real AWS credentials on
// disk would otherwise have this suite sign MinIO requests with them.
func run(t *testing.T, dir string, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command(infrenaBin, append(args, "--plugin-dir", pluginDir)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"INFRENA_PLUGIN_PATH=", "HOME="+t.TempDir(),
		"AWS_SHARED_CREDENTIALS_FILE="+noCredentials, "AWS_CONFIG_FILE="+noCredentials,
		"AWS_PROFILE=", "AWS_SESSION_TOKEN=",
		"AWS_ACCESS_KEY_ID="+access, "AWS_SECRET_ACCESS_KEY="+secret,
	)
	out, err := cmd.CombinedOutput()
	code := 0
	if exitErr, ok := err.(*exec.ExitError); ok {
		code = exitErr.ExitCode()
	} else if err != nil {
		t.Fatalf("running infrena %v: %v", args, err)
	}
	return string(out), code
}

func expect(t *testing.T, dir string, wantCode int, want []string, args ...string) string {
	t.Helper()
	out, code := run(t, dir, args...)
	if code != wantCode {
		t.Fatalf("infrena %s: exit %d, want %d\n%s", strings.Join(args, " "), code, wantCode, out)
	}
	for _, w := range want {
		if !strings.Contains(out, w) {
			t.Fatalf("infrena %s: output lacks %q\n%s", strings.Join(args, " "), w, out)
		}
	}
	return out
}

func keysIn(t *testing.T, bucketName string) []string {
	t.Helper()
	var keys []string
	for info := range admin.ListObjects(context.Background(), bucketName, minio.ListObjectsOptions{Recursive: true}) {
		if info.Err != nil {
			t.Fatalf("listing %s: %v", bucketName, info.Err)
		}
		keys = append(keys, info.Key)
	}
	return keys
}

// stateInBucket reads the state object out of the bucket and decodes it.
//
// NOT THROUGH INFRENA. `infrena state list` is tested separately and for its
// own sake; this is the independent witness that the bytes are really in the
// store, under the key the `backend:` block asked for.
func stateInBucket(t *testing.T, bucketName, environment string) (raw []byte, decoded struct {
	Version     int                        `json:"version"`
	Serial      uint64                     `json:"serial"`
	Project     string                     `json:"project"`
	Environment string                     `json:"environment"`
	Resources   map[string]json.RawMessage `json:"resources"`
}) {
	t.Helper()
	key := "infrena/" + environment + ".json"
	object, err := admin.GetObject(context.Background(), bucketName, key, minio.GetObjectOptions{})
	if err != nil {
		t.Fatalf("reading %s/%s: %v", bucketName, key, err)
	}
	defer object.Close()
	raw, err = io.ReadAll(object)
	if err != nil {
		t.Fatalf("reading %s/%s: %v\nthe bucket holds %v", bucketName, key, err, keysIn(t, bucketName))
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("%s/%s is not a state document: %v\n%s", bucketName, key, err, raw)
	}
	return raw, decoded
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// TestTheWorkflowThroughRealInfrenaCommands is the whole of step 2 in one
// line: apply, re-plan, list, destroy, against a bucket rather than a
// directory, with every claim about the bucket checked against the bucket.
func TestTheWorkflowThroughRealInfrenaCommands(t *testing.T) {
	bucketName := bucket(t)
	dir := project(t, bucketName)

	t.Run("nothing is in the bucket before the first apply", func(t *testing.T) {
		if keys := keysIn(t, bucketName); len(keys) != 0 {
			t.Fatalf("a fresh bucket holds %v", keys)
		}
		// And `plan` on a project that has never applied is the ordinary
		// case, not a broken bucket: the backend's Get of a missing key
		// has to read as empty all the way up to the command.
		expect(t, dir, 2, []string{"3 to create"}, "plan", "dev")
	})

	t.Run("apply writes the state into the bucket", func(t *testing.T) {
		expect(t, dir, 2, []string{"Apply complete", "0 failed"}, "apply", "dev", "--auto-approve")

		raw, st := stateInBucket(t, bucketName, "dev")
		if st.Project != "s3-backend-e2e" || st.Environment != "dev" {
			t.Errorf("the object in the bucket is state for %q/%q", st.Project, st.Environment)
		}
		for _, name := range []string{"network", "db", "app"} {
			if _, ok := st.Resources[name]; !ok {
				t.Errorf("the state in the bucket records no %q:\n%s", name, raw)
			}
		}
		// The lock is NOT left behind. A backend that wrote state and kept
		// the lock would pass every read-shaped check here and wedge the
		// environment for the next run.
		for _, key := range keysIn(t, bucketName) {
			if strings.HasSuffix(key, ".lock") {
				t.Errorf("apply left the lock object %s behind", key)
			}
		}
		if keys := keysIn(t, bucketName); len(keys) != 1 {
			t.Errorf("the bucket holds %v, want just the state object", keys)
		}
	})

	t.Run("a re-plan reading that state is clean", func(t *testing.T) {
		// Exit 0 is "no changes". A plan that proposed anything
		// would mean the state infrena read back is not the state it wrote.
		expect(t, dir, 0, nil, "plan", "dev")
	})

	t.Run("state list reads the same state through infrena", func(t *testing.T) {
		out := expect(t, dir, 0, []string{"network", "db", "app"}, "state", "list", "dev")
		if strings.Contains(out, "No resources are managed") {
			t.Fatalf("state list found nothing:\n%s", out)
		}
	})

	t.Run("destroy empties the state and leaves it in the bucket", func(t *testing.T) {
		before := mustSerial(t, bucketName)
		expect(t, dir, 2, []string{"0 failed"}, "destroy", "dev", "--auto-approve")

		raw, st := stateInBucket(t, bucketName, "dev")
		if len(st.Resources) != 0 {
			t.Errorf("after destroy the state in the bucket still records %d resources:\n%s", len(st.Resources), raw)
		}
		// An emptied state is not a deleted one: the object stays, with a
		// higher serial, which is how the next run knows this environment
		// was applied and then destroyed rather than never applied.
		if st.Serial <= before {
			t.Errorf("serial after destroy = %d, was %d before: the destroy did not write state", st.Serial, before)
		}
		for _, key := range keysIn(t, bucketName) {
			if strings.HasSuffix(key, ".lock") {
				t.Errorf("destroy left the lock object %s behind", key)
			}
		}
	})
}

func mustSerial(t *testing.T, bucketName string) uint64 {
	t.Helper()
	_, st := stateInBucket(t, bucketName, "dev")
	return st.Serial
}

// TestASecondApplyIsRefusedWhileAnotherHoldsTheLock is invariant 5 as a user
// meets it: two infrena runs, two machines, one bucket.
//
// TWO PROJECT DIRECTORIES, ONE BUCKET. The fake provider's cloud is a file
// inside each project, so the two runs have entirely separate infrastructure
// and share exactly one thing — the state and its lock in S3. That is the
// situation remote state exists for, and it is not expressible with one
// directory.
//
// The race is made winnable rather than left to chance: the fake cloud is
// given a latency, so the run that locks first is still working when the
// second asks. Both orders are accepted — whichever run locks first is the
// holder — because asserting WHICH one wins would be asserting a scheduling
// order, and the rule is that exactly one does.
func TestASecondApplyIsRefusedWhileAnotherHoldsTheLock(t *testing.T) {
	bucketName := bucket(t)
	dirs := []string{project(t, bucketName), project(t, bucketName)}
	for _, dir := range dirs {
		// Pre-written rather than edited after an apply: the provider
		// creates this file on its first write, and the latency has to be
		// in place before the race rather than after it.
		write(t, filepath.Join(dir, ".infrena", "fake-cloud.json"), `{"resources":{},"latency_ms":900}`)
	}

	type result struct {
		out  string
		code int
	}
	results := make([]result, len(dirs))
	var wg sync.WaitGroup
	for i, dir := range dirs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// The second run is started a beat later so that it asks for
			// the lock while the first is applying rather than while both
			// are still compiling.
			time.Sleep(time.Duration(i) * 400 * time.Millisecond)
			out, code := run(t, dir, "apply", "dev", "--auto-approve")
			results[i] = result{out: out, code: code}
		}()
	}
	wg.Wait()

	var applied, refused int
	for i, r := range results {
		switch {
		case r.code == 2 && strings.Contains(r.out, "0 failed"):
			applied++
		case strings.Contains(r.out, "is locked"):
			refused++
			// The refusal has to say WHO, or a user reading it has
			// nobody to go and check on before forcing the lock off.
			for _, want := range []string{"is held by", "pid"} {
				if !strings.Contains(r.out, want) {
					t.Errorf("run %d was refused without naming the holder (%q missing):\n%s", i, want, r.out)
				}
			}
		default:
			t.Fatalf("run %d exited %d and neither applied nor hit the lock:\n%s", i, r.code, r.out)
		}
	}
	if applied != 1 || refused != 1 {
		t.Fatalf("%d of 2 concurrent applies succeeded and %d were refused for the lock; want exactly one of each\n--- run 0 ---\n%s\n--- run 1 ---\n%s",
			applied, refused, results[0].out, results[1].out)
	}

	// The winner still released its lock, and its state is in the bucket.
	for _, key := range keysIn(t, bucketName) {
		if strings.HasSuffix(key, ".lock") {
			t.Errorf("a lock object survived both runs: %s", key)
		}
	}
	if _, st := stateInBucket(t, bucketName, "dev"); len(st.Resources) != 3 {
		t.Errorf("the state in the bucket records %d resources, want the winner's 3", len(st.Resources))
	}
}

// TestALockLeftBehindIsBrokenByStateUnlock covers the other half of the same
// story: the run that held the lock is gone, and the operator has to be able
// to say so. `infrena state unlock` reads the holder out of the bucket, prints
// it, and removes the object — all of it through the plugin.
func TestALockLeftBehindIsBrokenByStateUnlock(t *testing.T) {
	bucketName := bucket(t)
	dir := project(t, bucketName)

	// A lock exactly as the backend writes one, put there directly: this is
	// the run that was killed, and nothing this suite can start would leave
	// one behind reliably.
	holder := `{"environment":"dev","pid":424242,"host":"other-machine","user":"someone-else","operation":"apply","at":"2026-09-17T09:00:00Z"}`
	if _, err := admin.PutObject(context.Background(), bucketName, "infrena/dev.lock",
		strings.NewReader(holder), int64(len(holder)), minio.PutObjectOptions{}); err != nil {
		t.Fatal(err)
	}

	out, code := run(t, dir, "apply", "dev", "--auto-approve")
	if code == 0 || !strings.Contains(out, "is locked") {
		t.Fatalf("apply against a locked environment exited %d:\n%s", code, out)
	}
	for _, want := range []string{"someone-else", "other-machine", "424242"} {
		if !strings.Contains(out, want) {
			t.Errorf("the refusal does not name the holder's %q:\n%s", want, out)
		}
	}

	expect(t, dir, 0, []string{"someone-else", "other-machine"}, "state", "unlock", "dev")
	for _, key := range keysIn(t, bucketName) {
		if strings.HasSuffix(key, ".lock") {
			t.Fatalf("state unlock left %s in the bucket", key)
		}
	}
	// And the environment is usable again, which is the only proof that
	// unlock removed the lock rather than merely reporting it.
	expect(t, dir, 2, []string{"0 failed"}, "apply", "dev", "--auto-approve")
}
