package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"daemonlord.ygg/madshare/api/storage"
	"daemonlord.ygg/madshare/database"
	"daemonlord.ygg/madshare/federation"
)

// cacheServer mounts the admin group over a cache directory with real files in
// it, so removal is exercised against a filesystem rather than a mock.
func cacheServer(t *testing.T, repo *fakeRepo, fed FederationNode, mn MadnetworkStore) (*httptest.Server, string) {
	t.Helper()
	cacheDir := t.TempDir()
	r := chi.NewRouter()
	RegisterAdmin(r, Deps{
		Repo: repo, Federation: fed, Madnetwork: mn, MadnetworkCacheDir: cacheDir,
	})
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv, cacheDir
}

// seedCached writes a cache file and its index row together, as a completed
// fetch leaves them.
func seedCached(t *testing.T, repo *fakeRepo, cacheDir, hash, title string, body []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(cacheDir, hash), body, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := repo.PutMadnetworkCacheEntry(context.Background(), &database.MadnetworkCacheEntry{
		Hash: hash, ByteSize: int64(len(body)), Filename: title + ".mp3",
		Title: title, FetchedAt: 100, LastUsedAt: 100,
	}); err != nil {
		t.Fatal(err)
	}
}

// id3v2Blob is a minimal MP3: an ID3v2 header (which is what a tag reader keys
// the CONTAINER off) carrying a title and artist.
func id3v2Blob(title, artist string) []byte {
	frame := func(id, text string) []byte {
		body := append([]byte{0}, text...) // ISO-8859-1 encoding byte
		size := len(body)
		return append(append([]byte(id),
			byte(size>>24), byte(size>>16), byte(size>>8), byte(size), 0, 0), body...)
	}
	frames := append(frame("TIT2", title), frame("TPE1", artist)...)
	n := len(frames)
	head := append([]byte("ID3\x03\x00\x00"),
		byte((n>>21)&0x7f), byte((n>>14)&0x7f), byte((n>>7)&0x7f), byte(n&0x7f))
	return append(append(head, frames...), make([]byte, 256)...)
}

func cacheGetJSON(t *testing.T, url string) map[string]any {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200", url, resp.StatusCode)
	}
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

