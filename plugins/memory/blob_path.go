package memory

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// Blob names are hex-encoded rather than validated.
//
// The tenant id and the message id both become path segments, and a check that
// rejects bad spellings cannot cover every way two names collide. The one that
// bit us was case: on APFS and on Windows "tenant-a" and "tenant-A" are the
// same directory, so two tenants shared a blob namespace. os.Root does not see
// that either — it guarantees confinement, not distinctness.
//
// Hex output is drawn from [0-9a-f], so a separator, a dot segment, an empty
// segment, a NUL, a Windows reserved device name and a case-only difference
// are all unrepresentable rather than rejected. The mapping is injective, so
// distinct inputs always name distinct files.
//
// The cost is that each segment doubles in length. A tenant is capped at 100
// bytes where it enters the engine, giving a 200-byte directory name; a
// message id is usable to 127 bytes before the 255-byte filename limit. Ids
// are server-generated time UUIDs (36 bytes), so that is slack rather than a
// constraint, and an over-long id surfaces as an ordinary write error.

// tenantBlobDir returns the root-relative directory holding one tenant's blobs.
func tenantBlobDir(tenant spi.TenantID) string {
	return hex.EncodeToString([]byte(tenant))
}

// blobName returns the root-relative path of one message blob.
func blobName(tenant spi.TenantID, id string) string {
	return tenantBlobDir(tenant) + "/" + hex.EncodeToString([]byte(id))
}

// tempBlobAttempts bounds the exclusive-create retry. A collision needs two
// callers to draw the same 128 random bits, so anything beyond a couple of
// attempts means the filesystem is refusing writes for another reason and
// spinning would hide it.
const tempBlobAttempts = 10

// createTempBlob is the *os.Root replacement for os.CreateTemp, which the
// rooted API does not provide. It returns an open handle and the file's
// root-relative name; the caller closes the handle and, on any failure after
// this point, removes the name through the same root.
//
// The "tmp-" prefix contains a byte hex never emits, so a half-written temp
// file can never occupy the name of a real blob.
func createTempBlob(root *os.Root, dir string) (*os.File, string, error) {
	var suffix [16]byte
	for attempt := 0; attempt < tempBlobAttempts; attempt++ {
		if _, err := rand.Read(suffix[:]); err != nil {
			return nil, "", fmt.Errorf("failed to draw temp blob suffix: %w", err)
		}
		name := dir + "/tmp-" + hex.EncodeToString(suffix[:])
		f, err := root.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			return f, name, nil
		}
		if !os.IsExist(err) {
			return nil, "", fmt.Errorf("failed to create temp blob file: %w", err)
		}
	}
	return nil, "", fmt.Errorf("failed to create temp blob file: %d name collisions", tempBlobAttempts)
}
