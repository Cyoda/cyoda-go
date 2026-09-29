package auth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// IssueRequest is a validated POST /oauth/keys/keypair.
type IssueRequest struct {
	Audience       string
	ValidFrom      time.Time
	ValidTo        time.Time
	Invalidate     bool
	GracePeriodSec int64
}

func newKID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("failed to generate key id: %w", err)
	}
	return hex.EncodeToString(b), nil
}

func notFound(kid string) error { return fmt.Errorf("%w: %s", ErrKeyPairNotFound, kid) }

// postWriteContext bounds the classification of a record whose write already
// committed: immune to the caller's cancellation (a request cancelled in its
// last moment must not mark the change broken in the copy), but not
// unbounded either, so a genuinely wedged classification (e.g. a vault
// callout that never returns) still gives up — the same restore-context
// pattern replica.go's writeAll uses to bound a compensating write.
func (s *KVKeyStore) postWriteContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), s.rep.cfg.interval)
}

// Issue creates a key pair and, with Invalidate, ends every issued sibling of
// its audience (spec §5.7). The new record is written before the siblings; if
// a sibling write fails, writeAll (replica.go) tries to undo every write
// already made — see its doc comment for what that guarantees and does not:
// a failed undo is logged at ERROR with the keys left changed, and a crash
// between writes can still leave a rotation half applied.
func (s *KVKeyStore) Issue(ctx context.Context, req IssueRequest) (*KeyPair, error) {
	var issued *KeyPair
	err := s.rep.mutate(func() (func(map[string]*signingEntry), bool, error) {
		kid, err := newKID()
		if err != nil {
			return nil, false, err
		}
		meta := KeyMeta{KID: kid, Audience: req.Audience, Algorithm: "RS256", Owner: s.vault.Owner()}
		spki, sealed, _, err := s.vault.Generate(ctx, meta)
		if err != nil {
			return nil, false, fmt.Errorf("failed to generate key pair: %w", err)
		}
		vt := req.ValidTo
		data, err := encodeSigningRecord(signingRecord{
			Kind: recordKindIssued, KID: kid, Audience: req.Audience, Algorithm: "RS256", Active: true,
			ValidFrom: fmtTime(req.ValidFrom), ValidTo: fmtTimePtr(&vt),
			PublicKey: base64.StdEncoding.EncodeToString(spki),
			Vault:     &vaultReference{Kind: s.vault.Kind(), Owner: s.vault.Owner(), Sealed: base64.StdEncoding.EncodeToString(sealed)},
		})
		if err != nil {
			return nil, false, err
		}
		writes := []kvWrite{{key: kid, value: data}}
		if req.Invalidate {
			sib, err := s.siblingWrites(ctx, req.Audience, kid, req.GracePeriodSec)
			if err != nil {
				return nil, false, err
			}
			writes = append(writes, sib...)
		}
		if err := s.rep.writeAll(ctx, writes); err != nil {
			return nil, true, err
		}
		pctx, cancel := s.postWriteContext(ctx)
		entries := s.classifyWrites(pctx, writes)
		cancel()
		p := entries[kid].pair
		issued = &p
		return func(recs map[string]*signingEntry) {
			for k, e := range entries {
				recs[k] = e
			}
		}, true, nil
	})
	if err != nil {
		return nil, err
	}
	return issued, nil
}

