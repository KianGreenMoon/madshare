//go:build tests && !nofederation

package main

// Sharing scope (federation F5, docs/architecture/federation.md §Sharing scope):
// the lab-side knobs for share depth and guest-playability, plus the vocabulary
// shared between the CLI, the status readout and the check pass.
//
// Scope is the first federation rule that makes two friends see DIFFERENT
// catalogs, so a lab that cannot set it cannot show the interesting half of F5.
// The knobs are driven over the same admin API an operator uses — the node
// default through /api/admin/settings/madnetwork, per-recording depth through
// the recordings access endpoint — because a lab that wrote share_depth straight
// into SQLite would prove the column works and nothing about the server.

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"daemonlord.ygg/madshare/federation"
)

// parseDepth turns a CLI word into a sharing scope. The words exist because the
// numbers are not memorable: -1 and 1<<20 encode the two ends of what is, since
// F7, a set of three named scopes rather than a ladder.
func parseDepth(word string) (int, error) {
	switch strings.ToLower(strings.TrimSpace(word)) {
	case "private", "local", "none":
		return federation.DepthPrivate, nil
	case "friends", "direct":
		return federation.DepthFriends, nil
	case "network", "madnetwork", "all", "unlimited":
		return federation.DepthUnlimited, nil
	}
	n, err := strconv.Atoi(strings.TrimSpace(word))
	if err != nil {
		return 0, fmt.Errorf("scope %q: want local, friends or network", word)
	}
	if !federation.ValidDepth(n) {
		return 0, fmt.Errorf("scope %d is not one of local (%d), friends (%d) or network (%d)",
			n, federation.DepthPrivate, federation.DepthFriends, federation.DepthUnlimited)
	}
	return n, nil
}

// depthWord is parseDepth's inverse, for readouts. A value that is none of the
// three is rendered as itself rather than rounded: post-migration-035 it can only
// come from a row the migration missed, and a lab readout is where that should
// be visible.
func depthWord(d int) string {
	switch d {
	case federation.DepthPrivate:
		return "local"
	case federation.DepthFriends:
		return "friends"
	case federation.DepthUnlimited:
		return "network"
	default:
		return fmt.Sprintf("legacy(%d)", d)
	}
}

// ── Node-level scope ─────────────────────────────────────────────────────────

// madnetworkSettings is the runtime policy card on /admin/settings.
type madnetworkSettings struct {
	Autoapprove       bool `json:"autoapprove_downloads"`
	SeedEnabled       bool `json:"seed_enabled"`
	SeedCache         bool `json:"seed_cache"`
	HideUnavailable   bool `json:"hide_unavailable"`
	DefaultShareDepth int  `json:"default_share_depth"`
	// ServeGuests answers mesh nodes outside the community with guest-playable
	// content (F7, default off). A pointer so a POST that carries the card back
	// unchanged cannot flip it: the endpoint reads absent as "leave alone".
	ServeGuests *bool `json:"serve_guests,omitempty"`
}

func (n *node) madnetworkSettings() (madnetworkSettings, error) {
	var s madnetworkSettings
	err := n.getJSON("/api/admin/settings/madnetwork", &s)
	return s, err
}

// setDefaultDepth changes the node's default sharing scope, preserving the rest
// of the policy — the endpoint takes the whole card, and a POST that dropped the
// seeding flags would silently turn seeding off.
func (n *node) setDefaultDepth(depth int) error {
	cur, err := n.madnetworkSettings()
	if err != nil {
		return err
	}
	cur.DefaultShareDepth = depth
	return n.postJSON("/api/admin/settings/madnetwork", cur, nil)
}

// setServeGuests opens or closes this node to mesh nodes outside its community
// (F7). Off is the default, so `check` has to open it to test the guest arm at
// all — which is the point of the switch.
func (n *node) setServeGuests(on bool) error {
	cur, err := n.madnetworkSettings()
	if err != nil {
		return err
	}
	cur.ServeGuests = &on
	return n.postJSON("/api/admin/settings/madnetwork", cur, nil)
}

// ── Per-recording scope ──────────────────────────────────────────────────────

// recordingRow is the slice of /api/admin/recordings the lab cares about.
type recordingRow struct {
	ID            int64  `json:"id"`
	Title         string `json:"title"`
	Artist        string `json:"artist"`
	GuestPlayable bool   `json:"guest_playable"`
	// ShareDepth is nil when the recording inherits the node default.
	ShareDepth *int `json:"share_depth"`
}

type recordingList struct {
	Items []recordingRow `json:"items"`
	Total int            `json:"total"`
	// NodeShareDepth is what a nil ShareDepth currently resolves to.
	NodeShareDepth int `json:"node_share_depth"`
}

func (n *node) recordings() (recordingList, error) {
	var out recordingList
	err := n.getJSON("/api/admin/recordings?limit=500", &out)
	return out, err
}

// setRecordingScope patches one recording's access. Both fields are optional and
// share_depth is three-valued on the wire — absent leaves it alone, null clears
// the override, a number pins it — so this mirrors that rather than flattening
// it: `inherit` must not be spelled the same way as "don't touch".
func (n *node) setRecordingScope(id int64, depth *int, inherit bool, guest *bool) error {
	body := map[string]any{}
	switch {
	case inherit:
		body["share_depth"] = nil
	case depth != nil:
		body["share_depth"] = *depth
	}
	if guest != nil {
		body["guest_playable"] = *guest
	}
	if len(body) == 0 {
		return fmt.Errorf("nothing to set")
	}
	return n.patchJSON(fmt.Sprintf("/api/admin/recordings/%d/access", id), body, nil)
}

