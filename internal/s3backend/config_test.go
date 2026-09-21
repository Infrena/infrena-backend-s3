package s3backend

import (
	"strings"
	"testing"
)

func TestParseConfigTakesWhatTheBackendBlockCarries(t *testing.T) {
	got, err := ParseConfig(map[string]any{
		"bucket":  "some-bucket-name",
		"profile": "my-bucket-profile",
		"path":    "/infrena/",
		"region":  "us-east-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Bucket != "some-bucket-name" || got.Profile != "my-bucket-profile" {
		t.Errorf("Config = %+v", got)
	}
}

// A bucket is the one thing with no sensible default.
func TestAMissingBucketIsAnErrorNamingIt(t *testing.T) {
	_, err := ParseConfig(map[string]any{"path": "/infrena/"})
	if err == nil {
		t.Fatal("a config with no bucket was accepted")
	}
	if !strings.Contains(err.Error(), "bucket") {
		t.Errorf("error does not name the missing key: %v", err)
	}
}

// A profile NAMES a credential. A key IS one, and must never sit in a file
// that is committed to git. Refusing loudly is better than accepting it and
// having it end up in a repository.
func TestAnAccessKeyInConfigurationIsRefused(t *testing.T) {
	for _, key := range []string{"access_key", "secret_key", "access_key_id", "secret_access_key"} {
		_, err := ParseConfig(map[string]any{"bucket": "b", key: "AKIAEXAMPLE"})
		if err == nil {
			t.Errorf("%q was accepted as configuration", key)
			continue
		}
		if !strings.Contains(err.Error(), "profile") {
			t.Errorf("%q: refusal does not point at the alternative: %v", key, err)
		}
	}
}

// Infrena cannot refuse an unknown key in `backend:` because it cannot know
// this plugin's keys. This plugin does know them, so a typo stops here rather
// than being silently ignored — the asymmetry is the point.
func TestAnUnknownKeyIsAnErrorNamingIt(t *testing.T) {
	_, err := ParseConfig(map[string]any{"bucket": "b", "buckte": "b"})
	if err == nil {
		t.Fatal("a misspelled key was accepted")
	}
	if !strings.Contains(err.Error(), "buckte") {
		t.Errorf("error does not name the key: %v", err)
	}
}

// Keys are built by the backend, so the key space has no surprises in it.
func TestKeysAreBuiltUnderThePath(t *testing.T) {
	c := Config{Bucket: "b", Path: "/infrena/"}
	if got := c.keyFor("production"); got != "infrena/production.json" {
		t.Errorf("keyFor = %q", got)
	}
	if got := c.lockKeyFor("production"); got != "infrena/production.lock" {
		t.Errorf("lockKeyFor = %q", got)
	}
}

// An unset path puts state at the bucket root rather than erroring, and a
// path with or without slashes means the same thing.
func TestPathIsNormalised(t *testing.T) {
	for _, p := range []string{"", "/", "infrena", "/infrena", "infrena/", "/infrena/"} {
		c := Config{Bucket: "b", Path: p}
		got := c.keyFor("dev")
		if strings.HasPrefix(got, "/") || strings.Contains(got, "//") {
			t.Errorf("path %q produced key %q", p, got)
		}
	}
}

// A key that takes a string and is given a number is a mistake worth naming,
// not something to coerce: `region: 1` means the user meant something else.
func TestAKeyOfTheWrongTypeIsRefused(t *testing.T) {
	if _, err := ParseConfig(map[string]any{"bucket": "b", "region": 1}); err == nil {
		t.Error("a numeric region was accepted")
	}
	if _, err := ParseConfig(map[string]any{"bucket": "b", "path_style": "yes"}); err == nil {
		t.Error("a string path_style was accepted")
	}
}

// path_style is a pointer because "unset" is a real third state: it defaults
// to on for a custom endpoint and off for AWS, and a plain bool could not
// tell an explicit `path_style: false` from an absent one.
func TestPathStyleDefaultsPerEndpointButIsHonouredWhenSet(t *testing.T) {
	c, err := ParseConfig(map[string]any{"bucket": "b"})
	if err != nil {
		t.Fatal(err)
	}
	if c.PathStyle != nil {
		t.Errorf("PathStyle = %v, want unset", *c.PathStyle)
	}
	if c.pathStyle() {
		t.Error("AWS defaulted to path-style addressing")
	}

	c, err = ParseConfig(map[string]any{"bucket": "b", "endpoint": "http://127.0.0.1:9000"})
	if err != nil {
		t.Fatal(err)
	}
	if !c.pathStyle() {
		t.Error("a custom endpoint did not default to path-style addressing")
	}

	c, err = ParseConfig(map[string]any{"bucket": "b", "endpoint": "http://127.0.0.1:9000", "path_style": false})
	if err != nil {
		t.Fatal(err)
	}
	if c.pathStyle() {
		t.Error("an explicit path_style: false was overridden by the endpoint default")
	}
}

// The client is built from the configuration alone, so a plain bucket with no
// endpoint reaches AWS and a custom endpoint is honoured scheme and all.
func TestNewClientHonoursTheEndpointAndItsScheme(t *testing.T) {
	c, err := ParseConfig(map[string]any{"bucket": "b", "endpoint": "http://127.0.0.1:9000", "profile": "p"})
	if err != nil {
		t.Fatal(err)
	}
	client, err := newClient(c)
	if err != nil {
		t.Fatal(err)
	}
	if got := client.EndpointURL().String(); got != "http://127.0.0.1:9000" {
		t.Errorf("EndpointURL = %q", got)
	}

	c, err = ParseConfig(map[string]any{"bucket": "b"})
	if err != nil {
		t.Fatal(err)
	}
	client, err = newClient(c)
	if err != nil {
		t.Fatal(err)
	}
	if got := client.EndpointURL().String(); got != "https://s3.amazonaws.com" {
		t.Errorf("EndpointURL with no endpoint set = %q", got)
	}
}

// Configure is where a bad `backend:` block is caught, and it must leave the
// backend refusing work rather than half configured.
func TestConfigureRefusesABlockWithNoBucket(t *testing.T) {
	b := New()
	if err := b.Configure(t.Context(), map[string]any{"path": "/infrena/"}); err == nil {
		t.Fatal("a bucketless block configured the backend")
	}
	if _, err := b.List(t.Context()); err == nil {
		t.Error("the backend served a call after a failed Configure")
	}
}
