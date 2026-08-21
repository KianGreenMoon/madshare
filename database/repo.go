package database

import (
	"context"
	"database/sql"
)

// Repository is the persistence boundary the upload handler depends on.
// Implementations must be safe for concurrent use.
type Repository interface {
	// GetFileByHash returns the file row for hash, or (nil, nil) on miss.
	GetFileByHash(ctx context.Context, hash string) (*File, error)

	// InsertFile creates a files row plus everything the tagset invariant
	// demands — a singleton recording, the offered tagset (from meta's
	// descriptive fields + f.ReviewState/f.UploadedBy), the file_uploads row,
	// and the tech media_metadata row — in a single transaction. On success,
	// f.ID and f.RecordingID are populated.
	InsertFile(ctx context.Context, f *File, upload *FileUpload, meta *MediaMetadata) error

	// RecordUpload inserts a new file_uploads row for an existing file.
	// A duplicate (file_id, filename) tuple is silently ignored.
	RecordUpload(ctx context.Context, fileID int64, filename string) error

	// ListFiles returns all files ordered by upload time descending,
	// joined with their first filename and media metadata.
	ListFiles(ctx context.Context) ([]*FileListEntry, error)

	// ListFilesPage returns one filtered + sorted page of the file listing, and
	// CountFiles the total matching the same filter (for "page N of M"). The
	// paginated listing path; see docs/architecture/file-list-scaling.md.
	ListFilesPage(ctx context.Context, q FileListQuery) ([]*FileListEntry, error)
	CountFiles(ctx context.Context, f FileFilter) (int, error)

	// Trash · Appearances lens (recording-tagsets P7c) — tagset-rooted, one row
	// per appearance. Paged like the live library: ListTrashedAppearancesPage +
	// CountTrashedAppearances drive the page/total, and
	// TrashedAppearanceIDsByFilter resolves the "select all N matching" set.
	// Everything is addressed by tagset id: a blob can host several trashed
	// appearances, and an absorbed/purged one has no blob at all.
	ListTrashedAppearancesPage(ctx context.Context, q FileListQuery) ([]*AppearanceEntry, error)
	CountTrashedAppearances(ctx context.Context, f FileFilter) (int, error)
	TrashedAppearanceIDsByFilter(ctx context.Context, f FileFilter) ([]int64, error)

	// The lens's bulk actions, each one transaction: restore (deleted_at flip),
	// permanent delete (the purge composition, purgeTagsetsTx; live ids skipped,
	// freed blobs returned for post-commit reclaim), and the tag edit.
	BulkRestoreTagsets(ctx context.Context, tagsetIDs []int64) (int, error)
	BulkHardDeleteTagsets(ctx context.Context, tagsetIDs []int64) (int, []DeletedBlob, error)
	// owner, when valid, narrows the edit to that user's own editable staging
	// (the My-uploads bulk edit); invalid = the unscoped metadata.edit path.
	BulkUpdateTagsetMetadata(ctx context.Context, tagsetIDs []int64, owner sql.NullInt64, p MetadataPatch) (affected int, notFound []int64, err error)

	// Full Library · All Appearances — the live twin of the Trash lens: one row
	// per live approved appearance, playing its recording's ladder-best
	// rendition. Same paging/filter/select-all triple, addressed by tagset id.
	ListAppearancesPage(ctx context.Context, q FileListQuery) ([]*AppearanceEntry, error)
	CountAppearances(ctx context.Context, f FileFilter) (int, error)
	AppearanceIDsByFilter(ctx context.Context, f FileFilter) ([]int64, error)

	// The live lens's bulk access edit: access lives on the recording, so each
	// live approved appearance forwards the value to its recording (the
	// tagset-addressed counterparts of BulkSetLicense/BulkSetGuestPlayable).
	BulkSetLicenseByTagsets(ctx context.Context, tagsetIDs []int64, license string) (int, error)
	BulkSetGuestPlayableByTagsets(ctx context.Context, tagsetIDs []int64, guest bool) (int, error)
	// BulkSetShareDepthByTagsets is the same arm for the madnetwork share scope
	// (F5) — how far along the friendship chain the selection travels.
	BulkSetShareDepthByTagsets(ctx context.Context, tagsetIDs []int64, depth ShareDepthUpdate) (int, error)

	// Files perspective of Trash (gc-model.md): the file-grain lens over
	// soft-removed blobs (files.deleted_at). Paged like the other listings;
	// RemovedFileIDsByFilter resolves the "select all N matching" set (file ids,
	// not hashes — the Files ops are file-id-addressed like the renditions
	// endpoints). HardDeleteRemovedFile / BulkHardDeleteRemovedFiles are the only
	// per-file purge (a recording losing its last file has its appearances
	// trashed by the scoped reap, never destroyed); they return the blobs to
	// reclaim after commit.
	ListRemovedFilesPage(ctx context.Context, q FileListQuery) ([]*FileListEntry, error)
	CountRemovedFiles(ctx context.Context, f FileFilter) (int, error)
	RemovedFileIDsByFilter(ctx context.Context, f FileFilter) ([]int64, error)
	BulkRestoreRemovedFiles(ctx context.Context, fileIDs []int64) (restored int, err error)
	HardDeleteRemovedFile(ctx context.Context, fileID int64) (blobs []DeletedBlob, found bool, err error)
	BulkHardDeleteRemovedFiles(ctx context.Context, fileIDs []int64) (deleted int, blobs []DeletedBlob, err error)

	// StorageByteBreakdown partitions the files table's logical byte_size total
	// by state (live library / on review / in trash). Backs the audio, review,
	// and trash disk-usage categories (see adminStorageStats).
	StorageByteBreakdown(ctx context.Context) (StorageByteBreakdown, error)

	// The cover-variant byte index (migration 043) — what makes "images" the
	// fifth INDEXED disk-usage category instead of a DirSize walk run inline on
	// every dashboard load. The variants directory stays authoritative and these
	// rows only describe it: SetImageVariantBytes is written by the imageproc
	// pool once a variant set lands, ImageVariantBytes is the panel's sum, and
	// ReconcileImageVariants re-walks the tree at startup so a crash mid-write or
	// an edit by hand cannot leave the index lying.
	SetImageVariantBytes(ctx context.Context, imageHash string, bytes int64) error
	ImageVariantBytes(ctx context.Context) (int64, error)
	ReconcileImageVariants(ctx context.Context, variantsImagesDir string) (int, error)

	// The madnetwork download cache index (docs/architecture/madnetwork-cache.md).
	// On Repository rather than the madnetwork store on purpose: the cache
	// outlives federation being switched off, and a node that turned it off would
	// otherwise stop reporting — and stop being able to clean up — disk it is
	// still occupying.
	//
	// MadnetworkCacheBytes is the footprint, the fifth disk-usage category.
	// PutMadnetworkCacheEntry records a blob that has landed;
	// TouchMadnetworkCache moves the last-used clock for a LOCAL read;
	// DeleteMadnetworkCacheEntry makes the index agree that a file is gone.
	MadnetworkCacheBytes(ctx context.Context) (int64, error)
	PutMadnetworkCacheEntry(ctx context.Context, e *MadnetworkCacheEntry) error
	TouchMadnetworkCache(ctx context.Context, hash string, at int64) error
	DeleteMadnetworkCacheEntry(ctx context.Context, hash string) error
	// The control page's three reads over one filter — the page, its headline
	// count/bytes, and the "select all N matching" set. They share one predicate
	// so a bulk removal can never target a different set than the one on screen.
	ListMadnetworkCachePage(ctx context.Context, q MadnetworkCacheQuery) ([]*MadnetworkCacheEntry, error)
	GetMadnetworkCacheEntry(ctx context.Context, hash string) (*MadnetworkCacheEntry, error)
	CountMadnetworkCache(ctx context.Context, f MadnetworkCacheFilter) (int, int64, error)
	MadnetworkCacheHashes(ctx context.Context, f MadnetworkCacheFilter) ([]string, error)

	// Per-blob swarm traffic (docs/architecture/swarm-admin.md). On Repository
	// for the same reason as the cache index: these bytes moved whether or not a
	// federation node is running now, and the page that reports them must work
	// with federation switched off.
	//
	// AddSwarmTraffic is the ONLY writer of either ledger, called by the flusher
	// draining the node's in-memory counters — the per-blob deltas and the
	// per-counterparty ones land in one transaction, because they count the same
	// bytes. SwarmTrafficTotals is the node's all-time contribution (SUM, not a
	// second set of counters); the two Forget calls are the only deleters, and
	// neither runs as a side effect of housekeeping.
	AddSwarmTraffic(ctx context.Context, deltas []SwarmTrafficDelta,
		peers []SwarmPeerTrafficDelta, at int64) error
	SwarmTrafficTotals(ctx context.Context) (SwarmTraffic, error)
	GetSwarmTraffic(ctx context.Context, hash string) (*SwarmTraffic, error)
	ForgetSwarmTraffic(ctx context.Context, hashes []string) (int, error)
	// ListSwarmPeerTraffic is who this node has traded with, all time, with the
	// name and class joined at read time (docs/architecture/swarm-admin.md
	// §Migration 042). Bounded by the community, not by the library.
	ListSwarmPeerTraffic(ctx context.Context) ([]SwarmPeerTraffic, error)
	ResolveSwarmPeers(ctx context.Context, keys []string) ([]SwarmPeerTraffic, error)
	ForgetSwarmPeerTraffic(ctx context.Context, keys []string) (int, error)
	ForgetAllSwarmPeerTraffic(ctx context.Context) (int, error)

	// ListArtists returns one entry per effective artist name, ordered
	// alphabetically. album_artist is preferred over artist for grouping.
	ListArtists(ctx context.Context) ([]*ArtistEntry, error)

	// ListArtistsPage is the cursor-paginated ListArtists backing the public
	// library's infinite scroll: up to `limit` artists after `cursor` in the same
	// order, plus the next-page cursor ("" = end). guest narrows to reachable.
	ListArtistsPage(ctx context.Context, cursor string, limit int, guest bool) ([]*ArtistEntry, string, error)

	// ListAlbumsByArtistID returns the albums of one artist, addressed by its
	// stable surrogate id. Tracks with no album are grouped under Title="" (the
	// unknown-album entity).
	ListAlbumsByArtistID(ctx context.Context, artistID int64) ([]*AlbumEntry, error)

	// ListTracksByAlbumID returns the tracks of one album, addressed by its
	// stable surrogate id (which already pins the artist). The "Other" bucket is
	// reached via the unknown-album entity's id.
	ListTracksByAlbumID(ctx context.Context, albumID int64) ([]*TrackEntry, error)

	// ListFilesGuest, ListArtistsGuest, ListAlbumsByArtistIDGuest, and
	// ListTracksByAlbumIDGuest are the guest counterparts of the listings
	// above: they return only what an anonymous / capability-less request may
	// reach (the guest-playable / license policy). Callers holding content.access
	// use the unfiltered variants.
	ListFilesGuest(ctx context.Context) ([]*FileListEntry, error)
	ListArtistsGuest(ctx context.Context) ([]*ArtistEntry, error)
	ListAlbumsByArtistIDGuest(ctx context.Context, artistID int64) ([]*AlbumEntry, error)
	ListTracksByAlbumIDGuest(ctx context.Context, albumID int64) ([]*TrackEntry, error)

	// Search returns artists, albums, and tracks whose names/titles contain q
	// (case-insensitive LIKE). q="" returns empty results immediately.
	Search(ctx context.Context, q string) (*SearchResults, error)

	// SearchGuest is Search restricted to content an anonymous / capability-less
	// request can reach (the guest-playable / license policy).
	SearchGuest(ctx context.Context, q string) (*SearchResults, error)

	// UpsertArtistImage stores (or replaces) the image reference for an artist
	// entity, keyed by artists.id.
	UpsertArtistImage(ctx context.Context, artistID int64, objectKey, mimeType string, updatedAt int64) error

	// UpsertAlbumImage stores (or replaces) the image reference for an album
	// entity, keyed by albums.id.
	UpsertAlbumImage(ctx context.Context, albumID int64, objectKey, mimeType string, updatedAt int64) error

	// GetArtistImage returns the image object key and MIME type for an artist
	// entity. Returns found=false (no error) when no image is stored.
	GetArtistImage(ctx context.Context, artistID int64) (objectKey, mimeType string, found bool, err error)

	// GetAlbumImage returns the image object key and MIME type for an album
	// entity. Returns found=false (no error) when no image is stored.
	GetAlbumImage(ctx context.Context, albumID int64) (objectKey, mimeType string, found bool, err error)

	// LookupArtistID returns the artists.id for a display name (matched by its
	// normalized key), or found=false. Lookup-only (never creates a row) — for
	// read paths that must not materialize entities for unknown names.
	LookupArtistID(ctx context.Context, name string) (id int64, found bool, err error)

	// AlbumNames returns an album entity's display identity (artist name,
	// title), for the cover backfill's claims lookup.
	AlbumNames(ctx context.Context, albumID int64) (artist, title string, found bool, err error)
	// LookupAlbumID returns the albums.id for (artist, album) matched by their
	// normalized keys, or found=false. Lookup-only.
	LookupAlbumID(ctx context.Context, artist, album string) (id int64, found bool, err error)

	// ResolveArtistID get-or-creates the artist entity for a display name and
	// returns its id. For cover-write paths that may target an entity with no
	// other attachment. Idempotent.
	ResolveArtistID(ctx context.Context, name string) (int64, error)

	// ResolveAlbumID get-or-creates the (artist, album) entity and returns the
	// album id. For cover-write paths.
	ResolveAlbumID(ctx context.Context, artist, album string) (int64, error)

	// RenameArtist changes an artist entity's display name (and dedup key) in
	// place; tracks and cover follow via FKs. Returns ErrEntityNotFound or
	// ErrNameConflict (the target name is already taken — that is a merge).
	RenameArtist(ctx context.Context, artistID int64, newName string) error

	// RenameAlbum changes an album entity's title (and dedup key) in place.
	// Returns ErrEntityNotFound or ErrNameConflict (a different album under the
	// same artist already has that title).
	RenameAlbum(ctx context.Context, albumID int64, newTitle string) error

	// MergeArtists merges artist fromID into intoID (tracks/albums repointed,
	// colliding albums collapsed, covers moved if the target lacks one, source
	// deleted). Returns ErrMergeSelf / ErrEntityNotFound.
	MergeArtists(ctx context.Context, fromID, intoID int64) error

	// MergeAlbums merges album fromID into intoID (tracks repointed onto the
	// target and its artist, cover moved if absent, source deleted). Returns
	// ErrMergeSelf / ErrEntityNotFound.
	MergeAlbums(ctx context.Context, fromID, intoID int64) error

	// MergeArtistsPreview / MergeAlbumsPreview report what the corresponding
	// merge would do (tracks moved, albums collapsed, cover/orphan effects)
	// without mutating anything. Same ErrMergeSelf / ErrEntityNotFound contract.
	MergeArtistsPreview(ctx context.Context, fromID, intoID int64) (*MergePreview, error)
	MergeAlbumsPreview(ctx context.Context, fromID, intoID int64) (*MergePreview, error)

	// HardDeleteFileByHash permanently removes the files row for hash (cascading
	// to its file_uploads, media_metadata, and tagset rows; a recording left
	// fileless goes with it). Works on both live and trashed files. Used by
	// PruneDangling. found is false (no error) when no row matches.
	HardDeleteFileByHash(ctx context.Context, hash string) (filenames []string, found bool, err error)

	// RestoreFileByHash restores every trashed appearance of the blob's
	// recording and revives the rendition itself — the uploader-facing
	// restore-via-reupload path (docs/architecture/moderation.md). The admin
	// Trash lens restores by tagset id instead (BulkRestoreTagsets /
	// RestoreTagset); this one is addressed by content hash because a re-upload
	// only knows the bytes (the rows are found via the recording edge, GC model).
	RestoreFileByHash(ctx context.Context, hash string) (bool, error)

	// GetTrashRestorePolicy reads the trash-restore policy (reupload_restores /
	// inform / uploader_restore); defaults to reupload_restores when unset.
	GetTrashRestorePolicy(ctx context.Context) (string, error)

	// ListFileRefs returns one FileRef per files row, each carrying the
	// content hash and the filenames recorded for it, ordered by file id.
	ListFileRefs(ctx context.Context) ([]FileRef, error)

	// Reap runs the GC-model collection passes (docs/architecture/gc-model.md):
	// quarantine the files of appearance-less recordings, trash the appearances
	// of file-less recordings, delete empty recording husks. The prune pass's
	// standing backstop; a converged library reports zero stats.
	Reap(ctx context.Context) (ReapStats, error)

	// RecordAudit appends a row to the audit log. actorUserID is invalid for
	// system/anonymous actions.
	RecordAudit(ctx context.Context, actorUserID sql.NullInt64, action, target, detail string) error

	// FileAccessibleByHash reports whether an anonymous / capability-less request
	// may play/download the file with the given hash (the guest-playable /
	// license policy). Callers holding the content.access permission bypass this.
	// Unknown hashes return false.
	FileAccessibleByHash(ctx context.Context, hash string) (bool, error)

	// --- Moderation review bucket (docs/architecture/moderation.md) ---

	// ListUploadsByUser returns the user's staged files (non-trashed, review
	// state other than approved), newest first. Backs the "My uploads" tab.
	ListUploadsByUser(ctx context.Context, userID int64) ([]*ReviewEntry, error)

	// ListPendingReview returns every staged file with its uploader's name,
	// ordered for by-uploader grouping in the moderation queue.
	ListPendingReview(ctx context.Context) ([]*ReviewEntry, error)

	// The two staging listings are paged like the live library. The *Page forms
	// return one filtered/sorted window; the Count* forms return the total (set
	// ReviewFilter.States to scope it to the selectable subset); the
	// *TagsetIDsBy* forms resolve the "select all N matching" set (tagset ids —
	// the review row identity) for the bulk endpoints. See
	// docs/architecture/file-list-scaling.md.
	ListUploadsByUserPage(ctx context.Context, q ReviewListQuery) ([]*ReviewEntry, error)
	CountUploadsByUser(ctx context.Context, f ReviewFilter) (int, error)
	UploadTagsetIDsByUserFilter(ctx context.Context, f ReviewFilter) ([]int64, error)
	ListPendingReviewPage(ctx context.Context, q ReviewListQuery) ([]*ReviewEntry, error)
	CountPendingReview(ctx context.Context, f ReviewFilter) (int, error)
	PendingReviewTagsetIDsByFilter(ctx context.Context, f ReviewFilter) ([]int64, error)

	// UpdateReviewState applies a guarded review-state transition to one
	// appearance by tagset id (single UPDATE: state must be in From, non-trashed,
	// owner matching when OwnerID is set). found is false when no row satisfies
	// the guard.
	UpdateReviewState(ctx context.Context, tagsetID int64, t ReviewTransition) (found bool, err error)

	// ApproveSubmission publishes one appearance with the moderator's per-piece
	// decisions applied atomically (recording-tagsets P4): forceNew splits the
	// blob into a new pinned recording first; dropBytes soft-removes the submitted
	// rendition after publishing (keep-appearance-drop-blob). found is false for a
	// non-actionable tagset.
	ApproveSubmission(ctx context.Context, tagsetID int64, dropBytes, forceNew bool) (found bool, err error)

	// BulkUpdateReviewState applies one guarded transition to a tagset-id set in
	// a single chunked transaction (same guard as UpdateReviewState), returning
	// the number of appearances that actually transitioned. Backs the bulk
	// moderation approve/return — one transaction instead of one write + audit
	// per row.
	BulkUpdateReviewState(ctx context.Context, tagsetIDs []int64, t ReviewTransition) (int, error)

	// TagsetReviewInfo is the narrow (state, owner, trashed) lookup on one
	// appearance, used by the My-uploads ownership/editability checks. found is
	// false on unknown id.
	TagsetReviewInfo(ctx context.Context, tagsetID int64) (state string, owner sql.NullInt64, deleted bool, found bool, err error)

	// BlobPubliclyVisible reports whether the blob belongs to the public
	// library: a surviving rendition of a recording with ≥1 approved,
	// non-trashed appearance (the recording-level half of the blob gate;
	// FileAccessibleByHash adds the guest/license policy for anonymous
	// callers).
	BlobPubliclyVisible(ctx context.Context, hash string) (visible, found bool, err error)

	// StageRestoredFile demotes a just-restored approved file to the
	// restorer's draft so an upload-initiated restore re-enters the staging
	// pipeline instead of silently republishing. No-op (false) for files that
	// were trashed while pending.
	StageRestoredFile(ctx context.Context, hash string, ownerID sql.NullInt64) (bool, error)

	// DiscardOwnUpload soft-deletes the owner's editable (draft/returned)
	// appearance by tagset id; found=false for submitted, foreign, or unknown.
	DiscardOwnUpload(ctx context.Context, tagsetID, ownerID int64) (bool, error)

	// BulkDiscardOwnUploads soft-deletes a set of the owner's editable
	// (draft/returned) appearances in one chunked transaction (same guard as
	// DiscardOwnUpload), returning how many were removed. Backs the My-uploads
	// bulk "remove" — one transaction instead of one write + audit per row.
	BulkDiscardOwnUploads(ctx context.Context, tagsetIDs []int64, ownerID int64) (int, error)

	// BulkTrashTagsets soft-deletes a set of appearances by id in one chunked
	// transaction — the moderator's bulk discard (tagset Trash, no owner guard).
	// Returns how many were trashed. Soft delete never cascades.
	BulkTrashTagsets(ctx context.Context, tagsetIDs []int64) (int, error)

	// AttachDraftTagset offers a new draft appearance on an existing blob's
	// recording (recording-tagsets P4, byte-dup upload → draft tagset), unless an
	// identical live appearance already exists. Returns the tagset id and whether
	// it was created; (0,false,nil) for a trashed/unknown file.
	AttachDraftTagset(ctx context.Context, fileID int64, ownerID sql.NullInt64, meta *MediaMetadata, filename string) (tagsetID int64, created bool, err error)

	// --- Cover image variants & async job queue (Phase 1: upload & covers) ---

	// EnqueueImageJob inserts a pending image-variant job. Idempotent per
	// image_hash (at most one active job); a duplicate enqueue is a no-op.
	EnqueueImageJob(ctx context.Context, coverType, subjectKey, imageHash string, now int64) error

	// ClaimImageJob atomically claims the oldest pending job (flipping it to
	// running) and returns it, or (nil, nil) when the queue is empty.
	ClaimImageJob(ctx context.Context) (*ImageJob, error)

	// FinishImageJob records a claimed job's outcome, owning the
	// done/retry/failed decision and flagging variants_ready on success.
	FinishImageJob(ctx context.Context, id int64, jobErr error) error

	// ResetStaleJobs returns all running jobs to pending (startup recovery).
	ResetStaleJobs(ctx context.Context) error

	// EnqueueAnalysisJob inserts a pending media-analysis job (ffprobe + fpcalc)
	// for a file. Idempotent per file_id (at most one active job); a duplicate
	// enqueue is a no-op. See docs/architecture/recordings.md (P0). The worker
	// methods (ClaimAnalysisJob/FinishAnalysisJob/UpsertTechColumns/
	// InsertAudioFingerprint) live on *database.DB only, behind mediaproc's own
	// interface, so they do not widen this Repository.
	EnqueueAnalysisJob(ctx context.Context, fileID, now int64) error

	// ListDuplicateRecordings returns every recording with >1 non-trashed
	// rendition (the duplicates admin page; recordings P2), each with its
	// renditions' tech info + display fields.
	ListDuplicateRecordings(ctx context.Context) ([]DuplicateRecording, error)

	// SplitRendition detaches a file into a new pinned recording (the "save as
	// another composition" action). Found is false when no live file matches;
	// StrandedAppearances > 0 is a refusal (the split would take the recording's
	// last rendition and orphan appearances not read from that blob).
	SplitRendition(ctx context.Context, fileID int64) (SplitRenditionOutcome, error)

	// AbsorbRenditions keeps keepFileID's blob and absorbs absorbFileIDs into the
	// recording — their blobs soft-removed, their distinct appearances preserved,
	// redundant/nameless ones dropped (recording-tagsets P3). AbsorbOutcome.Found
	// is false (no error) on a stale selection (a non-live-rendition id).
	AbsorbRenditions(ctx context.Context, recordingID, keepFileID int64, absorbFileIDs []int64) (AbsorbOutcome, error)

	// BulkAbsorbKeepBest absorbs each recording's non-best live renditions into
	// its ladder-best in one transaction ("keep best" over a set); single-rendition
	// recordings are skipped. Returns recordings absorbed + renditions removed.
	BulkAbsorbKeepBest(ctx context.Context, recordingIDs []int64) (recordingsAbsorbed, renditionsRemoved int, err error)

	// --- Recording curation (/admin/library#recordings, recording-tagsets P5) ---

	// ListRecordings returns one page of the recordings admin listing (newest
	// first) with the primary appearance's display fields and the count chips.
	ListRecordings(ctx context.Context, opts RecordingListOptions) ([]RecordingRow, error)

	// CountRecordings returns the total matching the listing's filter + search.
	CountRecordings(ctx context.Context, opts RecordingListOptions) (int, error)

	// GetRecordingDetail loads one recording with both arms (renditions incl.
	// soft-removed blobs, appearances incl. trashed); nil when unknown.
	GetRecordingDetail(ctx context.Context, recordingID int64) (*RecordingDetail, error)

	// MergeRecordings folds the source recordings into the target: renditions
	// move pinned, appearances move with identity dedup (target's copy wins,
	// nameless dropped), sources removed. Found is false on a stale selection.
	MergeRecordings(ctx context.Context, targetID int64, sourceIDs []int64) (MergeOutcome, error)

	// MoveTagset re-homes an appearance onto another existing recording; the
	// refusals (last appearance, identity collision, same recording) are
	// outcomes, not errors.
	MoveTagset(ctx context.Context, tagsetID, targetRecordingID int64) (MoveTagsetOutcome, error)

	// SetPrimaryTagset makes the appearance the one naming its recording; found
	// is false when it does not belong to the recording.
	SetPrimaryTagset(ctx context.Context, recordingID, tagsetID int64) (bool, error)

	// CreateAppearance adds a hand-authored, blobless, approved appearance to an
	// existing recording (the /admin/library#recordings "Add appearance" form). The
	// refusals — unknown recording, nameless (meaningful rule), identity
	// collision, empty title — are outcomes, not errors.
	CreateAppearance(ctx context.Context, recordingID int64, in AppearanceInput, createdBy sql.NullInt64) (CreateAppearanceOutcome, error)

	// RestoreTagset clears one appearance's trash mark (the tagset-addressed
	// inverse of the per-appearance discard), so a trashed appearance whose
	// origin blob was absorbed/purged — unreachable from the hash-addressed
	// Trash — is restorable from the recordings view. found is false when no
	// trashed tagset matches the id.
	RestoreTagset(ctx context.Context, tagsetID int64) (bool, error)

	// HardDeleteTrashedTagset permanently removes one trashed appearance via the
	// purge composition (last one → the abandoned recording is reclaimed, files
	// and blobs included), returning the blobs to reclaim after commit. It
	// refuses a live appearance (Found && !Trashed) — permanent delete is
	// Trash-only.
	HardDeleteTrashedTagset(ctx context.Context, tagsetID int64) (HardDeleteTagsetOutcome, error)

	// TrashRecording soft-deletes every non-trashed appearance of the recording
	// (whole-recording Trash — dormant, fully restorable), returning the count
	// newly trashed; found is false for an unknown id.
	TrashRecording(ctx context.Context, recordingID int64) (appearances int, found bool, err error)

	// BulkTrashRecordings is TrashRecording over a set in one transaction
	// (unknown ids skipped).
	BulkTrashRecordings(ctx context.Context, recordingIDs []int64) (recordings, appearances int, err error)

	// HardDeleteRecording permanently removes the recording with all appearances
	// and files via the purge composition, returning the blobs to reclaim after
	// commit.
	HardDeleteRecording(ctx context.Context, recordingID int64) (RecordingDeleteOutcome, error)

	// Recordings perspective of Trash (gc-model.md): the recording-grain lens
	// over recordings wholly out of the library. List/Count force the "trashed"
	// membership; RestoreRecording un-trashes every appearance and restores a
	// rendition when dormant; the bulk variants back the Recordings bin's
	// "Restore selected" / "Delete selected" (one transaction each).
	ListTrashedRecordings(ctx context.Context, search string, limit, offset int) ([]RecordingRow, error)
	CountTrashedRecordings(ctx context.Context, search string) (int, error)

	// TrashedRecordingIDs resolves the Trash Recordings bin's "select all N"
	// set: every recording wholly out of the library.
	TrashedRecordingIDs(ctx context.Context) ([]int64, error)
	RestoreRecording(ctx context.Context, recordingID int64) (found bool, err error)
	BulkRestoreRecordings(ctx context.Context, recordingIDs []int64) (restored int, err error)
	BulkHardDeleteRecordings(ctx context.Context, recordingIDs []int64) (deleted int, blobs []DeletedBlob, err error)

	// SetRecordingAccess updates the recording-level license / guest-playable /
	// madnetwork share-depth fields (nil = unchanged; explicit guest sets the
	// manual override; see ShareDepthUpdate for the depth's three states).
	SetRecordingAccess(ctx context.Context, recordingID int64, license *string, guest *bool, depth ShareDepthUpdate) (bool, error)

	// RemoveRendition soft-removes a blob (files.deleted_at; last one → dormant
	// recording) and RestoreRendition brings it back — the renditions arm's
	// per-row actions. found is false when no matching live/removed file exists.
	RemoveRendition(ctx context.Context, fileID int64) (bool, error)
	RestoreRendition(ctx context.Context, fileID int64) (bool, error)

	// IsDuplicateSubmission reports whether the file duplicates already-approved
	// content (recordings P3): by fingerprint/recording when one exists, else a
	// non-default tag collision. Suppresses self-approve and flags the queue.
	IsDuplicateSubmission(ctx context.Context, hash string) (bool, error)

	// ClassifySubmission classifies the staged submission with the given tagset
	// id — case A/B/C, appearance-collision, and the ladder compare
	// (recording-tagsets P4). found is false for an unknown/trashed/approved id.
	ClassifySubmission(ctx context.Context, tagsetID int64) (SubmissionClass, bool, error)

	// RecordingRenditionsByTagsetID returns the surviving renditions of the
	// appearance's recording (the player's quality control), display fields
	// filled from the addressed tagset. An unknown/unavailable tagset yields
	// nil.
	RecordingRenditionsByTagsetID(ctx context.Context, tagsetID int64) ([]DuplicateRendition, error)

	// SetAlbumCover inserts/replaces an album cover row (keyed by albums.id) with
	// variant-tracking fields (variants_ready reset to 0).
	SetAlbumCover(ctx context.Context, albumID int64, imageHash, sourceExt, objectKey, mimeType string, now int64) error

	// SetAlbumCoverIfAbsent inserts an album cover row (keyed by albums.id) only
	// when none exists, reporting inserted=true exactly when this call created it.
	// Race-free fill-if-missing: it never overwrites an existing cover.
	SetAlbumCoverIfAbsent(ctx context.Context, albumID int64, imageHash, sourceExt, objectKey, mimeType string, now int64) (bool, error)

	// GetAlbumCoverStatus returns the variant-tracking state for an album cover;
	// found is false when no row exists.
	GetAlbumCoverStatus(ctx context.Context, albumID int64) (imageHash, sourceExt string, variantsReady, found bool, err error)

	// AlbumCoverByHash reports whether some album's cover is keyed by this
	// image hash and whether its variants are ready — the cover relay's
	// local-first check.
	AlbumCoverByHash(ctx context.Context, imageHash string) (sourceExt string, ready, found bool, err error)
	// HasAlbumCover reports whether an album_images row exists for the album entity.
	HasAlbumCover(ctx context.Context, albumID int64) (bool, error)

	// --- Base metadata editing (Phase 5: upload & covers) ---

	// UpdateFileMetadata writes the provided fields (nil = leave unchanged) onto
	// the tagset of the file with the given content hash and returns
	// the updated combined row. Returns ErrFileNotFound when no file matches, or an error
	// wrapping ErrInvalidMetadata when a numeric field carries a bad value.
	UpdateFileMetadata(ctx context.Context, hash string, p MetadataPatch) (*MediaMetadata, error)

	// UpdateTagsetMetadata writes the patch onto one appearance by tagset id and
	// returns the combined row (recording-tagsets P4 — the review / My-uploads
	// edit target). ErrFileNotFound when no tagset matches.
	UpdateTagsetMetadata(ctx context.Context, tagsetID int64, p MetadataPatch) (*MediaMetadata, error)

	// TagsetMetadataByID loads the editable metadata (tags + tech) for one
	// appearance, for the edit modal to prefill. ErrFileNotFound when unknown.
	TagsetMetadataByID(ctx context.Context, tagsetID int64) (*MediaMetadata, error)

	// FileMetadataByHash loads the editable metadata (tagset + tech) for the file
	// with the given content hash. Returns ErrFileNotFound when no file matches.
	FileMetadataByHash(ctx context.Context, hash string) (*MediaMetadata, error)

	// TagsetSuggestSubject loads the origin-blob identity + analysis facts behind
	// one appearance for the tag-suggestion endpoint
	// (docs/architecture/tag-suggestions.md). found is false on an unknown id.
	TagsetSuggestSubject(ctx context.Context, tagsetID int64) (*SuggestSubject, bool, error)

	// RecodeTagsetsText re-decodes each appearance's stored text tags with recode
	// (the bulk charset fix, docs/architecture/tag-suggestions.md); untouched
	// unless recode reports a change. A valid owner narrows the scope to that
	// user's own editable staging (the My-uploads path). Ids outside the scope
	// are reported in notFound, not fatal.
	RecodeTagsetsText(ctx context.Context, tagsetIDs []int64, owner sql.NullInt64, recode func(string) (string, bool)) (affected int, notFound []int64, err error)

	// --- Playlists & favorites (docs/api/playlists.md) ---
	// All playlist methods are scoped to userID; a playlist id belonging to a
	// different user yields ErrPlaylistNotFound (mapped to 404, never 403).

	// ListPlaylists returns the user's playlists with item counts (favorites
	// first). Does not create the favorites row.
	ListPlaylists(ctx context.Context, userID int64) ([]*Playlist, error)

	// EnsureFavoritesPlaylist returns the user's favorites playlist id,
	// creating it if absent. Idempotent.
	EnsureFavoritesPlaylist(ctx context.Context, userID int64) (int64, error)

	// CreatePlaylist creates a regular playlist, optionally seeded with local
	// items and/or remote madnetwork refs
	// (tagset ids then remote refs, in order). Any unknown/unavailable
	// appearance fails the whole create with ErrFileNotFound; a malformed
	// remote hash with ErrBadRemoteRef.
	CreatePlaylist(ctx context.Context, userID int64, name string, tagsetIDs []int64, remote []RemoteTrackRef) (*Playlist, error)

	// GetPlaylist returns the playlist and its items in order. Unavailable
	// appearances stay listed (Trashed=true); hard-deleted tagsets vanish via
	// FK cascade.
	GetPlaylist(ctx context.Context, userID, playlistID int64) (*Playlist, []*PlaylistItemEntry, error)

	// RenamePlaylist / DeletePlaylist operate on regular playlists only;
	// favorites returns ErrPlaylistSystem.
	RenamePlaylist(ctx context.Context, userID, playlistID int64, name string) error
	DeletePlaylist(ctx context.Context, userID, playlistID int64) error

	// AddPlaylistItems atomically appends tracks by tagset id and/or remote
	// madnetwork ref; any unknown/unavailable appearance fails the batch with
	// ErrFileNotFound, a malformed remote hash with ErrBadRemoteRef. On the
	// favorites playlist, already-present tracks are skipped.
	AddPlaylistItems(ctx context.Context, userID, playlistID int64, tagsetIDs []int64, remote []RemoteTrackRef) (added int, err error)

	// RemovePlaylistItem removes one item by its id; found is false (no error)
	// when the item is not in that playlist.
	RemovePlaylistItem(ctx context.Context, userID, playlistID, itemID int64) (found bool, err error)

	// ReorderPlaylist rewrites the item order; itemIDs must be a permutation of
	// the playlist's current item ids (ErrBadReorder otherwise).
	ReorderPlaylist(ctx context.Context, userID, playlistID int64, itemIDs []int64) error

	// ToggleFavorite flips the appearance's membership in the user's favorites
	// playlist (created on first use) and returns the resulting state.
	// Unknown or unavailable tagsets return ErrFileNotFound.
	ToggleFavorite(ctx context.Context, userID, tagsetID int64) (liked bool, err error)

	// ListFavoriteTagsetIDs returns the user's visible favorite tagset ids in
	// order.
	ListFavoriteTagsetIDs(ctx context.Context, userID int64) ([]int64, error)

	// ToggleRemoteFavorite flips a remote madnetwork track's membership in the
	// user's favorites (display text captured on first like); returns the
	// resulting state. A malformed hash returns ErrBadRemoteRef.
	ToggleRemoteFavorite(ctx context.Context, userID int64, ref RemoteTrackRef) (liked bool, err error)

	// ListFavoriteRemoteHashes returns the remote hashes in the user's
	// favorites, in order.
	ListFavoriteRemoteHashes(ctx context.Context, userID int64) ([]string, error)

	// RepointRemotePlaylistItems turns remote playlist rows whose hash now
	// lives in the library into local rows (or drops duplicates); returns the
	// number of rows handled. Idempotent.
	RepointRemotePlaylistItems(ctx context.Context) (int, error)
}
