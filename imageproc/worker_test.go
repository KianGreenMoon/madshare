package imageproc

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"os"
	"path/filepath"
	"testing"
	"time"

	"daemonlord.ygg/madshare/database"
	"daemonlord.ygg/madshare/media"
	"github.com/disintegration/imaging"
)

// writeOriginalJPEG writes a source original under sourceImagesDir/<hash>/original.jpg
// (the post-split source tree the worker reads from) and returns its full image hash.
func writeOriginalJPEG(t *testing.T, sourceImagesDir string) (imageHash string) {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, 320, 240))
	for y := 0; y < 240; y++ {
		for x := 0; x < 320; x++ {
			img.Set(x, y, color.NRGBA{R: 30, G: 160, B: 220, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := imaging.Encode(&buf, img, imaging.JPEG); err != nil {
		t.Fatalf("encode original: %v", err)
	}
	imageHash = media.ImageHash(buf.Bytes())
	dir := filepath.Join(sourceImagesDir, imageHash)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	dest := filepath.Join(dir, media.VariantOriginal+".jpg")
	if err := os.WriteFile(dest, buf.Bytes(), 0o644); err != nil {
		t.Fatalf("write original: %v", err)
	}
	return imageHash
}

func TestPool_ProcessesJob(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	db, err := database.Open(dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	sourceImagesDir := t.TempDir()
	variantsImagesDir := t.TempDir()
	ctx := context.Background()

	const (
		artist = "Artist"
		album  = "Album"
	)
	imageHash := writeOriginalJPEG(t, sourceImagesDir)

	objectKey := media.VariantPath(imageHash, media.VariantOriginal, ".jpg")
	albumID, err := db.ResolveAlbumID(ctx, artist, album)
	if err != nil {
		t.Fatalf("resolve album: %v", err)
	}
	if err := db.SetAlbumCover(ctx, albumID, imageHash, ".jpg", objectKey, "image/jpeg", time.Now().Unix()); err != nil {
		t.Fatalf("set album cover: %v", err)
	}
	if err := db.EnqueueImageJob(ctx, "album", artist+"\x1f"+album, imageHash, time.Now().Unix()); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	pool := NewPool(db, sourceImagesDir, variantsImagesDir, 1)
	go pool.Start(runCtx)
	pool.Notify()

	// Poll until the worker flips variants_ready, or time out.
	deadline := time.Now().Add(5 * time.Second)
	var ready bool
	for time.Now().Before(deadline) {
		_, _, r, found, err := db.GetAlbumCoverStatus(ctx, albumID)
		if err != nil {
			t.Fatalf("status: %v", err)
		}
		if found && r {
			ready = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	if !ready {
		t.Fatal("variants_ready never became true")
	}

	// Every derived variant file must exist under the variants tree...
	for _, name := range media.DerivedVariants {
		p := filepath.Join(variantsImagesDir, filepath.FromSlash(media.VariantPath(imageHash, name, ".jpg")))
		if _, err := os.Stat(p); err != nil {
			t.Errorf("variant %q not written: %v", name, err)
		}
	}
	// ...and the source original must NOT be written into the variants tree (it is
	// never served from there; it stays the unserved seed in the source tree).
	orig := filepath.Join(variantsImagesDir, imageHash, media.VariantOriginal+".jpg")
	if _, err := os.Stat(orig); err == nil {
		t.Errorf("source original leaked into variants tree at %s", orig)
	}
}
