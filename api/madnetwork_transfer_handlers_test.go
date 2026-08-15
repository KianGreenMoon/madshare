package api

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"daemonlord.ygg/madshare/auth"
	"daemonlord.ygg/madshare/federation"
)

// fakeTransfer implements federation.Transfer over a real temp file whose
// content may still be growing (the cache-through case).
type fakeTransfer struct {
	hash, name, path string

	mu       sync.Mutex
	size     int64
	progress int64
	err      error
	done     chan struct{}
}

func newFakeTransfer(t *testing.T, hash, name string, size int64) *fakeTransfer {
	t.Helper()
	path := filepath.Join(t.TempDir(), "blob.part")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	return &fakeTransfer{hash: hash, name: name, path: path, size: size, done: make(chan struct{})}
}

func (f *fakeTransfer) append(t *testing.T, b []byte) {
	t.Helper()
	fh, err := os.OpenFile(f.path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fh.Write(b); err != nil {
		t.Fatal(err)
	}
	fh.Close()
	f.mu.Lock()
	f.progress += int64(len(b))
	f.mu.Unlock()
}

func (f *fakeTransfer) finish() { close(f.done) }

func (f *fakeTransfer) Hash() string     { return f.hash }
func (f *fakeTransfer) Filename() string { return f.name }
func (f *fakeTransfer) Size() int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.size
}
func (f *fakeTransfer) Progress() int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.progress
}
func (f *fakeTransfer) Available(offset int64) int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.progress > offset {
		return f.progress - offset
	}
	return 0
}
func (f *fakeTransfer) Done() <-chan struct{}   { return f.done }
func (f *fakeTransfer) Abandon()                {}
func (f *fakeTransfer) Err() error              { return f.err }
func (f *fakeTransfer) Open() (*os.File, error) { return os.Open(f.path) }
func (f *fakeTransfer) Stats() federation.TransferStats {
	return federation.TransferStats{Hash: f.hash, Size: f.Size(), Progress: f.Progress()}
}
func (f *fakeTransfer) WaitFor(ctx context.Context, offset int64) error {
	for {
		if f.Progress() > offset {
			return nil
		}
		select {
		case <-f.done:
			if f.err != nil {
				return f.err
			}
			if f.Progress() > offset {
				return nil
			}
			return io.EOF
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// fakeBlobFederation wraps fakeFederation with a configurable transfer.
type fakeBlobFederation struct {
	fakeFederation
	blob federation.Transfer
}

func (f *fakeBlobFederation) EnsureBlob(context.Context, string) (federation.Transfer, error) {
	if f.blob == nil {
		return nil, federation.ErrNoHolder
	}
	return f.blob, nil
}

const streamTestHash = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func streamServer(t *testing.T, tr federation.Transfer) *httptest.Server {
	t.Helper()
	r := chi.NewRouter()
	RegisterAPI(r, Deps{
		Madnetwork: &fakeMadnetwork{},
		Federation: &fakeBlobFederation{blob: tr},
	})
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv
}

// TestMadnetworkStream_Growing relays a transfer that is still downloading:
// the response must carry the full length up front and deliver every byte,
// including the part that arrives only after the request started.
func TestMadnetworkStream_Growing(t *testing.T) {
	content := []byte("0123456789abcdefghij")
	tr := newFakeTransfer(t, streamTestHash, "song.mp3", int64(len(content)))
	tr.append(t, content[:5]) // a prefix is already on disk
	go func() {
		time.Sleep(50 * time.Millisecond)
		tr.append(t, content[5:])
		tr.finish()
	}()
	srv := streamServer(t, tr)

	resp, err := http.Get(srv.URL + "/api/madnetwork/stream/" + streamTestHash)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Length"); got != "20" {
		t.Errorf("Content-Length = %q, want 20 (known up front)", got)
	}
	if got := resp.Header.Get("Content-Type"); got != "audio/mpeg" {
		t.Errorf("Content-Type = %q, want audio/mpeg (from the origin filename)", got)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != string(content) {
		t.Errorf("body = %q, want the full content", body)
	}
}

// TestMadnetworkStream_Range: a mid-file range against a growing transfer
// waits for the bytes and answers 206 with the exact slice.
func TestMadnetworkStream_Range(t *testing.T) {
	content := []byte("0123456789abcdefghij")
	tr := newFakeTransfer(t, streamTestHash, "song.mp3", int64(len(content)))
	go func() {
		time.Sleep(30 * time.Millisecond)
		tr.append(t, content)
		tr.finish()
	}()
	srv := streamServer(t, tr)

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/madnetwork/stream/"+streamTestHash, nil)
	req.Header.Set("Range", "bytes=10-14")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("status = %d, want 206", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Range"); got != "bytes 10-14/20" {
		t.Errorf("Content-Range = %q", got)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "abcde" {
		t.Errorf("range body = %q, want abcde", body)
	}
}

// TestMadnetworkStream_NoHolder: an unadvertised hash is 404.
func TestMadnetworkStream_NoHolder(t *testing.T) {
	srv := streamServer(t, nil)
	resp, err := http.Get(srv.URL + "/api/madnetwork/stream/" + streamTestHash)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

// TestMadnetworkDownload_NotAdvertised: downloading a hash no friend offers
// is refused before any transfer starts.
func TestMadnetworkDownload_NotAdvertised(t *testing.T) {
	srv := streamServer(t, nil)
	resp, err := http.Post(srv.URL+"/api/madnetwork/download", "application/json",
		strings.NewReader(`{"hash":"`+streamTestHash+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

// TestMadnetworkTransferStatus_None: polling an unknown hash reports "none".
func TestMadnetworkTransferStatus_None(t *testing.T) {
	srv := streamServer(t, nil)
	resp, err := http.Get(srv.URL + "/api/madnetwork/transfers/" + streamTestHash)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}

func TestParseByteRange(t *testing.T) {
	cases := []struct {
		spec        string
		start, end  int64
		partial, ok bool
	}{
		{"", 0, 19, false, true},
		{"bytes=0-0", 0, 0, true, true},
		{"bytes=5-", 5, 19, true, true},
		{"bytes=5-9", 5, 9, true, true},
		{"bytes=5-99", 5, 19, true, true},     // end clamped
		{"bytes=-4", 16, 19, true, true},      // suffix
		{"bytes=20-", 0, 0, false, false},     // start beyond EOF → 416
		{"bytes=9-5", 0, 0, false, false},     // inverted → 416
		{"bytes=1-2,4-5", 0, 19, false, true}, // multi-range → full entity
		{"items=1-2", 0, 19, false, true},     // foreign unit → full entity
	}
	for _, c := range cases {
		start, end, partial, ok := parseByteRange(c.spec, 20)
		if start != c.start || end != c.end || partial != c.partial || ok != c.ok {
			t.Errorf("parseByteRange(%q) = (%d,%d,%v,%v), want (%d,%d,%v,%v)",
				c.spec, start, end, partial, ok, c.start, c.end, c.partial, c.ok)
		}
	}
}

// TestDownloadEvictsTheCachedDuplicate is the write-path half of the fix for the
// scope leak the F8 mesh verification found (.issues/open-issues.md, "Cache
// seeding overrides a recording's sharing scope").
//
// The case exercised is the cheap one — bytes this node already holds — because
// it reaches the eviction without a mesh: whatever brought the blob into the
// library, a cache copy of it is a duplicate served under a rule that ignores
// the recording's sharing scope, so it must not survive the request.
func TestDownloadEvictsTheCachedDuplicate(t *testing.T) {
	fed := &fakeFederation{}
	srv, db := newModerationServerWithNetwork(t, fed)
	admin := clientFor(t, srv.URL, "admin", testAdminPassword)
	makeUser(t, db, "up", "uploader-pass-1", auth.RoleUploader)
	up := clientFor(t, srv.URL, "up", "uploader-pass-1")

	hash, _ := uploadStaged(t, up, srv.URL, "held.mp3")

	ctx := context.Background()
	if _, err := db.InsertFederationPeer(ctx, &federation.ExternalNode{
		PublicKey: "aa11", Label: "friendly", TrustState: federation.PeerFriend, TrustedAt: 1000,
	}); err != nil {
		t.Fatalf("insert peer: %v", err)
	}
	src, err := db.EnsureCatalogSource(ctx, "aa11", 1000)
	if err != nil {
		t.Fatalf("ensure source: %v", err)
	}
	if err := db.ReplaceSourceCatalog(ctx, src.ID, "s1", 100, []federation.CatalogEntry{{
		Key: "e1", RecordingKey: "r1", Title: "Held", Artist: "Band",
		Renditions: []federation.CatalogRendition{{Hash: hash, Size: 1000, Codec: "mp3"}},
	}}); err != nil {
		t.Fatalf("ReplaceSourceCatalog: %v", err)
	}

	var body struct {
		Existed bool `json:"existed"`
	}
	if code := doJSON(t, admin, http.MethodPost, srv.URL+"/api/madnetwork/download",
		map[string]any{"hash": hash}, &body); code != http.StatusOK {
		t.Fatalf("download = %d, want 200 (bytes already held)", code)
	}
	if !body.Existed {
		t.Fatal("handler did not take the already-held path")
	}
	if len(fed.evicted) != 1 || fed.evicted[0] != hash {
		t.Errorf("evicted = %v, want exactly [%s] — a blob in the library must not stay in the cache",
			fed.evicted, hash)
	}
}
