package api

import (
	"encoding/json"
	"strconv"

	"daemonlord.ygg/madshare/database"
	"daemonlord.ygg/madshare/federation"
)

// Share-depth request plumbing (federation F5, docs/architecture/federation-access.md
// §Sharing scope). A share-depth edit is three-valued and JSON says so directly:
//
//	field absent   leave the recording's scope alone
//	null           clear the override — inherit the node default
//	a number       pin the depth (-1 private, 0 friends, n hops, ∞ = unlimited)
//
// Which is why the field is decoded as json.RawMessage: a *int would collapse
// "absent" and "null" into the same nil, and those are opposite instructions.

// parseShareDepthUpdate turns the raw field into a typed update. ok is false for
// a malformed or out-of-range value.
func parseShareDepthUpdate(raw json.RawMessage) (database.ShareDepthUpdate, bool) {
	if len(raw) == 0 {
		return database.ShareDepthUpdate{}, true // absent — unchanged
	}
	if string(raw) == "null" {
		return database.ShareDepthUpdate{Set: true, Inherit: true}, true
	}
	var d int
	if err := json.Unmarshal(raw, &d); err != nil {
		return database.ShareDepthUpdate{}, false
	}
	if !federation.ValidDepth(d) {
		return database.ShareDepthUpdate{}, false
	}
	return database.ShareDepthUpdate{Set: true, Depth: d}, true
}

// shareDepthLabel renders an update for the audit log.
func shareDepthLabel(u database.ShareDepthUpdate) string {
	switch {
	case !u.Set:
		return "unchanged"
	case u.Inherit:
		return "inherit"
	case u.Depth == federation.DepthPrivate:
		return "private"
	case u.Depth >= federation.DepthUnlimited:
		return "network"
	default:
		return strconv.Itoa(u.Depth)
	}
}
