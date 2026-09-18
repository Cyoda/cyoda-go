package memory

import (
	"os"
	"path"
	"strings"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// TestBlobName_IsUnspellable asserts the property the encoding buys: no tenant
// id and no message id, however hostile, can produce a name containing a path
// separator, a dot segment, or an upper-case byte. Traversal and case
// collision stop being rejections and become unrepresentable.
//
// The empty string is deliberately absent from the list. hex("") is "", so an
// empty input yields an empty segment no encoding can rescue. Neither input can
// be empty here: an empty tenant is refused by the factory before a
// MessageStore exists (TestMessageStore_EmptyTenantRefusedByFactory), and an
// empty id is refused by Save itself (TestMessageStore_SaveRejectsEmptyID).
func TestBlobName_IsUnspellable(t *testing.T) {
	hostile := []string{
		"../victim", "..", ".", "a/b", `a\b`, "a:b", "NUL", "COM1",
		"tenant\x00", "tenant\n", strings.Repeat("x", 300),
	}
	for _, tenant := range hostile {
		for _, id := range hostile {
			name := blobName(spi.TenantID(tenant), id)
			if strings.Count(name, "/") != 1 {
				t.Fatalf("blobName(%q, %q) = %q: want exactly one separator", tenant, id, name)
			}
			for _, seg := range strings.Split(name, "/") {
				if seg == "" || seg == "." || seg == ".." {
					t.Fatalf("blobName(%q, %q) = %q: degenerate segment %q", tenant, id, name, seg)
				}
				for i := 0; i < len(seg); i++ {
					if c := seg[i]; !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
						t.Fatalf("blobName(%q, %q) = %q: non-hex byte %q", tenant, id, name, c)
					}
				}
			}
		}
	}
}

// TestBlobName_DistinctTenantsNeverShareADirectory is the case-collision
// regression. On APFS and on Windows, "tenant-a" and "tenant-A" are one
// directory; os.Root does not see that, because it guarantees confinement, not
// distinctness. Hex encoding is what separates them.
func TestBlobName_DistinctTenantsNeverShareADirectory(t *testing.T) {
	pairs := [][2]string{
		{"tenant-a", "tenant-A"},
		{"SYSTEM", "system"},
		{"Acme", "aCME"},
	}
	for _, p := range pairs {
		a := path.Dir(blobName(spi.TenantID(p[0]), "id"))
		b := path.Dir(blobName(spi.TenantID(p[1]), "id"))
		if a == b {
			t.Fatalf("tenants %q and %q share directory %q", p[0], p[1], a)
		}
	}
}

// TestCreateTempBlob_ExclusiveAndCleanable pins the replacement for
// os.CreateTemp, which *os.Root does not provide: each call must return a
// distinct, newly created file that can be removed through the same root.
func TestCreateTempBlob_ExclusiveAndCleanable(t *testing.T) {
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	defer root.Close()

	const tenantDir = "abcdef"
	if err := root.MkdirAll(tenantDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		f, name, err := createTempBlob(root, tenantDir)
		if err != nil {
			t.Fatalf("createTempBlob: %v", err)
		}
		if seen[name] {
			t.Fatalf("createTempBlob returned a duplicate name %q", name)
		}
		seen[name] = true
		if !strings.HasPrefix(name, tenantDir+"/") {
			t.Fatalf("temp name %q is not under %q", name, tenantDir)
		}
		if err := f.Close(); err != nil {
			t.Fatalf("close: %v", err)
		}
		if err := root.Remove(name); err != nil {
			t.Fatalf("remove %q: %v", name, err)
		}
	}
}
