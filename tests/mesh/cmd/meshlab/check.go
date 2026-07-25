//go:build tests && !nofederation

package main

// `meshlab check` — the sharing-scope rules, asserted against the running lab.
//
// Everything else meshlab does is a knob for a person to turn. This is the one
// command that answers rather than shows, and it exists because F5's central
// claim is a NEGATIVE one: catalog and bytes read a single rule, so what an
// audience is not shown it also cannot fetch. Negatives are exactly what a
// browser walkthrough is bad at — nothing appears, which looks the same as a
// feature that silently does nothing.
//
// The cases run against real madshare processes over the real mesh, with a real
// outsider (probe.go) doing the asking. Two of them are worth naming, because
// they are the ones an in-process test cannot make:
//
//   - the guest-open swarm serves a stranger the BYTES, verified against the
//     content hash — not merely a 200;
//   - the byte gate is LIVE, so a friend holding a stale catalog that still
//     advertises a now-private track is refused anyway. Staleness is the normal
//     state of a federated catalog (15-minute sync), which makes this the
//     realistic case rather than the exotic one.

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"daemonlord.ygg/madshare/federation"
)

// checkCase is one assertion's outcome. Skipped cases carry OK=true and a reason
// — a lab that lacks a friend pair cannot answer the friend questions, and that
// is a shape of the lab, not a failure of the server.
type checkCase struct {
	Name    string `json:"name"`
	OK      bool   `json:"ok"`
	Skipped bool   `json:"skipped,omitempty"`
	Detail  string `json:"detail"`
}

type checkReport struct {
	Cases   []checkCase `json:"cases"`
	Passed  int         `json:"passed"`
	Failed  int         `json:"failed"`
	Skipped int         `json:"skipped"`
	Elapsed string      `json:"elapsed"`
}

func (r *checkReport) add(name string, ok bool, format string, args ...any) {
	c := checkCase{Name: name, OK: ok, Detail: fmt.Sprintf(format, args...)}
	if ok {
		r.Passed++
	} else {
		r.Failed++
	}
	r.Cases = append(r.Cases, c)
}

func (r *checkReport) skip(name, format string, args ...any) {
	r.Cases = append(r.Cases, checkCase{Name: name, OK: true, Skipped: true, Detail: fmt.Sprintf(format, args...)})
	r.Skipped++
}

// appearance is the slice of /api/admin/appearances the check needs: a live
// approved appearance and the hash of the rendition it plays.
type appearance struct {
	TagsetID    int64  `json:"tagset_id"`
	RecordingID int64  `json:"recording_id"`
	Hash        string `json:"hash"`
	Title       string `json:"title"`
}

func (n *node) appearances() ([]appearance, error) {
	var out struct {
		Items []appearance `json:"items"`
	}
	if err := n.getJSON("/api/admin/appearances?limit=500", &out); err != nil {
		return nil, fmt.Errorf("appearances on %s: %w", n.name, err)
	}
	return out.Items, nil
}

