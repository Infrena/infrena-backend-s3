package main_test

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/infrena/infrena/pkg/backendproto"
)

// The plugin must answer a handshake before it can do anything else. If this
// passes, the SDK wiring is right and every later task is about behaviour
// rather than plumbing.
//
// infrena's pkg/plugintest is NOT usable here: its Open takes a
// provider.Plugin and drives pkg/pluginsdk over an in-memory pipe, and there
// is no backend equivalent of it. Rather than invent half a harness in this
// repository, the test starts the real binary the way the host does — cookie
// in the environment, protocol on stdio — and reads the first line. That is a
// stricter test than an in-process one anyway: it proves the built binary
// serves, not just that the package compiles.
func TestThePluginServesAndAnswersAHandshake(t *testing.T) {
	bin := buildBinary(t)

	cmd := exec.Command(bin)
	cmd.Env = append(os.Environ(), backendproto.CookieEnv+"=test-cookie")
	cmd.Stderr = os.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = stdin.Close()
		_ = cmd.Wait()
	})

	var h backendproto.Handshake
	if err := readLine(t, stdout, &h); err != nil {
		t.Fatalf("reading the handshake: %v", err)
	}
	if !backendproto.IsSupported(h.Protocol) {
		t.Errorf("handshake protocol = %d, which this build of infrena cannot speak", h.Protocol)
	}
	if h.Name != "s3" {
		t.Errorf("handshake name = %q, want %q", h.Name, "s3")
	}

	// And it answers a request, so the loop is running rather than the
	// handshake being the only thing main() ever writes.
	req, err := json.Marshal(backendproto.Request{ID: 1, Method: backendproto.MethodShutdown})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stdin.Write(append(req, '\n')); err != nil {
		t.Fatal(err)
	}
	var resp backendproto.Response
	if err := readLine(t, stdout, &resp); err != nil {
		t.Fatalf("reading the shutdown response: %v", err)
	}
	if resp.ID != 1 {
		t.Errorf("response id = %d, want 1", resp.ID)
	}
	if resp.Error != nil {
		t.Errorf("shutdown failed: %v", resp.Error)
	}
}

// A binary run by hand is not a plugin: without the host's cookie it must say
// what it is and exit non-zero rather than sitting on a terminal waiting for
// protocol input, which is indistinguishable from a hang.
func TestRunningTheBinaryByHandExplainsItself(t *testing.T) {
	cmd := exec.Command(buildBinary(t))
	cmd.Env = append(os.Environ(), backendproto.CookieEnv+"=")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("the binary ran happily with no cookie")
	}
	if len(out) == 0 {
		t.Error("it exited without saying why")
	}
}

func buildBinary(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "infrena-backend-s3")
	build := exec.Command("go", "build", "-o", bin, "github.com/infrena/infrena-backend-s3/cmd/infrena-backend-s3")
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		t.Fatalf("go build: %v", err)
	}
	return bin
}

func readLine(t *testing.T, r io.Reader, into any) error {
	t.Helper()
	type result struct {
		line []byte
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
		if !sc.Scan() {
			ch <- result{err: io.EOF}
			return
		}
		ch <- result{line: sc.Bytes()}
	}()
	select {
	case got := <-ch:
		if got.err != nil {
			return got.err
		}
		return json.Unmarshal(got.line, into)
	case <-time.After(30 * time.Second):
		t.Fatal("the plugin wrote nothing within 30s")
		return nil
	}
}
