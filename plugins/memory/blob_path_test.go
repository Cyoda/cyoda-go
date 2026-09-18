package memory

import (
	"context"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
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
// regression, and it is the one that has to hold on every platform.
//
// The property is fold-distinctness, not mere inequality. Asserting a != b
// would follow from injectivity alone and would pass for the raw-name scheme
// this replaced, which is precisely the scheme under which "tenant-a" and
// "tenant-A" are ONE directory on APFS and on Windows — both release targets.
// EqualFold is what a case-insensitive filesystem does, so asserting the
// encoded names do not fold together states the real requirement, and it
// states it identically on Linux, where the raw names happen not to collide
// and a naive test is green against the bug.
//
// Hex output is lower-case only, so two distinct hex strings can never be
// EqualFold-equal — the encoding is what makes this hold rather than the
// filesystem's manners.
func TestBlobName_DistinctTenantsNeverShareADirectory(t *testing.T) {
	pairs := [][2]string{
		{"tenant-a", "tenant-A"},
		{"SYSTEM", "system"},
		{"Acme", "aCME"},
	}
	for _, p := range pairs {
		a := path.Dir(blobName(spi.TenantID(p[0]), "id"))
		b := path.Dir(blobName(spi.TenantID(p[1]), "id"))
		if strings.EqualFold(a, b) {
			t.Fatalf("tenants %q and %q fold to the same directory %q/%q", p[0], p[1], a, b)
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

// blobTenantCtx is this package's internal-test equivalent of the memory_test
// helper of the same shape. The tests below need StoreFactory.blobDir, which
// only a test in package memory can reach.
func blobTenantCtx(tid spi.TenantID) context.Context {
	uc := &spi.UserContext{
		UserID: "test-user",
		Tenant: spi.Tenant{ID: tid, Name: string(tid)},
		Roles:  []string{"USER"},
	}
	return spi.WithUserContext(context.Background(), uc)
}

// TestMessageStore_HostileTenantAndIDRoundTrip replaces the traversal-rejection
// test. Under hex naming these ids and tenants are no longer rejected — they
// are ordinary byte strings that round-trip and stay in their own directory.
func TestMessageStore_HostileTenantAndIDRoundTrip(t *testing.T) {
	f := NewStoreFactory()
	defer f.Close()

	hostile := []string{"../escape", "..", ".", "a/../../escape", `a\b`, "a:b"}
	for _, tenant := range hostile {
		for _, id := range hostile {
			ctx := blobTenantCtx(spi.TenantID(tenant))
			store, err := f.MessageStore(ctx)
			if err != nil {
				t.Fatalf("MessageStore(%q): %v", tenant, err)
			}
			payload := tenant + "|" + id
			if err := store.Save(ctx, id, spi.MessageHeader{}, spi.MessageMetaData{},
				strings.NewReader(payload)); err != nil {
				t.Fatalf("Save(%q,%q): %v", tenant, id, err)
			}
			_, _, rc, err := store.Get(ctx, id)
			if err != nil {
				t.Fatalf("Get(%q,%q): %v", tenant, id, err)
			}
			got, _ := io.ReadAll(rc)
			rc.Close()
			if string(got) != payload {
				t.Errorf("Get(%q,%q) = %q, want %q", tenant, id, got, payload)
			}
		}
	}

	// Every regular file lives exactly two levels down: <hex tenant>/<hex id>.
	//
	// This is NOT a confinement proof, and must not be read as one. A blob
	// that genuinely escaped would land outside blobDir, where WalkDir rooted
	// at blobDir never looks. What it catches is sub-directory nesting — a
	// name that grew a separator and buried a blob one level deeper, inside
	// the root but outside the tenant's own directory. Confinement itself is
	// os.Root's job, enforced by the kernel on every openat/renameat, and is
	// not something a test in this process can usefully re-check.
	root := f.blobDir
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, relErr := filepath.Rel(root, p)
		if relErr != nil {
			return relErr
		}
		if depth := len(strings.Split(rel, string(filepath.Separator))); depth != 2 {
			t.Errorf("blob at depth %d: %q", depth, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
}

// TestMessageStore_CaseOnlyTenantsStayDistinct is the end-to-end demonstration
// of the regression: before hex naming, these two tenants shared a directory
// and tenant-a read back tenant-A's bytes — a cross-tenant read, not merely a
// lost write.
//
// It only EXHIBITS the bug on a case-insensitive filesystem (APFS, Windows).
// On Linux/ext4 the old raw-name scheme gave the two tenants separate
// directories, so this test passes against the buggy code there — a green here
// on CI is not proof that the bug is gone. The platform-independent guard is
// TestBlobName_DistinctTenantsNeverShareADirectory above, which asserts
// fold-distinctness of the encoded names; this one shows what that prevents.
func TestMessageStore_CaseOnlyTenantsStayDistinct(t *testing.T) {
	f := NewStoreFactory()
	defer f.Close()

	const id = "shared-id"
	cases := []struct{ tenant, payload string }{
		{"tenant-a", "payload-lower"},
		{"tenant-A", "payload-upper"},
	}
	for _, tc := range cases {
		ctx := blobTenantCtx(spi.TenantID(tc.tenant))
		store, err := f.MessageStore(ctx)
		if err != nil {
			t.Fatalf("MessageStore(%q): %v", tc.tenant, err)
		}
		if err := store.Save(ctx, id, spi.MessageHeader{}, spi.MessageMetaData{},
			strings.NewReader(tc.payload)); err != nil {
			t.Fatalf("Save(%q): %v", tc.tenant, err)
		}
	}

	for _, tc := range cases {
		ctx := blobTenantCtx(spi.TenantID(tc.tenant))
		store, err := f.MessageStore(ctx)
		if err != nil {
			t.Fatalf("MessageStore(%q): %v", tc.tenant, err)
		}
		_, _, rc, err := store.Get(ctx, id)
		if err != nil {
			t.Fatalf("Get(%q): %v", tc.tenant, err)
		}
		got, _ := io.ReadAll(rc)
		rc.Close()
		if string(got) != tc.payload {
			t.Fatalf("tenant %q read %q, want %q — the two tenants share a blob",
				tc.tenant, got, tc.payload)
		}
	}
}
