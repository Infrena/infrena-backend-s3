package s3backend

import (
	"os"
	"testing"

	"github.com/infrena/infrena/pkg/backendproto"
	"github.com/infrena/infrena/pkg/pluginmanifest"
)

// readManifest parses the repository's plugin.yaml with INFRENA'S OWN PARSER,
// which is the same code that reads the file when somebody judges whether this
// plugin is compatible with their host. A manifest that passes here is
// one infrena accepts, rather than one a second parser in this repository
// happened to agree with.
func readManifest(t *testing.T) *pluginmanifest.Manifest {
	t.Helper()
	data, err := os.ReadFile("../../plugin.yaml")
	if err != nil {
		t.Fatal(err)
	}
	m, warnings, err := pluginmanifest.Parse(data)
	if err != nil {
		t.Fatalf("plugin.yaml is not a manifest infrena accepts: %v", err)
	}
	if len(warnings) != 0 {
		t.Errorf("plugin.yaml parses with warnings, which a user would be shown: %v", warnings)
	}
	return m
}

// TestTheManifestDescribesThisPlugin.
//
// The protocol is compared EXACTLY rather than by containment: the manifest defines
// `protocol:` as the versions this release's binary speaks, and an SDK-built
// binary announces exactly one. A manifest listing the host's whole Supported
// set would claim a protocol this binary cannot speak.
//
// And it is backendproto's number, not pluginproto's. The two are deliberately
// separate: this plugin speaks about environments, bytes and
// locks, and nothing said about resource schemas is a statement about it.
//
// The version is not compared here. The code reports 0.0.0-dev until a release
// stamps it, and scripts/release-check is what asserts tag == manifest ==
// binary.
func TestTheManifestDescribesThisPlugin(t *testing.T) {
	m := readManifest(t)
	if m.Name != PluginName {
		t.Errorf("plugin.yaml names %q, but this plugin is %q", m.Name, PluginName)
	}
	if len(m.Protocol) != 1 || m.Protocol[0] != backendproto.Version {
		t.Errorf("plugin.yaml's protocol is %v, but the SDK this plugin is built with speaks exactly [%d]; "+
			"change it in the same commit as go.mod's infrena require",
			m.Protocol, backendproto.Version)
	}
}

// TestTheManifestFloorIsTheOldestHostThatCanRunThisBinary.
//
// THE FLOOR IS A RUNTIME QUESTION AND go.mod's REQUIRE IS A BUILD-TIME ONE,
// and they are allowed to differ. That principle is unchanged and still the
// reason to think before raising this: go.mod requires more than the binary
// does, because live_test.go imports pkg/backendtest while the shipped binary
// links only pkg/backend, pkg/backendproto and pkg/backendsdk. Raising the
// floor to match go.mod would refuse a host this binary demonstrably works
// with, on the strength of a package only ever linked into a test.
//
// THE FLOOR IS 0.13.0 SINCE 2026-09-18, and both steps were forced. This release
// speaks backend protocol 2, and 0.12.0 was the first engine that accepted it:
// 0.8.0 through 0.11.1 support protocol 1 only and refuse this binary at the
// handshake. `Supported` being a set protects an OLD plugin on a NEW host,
// which is the direction that matters day to day; nothing protects a new
// plugin on an old host.
//
// Then every release before 0.13.0 was DELETED as stale, tags included, so the
// floor moved again for a reason that has nothing to do with capability: a
// floor may only name a release that exists.
//
// The versions below are therefore about the PROTOCOL now, not about when
// backends were introduced. 0.8.0 is still where backends began — there is no
// pkg/backend before it — but it is no longer where this binary stops.
func TestTheManifestFloorIsTheOldestHostThatCanRunThisBinary(t *testing.T) {
	m := readManifest(t)
	if m.Infrena.IsZero() {
		t.Fatal("plugin.yaml has no infrena: floor, so it claims to run against every release ever published, including every one from before backends existed")
	}
	for version, want := range map[string]bool{
		// Before backends existed at all.
		"0.4.0": false, "0.6.2": false, "0.7.0": false, "0.7.1": false,
		// Backends exist, but speak protocol 1 only and refuse this binary.
		"0.8.0": false, "0.9.0": false, "0.11.1": false,
		// Deleted as stale, so below the floor.
		"0.12.0": false, "0.12.9": false, "0.13.0": false, "0.13.9": false,
		// The oldest release that still exists, and everything after it.
		"0.14.0": true, "0.14.9": true, "1.0.0": true,
	} {
		if got := m.AllowsInfrena(version); got != want {
			t.Errorf("plugin.yaml's infrena: %q allows %s = %v, want %v", m.Infrena, version, got, want)
		}
	}
}

// TestTheManifestShipsEveryPlatformInfrenaDoes. Eight, matching infrena's own
// release matrix: a host that runs somewhere this plugin has no build for can
// use every backend except this one, which reads as infrena being broken on
// that platform rather than as a plugin that was not published for it.
func TestTheManifestShipsEveryPlatformInfrenaDoes(t *testing.T) {
	m := readManifest(t)
	for _, want := range []string{
		"linux/amd64", "linux/arm64", "linux/arm", "linux/386",
		"darwin/amd64", "darwin/arm64", "windows/amd64", "windows/arm64",
	} {
		p, err := pluginmanifest.ParsePlatform(want)
		if err != nil {
			t.Fatal(err)
		}
		if !m.Supports(p) {
			t.Errorf("plugin.yaml publishes no build for %s", want)
		}
	}
	if len(m.Platforms) != 8 {
		t.Errorf("plugin.yaml lists %d platforms, want infrena's eight: %v", len(m.Platforms), m.Platforms)
	}
}
