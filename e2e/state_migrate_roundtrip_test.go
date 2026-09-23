//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/minio/minio-go/v7"
)

// The local -> S3 -> local state round trip.
//
// THIS IS THE ONE TEST A FAKE CANNOT STAND IN FOR, and the reason is the last
// assertion rather than the first. Either backend alone can be shown to read
// its own writing, which proves nothing about the pair: a backend that dropped
// a field on the way out and invented it again on the way back would pass such
// a test perfectly. Going OUT to a real object store and BACK is what makes
// the two disagree if they disagree, because the bytes that come home were
// written by one implementation and read by the other.
//
// IT LIVES HERE RATHER THAN IN INFRENA, for the reason every other suite in
// this repository does: a backend proves it works against infrena, and infrena
// does not carry a test that needs one of its backends checked out beside it
// and a live store running. This suite already builds all three binaries and
// has a store, so the test costs it nothing but the file.

// TestStateSurvivesARoundTripThroughThisBackend.
//
// One test rather than six, because every step depends on the state the one
// before it left behind and a suite of independent cases would have to rebuild
// the world each time -- against a real store, from a real apply. The steps are
// numbered in the output so a failure says which one broke.
func TestStateSurvivesARoundTripThroughThisBackend(t *testing.T) {
	bucketName := bucket(t)
	environments := []string{"dev", "production"}

	// ---------------------------------------------------------------
	// 1. A project with LOCAL state and two environments, each holding
	//    real resources after an apply.
	// ---------------------------------------------------------------
	dir := t.TempDir()
	write(t, filepath.Join(dir, "infrena.yml"), localProject)

	for _, environment := range environments {
		// The REPORT, not the exit code. An apply that made changes exits 2,
		// which says there were changes and not whether they worked.
		out, _ := run(t, dir, "apply", environment, "--auto-approve")
		if !strings.Contains(out, "Apply complete") || !strings.Contains(out, "0 failed") {
			t.Fatalf("step 1: apply %s did not complete:\n%s", environment, out)
		}
	}
	// The bytes the whole test is about. Read before anything else touches
	// them, because from here on every command may rewrite them.
	original := map[string][]byte{}
	for _, environment := range environments {
		original[environment] = readBytes(t, localStatePath(dir, environment))
		if len(original[environment]) == 0 {
			t.Fatalf("step 1: %s has no local state after an apply", environment)
		}
	}

	// ---------------------------------------------------------------
	// 2. Point `backend:` at S3, `migrate_from:` at local, migrate.
	// ---------------------------------------------------------------
	write(t, filepath.Join(dir, "infrena.yml"), projectMigratingTo(bucketName))
	if out, code := run(t, dir, "state", "migrate"); code != 0 {
		t.Fatalf("step 2: state migrate exit = %d:\n%s", code, out)
	}

	// ---------------------------------------------------------------
	// 3. The objects are in the bucket, READ FROM THE BUCKET.
	// ---------------------------------------------------------------
	held := keysIn(t, bucketName)
	for _, environment := range environments {
		key := roundTripPrefix + "/" + environment + ".json"
		if !contains(held, key) {
			t.Errorf("step 3: %s is not in the bucket, which holds %v", key, held)
			continue
		}
		stored := objectAt(t, bucketName, key)
		t.Logf("step 3: %s holds %d bytes in the bucket", key, len(stored))
		if !sameStateContent(t, original[environment], stored) {
			t.Errorf("step 3: the object in the bucket is not the state that was migrated\n"+
				"local:\n%s\nbucket:\n%s", original[environment], stored)
		}
	}

	// THE LOCAL COPY GOES NOW, and it is not tidying. It is doing two jobs.
	//
	// It makes step 4 mean something: with the old state still on disk, a
	// `plan` that had quietly fallen back to local would be just as clean as
	// one that really read the bucket, and the step would pass either way.
	//
	// And it makes step 6 possible at all: left in place, local and the bucket
	// hold the same state, the return migration compares them, says "already
	// migrated" and correctly writes nothing -- so step 6 would compare a file
	// with itself and pass without a round trip ever having happened.
	for _, environment := range environments {
		if err := os.Remove(localStatePath(dir, environment)); err != nil {
			t.Fatalf("step 3: removing the local copy: %v", err)
		}
	}

	// ---------------------------------------------------------------
	// 4. A plan against the migrated state is CLEAN, and there is nothing
	//    left on disk for it to read. Exit 0, not 2: the migrated state is
	//    UNDERSTOOD, not merely stored, and a backend that round-tripped an
	//    attribute badly shows up here as drift rather than as an error.
	// ---------------------------------------------------------------
	for _, environment := range environments {
		if out, code := run(t, dir, "plan", environment); code != 0 {
			t.Fatalf("step 4: plan %s over migrated state exit = %d, want 0 (no changes):\n%s",
				environment, code, out)
		}
	}

	// ---------------------------------------------------------------
	// 5. Swap the blocks and migrate back.
	// ---------------------------------------------------------------
	write(t, filepath.Join(dir, "infrena.yml"), projectMigratingFrom(bucketName))
	if out, code := run(t, dir, "state", "migrate"); code != 0 {
		t.Fatalf("step 5: the return migration exit = %d:\n%s", code, out)
	}

	// ---------------------------------------------------------------
	// 6. THE POINT OF THE WHOLE TEST. The state that came home is the state
	//    that left, to the byte, once the two fields a WRITE stamps are set
	//    aside -- and the second assertion is what makes the first one mean
	//    something: those two fields are the ONLY difference, so nothing else
	//    moved, was reordered, or was re-encoded on the way.
	// ---------------------------------------------------------------
	for _, environment := range environments {
		returned := readBytes(t, localStatePath(dir, environment))
		if bytes.Equal(original[environment], returned) {
			t.Logf("step 6: %s came back byte-identical", environment)
			continue
		}
		if !sameStateContent(t, original[environment], returned) {
			t.Errorf("step 6: %s did not survive the round trip\nbefore:\n%s\nafter:\n%s",
				environment, original[environment], returned)
			continue
		}
		fields := differingFields(t, original[environment], returned)
		// LOGGED RATHER THAN LEFT IMPLICIT. A difference is a finding even
		// when it is an allowed one, and a reader of a green run should be
		// able to see exactly which bytes moved and what they were.
		t.Logf("step 6: %s came back identical except %v\nbefore: %s\nafter:  %s",
			environment, fields,
			writeStampsOf(t, original[environment]), writeStampsOf(t, returned))
		if !onlyWriteStamps(fields) {
			t.Errorf("step 6: %s came back differing in %v, and only serial and updated_at may differ\n"+
				"before:\n%s\nafter:\n%s", environment, fields, original[environment], returned)
		}
	}
}

