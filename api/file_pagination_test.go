package api

import (
	"fmt"
	"net/http"
	"testing"
)

// fileEnv mirrors the GET /api/files envelope for the pagination tests.
type fileEnv struct {
	Total  int              `json:"total"`
	Limit  int              `json:"limit"`
	Offset int              `json:"offset"`
	Items  []map[string]any `json:"items"`
}

// TestFilesPagination exercises the paginated /api/files envelope: total,
// limit/offset windowing, count-only (limit=0), the q filter, and clamping.
func TestFilesPagination(t *testing.T) {
	srv, db := newAuthTestServer(t)
	admin := clientFor(t, srv.URL, "admin", testAdminPassword)

	// Seed 5 approved files: 3 by "Alpha", 2 by "Beta".
	for i := 1; i <= 3; i++ {
		insertTaggedFile(t, db, fmt.Sprintf("%064d", i), "Alpha", "Album", fmt.Sprintf("A%d", i))
	}
	for i := 4; i <= 5; i++ {
		insertTaggedFile(t, db, fmt.Sprintf("%064d", i), "Beta", "Album", fmt.Sprintf("B%d", i))
	}

	get := func(query string) fileEnv {
		t.Helper()
		var e fileEnv
		if code := doJSON(t, admin, http.MethodGet, srv.URL+"/api/files"+query, nil, &e); code != http.StatusOK {
			t.Fatalf("GET /api/files%s = %d, want 200", query, code)
		}
		return e
	}

	// Default page: all 5, total reported.
	if e := get(""); e.Total != 5 || len(e.Items) != 5 {
		t.Errorf("default: total=%d items=%d, want 5/5", e.Total, len(e.Items))
	}
	// Windowed: first page of 2.
	if e := get("?limit=2&offset=0"); e.Total != 5 || len(e.Items) != 2 {
		t.Errorf("page0: total=%d items=%d, want 5/2", e.Total, len(e.Items))
	}
	// Last partial page.
	if e := get("?limit=2&offset=4"); len(e.Items) != 1 {
		t.Errorf("last page items=%d, want 1", len(e.Items))
	}
	// Count-only: total without rows.
	if e := get("?limit=0"); e.Total != 5 || len(e.Items) != 0 {
		t.Errorf("count-only: total=%d items=%d, want 5/0", e.Total, len(e.Items))
	}
	// Filter matches the artist.
	if e := get("?q=Beta&limit=50"); e.Total != 2 || len(e.Items) != 2 {
		t.Errorf("q=Beta: total=%d items=%d, want 2/2", e.Total, len(e.Items))
	}
	// limit above the max is clamped (not honored verbatim).
	if e := get("?limit=99999"); e.Limit != fileListMaxLimit {
		t.Errorf("limit clamp = %d, want %d", e.Limit, fileListMaxLimit)
	}
}

// TestBulkTrash covers POST /api/admin/files/bulk: explicit hashes, filter mode
// ("select all matching"), the empty-filter guardrail, and bad requests.
func TestBulkTrash(t *testing.T) {
	srv, db := newAuthTestServer(t)
	admin := clientFor(t, srv.URL, "admin", testAdminPassword)

	for i := 1; i <= 3; i++ {
		insertTaggedFile(t, db, fmt.Sprintf("%064d", i), "Alpha", "Album", fmt.Sprintf("A%d", i))
	}
	for i := 4; i <= 5; i++ {
		insertTaggedFile(t, db, fmt.Sprintf("%064d", i), "Beta", "Album", fmt.Sprintf("B%d", i))
	}

	total := func() int {
		var e fileEnv
		doJSON(t, admin, http.MethodGet, srv.URL+"/api/files?limit=0", nil, &e)
		return e.Total
	}
	bulk := func(body map[string]any) (int, int) {
		t.Helper()
		var out struct {
			OK       bool `json:"ok"`
			Affected int  `json:"affected"`
		}
		code := doJSON(t, admin, http.MethodPost, srv.URL+"/api/admin/files/bulk", body, &out)
		return code, out.Affected
	}

	// Explicit hashes: trash the two Beta files.
	if code, n := bulk(map[string]any{"action": "trash", "hashes": []string{
		fmt.Sprintf("%064d", 4), fmt.Sprintf("%064d", 5),
	}}); code != http.StatusOK || n != 2 {
		t.Fatalf("bulk hashes = %d affected=%d, want 200/2", code, n)
	}
	if got := total(); got != 3 {
		t.Errorf("total after hash trash = %d, want 3", got)
	}

	// Filter mode: trash everything matching q=Alpha.
	if code, n := bulk(map[string]any{"action": "trash", "filter": map[string]any{"q": "Alpha"}}); code != http.StatusOK || n != 3 {
		t.Fatalf("bulk filter = %d affected=%d, want 200/3", code, n)
	}
	if got := total(); got != 0 {
		t.Errorf("total after filter trash = %d, want 0", got)
	}

	// Guardrail: an empty filter without "all" is refused.
	if code, _ := bulk(map[string]any{"action": "trash", "filter": map[string]any{}}); code != http.StatusBadRequest {
		t.Errorf("empty filter without all = %d, want 400", code)
	}
	// Both hashes and filter is ambiguous → 400.
	if code, _ := bulk(map[string]any{"action": "trash", "hashes": []string{fmt.Sprintf("%064d", 1)}, "filter": map[string]any{"q": "x"}}); code != http.StatusBadRequest {
		t.Errorf("hashes+filter = %d, want 400", code)
	}
	// Unknown action → 400.
	if code, _ := bulk(map[string]any{"action": "explode", "hashes": []string{fmt.Sprintf("%064d", 1)}}); code != http.StatusBadRequest {
		t.Errorf("unknown action = %d, want 400", code)
	}
}
