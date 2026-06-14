package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"daemonlord.ygg/madshare/database"
	"github.com/go-chi/chi/v5"
)

func decodeDuplicates(t *testing.T, body []byte) []duplicateRecordingDTO {
	t.Helper()
	var out []duplicateRecordingDTO
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode: %v\nbody: %s", err, body)
	}
	return out
}

func TestDuplicatesList_RanksAndSuggests(t *testing.T) {
	repo := &fakeRepo{duplicateRecordings: []database.DuplicateRecording{{
		RecordingID: 7,
		Renditions: []database.DuplicateRendition{
			{FileID: 1, Hash: "h1", ObjectKey: "h1/a.mp3", Codec: "mp3", MimeType: "audio/mpeg", Bitrate: 320000, ByteSize: 9_000_000},
			{FileID: 2, Hash: "h2", ObjectKey: "h2/a.flac", Codec: "flac", MimeType: "audio/flac", SampleRate: 44100, BitDepth: 16, ByteSize: 25_000_000},
		},
	}}}
	h := &handler{repo: repo}

	rr := httptest.NewRecorder()
	h.duplicatesList(rr, httptest.NewRequest(http.MethodGet, "/api/admin/duplicates", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	dups := decodeDuplicates(t, rr.Body.Bytes())
	if len(dups) != 1 || dups[0].RecordingID != 7 {
		t.Fatalf("got %+v, want one recording id 7", dups)
	}
	// The FLAC must be ranked best (lossless beats lossy), with a play URL.
	var flac, mp3 duplicateRenditionDTO
	for _, r := range dups[0].Renditions {
		switch r.FileID {
		case 2:
			flac = r
		case 1:
			mp3 = r
		}
	}
	if flac.Rank != 1 || !flac.Best {
		t.Errorf("flac rank=%d best=%v, want 1/true", flac.Rank, flac.Best)
	}
	if mp3.Rank != 2 || mp3.Best {
		t.Errorf("mp3 rank=%d best=%v, want 2/false", mp3.Rank, mp3.Best)
	}
	if flac.URL != "/files/h2/a.flac" {
		t.Errorf("flac url = %q, want /files/h2/a.flac", flac.URL)
	}
	if dups[0].Suggestion == "" {
		t.Error("expected a non-empty suggestion")
	}
}

func TestDuplicatesList_DegradedNoTech(t *testing.T) {
	// ffprobe absent: no codec/bitrate. Format falls back to MIME and the
	// suggestion admits there is nothing to rank on.
	repo := &fakeRepo{duplicateRecordings: []database.DuplicateRecording{{
		RecordingID: 1,
		Renditions: []database.DuplicateRendition{
			{FileID: 1, Hash: "h1", ObjectKey: "h1/a.mp3", MimeType: "audio/mpeg", ByteSize: 3_000_000},
			{FileID: 2, Hash: "h2", ObjectKey: "h2/b.mp3", MimeType: "audio/mpeg", ByteSize: 8_000_000},
		},
	}}}
	h := &handler{repo: repo}

	rr := httptest.NewRecorder()
	h.duplicatesList(rr, httptest.NewRequest(http.MethodGet, "/api/admin/duplicates", nil))
	dups := decodeDuplicates(t, rr.Body.Bytes())
	if dups[0].Suggestion == "" || dups[0].Renditions[0].Format != "audio/mpeg" {
		t.Errorf("degraded: suggestion=%q format=%q", dups[0].Suggestion, dups[0].Renditions[0].Format)
	}
	// Larger file ranks first in the degraded (size-only) ladder.
	best := map[int64]bool{}
	for _, r := range dups[0].Renditions {
		best[r.FileID] = r.Best
	}
	if !best[2] || best[1] {
		t.Errorf("degraded ranking: file2 best=%v file1 best=%v, want true/false", best[2], best[1])
	}
}

// splitRequest builds a request with the {file_id} chi route param set.
func splitRequest(fileID string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/api/admin/duplicates/"+fileID+"/split", nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("file_id", fileID)
	return req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
}

func TestDuplicatesSplit_OK(t *testing.T) {
	repo := &fakeRepo{}
	h := &handler{repo: repo}
	rr := httptest.NewRecorder()
	h.duplicatesSplit(rr, splitRequest("5"))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if repo.splitFileID != 5 {
		t.Errorf("split file id = %d, want 5", repo.splitFileID)
	}
}

func TestDuplicatesSplit_NotFound(t *testing.T) {
	h := &handler{repo: &fakeRepo{splitNotFound: true}}
	rr := httptest.NewRecorder()
	h.duplicatesSplit(rr, splitRequest("5"))
	if rr.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rr.Code)
	}
}

func renditionsRequest(hash string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/api/tracks/"+hash+"/renditions", nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("hash", hash)
	return req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
}

func TestTrackRenditions_OKRanked(t *testing.T) {
	repo := &fakeRepo{renditions: []database.DuplicateRendition{
		{FileID: 1, Hash: "h1", ObjectKey: "h1/a.mp3", Codec: "mp3", MimeType: "audio/mpeg", Bitrate: 320000, ByteSize: 9_000_000},
		{FileID: 2, Hash: "h2", ObjectKey: "h2/a.flac", Codec: "flac", MimeType: "audio/flac", SampleRate: 44100, BitDepth: 16, ByteSize: 25_000_000},
	}}
	h := &handler{repo: repo}
	rr := httptest.NewRecorder()
	h.trackRenditions(rr, renditionsRequest(strings.Repeat("a", 64)))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	var rends []duplicateRenditionDTO
	if err := json.Unmarshal(rr.Body.Bytes(), &rends); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(rends) != 2 {
		t.Fatalf("got %d renditions, want 2", len(rends))
	}
	for _, r := range rends {
		if r.FileID == 2 && (!r.Best || r.Rank != 1) {
			t.Errorf("flac should be best/rank1, got best=%v rank=%d", r.Best, r.Rank)
		}
		if r.FileID == 2 && r.URL != "/files/h2/a.flac" {
			t.Errorf("flac url = %q", r.URL)
		}
	}
}

func TestTrackRenditions_NotFound(t *testing.T) {
	h := &handler{repo: &fakeRepo{}} // renditions nil
	rr := httptest.NewRecorder()
	h.trackRenditions(rr, renditionsRequest(strings.Repeat("a", 64)))
	if rr.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rr.Code)
	}
}

func TestTrackRenditions_BadHash(t *testing.T) {
	h := &handler{repo: &fakeRepo{}}
	rr := httptest.NewRecorder()
	h.trackRenditions(rr, renditionsRequest("nothex"))
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rr.Code)
	}
}

func TestDuplicatesSplit_BadID(t *testing.T) {
	h := &handler{repo: &fakeRepo{}}
	for _, bad := range []string{"abc", "0", "-1"} {
		rr := httptest.NewRecorder()
		h.duplicatesSplit(rr, splitRequest(bad))
		if rr.Code != http.StatusBadRequest {
			t.Errorf("file_id=%q: status = %d, want 400", bad, rr.Code)
		}
	}
}