// roundTripPrefix is the `path:` the project's backend block asks for, so the
// keys under test are not at the bucket root and a prefix bug has somewhere to
// show itself. Deliberately not the other suite's prefix: these two tests share
// a store, and a shared key would make either one depend on the other's order.
const roundTripPrefix = "state"

// localProject is step 1's configuration: two resources rather than one, and
// one of them REFERRING to the other, so the state under test holds a resolved
// reference and not just a scalar.
const localProject = `
project: roundtrip

environments:
  dev: {}
  production: {}

resources:
  network:
    type: fake.network
    cidr: 10.20.0.0/16
  database:
    type: fake.database
    engine: postgres
    network: ${network.id}
`

// projectMigratingTo is step 2: state lives in the bucket now, and local is
// named rather than left out, because leaving it out already means "no
// migration".
func projectMigratingTo(bucketName string) string {
	return localProject + fmt.Sprintf(`
backend:
  plugin: s3
  bucket: %s
  path: /%s/
  endpoint: %s
  region: %s
  path_style: true

migrate_from:
  plugin: local
`, bucketName, roundTripPrefix, endpoint, region)
}

// projectMigratingFrom is step 5, which is the same two blocks the other way
// round. Nothing else in the file changes.
func projectMigratingFrom(bucketName string) string {
	return localProject + fmt.Sprintf(`
backend:
  plugin: local

migrate_from:
  plugin: s3
  bucket: %s
  path: /%s/
  endpoint: %s
  region: %s
  path_style: true
`, bucketName, roundTripPrefix, endpoint, region)
}

