package sources_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"

	"daemonlord.ygg/madshare/sources"
)

// toSummary parses a persisted summary_json into a ScanSummary.
func toSummary(t *testing.T, s string) sources.ScanSummary {
	t.Helper()
	var sum sources.ScanSummary
	if err := json.Unmarshal([]byte(s), &sum); err != nil {
		t.Fatalf("parse summary %q: %v", s, err)
	}
	return sum
}

// sha256Hex returns the lowercase hex SHA-256 of s — the content hash the scan
// engine computes for a file holding exactly those bytes.
func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