// check runs the pass and always restores the scope it changed, including on a
// failure — a check that leaves a track private has broken the lab for whatever
// the operator does next.
func (l *lab) check() (*checkReport, error) {
	started := time.Now()
	rep := &checkReport{}

	holder, subject, err := l.pickSubject()
	if err != nil {
		return nil, err
	}
	friend := l.friendOf(holder)

	// Remember the subject's scope and put it back whatever happens.
	before, err := l.recordingScope(holder, subject.RecordingID)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err := l.restoreScope(holder, subject.RecordingID, before); err != nil {
			rep.add("restore scope", false, "could not restore recording %d on %s: %v",
				subject.RecordingID, holder.name, err)
		}
	}()

	p, err := l.ensureProbe()
	if err != nil {
		return nil, err
	}
	if err := p.wait(holder, 60*time.Second); err != nil {
		return nil, err
	}

	l.logger.Printf("check: subject %q (recording %d) on %s, hash %s…",
		subject.Title, subject.RecordingID, holder.name, short(subject.Hash))

	// ── The friends-only surfaces ────────────────────────────────────────────
	// Ping is deliberately open (meshAuth refuses only BLOCKED peers), which is
	// what makes the next two meaningful: the outsider clearly reached the node.
	if code, _, err := p.get(holder, "/madnetwork/v0/ping"); err != nil {
		rep.add("outsider reaches the mesh", false, "ping: %v", err)
	} else {
		rep.add("outsider reaches the mesh", code == http.StatusOK, "ping = %d, want 200", code)
	}
	for _, route := range []string{"/madnetwork/v0/catalog", "/madnetwork/v0/holdings"} {
		name := "outsider refused " + strings.TrimPrefix(route, "/madnetwork/v0/")
		code, _, err := p.get(holder, route)
		if err != nil {
			rep.add(name, false, "%s: %v", route, err)
			continue
		}
		rep.add(name, code == http.StatusForbidden, "%s = %d, want 403 (friends only)", route, code)
	}

	// ── The outsider vs. one blob, through three scopes ──────────────────────
	// Normal: published, not guest-playable. A stranger must not even learn it
	// exists, which is why the expectation is 404 and not 403.
	if err := l.setScope(holder, subject.RecordingID, ptr(federation.DepthFriends), false, ptr(false)); err != nil {
		return nil, err
	}
	l.probeBlob(rep, p, holder, subject, "normal blob is invisible to an outsider", http.StatusNotFound, false)

	// Guest-playable: the open swarm. Bytes, not just a status.
	if err := l.setScope(holder, subject.RecordingID, nil, false, ptr(true)); err != nil {
		return nil, err
	}
	l.probeBlob(rep, p, holder, subject, "guest-playable blob serves an outsider", http.StatusOK, true)

	// Private beats guest-playable: depth is checked before the guest policy, so
	// "not on the network" means exactly that.
	if err := l.setScope(holder, subject.RecordingID, ptr(federation.DepthPrivate), false, nil); err != nil {
		return nil, err
	}
	l.probeBlob(rep, p, holder, subject, "private beats guest-playable", http.StatusNotFound, false)

	// ── The node's own view ──────────────────────────────────────────────────
	// The self-merge is filtered by depth, so a private recording leaves the
	// network page while staying in the local library. No sync involved.
	if own, err := l.madnetworkCount(holder); err != nil {
		rep.add("private track leaves the node's own /madnetwork", false, "summary: %v", err)
	} else {
		if err := l.setScope(holder, subject.RecordingID, nil, true, nil); err != nil {
			return nil, err
		}
		shared, err2 := l.madnetworkCount(holder)
		if err := l.setScope(holder, subject.RecordingID, ptr(federation.DepthPrivate), false, nil); err != nil {
			return nil, err
		}
		if err2 != nil {
			rep.add("private track leaves the node's own /madnetwork", false, "summary: %v", err2)
		} else {
			rep.add("private track leaves the node's own /madnetwork", shared == own+1,
				"madnetwork on %s: %d while private, %d while shared (want +1)", holder.name, own, shared)
		}
	}

	// ── The live byte gate vs. a stale catalog ───────────────────────────────
	if friend == nil {
		rep.skip("friend is refused a now-private track", "no friend pair in this lab (-friends none?)")
		rep.skip("friend can stream a shared track", "no friend pair in this lab (-friends none?)")
	} else if held, err := friend.holdsLocally(subject.Hash); err != nil || held {
		// A friend that has the bytes in its OWN library never asks the holder,
		// so the byte gate is not what would be under test. Only possible when
		// two nodes were seeded the same file.
		rep.skip("friend is refused a now-private track", "%s holds this hash in its own library", friend.name)
		rep.skip("friend can stream a shared track", "%s holds this hash in its own library", friend.name)
	} else {
		// Drop whatever this friend cached from an earlier pass. Without it the
		// check is not idempotent: the success case below leaves the blob in the
		// friend's cache, EnsureBlob short-circuits to it on the next run, and
		// the refusal case reads 200 — a true answer to a question it stopped
		// asking. The lab owns these directories, so clearing one is fair game;
		// letting the assertion quietly depend on a fresh lab is not.
		if err := friend.dropCachedBlob(subject.Hash); err != nil {
			rep.add("friend is refused a now-private track", false,
				"could not clear %s's cache of %s: %v", friend.name, short(subject.Hash), err)
		} else {
			// Still private here. The friend's cached catalog was synced before
			// the change and still advertises the hash, so this asks the byte
			// gate directly — which is the point.
			code, _, err := friend.streamStatus(subject.Hash, 90*time.Second)
			switch {
			case err != nil:
				rep.add("friend is refused a now-private track", false, "stream from %s: %v", friend.name, err)
			default:
				rep.add("friend is refused a now-private track", code != http.StatusOK,
					"%s streaming %s's private track = %d, want any failure (its catalog is stale but the bytes are gated live)",
					friend.name, holder.name, code)
			}
		}

		// Back to shared, and the same friend must get the bytes.
		if err := l.setScope(holder, subject.RecordingID, nil, true, nil); err != nil {
			return nil, err
		}
		code, body, err := friend.streamBody(subject.Hash, 120*time.Second)
		switch {
		case err != nil:
			rep.add("friend can stream a shared track", false, "stream from %s: %v", friend.name, err)
		case code != http.StatusOK:
			rep.add("friend can stream a shared track", false,
				"%s streaming %s's track = %d, want 200", friend.name, holder.name, code)
		default:
			got := sha256Hex(body)
			rep.add("friend can stream a shared track", got == subject.Hash,
				"%s received %d bytes from %s, sha256 %s… (want %s…)",
				friend.name, len(body), holder.name, short(got), short(subject.Hash))
		}
	}

	rep.Elapsed = time.Since(started).Truncate(time.Millisecond).String()
	return rep, nil
}

