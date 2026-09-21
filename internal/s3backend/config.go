package s3backend

import (
	"fmt"
	"path"
	"slices"
	"strings"
)

// Config is the project's `backend:` block, after `plugin:` has been taken out
// of it by infrena.
//
// Everything here is configuration in the sense that it is safe to commit:
// names of things, and where they live. There is deliberately nowhere to put a
// credential. See ParseConfig.
type Config struct {
	// Bucket holds the state. It has no default.
	Bucket string
	// Path is the prefix inside the bucket. Empty means the bucket root.
	Path string
	// Profile names a credential in the shared AWS credentials or config
	// file. It never holds one.
	Profile string
	// Region is the bucket's region.
	Region string
	// Endpoint points at a store that is not AWS. Empty means AWS.
	Endpoint string
	// PathStyle chooses bucket-in-the-path addressing over
	// bucket-in-the-hostname.
	//
	// A POINTER because "unset" is a real third state: the default is on
	// for a custom endpoint and off for AWS, and a plain bool could not
	// tell an explicit `path_style: false` from an absent one — which would
	// silently override the only setting most self-hosted stores need.
	PathStyle *bool
}

// The keys this backend reads. Listed once, so a refusal can print them.
var knownKeys = []string{"bucket", "endpoint", "path", "path_style", "profile", "region"}

// The keys that carry a secret. Every spelling of them is refused rather than
// read, including the ones this backend would otherwise ignore as unknown, so
// that the refusal explains itself instead of reading as a typo.
var secretKeys = []string{"access_key", "access_key_id", "secret_access_key", "secret_key", "session_token", "token"}

// ParseConfig reads the `backend:` block.
//
// AN UNKNOWN KEY IS AN ERROR HERE, and that is the opposite of what infrena
// does with the same block. Infrena cannot refuse what it does not recognise:
// it has no idea whether an s3 backend takes `profile`, and refusing unknown
// keys in the core would make every backend option a change to infrena. This
// plugin is the other side of that boundary — it knows exactly which keys it
// reads — so a typo is caught here, where it can be named, rather than
// silently ignored and discovered when state turns up somewhere unexpected.
//
// AN ACCESS KEY IS REFUSED. A profile NAMES a credential; an access key IS
// one, and `backend:` lives in infrena.yml, which is committed to git. Accepting
// one would work perfectly and put a long-lived secret in a repository, so it
// is refused pointing at `profile:` instead.
func ParseConfig(raw map[string]any) (Config, error) {
	var c Config

	keys := make([]string, 0, len(raw))
	for k := range raw {
		keys = append(keys, k)
	}
	// Sorted so a block with several problems reports the same one every
	// time rather than whichever the map happened to yield first.
	slices.Sort(keys)

	for _, k := range keys {
		if slices.Contains(secretKeys, k) {
			return Config{}, fmt.Errorf("`backend.%s` is a secret and this backend will not read one: infrena.yml is committed to git. Set `profile:` to name a profile in your AWS credentials file instead, or leave credentials to the environment or an instance role", k)
		}

		var err error
		switch k {
		case "bucket":
			c.Bucket, err = asString(k, raw[k])
		case "path":
			c.Path, err = asString(k, raw[k])
		case "profile":
			c.Profile, err = asString(k, raw[k])
		case "region":
			c.Region, err = asString(k, raw[k])
		case "endpoint":
			c.Endpoint, err = asString(k, raw[k])
		case "path_style":
			var b bool
			if b, err = asBool(k, raw[k]); err == nil {
				c.PathStyle = &b
			}
		default:
			return Config{}, fmt.Errorf("`backend.%s` is not a key the s3 backend reads. It reads %s", k, strings.Join(knownKeys, ", "))
		}
		if err != nil {
			return Config{}, err
		}
	}

	if c.Bucket == "" {
		return Config{}, fmt.Errorf("`backend.bucket` is required: the s3 backend has to be told which bucket holds this project's state, and there is no sensible default for it")
	}
	return c, nil
}

func asString(key string, v any) (string, error) {
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("`backend.%s` must be text, but it is %T", key, v)
	}
	return s, nil
}

func asBool(key string, v any) (bool, error) {
	b, ok := v.(bool)
	if !ok {
		return false, fmt.Errorf("`backend.%s` must be true or false, but it is %T", key, v)
	}
	return b, nil
}

// keyFor is where an environment's state lives.
//
// The backend builds every key itself, so the key space holds exactly what
// this code put there: no key starts with a slash, none contains an empty
// segment, and `path: /infrena/`, `path: infrena` and `path: infrena/` are the
// same place. A user who wrote the prefix three different ways in three
// projects gets one layout.
func (c Config) keyFor(environment string) string {
	return c.prefixed(environment + ".json")
}

// lockKeyFor sits beside keyFor rather than under a directory of its own, so
// one listing sees both and a lock can never outlive the bucket its state is
// in.
func (c Config) lockKeyFor(environment string) string {
	return c.prefixed(environment + ".lock")
}

// listPrefix is what a listing asks the store for: the path with its trailing
// separator, so that `path: infrena` lists infrena/ and not the neighbouring
// infrena-archive/ as well.
func (c Config) listPrefix() string {
	prefix := strings.Trim(c.Path, "/")
	if prefix == "" {
		return ""
	}
	return prefix + "/"
}

func (c Config) prefixed(name string) string {
	prefix := strings.Trim(c.Path, "/")
	if prefix == "" {
		return name
	}
	// path.Join, not fmt.Sprintf: it collapses the empty segments a
	// hand-written prefix like `a//b` would otherwise leave in the key.
	return path.Join(prefix, name)
}

// pathStyle reports whether to address the bucket in the path rather than in
// the hostname.
//
// On by default for a custom endpoint and off for AWS, because most
// self-hosted and S3-compatible stores serve only path-style addressing while
// AWS prefers virtual-host style. An explicit setting always wins.
func (c Config) pathStyle() bool {
	if c.PathStyle != nil {
		return *c.PathStyle
	}
	return c.Endpoint != ""
}
