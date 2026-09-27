package auth

import "testing"

// signerCached is a white-box accessor for the test below: it reaches into
// the classifier's signer cache directly rather than adding a production
// accessor whose only caller would be a test.
func signerCached(s *KVKeyStore, kid string) bool {
	s.cls.signers.mu.Lock()
	defer s.cls.signers.mu.Unlock()
	_, ok := s.cls.signers.m[kid]
	return ok
}

// A record deleted from the store and re-read must not leave its opened
// signer cached forever: retain() runs after every swap/apply alongside
// logClassChanges so a deleted (or retired, or reclassified) owned record's
// signer leaves memory instead of accumulating.
func TestKVKeyStore_RetainEvictsSignerOfDeletedRecord(t *testing.T) {
	ctx := replicaSystemCtx()
	kv := newReplicaKV(t)
	boot := loadFixtureKey(t)
	kid, err := DeriveKID(&boot.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	v, err := NewWrappedVault(boot, kid)
	if err != nil {
		t.Fatal(err)
	}
	if err := kv.Put(ctx, signingKeysNamespace, "issued-1", issuedRecord(t, v, "issued-1", "client")); err != nil {
		t.Fatal(err)
	}
	s, err := NewKVKeyStore(ctx, kv, KVKeyStoreConfig{Bootstrap: boot, BootstrapAudience: "client"})
	if err != nil {
		t.Fatal(err)
	}
	if !signerCached(s, "issued-1") {
		t.Fatal("expected the opened signer to be cached after the initial load")
	}
	if err := kv.Delete(ctx, signingKeysNamespace, "issued-1"); err != nil {
		t.Fatal(err)
	}
	if err := s.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if signerCached(s, "issued-1") {
		t.Fatal("expected the deleted record's cached signer to have been evicted")
	}
}