// localStatePath is where the built-in backend keeps an environment's state:
// `.infrena/state/<environment>.json`, which this file names directly rather
// than asking infrena, for the reason the bucket is read directly.
func localStatePath(dir, environment string) string {
	return filepath.Join(dir, ".infrena", "state", environment+".json")
}

func readBytes(t *testing.T, path string) []byte {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func contains(haystack []string, want string) bool {
	for _, got := range haystack {
		if got == want {
			return true
		}
	}
	return false
}

// objectAt reads one key, beside the backend rather than through it.
//
// stateInBucket is the same idea for the other suite's fixed key; this one
// takes the key, because the prefix under test is part of what step 3 checks.
func objectAt(t *testing.T, bucketName, key string) []byte {
	t.Helper()
	object, err := admin.GetObject(context.Background(), bucketName, key, minio.GetObjectOptions{})
	if err != nil {
		t.Fatalf("reading %s/%s: %v", bucketName, key, err)
	}
	defer object.Close()
	body, err := io.ReadAll(object)
	if err != nil {
		t.Fatalf("reading %s/%s: %v\nthe bucket holds %v", bucketName, key, err, keysIn(t, bucketName))
	}
	return body
}

// ---------------------------------------------------------------------------
// Comparing two states
// ---------------------------------------------------------------------------

// sameStateContent reports whether two encoded states hold the same thing,
// with the two fields a write stamps set aside.
//
// Serial and UpdatedAt are stamped by whichever backend performed the WRITE,
// so a faithful copy necessarily carries different values for both and a raw
// byte comparison of a round trip can never pass. They are the only two fields
// cleared; every resource, attribute and key ordering crosses into the
// comparison exactly as it was stored, which is what makes this a comparison of
// the state rather than of a summary of it.
func sameStateContent(t *testing.T, a, b []byte) bool {
	t.Helper()
	return bytes.Equal(withoutWriteStamps(t, a), withoutWriteStamps(t, b))
}

func withoutWriteStamps(t *testing.T, encoded []byte) []byte {
	t.Helper()
	doc := decodeState(t, encoded)
	delete(doc, "serial")
	delete(doc, "updated_at")
	out, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// differingFields names the top-level keys whose values are not identical, so
// a failure says WHICH field moved rather than printing two documents and
// leaving the reader to diff them.
func differingFields(t *testing.T, a, b []byte) []string {
	t.Helper()
	left, right := decodeState(t, a), decodeState(t, b)
	seen := map[string]bool{}
	var names []string
	for _, doc := range []map[string]json.RawMessage{left, right} {
		for name := range doc {
			if seen[name] {
				continue
			}
			seen[name] = true
			if !bytes.Equal(left[name], right[name]) {
				names = append(names, name)
			}
		}
	}
	// Sorted, so a failure message says the same thing twice running. Map
	// iteration order would otherwise make one report read differently from
	// the next for an identical difference.
	sort.Strings(names)
	return names
}

// onlyWriteStamps reports whether a difference is entirely bookkeeping about
// the write itself. Anything else is a backend disagreeing about what state is.
func onlyWriteStamps(fields []string) bool {
	for _, name := range fields {
		if name != "serial" && name != "updated_at" {
			return false
		}
	}
	return true
}

// writeStampsOf renders just the two fields a write may change, for a log line
// that says what differed rather than making a reader diff two documents.
func writeStampsOf(t *testing.T, encoded []byte) string {
	t.Helper()
	doc := decodeState(t, encoded)
	return fmt.Sprintf("serial=%s updated_at=%s", doc["serial"], doc["updated_at"])
}

func decodeState(t *testing.T, encoded []byte) map[string]json.RawMessage {
	t.Helper()
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &doc); err != nil {
		t.Fatalf("state is not JSON: %v\n%s", err, encoded)
	}
	return doc
}
