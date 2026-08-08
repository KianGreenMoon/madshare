# UI docs

Two kinds of document live here, and the difference decides whether a second
client has to obey them:

- **Contracts** — behaviour both the web UI and any other client must implement
  the same way. A change to one of these is a change to what madshare *is*, and
  the doc moves in the same commit as the code.
- **Web-UI notes** — how the browser implementation is put together. Useful for
  working on `webui/`, irrelevant to a native client, which has its own answers
  to the same problems.

## If you are building a client

Read in this order. The first three are the ones people skip and then get wrong.

| # | Doc | What it settles |
|---|---|---|
| 1 | [`madplayer.md`](madplayer.md) | The native client: what it is, the two levels of ambition, the API surface a player needs, the capability-token flow, and what the server already computes so you don't re-implement it |
| 2 | [`artists-and-performers.md`](artists-and-performers.md) | **Which names get an artist row, and what is under each.** The rule is server-side in both catalogs; the one way to get it wrong is to group track rows client-side |
| 3 | [`player-and-queue.md`](player-and-queue.md) | Queue, shuffle, repeat, resume, remote tracks, failure handling. Shuffle *reorders the queue* — it is not "pick a random next track" |
| 4 | [`library-page.md`](library-page.md) | The local browse surface: drill, paging, row identity (`tagset_id`), disc grouping, search, guest narrowing |
| 5 | [`madnetwork-page.md`](madnetwork-page.md) | The network browse: merged catalog, lanes, availability, Materialize, version ordering |
| 6 | [`madnetwork-nodes.md`](madnetwork-nodes.md) | The node directory and the per-node page; why a node is addressed by its **public key** |
| 7 | [`user-settings.md`](user-settings.md) | Per-account controls: password, API tokens (the credential a non-browser client uses), theme |

Cross-cutting rules a client must also honour live outside this directory:

- `docs/architecture/recording-tagsets.md` — why a track is a `tagset_id` and not
  a file hash.
- `docs/architecture/disc-numbering.md` — untagged / `0` / `N` are three distinct
  discs. One rule, shared by every surface that groups tracks.
- `docs/architecture/auth.md` — permissions, sessions, and what "default-deny"
  covers. `docs/api/tokens.md` is the bearer-token credential.
- `docs/architecture/recordings.md` — the quality ladder behind every play `url`.

## Web-UI implementation notes

| Doc | Subject |
|---|---|
| [`shells.md`](shells.md) | The two chromes (listening shell + admin shell), the `/` front door, the responsive header, the cross-shell upload page |
| [`toast.md`](toast.md) | The one transient-message module |
| [`clipboard.md`](clipboard.md) | Why copying goes through `clipboard.js`: real deployments are plain HTTP, where `navigator.clipboard` is undefined |

`clipboard.md` is a web-only *problem* but a cross-client *lesson*: the origins
madshare is actually reached on (a yggdrasil address, a plain-HTTP reverse proxy)
are not secure contexts, and `localhost` is — so a whole class of bug cannot be
reproduced in local development. Bind `addr = ""` and load the LAN address before
concluding something works.

## Keeping these honest

Several contracts here name the test that pins them — `TestHopsMatchTheMap`,
`TestUnifiedArtistBrowse_PerformerOnCompilation`, `TestMadnetworkPerformerCredits`.
If a change makes one of those fail, it is changing the contract, not the test.

Superseded material is **deleted**, not kept in a history section. A doc says what
is true now; `git log` says what used to be.
