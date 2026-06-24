package auth

import (
	"crypto/rand"
	"crypto/rsa"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

func TestGetTrustedKeyByKID_Found(t *testing.T) {
	s := NewInMemoryTrustedKeyStore()
	priv, _ := rsa.GenerateKey(rand.Reader, 2048)
	tk := &TrustedKey{KID: "k1", TenantID: spi.TenantID("ta"), PublicKey: &priv.PublicKey, Audience: "human", Active: true, ValidFrom: time.Now()}
	_ = s.Register(tk, RotateOptions{})
	got, err := getTrustedKeyByKID(s, "k1")
	if err != nil || got.KID != "k1" {
		t.Fatalf("got=%+v err=%v", got, err)
	}
}

func TestGetTrustedKeyByKID_PastValidTo_NotFound(t *testing.T) {
	s := NewInMemoryTrustedKeyStore()
	priv, _ := rsa.GenerateKey(rand.Reader, 2048)
	past := time.Now().Add(-1 * time.Hour)
	tk := &TrustedKey{KID: "expired", TenantID: spi.TenantID("t"), PublicKey: &priv.PublicKey, Audience: "human", Active: false, ValidFrom: past, ValidTo: &past}
	_ = s.Register(tk, RotateOptions{})
	if _, err := getTrustedKeyByKID(s, "expired"); err == nil {
		t.Error("past-ValidTo should not surface")
	}
}

func TestGetTrustedKeyByKID_Missing(t *testing.T) {
	s := NewInMemoryTrustedKeyStore()
	if _, err := getTrustedKeyByKID(s, "missing"); err == nil {
		t.Error("expected not-found")
	}
}
