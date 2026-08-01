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
//   - the guest swarm, once an admin opens it, serves a stranger the BYTES,
//     verified against the content hash — not merely a 200. Since F7 it is off
//     by default, so the case immediately before it is that the same request is
//     refused while the switch is closed;
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
	// The guest switch is node-level, so leaving it open would change every later
	// command's answers, not just this recording's. Closed is the default and the
	// only state the rest of the lab should ever see.
	defer func() {
		if err := l.setServeGuests(holder, false); err != nil {
			rep.add("close the guest switch", false, "%v", err)
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

	// Guest-playable, with the node closed to guests — the F7 default posture:
	// everything to our community, nothing outside it. Marking a track
	// guest-playable is a statement about *local* visitors and buys an outsider
	// nothing on its own.
	if err := l.setScope(holder, subject.RecordingID, nil, false, ptr(true)); err != nil {
		return nil, err
	}
	l.probeBlob(rep, p, holder, subject,
		"guest-playable blob still refused while guests are closed", http.StatusNotFound, false)

	// A guest never outranks a member, so opening the node is not enough while the
	// track is still scoped Direct friends — the guest switch widens *who* may be
	// answered, never *what* they are answered with. This case caught a stale F5
	// expectation on the real lab: under F5 the guest audience sat at distance 0
	// and guest-playable overrode the scope, which is exactly the back door F7
	// closed.
	if err := l.setServeGuests(holder, true); err != nil {
		return nil, err
	}
	l.probeBlob(rep, p, holder, subject,
		"guests opened still cannot reach a Direct-friends track", http.StatusNotFound, false)

	// Scoped back to the node default (Madnetwork) and still guest-playable: now
	// the swarm serves it. Bytes, not just a status.
	if err := l.setScope(holder, subject.RecordingID, nil, true, nil); err != nil {
		return nil, err
	}
	l.probeBlob(rep, p, holder, subject, "guest-playable blob serves an outsider once opened", http.StatusOK, true)

	// Private beats guest-playable, with the node still open to guests — which is
	// the sharper form of the case: scope is checked before the guest policy, so
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

	// ── The outsider carrying a vouch (F7 item 9) ────────────────────────────
	// Everything above asked what a stranger gets. This asks what changes when a
	// node we can place says "this one is mine". Last on purpose: it rewrites the
	// subject's scope twice, and every case above depends on the scope it was
	// left in — the deferred restore is the only thing that has to run after it.
	l.checkListenerToken(rep, p, holder, friend, subject)

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

// issueToken asks this node to vouch for a bearer key the way a madplayer's home
// server does (F7 item 9): an ordinary authenticated API call, no node card and
// no friendship anywhere in it.
func (n *node) issueToken(bearerKey string) (string, error) {
	var grant struct {
		Token string `json:"token"`
	}
	if err := n.postJSON("/api/madnetwork/token", map[string]string{"node_key": bearerKey}, &grant); err != nil {
		return "", err
	}
	if grant.Token == "" {
		return "", fmt.Errorf("%s issued an empty token", n.name)
	}
	return grant.Token, nil
}

// checkListenerToken is F7 item 9 on real processes: the same outsider node, the
// same key and the same connection, served or refused purely on whether it
// carries a vouch from a node the answering node can place.
//
// The shape matters more than the codes. The token is issued by `home` and
// presented to `holder`, which is a DIFFERENT node — a madplayer's whole problem
// is that its home server's friends have never heard of it, and a token that
// only worked against its issuer would solve nothing. It is also why this cannot
// be asserted with lab nodes alone: every lab node is somebody's friend, so only
// the probe can ask the question a stranger asks.
func (l *lab) checkListenerToken(rep *checkReport, p *probe, holder, home *node, subject appearance) {
	names := []string{
		"listener node without a token is refused",
		"home server issues a capability token",
		"vouched listener node is served by a node that is not its home",
		"a token buys membership, never friendship",
	}
	// The issuer has to be a node the holder can place, and a direct friend is
	// the cheapest one to be sure of. Without a friend pair there is nobody in
	// this lab whose vouch would mean anything.
	if home == nil {
		for _, n := range names {
			rep.skip(n, "no friend pair in this lab (-friends none?)")
		}
		return
	}
	blob := "/madnetwork/v0/blob/" + subject.Hash

	// Scoped to the madnetwork (the node default) and closed to guests: the
	// posture a listener node actually meets.
	if err := l.setScope(holder, subject.RecordingID, nil, false, ptr(false)); err != nil {
		rep.add("listener node: scope the subject", false, "%v", err)
		return
	}
	if err := l.setServeGuests(holder, false); err != nil {
		rep.add("listener node: close the guest switch", false, "%v", err)
		return
	}

	// The control, and the reason the next assertion means anything: without a
	// vouch this node is served nothing at all.
	if code, _, err := p.getAs(holder, blob, ""); err != nil {
		rep.add("listener node without a token is refused", false, "blob: %v", err)
	} else {
		rep.add("listener node without a token is refused", code == http.StatusNotFound,
			"blob = %d, want 404", code)
	}

	token, err := home.issueToken(p.key())
	if err != nil {
		rep.add("home server issues a capability token", false, "%v", err)
		return
	}
	rep.add("home server issues a capability token", true, "%s vouched for the probe", home.name)

	code, body, err := p.getAs(holder, blob, token)
	switch {
	case err != nil:
		rep.add("vouched listener node is served by a node that is not its home", false, "blob: %v", err)
	case code != http.StatusOK:
		rep.add("vouched listener node is served by a node that is not its home", false,
			"blob = %d, want 200 (token issued by %s, presented to %s)", code, home.name, holder.name)
	default:
		got := sha256Hex(body)
		rep.add("vouched listener node is served by a node that is not its home",
			got == subject.Hash, "blob = %d, %d bytes, sha256 %s… (want %s…)",
			code, len(body), short(got), short(subject.Hash))
	}

	// Membership, not friendship — the one thing the token must not buy. A
	// recording an admin restricted to hand-picked nodes stays off a device
	// nobody here picked.
	if err := l.setScope(holder, subject.RecordingID, ptr(federation.DepthFriends), false, nil); err != nil {
		rep.add("listener node: restrict the subject", false, "%v", err)
		return
	}
	if code, _, err := p.getAs(holder, blob, token); err != nil {
		rep.add("a token buys membership, never friendship", false, "blob: %v", err)
	} else {
		rep.add("a token buys membership, never friendship", code == http.StatusNotFound,
			"Direct-friends blob = %d, want 404 even with a valid token", code)
	}
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
	for _, pair := range l.friendGraph() {
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

// setServeGuests opens or closes the node to mesh nodes outside its community
// (F7). Node-level, unlike setScope, and not memoized either: the seeding policy
// is read per request.
func (l *lab) setServeGuests(n *node, on bool) error {
	if err := n.setServeGuests(on); err != nil {
		return fmt.Errorf("serve guests on %s: %w", n.name, err)
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