// probeBlob asks for one blob as the outsider and, when a body is expected,
// verifies it hashes to the content address. The manifest is asked for too:
// a manifest describes bytes we are willing to hand over, so the two must never
// disagree — an outsider that could read the chunk list of a blob it may not
// fetch would be a leak in its own right.
func (l *lab) probeBlob(rep *checkReport, p *probe, holder *node, subject appearance, name string, want int, wantBytes bool) {
	code, body, err := p.get(holder, "/madnetwork/v0/blob/"+subject.Hash)
	if err != nil {
		rep.add(name, false, "blob: %v", err)
		return
	}
	switch {
	case code != want:
		rep.add(name, false, "blob = %d, want %d", code, want)
	case wantBytes:
		got := sha256Hex(body)
		rep.add(name, got == subject.Hash, "blob = %d, %d bytes, sha256 %s… (want %s…)",
			code, len(body), short(got), short(subject.Hash))
	default:
		rep.add(name, true, "blob = %d", code)
	}

	mcode, _, merr := p.get(holder, "/madnetwork/v0/manifest/"+subject.Hash)
	if merr != nil {
		rep.add(name+" (manifest agrees)", false, "manifest: %v", merr)
		return
	}
	rep.add(name+" (manifest agrees)", mcode == want, "manifest = %d, want %d (same as the blob)", mcode, want)
}

// ── Lab plumbing for the check ───────────────────────────────────────────────

func ptr[T any](v T) *T { return &v }

// pickSubject finds a running node with a published track to experiment on.
func (l *lab) pickSubject() (*node, appearance, error) {
	for _, name := range l.names {
		n := l.nodes[name]
		if !n.running() {
			continue
		}
		apps, err := n.appearances()
		if err != nil {
			return nil, appearance{}, err
		}
		// Oldest first, so repeated runs pick the same track.
		sort.Slice(apps, func(i, j int) bool { return apps[i].TagsetID < apps[j].TagsetID })
		for _, a := range apps {
			if a.Hash != "" {
				return n, a, nil
			}
		}
	}
	return nil, appearance{}, fmt.Errorf("no node holds a published track — run `meshlab up -seed DIR` " +
		"(the scope rules are about what a library shows, so a check needs a library)")
}

