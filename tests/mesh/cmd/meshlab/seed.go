//go:build tests

package main

// Library seeding: give each node a distinct set of tracks so a browse against
// one node shows something only its friends could have supplied.
//
// The recipe is the k6 suite's, ported to Go rather than reinvented
// (tests/k6/prepare-data.sh): discover audio under TEST_AUDIO_DIR, do not
// generate it. Real files matter here — the quality ladder, the fingerprint and
// the duplicate detection all come from ffprobe/fpcalc reading actual audio, and
// a synthesized blob would produce a catalog that browses but ranks nothing.
//
// Distinct sets, not a shared one, because the whole question a lab answers is
// "can this node see what that node has" — if both nodes hold the same tracks,
// every browse looks right whether federation works or not.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// audioExts mirrors the server's accepted upload types (api's acceptedAudioTypes
// keys on extension, not on the declared Content-Type — see docs/api/upload.md).
var audioExts = map[string]bool{
	".mp3": true, ".flac": true, ".ogg": true, ".oga": true,
	".m4a": true, ".mp4": true, ".opus": true, ".wav": true,
}

// seedReport is what the /seed endpoint returns and the CLI prints.
type seedReport struct {
	Dir     string            `json:"dir"`
	PerNode int               `json:"per_node"`
	Total   int               `json:"total"`
	Nodes   map[string]int    `json:"nodes"`
	Errors  map[string]string `json:"errors,omitempty"`
}

// seed uploads a distinct slice of dir's audio to each node, submits it, and
// approves it — the full path a real upload takes to reach a library, and the
// only one that ends with a track in a catalog a friend can see.
func (l *lab) seed(dir string, perNode int) (*seedReport, error) {
	if dir == "" {
		dir = os.Getenv("TEST_AUDIO_DIR")
	}
	if dir == "" {
		return nil, fmt.Errorf("no audio directory: pass one or set TEST_AUDIO_DIR " +
			"(the lab discovers real files, it does not generate them)")
	}
	files, err := discoverAudio(dir)
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("no audio files under %s (looked for %s)", dir, strings.Join(sortedExts(), " "))
	}
	if perNode <= 0 {
		perNode = len(files) / len(l.names)
		if perNode == 0 {
			perNode = 1
		}
	}
	if want := perNode * len(l.names); want > len(files) {
		return nil, fmt.Errorf("need %d files for %d nodes x %d each, found %d under %s",
			want, len(l.names), perNode, len(files), dir)
	}

	report := &seedReport{Dir: dir, PerNode: perNode, Nodes: map[string]int{}, Errors: map[string]string{}}
	for i, name := range l.names {
		n := l.nodes[name]
		slice := files[i*perNode : (i+1)*perNode]
		count, err := l.seedNode(n, slice)
		report.Nodes[name] = count
		report.Total += count
		if err != nil {
			report.Errors[name] = err.Error()
		}
	}
	if len(report.Errors) == 0 {
		report.Errors = nil
	}
	return report, nil
}

// seedNode uploads, submits and approves one node's slice.
//
// Uploads land as drafts (the review bucket, docs/architecture/moderation.md);
// the admin holds content.moderate so submitting self-approves, except for a
// duplicate-flagged piece which is always sent for review. The explicit approve
// pass afterwards is what clears those, so a lab never ends up with tracks the
// uploader thinks are published and the catalog does not.
func (l *lab) seedNode(n *node, files []string) (int, error) {
	uploaded := 0
	for _, path := range files {
		if err := n.upload(path, nil); err != nil {
			return uploaded, fmt.Errorf("upload %s: %w", filepath.Base(path), err)
		}
		uploaded++
	}
	// The upload response does not carry a tagset id on the new-file path — only
	// the dedup path echoes one (api/upload_handlers.go). So the drafts are read
	// back the way the upload page reads them, which is also the only way to
	// pick up a byte-duplicate that attached as a new appearance rather than
	// creating a row of its own.
	tagsetIDs, err := n.draftTagsets()
	if err != nil {
		return uploaded, err
	}
	if len(tagsetIDs) > 0 {
		if err := n.postJSON("/api/my/uploads/submit", map[string]any{"tagset_ids": tagsetIDs}, nil); err != nil {
			return uploaded, fmt.Errorf("submit on %s: %w", n.name, err)
		}
	}
	if err := l.approvePending(n); err != nil {
		return uploaded, err
	}
	// The analysis pipeline (ffprobe/fpcalc) runs after ingest and fills the
	// quality ladder. A catalog built before it finishes lists renditions with
	// nothing to rank, so wait for it rather than asserting against a half-built
	// library.
	if err := n.waitAnalysis(2 * time.Minute); err != nil {
		return uploaded, err
	}
	return uploaded, nil
}

// draftTagsets lists the uploader's staged appearances — anything not yet
// approved, which after a fresh upload pass is exactly what needs submitting.
func (n *node) draftTagsets() ([]int64, error) {
	var out struct {
		Items []struct {
			TagsetID int64  `json:"tagset_id"`
			State    string `json:"state"`
		} `json:"items"`
	}
	if err := n.getJSON("/api/my/uploads?limit=500", &out); err != nil {
		return nil, fmt.Errorf("my uploads on %s: %w", n.name, err)
	}
	var ids []int64
	for _, it := range out.Items {
		if it.State != "approved" {
			ids = append(ids, it.TagsetID)
		}
	}
	return ids, nil
}