// siblingWrites lists the stored records and returns the writes that end the
// siblings of a rotation: owned and broken issued records of the audience
// whose window is open. The signing key from configuration is never a
// sibling; only an invalidate or delete that names its key id ends it. Only
// the active flag and validTo change.
func (s *KVKeyStore) siblingWrites(ctx context.Context, audience, newKID string, grace int64) ([]kvWrite, error) {
	all, err := s.kv.List(ctx, signingKeysNamespace)
	if err != nil {
		return nil, fmt.Errorf("failed to list signing keys: %w", err)
	}
	now := time.Now()
	end := func(r *signingRecord, validTo *time.Time) {
		r.Active = false
		r.ValidTo = fmtTimePtr(graceExpiry(validTo, now, grace))
	}
	var writes []kvWrite
	for k, data := range all {
		if k == newKID || k == s.boot.kid {
			continue
		}
		e := s.cls.classify(ctx, k, data)
		if (e.class != classOwned && e.class != classBroken) || e.pair.Audience != audience || !windowOpen(e.pair.ValidTo, now) {
			continue
		}
		rec, _, _, _, _ := decodeSigningRecord(k, data)
		end(&rec, e.pair.ValidTo)
		b, err := encodeSigningRecord(rec)
		if err != nil {
			return nil, err
		}
		writes = append(writes, kvWrite{key: k, value: b, prev: data})
	}
	sort.Slice(writes, func(i, j int) bool { return writes[i].key < writes[j].key })
	return writes, nil
}

func (s *KVKeyStore) classifyWrites(ctx context.Context, writes []kvWrite) map[string]*signingEntry {
	out := make(map[string]*signingEntry, len(writes))
	for _, w := range writes {
		out[w.key] = s.cls.classify(ctx, w.key, w.value)
	}
	return out
}

// changeable reads one record this node may change: an owned or broken issued
// key pair, or this node's bootstrap key unless deleted (absent state → the
// default record, prev nil). Everything else is not found. It returns the
// classified pair alongside the record so a caller never has to re-parse a
// stored timestamp itself.
func (s *KVKeyStore) changeable(ctx context.Context, kid string) ([]byte, signingRecord, KeyPair, error) {
	data, err := s.kv.Get(ctx, signingKeysNamespace, kid)
	if errors.Is(err, spi.ErrNotFound) {
		if kid == s.boot.kid {
			return nil, defaultBootstrapRecord(kid), KeyPair{KID: kid, Bootstrap: true, Algorithm: "RS256", Active: true}, nil
		}
		return nil, signingRecord{}, KeyPair{}, notFound(kid)
	}
	if err != nil {
		return nil, signingRecord{}, KeyPair{}, fmt.Errorf("failed to read signing key: %w", err)
	}
	e := s.cls.classify(ctx, kid, data)
	ok := (kid == s.boot.kid && e.class == classBootstrapState && !e.deleted) ||
		(kid != s.boot.kid && (e.class == classOwned || e.class == classBroken))
	if !ok {
		return nil, signingRecord{}, KeyPair{}, notFound(kid)
	}
	// classify already ran decodeSigningRecord successfully to reach one of
	// the classes above, so this repeat decode of the same bytes cannot fail.
	rec, pair, _, _, _ := decodeSigningRecord(kid, data)
	return data, rec, pair, nil
}

// updateState changes the active flag or the window of one record.
func (s *KVKeyStore) updateState(ctx context.Context, kid string, change func(r *signingRecord, pair KeyPair)) (*KeyPair, error) {
	var out *KeyPair
	err := s.rep.mutate(func() (func(map[string]*signingEntry), bool, error) {
		prev, rec, pair, err := s.changeable(ctx, kid)
		if err != nil {
			return nil, false, err
		}
		change(&rec, pair)
		data, err := encodeSigningRecord(rec)
		if err != nil {
			return nil, false, err
		}
		if err := s.rep.writeAll(ctx, []kvWrite{{key: kid, value: data, prev: prev}}); err != nil {
			return nil, true, err
		}
		pctx, cancel := s.postWriteContext(ctx)
		e := s.cls.classify(pctx, kid, data)
		cancel()
		p := e.pair
		if kid == s.boot.kid {
			p.Audience, p.PublicKey = s.boot.audience, s.boot.public
		}
		out = &p
		return func(recs map[string]*signingEntry) { recs[kid] = e }, true, nil
	})
	return out, err
}

