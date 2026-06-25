package storage

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"runtime"
)

// memBufferLimit is the file size below which uploads are hashed entirely in
// memory. Above this threshold the upload is spooled to spoolDir so heap
// pressure stays bounded. Computed once at startup from heap headroom,
// capped at 50 MB.
var memBufferLimit = func() int64 {
	const hardCap = 50 << 20
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	avail := int64(ms.HeapIdle) / 4
	if avail <= 0 || avail > hardCap {
		return hardCap
	}
	return avail
}()

// HashUpload computes the SHA-256 of r. Files with declaredSize <= memBufferLimit
// are read entirely into memory. Larger files are spooled to a temp file in
// spoolDir so heap usage stays bounded.
//
// The returned cleanup func (if non-nil) must be deferred by the caller to
// remove the temp file once it is no longer needed.
func HashUpload(r io.Reader, declaredSize int64, spoolDir string) (
	hash string, content io.Reader, size int64, cleanup func(), err error,
) {
	h := sha256.New()

	if declaredSize <= memBufferLimit {
		buf, readErr := io.ReadAll(io.TeeReader(r, h))
		if readErr != nil {
			err = readErr
			return
		}
		hash = hex.EncodeToString(h.Sum(nil))
		content = bytes.NewReader(buf)
		size = int64(len(buf))
		return
	}

	// Large file: spool to a temp file while computing the hash.
	if err = os.MkdirAll(spoolDir, 0755); err != nil {
		return
	}
	tmp, tmpErr := os.CreateTemp(spoolDir, "upload-*")
	if tmpErr != nil {
		err = tmpErr
		return
	}
	cleanup = func() {
		tmp.Close()
		os.Remove(tmp.Name())
	}

	n, copyErr := io.Copy(io.MultiWriter(tmp, h), r)
	if copyErr != nil {
		err = copyErr
		return
	}
	if _, seekErr := tmp.Seek(0, io.SeekStart); seekErr != nil {
		err = seekErr
		return
	}

	hash = hex.EncodeToString(h.Sum(nil))
	content = tmp
	size = n
	return
}
