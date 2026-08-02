//go:build !nofederation

package federation

import (
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"daemonlord.ygg/madshare/config"
)

// The transport on its own: two meshes peered over a loopback underlay, one
// serving an ordinary http.Handler on a port of its mesh address, the other
// fetching it. No Node on either side — nothing here has a peer table, a
// catalog, or a protocol listener, which is the whole claim [[listen_mesh]]
// rests on (a server can be reachable from anywhere while federating with
// nobody).
//
// Port 80 is used deliberately: it is privileged on a kernel socket and free
// here, and the test runs unprivileged.
func TestMeshServesWithoutFederation(t *testing.T) {
	logger := log.New(io.Discard, "", 0)
	dir := t.TempDir()

	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	underlay := fmt.Sprintf("tcp://%s", probe.Addr())
	probe.Close()

	server, err := StartTransport(config.YggdrasilConfig{
		KeyFile: filepath.Join(dir, "server.key"),
		Listen:  []string{underlay},
	}, logger)
	if err != nil {
		t.Fatalf("start server mesh: %v", err)
	}
	defer server.Stop()

	visitor, err := StartTransport(config.YggdrasilConfig{
		KeyFile: filepath.Join(dir, "visitor.key"),
		Peers:   []string{underlay},
	}, logger)
	if err != nil {
		t.Fatalf("start visitor mesh: %v", err)
	}
	defer visitor.Stop()

	lis, err := server.ListenMesh(80)
	if err != nil {
		t.Fatalf("ListenMesh(80): %v", err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "library from %s", r.URL.Path)
	})}
	go srv.Serve(lis)
	defer srv.Close()

	client := &http.Client{
		Transport: &http.Transport{DialContext: visitor.DialContext},
		Timeout:   meshClientTimeout,
	}
	// No port in the URL: the point of defaulting to 80 is that the address is
	// the whole address.
	url := fmt.Sprintf("http://[%s]/library", server.Address())

	deadline := time.Now().Add(meshDeadline)
	for {
		resp, err := client.Get(url)
		if err != nil {
			if time.Now().After(deadline) {
				t.Fatalf("mesh listener never answered: %v", err)
			}
			time.Sleep(250 * time.Millisecond)
			continue
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if got, want := string(body), "library from /library"; got != want {
			t.Fatalf("body = %q, want %q", got, want)
		}
		return
	}
}

// A Node adopts the mesh it is given, so one process serves the madnetwork
// protocol on 1314 and the web UI on 80 from a single address and a single
// identity — and stopping the node stops the transport with it.
func TestNodeAdoptsMeshAndSharesTheAddress(t *testing.T) {
	logger := log.New(io.Discard, "", 0)
	dir := t.TempDir()

	mesh, err := StartTransport(config.YggdrasilConfig{
		KeyFile: filepath.Join(dir, "node.key"),
	}, logger)
	if err != nil {
		t.Fatalf("start mesh: %v", err)
	}
	addr := mesh.Address().String()

	n, err := Start(config.FederationConfig{Name: "adopter"}, nil, logger, WithMesh(mesh))
	if err != nil {
		mesh.Stop()
		t.Fatalf("start node on mesh: %v", err)
	}
	if got := n.Address().String(); got != addr {
		t.Errorf("node address = %s, want the mesh's own %s", got, addr)
	}
	if n.Mesh() != mesh {
		t.Error("Node.Mesh() did not return the adopted transport")
	}
	// Serving the UI beside the protocol must not need a second identity or a
	// second stack.
	lis, err := n.Mesh().ListenMesh(80)
	if err != nil {
		n.Stop()
		t.Fatalf("ListenMesh beside the protocol listener: %v", err)
	}
	lis.Close()

	n.Stop() // adopted: this stops the mesh too
	if _, err := mesh.ListenMesh(8081); err == nil {
		t.Error("mesh still accepts listeners after Node.Stop; the transport was not stopped")
	}
}

// The two facts an operator hands out — the address and the public key — land
// beside the key file as plain text, so a script never has to parse a log line
// or derive an address from an ed25519 key.
func TestMeshWritesIdentityFiles(t *testing.T) {
	dir := t.TempDir()
	key := filepath.Join(dir, "federation.key")
	m, err := StartTransport(config.YggdrasilConfig{KeyFile: key}, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatalf("StartTransport: %v", err)
	}
	defer m.Stop()

	for path, want := range map[string]string{
		key + ".pub":                          m.PublicKeyHex() + "\n",
		filepath.Join(dir, "federation.addr"): m.Address().String() + "\n",
	} {
		got, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("read %s: %v", filepath.Base(path), err)
			continue
		}
		if string(got) != want {
			t.Errorf("%s = %q, want %q", filepath.Base(path), got, want)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		// A public key and an address are things you publish; only the key
		// itself is 0600.
		if perm := info.Mode().Perm(); perm != 0o644 {
			t.Errorf("%s mode = %o, want 644", filepath.Base(path), perm)
		}
	}
}

// They are derived outputs, so a stale one is corrected rather than trusted: an
// .addr left over from a replaced key is a wrong address that looks
// authoritative, which is worse than no file at all.
func TestMeshRewritesStaleIdentityFiles(t *testing.T) {
	dir := t.TempDir()
	key := filepath.Join(dir, "federation.key")
	addrFile := filepath.Join(dir, "federation.addr")
	if err := os.WriteFile(key+".pub", []byte("deadbeef\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(addrFile, []byte("200:0000::1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := StartTransport(config.YggdrasilConfig{KeyFile: key}, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatalf("StartTransport: %v", err)
	}
	defer m.Stop()

	if got, _ := os.ReadFile(addrFile); string(got) != m.Address().String()+"\n" {
		t.Errorf("federation.addr = %q, want the running address %s", got, m.Address())
	}
	if got, _ := os.ReadFile(key + ".pub"); string(got) != m.PublicKeyHex()+"\n" {
		t.Errorf("federation.key.pub = %q, want the running key", got)
	}
}

// A read-only data dir is a legitimate deployment. A node that cannot write a
// convenience file is still a perfectly good node.
func TestMeshStartsWhenIdentityFilesCannotBeWritten(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: a read-only directory would not stop the write")
	}
	dir := t.TempDir()
	key := filepath.Join(dir, "federation.key")
	// Create the key first, then freeze the directory — loadOrCreateKey has to
	// succeed for the identity-file write to be the thing under test.
	m, err := StartTransport(config.YggdrasilConfig{KeyFile: key}, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatalf("first start: %v", err)
	}
	m.Stop()
	for _, f := range []string{key + ".pub", filepath.Join(dir, "federation.addr")} {
		if err := os.Remove(f); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })

	m2, err := StartTransport(config.YggdrasilConfig{KeyFile: key}, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatalf("start with an unwritable data dir: %v", err)
	}
	defer m2.Stop()
	if m2.Address() == nil {
		t.Error("node has no address")
	}
}

// Start without WithMesh keeps building its own transport from the [federation]
// keys, which is what every existing caller (and every test above) relies on.
func TestStartBuildsItsOwnMeshWhenNoneGiven(t *testing.T) {
	n, err := Start(config.FederationConfig{
		KeyFile: filepath.Join(t.TempDir(), "solo.key"),
	}, nil, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer n.Stop()
	if n.Mesh() == nil {
		t.Fatal("Node has no mesh")
	}
	if n.Address() == nil {
		t.Error("node has no mesh address")
	}
}