// friendOf returns a running node friended with n, or nil.
func (l *lab) friendOf(n *node) *node {
	for _, pair := range l.friendPairs {
		var other string
		switch n.name {
		case pair[0]:
			other = pair[1]
		case pair[1]:
			other = pair[0]
		default:
			continue
		}
		if o, ok := l.nodes[other]; ok && o.running() {
			return o
		}
	}
	return nil
}

// recordingScope reads one recording's current scope so the check can put it
// back.
func (l *lab) recordingScope(n *node, recordingID int64) (recordingRow, error) {
	recs, err := n.recordings()
	if err != nil {
		return recordingRow{}, err
	}
	for _, r := range recs.Items {
		if r.ID == recordingID {
			return r, nil
		}
	}
	return recordingRow{}, fmt.Errorf("recording %d not found on %s", recordingID, n.name)
}

func (l *lab) restoreScope(n *node, recordingID int64, before recordingRow) error {
	return l.setScope(n, recordingID, before.ShareDepth, before.ShareDepth == nil, &before.GuestPlayable)
}

// setScope patches one recording and gives the node a moment: the catalog
// snapshot is memoized for up to Intervals.SnapshotTTL (one minute in
// production), so a scope change is not visible to a fresh catalog request
// immediately. The BYTE endpoints are not memoized at all — they re-read the
// predicate per request — which is why the checks against them need no wait, and
// why the check does not depend on the snapshot TTL anywhere.
func (l *lab) setScope(n *node, recordingID int64, depth *int, inherit bool, guest *bool) error {
	if err := n.setRecordingScope(recordingID, depth, inherit, guest); err != nil {
		return fmt.Errorf("scope recording %d on %s: %w", recordingID, n.name, err)
	}
	return nil
}

func (l *lab) madnetworkCount(n *node) (int, error) {
	tracks, _, err := n.madnetworkView()
	return tracks, err
}

// ensureProbe starts the outsider on first use and keeps it for the lab's life —
// a yggdrasil node takes seconds to join, and a check that paid that every run
// would tempt anyone into running it less.
func (l *lab) ensureProbe() (*probe, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.probeNode != nil {
		return l.probeNode, nil
	}
	// Peer into the first node's underlay listener directly, not through a fault
	// link: the check asks authorization questions, and an answer shaped by
	// injected latency would be measuring the wrong thing.
	first := l.nodes[l.names[0]]
	p, err := startProbe(l.root, first.scheme+"://"+first.underlay, l.logger)
	if err != nil {
		return nil, err
	}
	l.probeNode = p
	return p, nil
}

// ── Node helpers for the stream cases ────────────────────────────────────────

// holdsLocally reports whether this node has the hash in its OWN library, where
// a madnetwork fetch short-circuits before any peer is asked.
func (n *node) holdsLocally(hash string) (bool, error) {
	apps, err := n.appearances()
	if err != nil {
		return false, err
	}
	for _, a := range apps {
		if a.Hash == hash {
			return true, nil
		}
	}
	return false, nil
}

// dropCachedBlob removes a hash from this node's madnetwork download cache, both
// the finished file and any half-fetched .part, so the next fetch has to go back
// to the mesh. Missing files are not an error — the usual case is a fresh lab.
func (n *node) dropCachedBlob(hash string) error {
	dir := filepath.Join(n.dir, "cache", "madnetwork")
	for _, p := range []string{filepath.Join(dir, hash), filepath.Join(dir, hash+".part")} {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

// streamStatus asks the relay for a hash and reports only the status, draining
// the body. Used where the expectation is a refusal.
func (n *node) streamStatus(hash string, timeout time.Duration) (int, string, error) {
	code, body, err := n.streamBody(hash, timeout)
	return code, strings.TrimSpace(string(body)), err
}

// streamBody fetches a whole remote track through this node's cache-through
// relay — the same request the web player makes.
func (n *node) streamBody(hash string, timeout time.Duration) (int, []byte, error) {
	return n.rawGet("/api/madnetwork/stream/"+hash, timeout)
}
