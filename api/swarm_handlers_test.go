package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"daemonlord.ygg/madshare/database"
	"daemonlord.ygg/madshare/federation"
)

// The swarm admin API (docs/architecture/swarm-admin.md §API). These run over a
// real database, not fakeRepo: the listing is a union query, which is SQL rather
// than a Repository method, and the point of most of these assertions is what
// that query does.

func newSwarmTestServer(t *testing.T, node FederationNode) (*httptest.Server, *database.DB) {
	t.Helper()
	db, err := database.Open(":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	r := chi.NewRouter()
	RegisterAdmin(r, Deps{Repo: db, Federation: node}) // no auth wired — gates pass through
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv, db
}

func swarmSeedFile(t *testing.T, db *database.DB, hash, title string, size int64) *database.File {
	t.Helper()
	f := &database.File{Hash: hash, ByteSize: size, MimeType: "audio/mpeg",
		ObjectKey: hash + "/x.mp3", ReviewState: database.ReviewApproved}
	err := db.InsertFile(context.Background(), f,
		&database.FileUpload{Filename: title + ".mp3"},
		&database.MediaMetadata{Title: title,
			Artist: sql.NullString{String: "An Artist", Valid: true}})
	if err != nil {
		t.Fatalf("InsertFile: %v", err)
	}
	return f
}

func swarmGET(t *testing.T, url string) map[string]any {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d", url, resp.StatusCode)
	}
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

func swarmPOST(t *testing.T, url, body string) (int, map[string]any) {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func TestSwarmList_EnvelopeAndScopes(t *testing.T) {
	srv, db := newSwarmTestServer(t, nil)
	swarmSeedFile(t, db, "aa11", "Local Song", 100)
	if err := db.PutMadnetworkCacheEntry(context.Background(), &database.MadnetworkCacheEntry{
		Hash: "bb22", ByteSize: 200, Title: "Fetched", FetchedAt: 1, LastUsedAt: 1,
	}); err != nil {
		t.Fatal(err)
	}

	all := swarmGET(t, srv.URL+"/api/admin/swarm")
	items, _ := all["items"].([]any)
	if len(items) != 2 || all["total"].(float64) != 2 {
		t.Fatalf("all scope = %d items / total %v, want 2", len(items), all["total"])
	}
	// The select-all banner reads selectable_total; it must agree with the page's
	// own total or a bulk action would target a different set.
	if all["selectable_total"] != all["total"] {
		t.Errorf("selectable_total %v != total %v", all["selectable_total"], all["total"])
	}
	if all["bytes"].(float64) != 300 {
		t.Errorf("bytes = %v, want 300", all["bytes"])
	}

	lib := swarmGET(t, srv.URL+"/api/admin/swarm?scope=library")
	if got, _ := lib["items"].([]any); len(got) != 1 {
		t.Errorf("library scope = %d items, want 1", len(got))
	}
	// An unknown scope must widen to "all", never to the wrong half.
	bogus := swarmGET(t, srv.URL+"/api/admin/swarm?scope=elsewhere")
	if got, _ := bogus["items"].([]any); len(got) != 2 {
		t.Errorf("unknown scope = %d items, want the full set", len(got))
	}
}

// A row says whether this node would serve it, so the page can answer "why
// isn't this seeding?" rather than silently omitting the blob.
func TestSwarmList_RowsExplainThemselves(t *testing.T) {
	srv, db := newSwarmTestServer(t, nil)
	swarmSeedFile(t, db, "aa11", "Approved", 100)
	draft := &database.File{Hash: "bb22", ByteSize: 50, MimeType: "audio/mpeg",
		ObjectKey: "bb22/x.mp3", ReviewState: database.ReviewDraft}
	if err := db.InsertFile(context.Background(), draft,
		&database.FileUpload{Filename: "d.mp3"}, &database.MediaMetadata{Title: "Draft"}); err != nil {
		t.Fatal(err)
	}

	body := swarmGET(t, srv.URL+"/api/admin/swarm")
	byHash := map[string]map[string]any{}
	for _, raw := range body["items"].([]any) {
		row := raw.(map[string]any)
		byHash[row["hash"].(string)] = row
	}
	if byHash["bb22"]["seedable"] != false {
		t.Errorf("draft row seedable = %v, want false", byHash["bb22"]["seedable"])
	}
	if byHash["bb22"]["review_state"] == "approved" {
		t.Error("draft row should carry its real review state")
	}
	if byHash["aa11"]["seedable"] != true {
		t.Errorf("approved row seedable = %v, want true", byHash["aa11"]["seedable"])
	}
}

// Traffic reaches the row from two places: the stored all-time counters, and the
// running node's session counters, which are added rather than folded in so the
// page does not appear to stall between flushes.
func TestSwarmList_CarriesStoredAndSessionTraffic(t *testing.T) {
	fed := &fakeFederation{traffic: federation.TrafficSnapshot{
		Hashes: map[string]federation.TrafficCounters{"aa11": {Up: 7}},
	}}
	srv, db := newSwarmTestServer(t, fed)
	swarmSeedFile(t, db, "aa11", "Moved", 100)
	if err := db.AddSwarmTraffic(context.Background(),
		[]database.SwarmTrafficDelta{{Hash: "aa11", Up: 900}}, 5000); err != nil {
		t.Fatal(err)
	}

	row := swarmGET(t, srv.URL+"/api/admin/swarm")["items"].([]any)[0].(map[string]any)
	if row["up_bytes"].(float64) != 900 {
		t.Errorf("stored up = %v, want 900", row["up_bytes"])
	}
	sess, ok := row["session"].(map[string]any)
	if !ok || sess["up_bytes"].(float64) != 7 {
		t.Errorf("session = %v, want the un-flushed 7 bytes", row["session"])
	}
}

// A blob being fetched right now carries its transfer, which is what draws the
// progress bar.
func TestSwarmList_LiveTransferRidesTheRow(t *testing.T) {
	fed := &fakeFederation{active: []federation.TransferStats{
		{Hash: "bb22", Size: 1000, Progress: 250, Mode: "swarm", Chunks: 4, ChunksDone: 1},
	}}
	srv, db := newSwarmTestServer(t, fed)
	if err := db.PutMadnetworkCacheEntry(context.Background(), &database.MadnetworkCacheEntry{
		Hash: "bb22", ByteSize: 1000, Title: "Arriving", FetchedAt: 1, LastUsedAt: 1,
	}); err != nil {
		t.Fatal(err)
	}

	row := swarmGET(t, srv.URL+"/api/admin/swarm")["items"].([]any)[0].(map[string]any)
	tr, ok := row["transfer"].(map[string]any)
	if !ok {
		t.Fatalf("row carries no transfer: %v", row)
	}
	if tr["progress"].(float64) != 250 || tr["mode"] != "swarm" {
		t.Errorf("transfer = %v, want the live fetch", tr)
	}
}

func TestSwarmSummary_ReportsTotalsAndFederationState(t *testing.T) {
	fed := &fakeFederation{
		upRate: 2048,
		traffic: federation.TrafficSnapshot{
			TrafficCounters: federation.TrafficCounters{Up: 11, Down: 22},
			Peers:           []federation.PeerTraffic{{Key: "abc", Up: 11}},
		},
	}
	srv, db := newSwarmTestServer(t, fed)
	if err := db.AddSwarmTraffic(context.Background(),
		[]database.SwarmTrafficDelta{{Hash: "aa11", Up: 1000, Down: 500}}, 1); err != nil {
		t.Fatal(err)
	}

	body := swarmGET(t, srv.URL+"/api/admin/swarm/summary")
	allTime := body["all_time"].(map[string]any)
	if allTime["up_bytes"].(float64) != 1000 || allTime["down_bytes"].(float64) != 500 {
		t.Errorf("all_time = %v", allTime)
	}
	session := body["session"].(map[string]any)
	if session["up_bytes"].(float64) != 11 {
		t.Errorf("session = %v", session)
	}
	if peers, _ := body["peers"].([]any); len(peers) != 1 {
		t.Errorf("peers = %v, want the one counterparty", body["peers"])
	}
	if body["federation"] != true {
		t.Error("summary should report a running node")
	}
	limits := body["limits"].(map[string]any)
	if limits["up"].(map[string]any)["effective_kib"].(float64) != 2 {
		t.Errorf("effective up = %v, want 2 KiB/s", limits["up"])
	}
}

// With federation off the page still works: the all-time figures come from the
// database and outlive any node. Only the live half disappears.
func TestSwarmSummary_WorksWithFederationOff(t *testing.T) {
	srv, db := newSwarmTestServer(t, nil)
	if err := db.AddSwarmTraffic(context.Background(),
		[]database.SwarmTrafficDelta{{Hash: "aa11", Up: 42}}, 1); err != nil {
		t.Fatal(err)
	}

	body := swarmGET(t, srv.URL+"/api/admin/swarm/summary")
	if body["federation"] != false {
		t.Error("summary should report no node")
	}
	if body["all_time"].(map[string]any)["up_bytes"].(float64) != 42 {
		t.Errorf("all_time lost with the node: %v", body["all_time"])
	}
	if active, _ := body["active"].([]any); len(active) != 0 {
		t.Errorf("active = %v, want empty", active)
	}
}

// The limits endpoint is three-valued: absent leaves a cap alone, null clears it
// back to the config file, and a number pins it — including 0, which is
// unlimited and a real override.
func TestSwarmLimits_ThreeValued(t *testing.T) {
	srv, db := newSwarmTestServer(t, &fakeFederation{})
	ctx := context.Background()

	if code, _ := swarmPOST(t, srv.URL+"/api/admin/swarm/limits", `{"up_kib":500,"down_kib":100}`); code != 200 {
		t.Fatalf("set = %d", code)
	}
	up, down, _ := db.GetSwarmRates(ctx)
	if up == nil || *up != 500 || down == nil || *down != 100 {
		t.Fatalf("stored = %v/%v, want 500/100", up, down)
	}

	// Absent field: unchanged.
	if code, _ := swarmPOST(t, srv.URL+"/api/admin/swarm/limits", `{"down_kib":250}`); code != 200 {
		t.Fatal("second set failed")
	}
	up, down, _ = db.GetSwarmRates(ctx)
	if up == nil || *up != 500 {
		t.Errorf("up = %v after a request that omitted it, want 500 unchanged", up)
	}
	if down == nil || *down != 250 {
		t.Errorf("down = %v, want 250", down)
	}

	// Explicit null: back to the config file.
	if code, _ := swarmPOST(t, srv.URL+"/api/admin/swarm/limits", `{"up_kib":null}`); code != 200 {
		t.Fatal("clear failed")
	}
	up, _, _ = db.GetSwarmRates(ctx)
	if up != nil {
		t.Errorf("up = %v after null, want no override", up)
	}

	// Explicit zero: an override meaning unlimited, NOT absence.
	if code, _ := swarmPOST(t, srv.URL+"/api/admin/swarm/limits", `{"up_kib":0}`); code != 200 {
		t.Fatal("zero failed")
	}
	up, _, _ = db.GetSwarmRates(ctx)
	if up == nil || *up != 0 {
		t.Errorf("up = %v after 0, want an explicit unlimited override", up)
	}

	if code, _ := swarmPOST(t, srv.URL+"/api/admin/swarm/limits", `{"up_kib":-5}`); code != 400 {
		t.Errorf("negative rate = %d, want 400", code)
	}
}

// The regression this endpoint split exists to prevent: posting rates must not
// touch the seeding policy, whose handler decodes its switches as plain bools
// with hard-coded defaults.
func TestSwarmLimits_DoNotTouchTheSeedingPolicy(t *testing.T) {
	srv, db := newSwarmTestServer(t, &fakeFederation{})
	ctx := context.Background()
	before, err := db.GetMadnetworkPolicy(ctx)
	if err != nil {
		t.Fatal(err)
	}
	before.SeedEnabled, before.AutoapproveDownloads = false, true
	if err := db.SetMadnetworkPolicy(ctx, before); err != nil {
		t.Fatal(err)
	}

	if code, _ := swarmPOST(t, srv.URL+"/api/admin/swarm/limits", `{"up_kib":500}`); code != 200 {
		t.Fatal("set failed")
	}

	after, err := db.GetMadnetworkPolicy(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if after.SeedEnabled != false || after.AutoapproveDownloads != true {
		t.Errorf("policy changed under a rate write: %+v", after)
	}
}

func TestSwarmForget_GuardrailAndDeletion(t *testing.T) {
	srv, db := newSwarmTestServer(t, nil)
	ctx := context.Background()
	// A real content address: the endpoint drops anything that is not one, since
	// a hash is what it addresses blobs by.
	hash := strings.Repeat("a", 64)
	swarmSeedFile(t, db, hash, "One", 10)
	if err := db.AddSwarmTraffic(ctx,
		[]database.SwarmTrafficDelta{{Hash: hash, Up: 100}}, 1); err != nil {
		t.Fatal(err)
	}

	// An empty filter means everything, and erasing the node's whole history has
	// to be asked for.
	if code, _ := swarmPOST(t, srv.URL+"/api/admin/swarm/stats/forget", `{}`); code != 400 {
		t.Errorf("unqualified forget-all = %d, want 400", code)
	}
	if row, _ := db.GetSwarmTraffic(ctx, hash); row == nil {
		t.Fatal("the refused request deleted anyway")
	}

	code, body := swarmPOST(t, srv.URL+"/api/admin/swarm/stats/forget",
		`{"hashes":["`+hash+`"]}`)
	if code != 200 || body["forgotten"].(float64) != 1 {
		t.Fatalf("forget = %d %v", code, body)
	}
	if row, _ := db.GetSwarmTraffic(ctx, hash); row != nil {
		t.Error("the row survived being forgotten")
	}
	// The blob itself is untouched — forgetting stats is not deleting anything.
	if f, _ := db.GetFileByHash(ctx, hash); f == nil {
		t.Error("forgetting traffic deleted the file")
	}
}

func TestSwarmFile_DetailAnd404(t *testing.T) {
	srv, db := newSwarmTestServer(t, nil)
	swarmSeedFile(t, db, strings.Repeat("a", 64), "Detailed", 10)

	body := swarmGET(t, srv.URL+"/api/admin/swarm/"+strings.Repeat("a", 64))
	if body["title"] != "Detailed" {
		t.Errorf("detail = %v", body)
	}

	resp, err := http.Get(srv.URL + "/api/admin/swarm/" + strings.Repeat("b", 64))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown hash = %d, want 404", resp.StatusCode)
	}

	resp, err = http.Get(srv.URL + "/api/admin/swarm/not-a-hash")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("malformed hash = %d, want 400", resp.StatusCode)
	}
}

// The live poll answers per-hash session deltas only for the hashes asked for —
// a page showing 100 rows must not be sent the whole session map.
func TestSwarmLive_AnswersOnlyTheHashesAsked(t *testing.T) {
	fed := &fakeFederation{traffic: federation.TrafficSnapshot{
		TrafficCounters: federation.TrafficCounters{Up: 5},
		Hashes: map[string]federation.TrafficCounters{
			"aa11": {Up: 5}, "bb22": {Down: 9},
		},
	}}
	srv, _ := newSwarmTestServer(t, fed)

	body := swarmGET(t, srv.URL+"/api/admin/swarm/live?hash=aa11")
	rows := body["rows"].(map[string]any)
	if len(rows) != 1 || rows["aa11"] == nil {
		t.Errorf("rows = %v, want only the requested hash", rows)
	}
	idle := swarmGET(t, srv.URL+"/api/admin/swarm/live")
	if len(idle["rows"].(map[string]any)) != 0 {
		t.Errorf("an idle poll returned rows: %v", idle["rows"])
	}
	if idle["session"].(map[string]any)["up_bytes"].(float64) != 5 {
		t.Error("an idle poll should still carry the session totals")
	}
}
