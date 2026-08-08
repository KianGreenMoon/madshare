package app_test

import (
	"bytes"
	"context"
	"errors"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"daemonlord.ygg/madshare/app"
	"daemonlord.ygg/madshare/auth"
	"daemonlord.ygg/madshare/config"
)

// embeddedConfig is the shape an embedder builds by hand: a data dir, no
// listener, no config file, and a provisioning credential nobody keeps.
func embeddedConfig(t *testing.T, dataDir string) (config.Config, string) {
	t.Helper()
	secret, err := app.GenerateSecret()
	if err != nil {
		t.Fatalf("GenerateSecret: %v", err)
	}
	cfg := config.Default()
	cfg.DataDir = dataDir
	cfg.Listen = nil
	cfg.Sources.AllowAny = true
	cfg.Auth.InitialAdminUser = "owner"
	cfg.Auth.InitialAdminPassword = secret
	cfg, err = cfg.Prepare()
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	return cfg, secret
}

func testLogger() (*log.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return log.New(&buf, "", 0), &buf
}

// The embedder's whole path: Start with no listeners brings a usable library up,
// provisions the owner, and releases everything on Stop.
func TestStart_NoListeners(t *testing.T) {
	dir := t.TempDir()
	cfg, _ := embeddedConfig(t, dir)
	lg, out := testLogger()

	inst, err := app.Start(context.Background(), cfg, app.WithLogger(lg))
	if err != nil {
		t.Fatalf("Start: %v\nlog:\n%s", err, out)
	}

	if !strings.Contains(out.String(), `created initial admin user "owner"`) {
		t.Errorf("expected the owner to be provisioned, log was:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(dir, "madshare.db")); err != nil {
		t.Errorf("database not created at the derived path: %v", err)
	}

	// A fresh library is empty, not broken: every browse call answers.
	ctx := context.Background()
	lib := inst.Library()
	arts, err := lib.Artists(ctx)
	if err != nil {
		t.Fatalf("Artists: %v", err)
	}
	if len(arts) != 0 {
		t.Errorf("Artists on a fresh library = %d rows, want 0", len(arts))
	}
	if _, _, err := lib.ArtistsPage(ctx, "", 20); err != nil {
		t.Errorf("ArtistsPage: %v", err)
	}
	if res, err := lib.Search(ctx, "anything"); err != nil {
		t.Errorf("Search: %v", err)
	} else if res == nil {
		t.Error("Search returned nil results")
	}
	if _, ok := lib.BlobPath("0000000000000000000000000000000000000000000000000000000000000000/x.mp3"); ok {
		t.Error("BlobPath resolved a hash nothing holds")
	}

	// The in-place import manager is available and unrestricted, which is what
	// [sources].allow_any bought.
	if s := inst.Sources(); s == nil || !s.Enabled() {
		t.Error("Sources() should be enabled under allow_any")
	}

	// Provisioning is only half of what an embedder needs: it has to be able to
	// act as the identity it created.
	id, ok, err := inst.UserID(ctx, "owner")
	if err != nil || !ok || id <= 0 {
		t.Errorf("UserID(owner) = (%d, %v, %v), want a real id", id, ok, err)
	}
	if _, ok, err := inst.UserID(ctx, "nobody"); err != nil || ok {
		t.Errorf("UserID(nobody) = (_, %v, %v), want (false, nil)", ok, err)
	}

	inst.Stop(context.Background())

	// Stop closed the store: a call that worked a moment ago must now fail rather
	// than read through a half-shut node.
	if _, err := inst.Library().Artists(context.Background()); err == nil {
		t.Error("Artists after Stop should fail — the database must be closed")
	}
	// And it is safe to say so twice.
	inst.Stop(context.Background())
}

// The startup passes are idempotent — they always had to be, and an embedder
// restarts far more often than a server does.
func TestStart_TwiceOnTheSameDataDir(t *testing.T) {
	dir := t.TempDir()
	cfg, _ := embeddedConfig(t, dir)

	first, err := app.Start(context.Background(), cfg)
	if err != nil {
		t.Fatalf("first Start: %v", err)
	}
	first.Stop(context.Background())

	lg, out := testLogger()
	second, err := app.Start(context.Background(), cfg, app.WithLogger(lg))
	if err != nil {
		t.Fatalf("second Start: %v\nlog:\n%s", err, out)
	}
	defer second.Stop(context.Background())

	if strings.Contains(out.String(), "created initial admin user") {
		t.Errorf("the second start must not provision an admin, log was:\n%s", out)
	}
	if !strings.Contains(out.String(), "users already exist") {
		t.Errorf("expected the unused-credential warning, log was:\n%s", out)
	}
}

// A failing Start leaves nothing running. The proof is indirect and better than
// counting goroutines: a leaked *database.DB would still hold the SQLite file, so
// the retry that succeeds is the assertion.
func TestStart_FailureReleasesEverything(t *testing.T) {
	dir := t.TempDir()
	cfg, secret := embeddedConfig(t, dir)
	cfg.Auth.InitialAdminPassword = "" // madshare refuses a fresh DB with no admin

	inst, err := app.Start(context.Background(), cfg)
	if err == nil {
		inst.Stop(context.Background())
		t.Fatal("Start on a fresh database with no admin credential should fail")
	}
	if !errors.Is(err, auth.ErrNoAdminCredential) {
		t.Errorf("Start error = %v, want ErrNoAdminCredential in the chain", err)
	}
	if inst != nil {
		t.Error("a failed Start must return a nil instance")
	}

	cfg.Auth.InitialAdminPassword = secret
	retry, err := app.Start(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Start after a failed one: %v (a leaked database handle would show up here)", err)
	}
	retry.Stop(context.Background())
}

// Serve is the reachable half, and asking it to bind nothing is a wiring mistake
// rather than a silent no-op.
func TestServe_NoListeners(t *testing.T) {
	cfg, _ := embeddedConfig(t, t.TempDir())
	inst, err := app.Start(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer inst.Stop(context.Background())

	if err := inst.Serve(); !errors.Is(err, app.ErrNoListeners) {
		t.Errorf("Serve with no listeners = %v, want ErrNoListeners", err)
	}
	if inst.Err() != nil {
		t.Error("Err() should stay nil when Serve bound nothing")
	}
}

// The server's own path, end to end: a [[listen]] entry serving the api group
// answers /healthz, and Stop takes it down.
func TestServe_HealthzAndShutdown(t *testing.T) {
	cfg, _ := embeddedConfig(t, t.TempDir())
	port := freePort(t)
	cfg.Listen = []config.ListenConfig{{
		Addr:  "127.0.0.1",
		Port:  port,
		Serve: []string{config.GroupAPI},
	}}
	cfg, err := cfg.Prepare()
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	inst, err := app.Start(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := inst.Serve(); err != nil {
		inst.Stop(context.Background())
		t.Fatalf("Serve: %v", err)
	}

	url := "http://127.0.0.1:" + strconv.Itoa(port) + "/healthz"
	res, err := http.Get(url)
	if err != nil {
		inst.Stop(context.Background())
		t.Fatalf("GET /healthz: %v", err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Errorf("GET /healthz = %d, want 200", res.StatusCode)
	}

	inst.Stop(context.Background())
	if _, err := http.Get(url); err == nil {
		t.Error("the listener should be closed after Stop")
	}
}

// A config that binds a port already in use fails at Serve, not at Start: the
// node is up by then, which is exactly why Serve returns instead of exiting.
func TestServe_BindConflict(t *testing.T) {
	cfg, _ := embeddedConfig(t, t.TempDir())
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	port := held.Addr().(*net.TCPAddr).Port
	cfg.Listen = []config.ListenConfig{{Addr: "127.0.0.1", Port: port, Serve: []string{config.GroupAPI}}}
	if cfg, err = cfg.Prepare(); err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	inst, err := app.Start(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer inst.Stop(context.Background())
	if err := inst.Serve(); err == nil {
		t.Error("Serve on a taken port should fail")
	}
}

// freePort returns a port nothing is listening on. Racy in principle, standard in
// practice, and the alternative (port 0) is refused by config validation.
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	// Give the kernel a moment to release it before the caller re-binds.
	time.Sleep(10 * time.Millisecond)
	return port
}
