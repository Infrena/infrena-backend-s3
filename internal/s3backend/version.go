package s3backend

// PluginName is what this plugin is called, and it is one name in three
// places: the `plugin: s3` a project writes in its `backend:` block, the
// `name: s3` in plugin.yaml, and the `infrena-backend-s3` the host looks for
// on disk, by convention. Written once here so the manifest test can
// compare the file against the code rather than against a second literal.
const PluginName = "s3"

// Version is the release this binary is, reported in the handshake.
//
// STAMPED AT BUILD TIME by scripts/build-release with
//
//	-ldflags "-X github.com/infrena/infrena-backend-s3/internal/s3backend.Version=<version>"
//
// and nowhere else. A version committed to a source file is a version somebody
// forgets to change, and scripts/release-check refuses a release where the
// tag, plugin.yaml and this disagree.
//
// The default says "dev" out loud rather than naming a plausible release, so a
// binary somebody built from a checkout cannot be mistaken for one that was
// published.
var Version = "0.0.0-dev"

// Version is the optional method backendsdk's handshake reads. Without it the
// SDK reports 0.0.0 for every build, which would make `infrena` unable to tell
// two releases of this backend apart in a diagnostic.
func (b *Backend) Version() string { return Version }