// scopeSummary is the per-node scope line in `meshlab status`: what the node
// publishes by default, and how many of its recordings say otherwise.
type scopeSummary struct {
	Default string `json:"default"`
	Private int    `json:"private"`
	Guest   int    `json:"guest"`
	Pinned  int    `json:"pinned"` // recordings carrying any explicit depth
}

func (n *node) scopeSummary() (scopeSummary, error) {
	s, err := n.madnetworkSettings()
	if err != nil {
		return scopeSummary{}, err
	}
	out := scopeSummary{Default: depthWord(s.DefaultShareDepth)}
	recs, err := n.recordings()
	if err != nil {
		return out, err
	}
	for _, r := range recs.Items {
		if r.GuestPlayable {
			out.Guest++
		}
		if r.ShareDepth == nil {
			continue
		}
		out.Pinned++
		if *r.ShareDepth <= federation.DepthPrivate {
			out.Private++
		}
	}
	return out, nil
}

// ── Lab operations ───────────────────────────────────────────────────────────

// scopeRequest is the control API's body for a scope change.
type scopeRequest struct {
	Node string `json:"node"`
	// Target: "default" (the node setting), or "tracks" (every recording).
	Target string `json:"target"`
	Depth  string `json:"depth,omitempty"` // a word or a number; "inherit" clears
	Guest  string `json:"guest,omitempty"` // "on" / "off"
	// Limit caps how many recordings a "tracks" change touches (0 = all), so a
	// lab can make ONE track private and keep the rest — which is the shape every
	// interesting assertion needs.
	Limit int `json:"limit,omitempty"`
}

type scopeReport struct {
	Node     string   `json:"node"`
	Applied  string   `json:"applied"`
	Affected int      `json:"affected"`
	Titles   []string `json:"titles,omitempty"`
}

func (l *lab) applyScope(req scopeRequest) (*scopeReport, error) {
	n, ok := l.nodes[req.Node]
	if !ok {
		return nil, fmt.Errorf("no node %q (have: %s)", req.Node, strings.Join(l.names, ", "))
	}
	if !n.running() {
		return nil, fmt.Errorf("node %q is down", req.Node)
	}

	switch req.Target {
	case "default":
		depth, err := parseDepth(req.Depth)
		if err != nil {
			return nil, err
		}
		if err := n.setDefaultDepth(depth); err != nil {
			return nil, fmt.Errorf("set default depth on %s: %w", n.name, err)
		}
		l.logger.Printf("scope %s: node default -> %s", n.name, depthWord(depth))
		return &scopeReport{Node: n.name, Applied: "default " + depthWord(depth)}, nil

	case "tracks":
		var (
			depth   *int
			inherit bool
			guest   *bool
			label   []string
		)
		if req.Depth != "" {
			if strings.EqualFold(req.Depth, "inherit") {
				inherit, label = true, append(label, "depth inherit")
			} else {
				d, err := parseDepth(req.Depth)
				if err != nil {
					return nil, err
				}
				depth, label = &d, append(label, "depth "+depthWord(d))
			}
		}
		if req.Guest != "" {
			on := req.Guest == "on" || req.Guest == "true" || req.Guest == "1"
			guest, label = &on, append(label, fmt.Sprintf("guest %v", on))
		}
		if depth == nil && !inherit && guest == nil {
			return nil, fmt.Errorf("nothing to set: pass a depth and/or guest on|off")
		}

		recs, err := n.recordings()
		if err != nil {
			return nil, fmt.Errorf("list recordings on %s: %w", n.name, err)
		}
		// Oldest first, so `-limit 1` picks the same recording every run — a lab
		// whose "the first track" moves between runs is not reproducible.
		sort.Slice(recs.Items, func(i, j int) bool { return recs.Items[i].ID < recs.Items[j].ID })
		targets := recs.Items
		if req.Limit > 0 && req.Limit < len(targets) {
			targets = targets[:req.Limit]
		}
		rep := &scopeReport{Node: n.name, Applied: strings.Join(label, ", ")}
		for _, r := range targets {
			if err := n.setRecordingScope(r.ID, depth, inherit, guest); err != nil {
				return nil, fmt.Errorf("recording %d on %s: %w", r.ID, n.name, err)
			}
			rep.Affected++
			rep.Titles = append(rep.Titles, r.Title)
		}
		l.logger.Printf("scope %s: %d recording(s) -> %s", n.name, rep.Affected, rep.Applied)
		return rep, nil

	default:
		return nil, fmt.Errorf("unknown scope target %q (default or tracks)", req.Target)
	}
}

// scopeAll collects every node's scope for the status readout.
func (l *lab) scopeAll() map[string]scopeSummary {
	out := map[string]scopeSummary{}
	for _, name := range l.names {
		n := l.nodes[name]
		if !n.running() {
			continue
		}
		if s, err := n.scopeSummary(); err == nil {
			out[name] = s
		}
	}
	return out
}