func postJSONTo(t *testing.T, url, body string) (int, map[string]any) {
	t.Helper()
	resp, err := http.Post(url, "application/json", bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// TestCacheList answers in the envelope file-list.js consumes, and reports a
// selectable_total so the shared list can offer "select all N".
func TestCacheList(t *testing.T) {
	repo := &fakeRepo{}
	srv, dir := cacheServer(t, repo, nil, nil)
	seedCached(t, repo, dir, cacheTestHash('1'), "Alpha", []byte("aaaa"))
	seedCached(t, repo, dir, cacheTestHash('2'), "Beta", []byte("bbbbbb"))

	got := cacheGetJSON(t, srv.URL+"/api/admin/cache")
	items, _ := got["items"].([]any)
	if len(items) != 2 {
		t.Fatalf("items = %d, want 2", len(items))
	}
	if got["total"] != float64(2) || got["selectable_total"] != float64(2) {
		t.Errorf("total/selectable_total = %v/%v, want 2/2", got["total"], got["selectable_total"])
	}
	if got["bytes"] != float64(10) {
		t.Errorf("bytes = %v, want 10", got["bytes"])
	}

	// The search box narrows the same set the bulk action would resolve.
	got = cacheGetJSON(t, srv.URL+"/api/admin/cache?q=beta")
	if got["total"] != float64(1) {
		t.Errorf("filtered total = %v, want 1", got["total"])
	}
}

// TestCacheBulkRemove: the file goes, the row goes with it, and clearing the
// whole cache has to be asked for explicitly.
func TestCacheBulkRemove(t *testing.T) {
	repo := &fakeRepo{}
	srv, dir := cacheServer(t, repo, nil, nil)
	keep, drop := cacheTestHash('3'), cacheTestHash('4')
	seedCached(t, repo, dir, keep, "Keep", []byte("keepkeep"))
	seedCached(t, repo, dir, drop, "Drop", []byte("drop"))

	code, res := postJSONTo(t, srv.URL+"/api/admin/cache/bulk",
		`{"action":"remove","hashes":["`+drop+`"]}`)
	if code != http.StatusOK {
		t.Fatalf("remove = %d, want 200", code)
	}
	if res["removed"] != float64(1) || res["bytes"] != float64(4) {
		t.Errorf("removed/bytes = %v/%v, want 1/4", res["removed"], res["bytes"])
	}
	if _, err := os.Stat(filepath.Join(dir, drop)); !os.IsNotExist(err) {
		t.Error("the cache file survived removal")
	}
	repo.mu.Lock()
	_, stillIndexed := repo.cacheIndex[drop]
	_, kept := repo.cacheIndex[keep]
	repo.mu.Unlock()
	if stillIndexed {
		t.Error("the index row outlived its file")
	}
	if !kept {
		t.Error("removal took a blob nobody selected")
	}

	// The guardrail every bulk endpoint here shares.
	if code, _ := postJSONTo(t, srv.URL+"/api/admin/cache/bulk", `{"action":"remove"}`); code != http.StatusBadRequest {
		t.Errorf("empty filter without all:true = %d, want 400", code)
	}
	code, res = postJSONTo(t, srv.URL+"/api/admin/cache/bulk", `{"action":"remove","all":true}`)
	if code != http.StatusOK || res["removed"] != float64(1) {
		t.Errorf("clear-all = %d/%v, want 200/1", code, res["removed"])
	}

	// Removing a hash that is already gone is a success: the caller's job was to
	// make the file and the index agree that it is not there.
	if code, _ := postJSONTo(t, srv.URL+"/api/admin/cache/bulk",
		`{"action":"remove","hashes":["`+drop+`"]}`); code != http.StatusOK {
		t.Errorf("removing an absent hash = %d, want 200", code)
	}
	if code, _ := postJSONTo(t, srv.URL+"/api/admin/cache/bulk", `{"action":"burn"}`); code != http.StatusBadRequest {
		t.Errorf("unknown action = %d, want 400", code)
	}
}

// TestCacheForgetsFilesDeletedOutsideTheServer: an operator's `rm`, a disk
// cleanup job or a restored backup can empty the cache behind the server's
// back. Nothing dangerous follows — seeding reads the directory, so a deleted
// file is never advertised to a peer — but the page must not go on counting
// bytes that are not there, and it must not need a restart to notice.
func TestCacheForgetsFilesDeletedOutsideTheServer(t *testing.T) {
	repo := &fakeRepo{}
	srv, dir := cacheServer(t, repo, nil, nil)
	gone, kept := cacheTestHash('g'), cacheTestHash('h')
	seedCached(t, repo, dir, gone, "Vanished", []byte("aaaa"))
	seedCached(t, repo, dir, kept, "Present", []byte("bbbbbb"))

	if err := os.Remove(filepath.Join(dir, gone)); err != nil {
		t.Fatal(err)
	}

	// The listing shows only what is real, and forgets the rest as it goes.
	got := cacheGetJSON(t, srv.URL+"/api/admin/cache")
	items, _ := got["items"].([]any)
	if len(items) != 1 {
		t.Errorf("items = %d, want 1 — a row whose file is gone describes nothing", len(items))
	}
	repo.mu.Lock()
	_, phantom := repo.cacheIndex[gone]
	_, survivor := repo.cacheIndex[kept]
	repo.mu.Unlock()
	if phantom {
		t.Error("the index still holds a row for a file deleted outside the server")
	}
	if !survivor {
		t.Error("a file that IS there was forgotten")
	}

	// And the figures follow, without a restart or a manual Rescan.
	got = cacheGetJSON(t, srv.URL+"/api/admin/cache")
	if got["total"] != float64(1) || got["bytes"] != float64(6) {
		t.Errorf("total/bytes = %v/%v, want 1/6 (only the file that exists)", got["total"], got["bytes"])
	}
}

// TestCacheAudioForgetsAPhantom: asking for bytes that are not there proves the
// row wrong, so the request that discovers it also cleans it up.
func TestCacheAudioForgetsAPhantom(t *testing.T) {
	repo := &fakeRepo{}
	srv, dir := cacheServer(t, repo, nil, nil)
	hash := cacheTestHash('i')
	seedCached(t, repo, dir, hash, "Gone", []byte("aaaa"))
	if err := os.Remove(filepath.Join(dir, hash)); err != nil {
		t.Fatal(err)
	}

	resp, err := http.Get(srv.URL + "/api/admin/cache/" + hash + "/audio")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
	repo.mu.Lock()
	_, phantom := repo.cacheIndex[hash]
	repo.mu.Unlock()
	if phantom {
		t.Error("the row survived a request that proved its file is gone")
	}
}

// TestCacheReapPartials is the leak this page exists to close: a `.part` left by
// a killed process was permanent dead disk. A partial belonging to a LIVE
// transfer must survive — that is the whole reason the reaper consults the node.
func TestCacheReapPartials(t *testing.T) {
	dead, live := cacheTestHash('5'), cacheTestHash('6')
	fed := &fakeFederation{active: []federation.TransferStats{{Hash: live}}}
	repo := &fakeRepo{}
	srv, dir := cacheServer(t, repo, fed, nil)

	for _, n := range []string{dead + ".part", live + ".part"} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("halfway"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// A finished blob is not a partial and must not be swept by this.
	seedCached(t, repo, dir, cacheTestHash('7'), "Whole", []byte("whole"))

	sum := cacheGetJSON(t, srv.URL+"/api/admin/cache/summary")
	partials, _ := sum["partials"].(map[string]any)
	if partials["count"] != float64(1) {
		t.Errorf("abandoned partials = %v, want 1 (the live one is not abandoned)", partials["count"])
	}
	inFlight, _ := sum["in_flight"].([]any)
	if len(inFlight) != 1 {
		t.Errorf("in_flight = %d, want 1", len(inFlight))
	}

	code, res := postJSONTo(t, srv.URL+"/api/admin/cache/partials/reap", `{}`)
	if code != http.StatusOK || res["removed"] != float64(1) {
		t.Fatalf("reap = %d/%v, want 200/1", code, res["removed"])
	}
	if _, err := os.Stat(filepath.Join(dir, dead+".part")); !os.IsNotExist(err) {
		t.Error("an abandoned partial survived the reap")
	}
	if _, err := os.Stat(filepath.Join(dir, live+".part")); err != nil {
		t.Error("a RUNNING transfer's partial was reaped — that is data loss mid-fetch")
	}
	if _, err := os.Stat(filepath.Join(dir, cacheTestHash('7'))); err != nil {
		t.Error("the reaper took a finished blob")
	}
}

// TestMaterializeNeedsNoClaim: a cached blob nobody advertises any more is
// still ours to keep. Materializing stages into the review bucket exactly like
// an upload, and an upload reads its tags from the file — so the old
// "no friend advertises this content" refusal was protecting nothing.
func TestMaterializeNeedsNoClaim(t *testing.T) {
	repo := &fakeRepo{}
	cacheDir := t.TempDir()
	r := chi.NewRouter()
	// fakeMadnetwork answers nil for MadnetworkEntryForHash: nobody describes
	// these bytes. fakeFederation's EnsureBlob fails, which is fine — the
	// decision under test is made before the fetch, and the staging path itself
	// is the one every other download already exercises.
	RegisterAPI(r, Deps{
		Repo: repo, Madnetwork: &fakeMadnetwork{}, Federation: &fakeFederation{},
		MadnetworkCacheDir: cacheDir,
	})
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)

	held, absent := cacheTestHash('a'), cacheTestHash('b')
	if err := os.WriteFile(filepath.Join(cacheDir, held), []byte("bytes"), 0o644); err != nil {
		t.Fatal(err)
	}

	code, _ := postJSONTo(t, srv.URL+"/api/madnetwork/download", `{"hash":"`+held+`"}`)
	if code != http.StatusAccepted {
		t.Errorf("materialize of an unadvertised CACHED blob = %d, want 202 — the bytes are right here", code)
	}
	code, _ = postJSONTo(t, srv.URL+"/api/madnetwork/download", `{"hash":"`+absent+`"}`)
	if code != http.StatusNotFound {
		t.Errorf("materialize of a hash we neither hold nor can place = %d, want 404", code)
	}
}

// completedCacheTransfer is a finished fetch over a real cache file — what
// EnsureBlob hands back for a blob already on disk, which is the offline case.
type completedCacheTransfer struct{ *fakeTransfer }

func (c *completedCacheTransfer) Stats() federation.TransferStats {
	s := c.fakeTransfer.Stats()
	s.Mode = "local"
	return s
}

// TestMaterializeOfflineStagesFromTheFile drives the STAGING path, not just the
// decision in front of it — which is what a decision-only test missed: with no
// claim, entryToMetadata was still being called on a nil entry and panicking, in
// a goroutine, taking the whole process down.
//
// The scenario is the reason Materialize exists: a device with no connectivity,
// adding a cached file to its library. Nothing advertises anything, and the
// blob was adopted into the index so it has no remembered filename either — so
// everything the staging needs has to come out of the file itself.
func TestMaterializeOfflineStagesFromTheFile(t *testing.T) {
	cacheDir, filesDir := t.TempDir(), t.TempDir()
	hash := cacheTestHash('e')
	// A real MP3 container (ID3v2), so the file can say what it is.
	body := id3v2Blob("Offline Track", "Offline Artist")
	path := filepath.Join(cacheDir, hash)
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
	ft := &fakeTransfer{hash: hash, name: hash, path: path,
		size: int64(len(body)), progress: int64(len(body)), done: make(chan struct{})}
	close(ft.done)

	repo := &fakeRepo{}
	h := &handler{
		repo: repo, cacheDir: cacheDir, storage: storage.NewLocal(filesDir),
		madnetwork: &fakeMadnetwork{}, // MadnetworkEntryForHash → nil: nobody describes it
		federation: &fakeBlobFederation{blob: &completedCacheTransfer{ft}},
	}

	job := &mnJob{state: "transferring"}
	h.runMadnetworkDownload(hash, nil, sql.NullInt64{}, job)

	job.mu.Lock()
	state, errText := job.state, job.errText
	job.mu.Unlock()
	if state != "staged" && state != "approved" {
		t.Fatalf("state = %q (%s), want staged/approved — an offline materialize must not need a claim", state, errText)
	}
	if repo.lastMeta == nil {
		t.Fatal("nothing was staged")
	}
	if repo.lastMeta.Title != "Offline Track" || repo.lastMeta.Artist.String != "Offline Artist" {
		t.Errorf("staged tags = %q/%q, want the FILE's own tags", repo.lastMeta.Title, repo.lastMeta.Artist.String)
	}
	// The name has to be derived from the container, or the blob lands with no
	// extension and the library cannot type it.
	if repo.lastFile == nil || !strings.HasSuffix(repo.lastFile.ObjectKey, ".mp3") {
		t.Errorf("object key = %q, want an .mp3 name derived from the file's own header",
			repo.lastFile.ObjectKey)
	}
	if repo.lastFile.MimeType != "audio/mpeg" {
		t.Errorf("mime = %q, want audio/mpeg", repo.lastFile.MimeType)
	}
}

// TestMaterializeRefusesWhatIsNotAudio: the fallback chain ends somewhere. A
// blob with no claim, no remembered name and no recognisable container is
// refused with a plain message — and, critically, does not panic the process.
func TestMaterializeRefusesWhatIsNotAudio(t *testing.T) {
	cacheDir := t.TempDir()
	hash := cacheTestHash('f')
	path := filepath.Join(cacheDir, hash)
	if err := os.WriteFile(path, []byte("not audio at all"), 0o644); err != nil {
		t.Fatal(err)
	}
	ft := &fakeTransfer{hash: hash, name: hash, path: path, size: 16, progress: 16, done: make(chan struct{})}
	close(ft.done)

	repo := &fakeRepo{}
	h := &handler{
		repo: repo, cacheDir: cacheDir, storage: storage.NewLocal(t.TempDir()),
		madnetwork: &fakeMadnetwork{},
		federation: &fakeBlobFederation{blob: &completedCacheTransfer{ft}},
	}
	job := &mnJob{state: "transferring"}
	h.runMadnetworkDownload(hash, nil, sql.NullInt64{}, job)

	job.mu.Lock()
	state := job.state
	job.mu.Unlock()
	if state != "failed" {
		t.Errorf("state = %q, want failed", state)
	}
	if repo.lastMeta != nil {
		t.Error("something unrecognisable was staged into the library")
	}
}

// TestCachedDownloadName: what a saved cache blob lands under. A finished
// transfer is named after its own path — which for a cache file is the hash —
// so the index is what remembers the origin's name across a restart.
func TestCachedDownloadName(t *testing.T) {
	repo := &fakeRepo{}
	h := &handler{repo: repo}
	ctx := context.Background()
	hash := cacheTestHash('c')

	if got := h.cachedDownloadName(ctx, hash, "live.flac"); got != "live.flac" {
		t.Errorf("with a live transfer name = %q, want live.flac", got)
	}
	if got := h.cachedDownloadName(ctx, hash, hash); got != hash {
		t.Errorf("unindexed = %q, want the hash (honest, if poor)", got)
	}

	repo.PutMadnetworkCacheEntry(ctx, &database.MadnetworkCacheEntry{
		Hash: hash, Filename: "origin.mp3", Title: "Song", Artist: "Band",
	})
	// A finished transfer reports the hash as its name; the index knows better.
	if got := h.cachedDownloadName(ctx, hash, hash); got != "origin.mp3" {
		t.Errorf("indexed = %q, want origin.mp3", got)
	}

	nameless := cacheTestHash('d')
	repo.PutMadnetworkCacheEntry(ctx, &database.MadnetworkCacheEntry{
		Hash: nameless, Title: "Song", Artist: "Band",
	})
	if got := h.cachedDownloadName(ctx, nameless, ""); got != "Band - Song" {
		t.Errorf("nameless = %q, want the tags (a row adopted from an old cache has no filename)", got)
	}
}

// TestCacheClaims: the rare "what does the network call this" view.
func TestCacheClaims(t *testing.T) {
	hash := cacheTestHash('8')
	mn := &fakeMadnetwork{claims: map[string][]*database.MadnetworkCacheClaim{
		hash: {{SourceKey: "ab", SourceName: "node A", Title: "Their Title"}},
	}}
	srv, _ := cacheServer(t, &fakeRepo{}, nil, mn)

	got := cacheGetJSON(t, srv.URL+"/api/admin/cache/"+hash+"/claims")
	claims, _ := got["claims"].([]any)
	if len(claims) != 1 {
		t.Fatalf("claims = %d, want 1", len(claims))
	}
	// A hash nobody advertises is an empty list, not an error: the blob is still
	// perfectly usable, which is the point of not depending on live claims.
	got = cacheGetJSON(t, srv.URL+"/api/admin/cache/"+cacheTestHash('0')+"/claims")
	if claims, _ := got["claims"].([]any); len(claims) != 0 {
		t.Errorf("claims for an unadvertised hash = %d, want 0", len(claims))
	}
}