func (s *KVKeyStore) Invalidate(ctx context.Context, kid string, graceSec int64) error {
	_, err := s.updateState(ctx, kid, func(r *signingRecord, pair KeyPair) {
		r.Active = false
		r.ValidTo = fmtTimePtr(graceExpiry(pair.ValidTo, time.Now(), graceSec))
	})
	if err == nil && kid == s.boot.kid {
		s.logRevokedBootstrap(slog.LevelWarn)
	}
	return err
}

func (s *KVKeyStore) Reactivate(ctx context.Context, kid string, from, to time.Time) (*KeyPair, error) {
	return s.updateState(ctx, kid, func(r *signingRecord, _ KeyPair) {
		r.Active = true
		r.ValidFrom = fmtTime(from)
		r.ValidTo = fmtTimePtr(&to)
	})
}

// deletedRecordFor marks rec's own KID deleted — terminal, verifies nothing.
func deletedRecordFor(rec signingRecord) signingRecord {
	rec.Deleted, rec.Active = true, false
	return rec
}

// deletedBootstrapRecord is the deleted bootstrap-state record Delete writes
// in place of a record it cannot decode, and in place of the bootstrap key's
// absent (never-touched) state: the same terminal shape either way.
func deletedBootstrapRecord(kid string) signingRecord {
	return deletedRecordFor(defaultBootstrapRecord(kid))
}

// writeRecord encodes rec, writes it in place of prev, and folds the change
// into the copy.
func (s *KVKeyStore) writeRecord(ctx context.Context, kid string, prev []byte, rec signingRecord) (func(map[string]*signingEntry), bool, error) {
	enc, err := encodeSigningRecord(rec)
	if err != nil {
		return nil, false, err
	}
	if err := s.rep.writeAll(ctx, []kvWrite{{key: kid, value: enc, prev: prev}}); err != nil {
		return nil, true, err
	}
	pctx, cancel := s.postWriteContext(ctx)
	e := s.cls.classify(pctx, kid, enc)
	cancel()
	return func(recs map[string]*signingEntry) { recs[kid] = e }, true, nil
}

// Delete removes an issued key pair (owned or broken). An undecodable record
// — at any key id — is instead replaced with a deleted bootstrap-state record,
// never removed outright: if that KID is some node's bootstrap key it stays
// revoked, and otherwise the record is a foreign bootstrap record every node
// already ignores (spec §5.5). Deleting this node's own bootstrap key marks
// its bootstrap state deleted — terminal: no API call removes that record. A
// record at a key that cannot be a key id is ignored, and not found here.
func (s *KVKeyStore) Delete(ctx context.Context, kid string) error {
	err := s.rep.mutate(func() (func(map[string]*signingEntry), bool, error) {
		data, err := s.kv.Get(ctx, signingKeysNamespace, kid)
		if errors.Is(err, spi.ErrNotFound) {
			if kid != s.boot.kid {
				return nil, false, notFound(kid)
			}
			return s.writeRecord(ctx, kid, nil, deletedBootstrapRecord(kid))
		}
		if err != nil {
			return nil, false, fmt.Errorf("failed to read signing key: %w", err)
		}
		e := s.cls.classify(ctx, kid, data)
		switch {
		case e.class == classUndecodable:
			return s.writeRecord(ctx, kid, data, deletedBootstrapRecord(kid))
		case kid == s.boot.kid:
			if e.class != classBootstrapState || e.deleted {
				return nil, false, notFound(kid)
			}
			rec, _, _, _, _ := decodeSigningRecord(kid, data)
			return s.writeRecord(ctx, kid, data, deletedRecordFor(rec))
		case e.class == classOwned || e.class == classBroken:
			if err := s.rep.writeAll(ctx, []kvWrite{{key: kid, prev: data}}); err != nil {
				return nil, true, err
			}
			return func(recs map[string]*signingEntry) { delete(recs, kid) }, true, nil
		default:
			return nil, false, notFound(kid)
		}
	})
	if err == nil && kid == s.boot.kid {
		s.logRevokedBootstrap(slog.LevelWarn)
	}
	return err
}
