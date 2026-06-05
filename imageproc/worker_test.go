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

func writeOriginalJPEG(t *testing.T, imagesDir, baseKey string) {
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
	dir := filepath.Join(imagesDir, baseKey)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	dest := filepath.Join(dir, media.VariantOriginal+".jpg")
	if err := os.WriteFile(dest, buf.Bytes(), 0o644); err != nil {
		t.Fatalf("write original: %v", err)
	}
}

func TestPool_ProcessesJob(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	db, err := database.Open(dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	imagesDir := t.TempDir()
	ctx := context.Background()

	const (
		artist  = "Artist"
		album   = "Album"
		baseKey = "abc123def4567890"
	)
	writeOriginalJPEG(t, imagesDir, baseKey)

	objectKey := media.VariantPath(baseKey, media.VariantOriginal, ".jpg")
	if err := db.SetAlbumCover(ctx, artist, album, baseKey, ".jpg", objectKey, "image/jpeg", time.Now().Unix()); err != nil {
		t.Fatalf("set album cover: %v", err)
	}
	if err := db.EnqueueImageJob(ctx, "album", artist+"\x1f"+album, baseKey, time.Now().Unix()); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	pool := NewPool(db, imagesDir, 1)
	go pool.Start(runCtx)
	pool.Notify()

	// Poll until the worker flips variants_ready, or time out.
	deadline := time.Now().Add(5 * time.Second)
	var ready bool
	for time.Now().Before(deadline) {
		_, _, r, found, err := db.GetAlbumCoverStatus(ctx, artist, album)
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

	// Every non-original variant file must exist on disk.
	for _, name := range media.AllVariants {
		if name == media.VariantOriginal {
			continue
		}
		p := filepath.Join(imagesDir, filepath.FromSlash(media.VariantPath(baseKey, name, ".jpg")))
		if _, err := os.Stat(p); err != nil {
			t.Errorf("variant %q not written: %v", name, err)
		}
	}
}