// approvePending approves everything sitting in the review queue.
func (l *lab) approvePending(n *node) error {
	var list struct {
		Items []struct {
			TagsetID int64  `json:"tagset_id"`
			State    string `json:"state"`
		} `json:"items"`
	}
	if err := n.getJSON("/api/admin/moderation?limit=500", &list); err != nil {
		return fmt.Errorf("review queue on %s: %w", n.name, err)
	}
	// Only "submitted" rows are approvable in bulk; a returned or draft row in
	// the queue is somebody's work in progress.
	var ids []int64
	for _, e := range list.Items {
		if e.State == "submitted" {
			ids = append(ids, e.TagsetID)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	if err := n.postJSON("/api/admin/moderation/bulk", map[string]any{
		"action": "approve", "tagset_ids": ids,
	}, nil); err != nil {
		return fmt.Errorf("approve on %s: %w", n.name, err)
	}
	return nil
}

// waitAnalysis waits for the approved library to stop changing, which is the
// cheapest externally visible proxy for "the analysis queue has drained".
//
// It watches the *durations*, not the track count: a track appears in
// /api/tracks the moment it is approved, but ffprobe fills its duration
// afterwards, and duration is the visible edge of the whole tech-column pass the
// quality ladder ranks on. Counting rows would declare victory immediately and
// hand a browse a library with nothing to rank.
func (n *node) waitAnalysis(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		pending, err := n.tracksAwaitingAnalysis()
		if err != nil {
			return err
		}
		if pending == 0 {
			return nil
		}
		time.Sleep(time.Second)
	}
	// A slow pipeline is not a seeding failure — ffprobe may simply be absent,
	// in which case durations never arrive and the ladder degrades to
	// format-and-size, which the lab is still usable with.
	return nil
}

// tracksAwaitingAnalysis counts approved tracks that still have no duration.
//
// The library is browsed album by album (/api/tracks is album-scoped), so this
// walks albums first. Every call carries the node's bearer token, which is not
// optional: content access is default-deny, so an unauthenticated read of the
// same endpoints reports an empty library on a node that has one — a very
// convincing way to misdiagnose federation.
func (n *node) tracksAwaitingAnalysis() (int, error) {
	pending := 0
	err := n.walkLibrary(func(duration *float64) {
		if duration == nil {
			pending++
		}
	})
	return pending, err
}

// madnetworkView reports what /madnetwork would show this node — the merged
// track count after the availability filter — and whether its own inbound mesh
// path is healthy.
//
// This is the observation the whole lab is built around. The count includes the
// node's own published set plus every friend's, minus anything held only by a
// friend outside the freshness window, computed at request time. So partitioning
// a friend and watching this number fall two minutes later is the availability
// feature working, end to end, on real servers.
//
// inbound_healthy is the fail-open signal: false means this node's own inbound
// reader is dead, the filter is switched off, and the browse shows the last
// known catalog with a banner rather than blanking.
func (n *node) madnetworkView() (tracks int, inboundHealthy bool, err error) {
	var out struct {
		Tracks         int  `json:"tracks"`
		InboundHealthy bool `json:"inbound_healthy"`
	}
	if err := n.getJSON("/api/madnetwork/summary", &out); err != nil {
		return 0, false, err
	}
	return out.Tracks, out.InboundHealthy, nil
}

// libraryCount reports how many approved tracks a node holds — what `meshlab
// status` shows so a seeded lab is visibly seeded.
func (n *node) libraryCount() (int, error) {
	total := 0
	err := n.walkLibrary(func(*float64) { total++ })
	return total, err
}

// walkLibrary drills artists → albums → tracks, the way the library page does:
// /api/albums is artist-scoped and /api/tracks is album-scoped, so there is no
// flat "every track" endpoint to shortcut through.
func (n *node) walkLibrary(visit func(duration *float64)) error {
	var artists []struct {
		ID int64 `json:"id"`
	}
	if err := n.getList("/api/artists", &artists); err != nil {
		return fmt.Errorf("artist list on %s: %w", n.name, err)
	}
	for _, ar := range artists {
		var albums []struct {
			ID int64 `json:"id"`
		}
		if err := n.getList(fmt.Sprintf("/api/albums?artist_id=%d", ar.ID), &albums); err != nil {
			return fmt.Errorf("album list on %s: %w", n.name, err)
		}
		for _, al := range albums {
			var tracks []struct {
				Duration *float64 `json:"duration"`
			}
			if err := n.getList(fmt.Sprintf("/api/tracks?album_id=%d", al.ID), &tracks); err != nil {
				return fmt.Errorf("track list on %s: %w", n.name, err)
			}
			for _, t := range tracks {
				visit(t.Duration)
			}
		}
	}
	return nil
}

// getList decodes a browse endpoint into a slice, accepting either shape these
// endpoints use: a bare array normally, or {"items": [...]} on the
// keyset-paginated branch (which /api/artists switches to when given a cursor).
// Guessing wrong is a decode error rather than a wrong answer, so handling both
// is cheaper than tracking which endpoint is in which mode.
func (n *node) getList(path string, out any) error {
	var raw json.RawMessage
	if err := n.getJSON(path, &raw); err != nil {
		return err
	}
	trimmed := strings.TrimSpace(string(raw))
	if strings.HasPrefix(trimmed, "[") {
		return json.Unmarshal(raw, out)
	}
	var env struct {
		Items json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return err
	}
	if len(env.Items) == 0 {
		return nil
	}
	return json.Unmarshal(env.Items, out)
}

// discoverAudio walks dir for files the server will accept, sorted so a given
// directory always splits the same way across nodes — a lab you cannot re-run
// identically is hard to reason about.
func discoverAudio(dir string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if audioExts[strings.ToLower(filepath.Ext(path))] {
			out = append(out, path)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("scanning %s: %w", dir, err)
	}
	sort.Strings(out)
	return out, nil
}

func sortedExts() []string {
	out := make([]string, 0, len(audioExts))
	for e := range audioExts {
		out = append(out, e)
	}
	sort.Strings(out)
	return out
}
