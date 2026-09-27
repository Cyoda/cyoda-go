package auth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
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

// Issue creates a key pair and, with Invalidate, ends every sibling of its
// audience (spec §5.7). The new record is written before the siblings; if a
// sibling write fails, every write is undone.
func (s *KVKeyStore) Issue(ctx context.Context, req IssueRequest) (*KeyPair, error) {
	var issued *KeyPair
	var bootstrapTouched bool
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
			sib, touched, err := s.siblingWrites(ctx, req.Audience, kid, req.GracePeriodSec)
			if err != nil {
				return nil, false, err
			}
			writes, bootstrapTouched = append(writes, sib...), touched
		}
		if err := s.rep.writeAll(ctx, writes); err != nil {
			return nil, true, err
		}
		entries := s.classifyWrites(ctx, writes)
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
	if bootstrapTouched {
		s.logRevokedBootstrap(slog.LevelWarn)
	}
	return issued, nil
}

// siblingWrites lists the stored records and returns the writes that end the
// siblings of a rotation: owned and broken issued records of the audience
// whose window is open, and the bootstrap key of that audience unless it is
// deleted. Only the active flag and validTo change.
func (s *KVKeyStore) siblingWrites(ctx context.Context, audience, newKID string, grace int64) ([]kvWrite, bool, error) {
	all, err := s.kv.List(ctx, signingKeysNamespace)
	if err != nil {
		return nil, false, fmt.Errorf("failed to list signing keys: %w", err)
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
			return nil, false, err
		}
		writes = append(writes, kvWrite{key: k, value: b, prev: data})
	}
	sort.Slice(writes, func(i, j int) bool { return writes[i].key < writes[j].key })
	touched := false
	if s.boot.audience == audience {
		prev, present := all[s.boot.kid]
		rec := defaultBootstrapRecord(s.boot.kid)
		eligible := true
		var validTo *time.Time
		if present {
			e := s.cls.classify(ctx, s.boot.kid, prev)
			if e.class != classBootstrapState || e.deleted {
				eligible = false
			} else {
				rec, _, _, _, _ = decodeSigningRecord(s.boot.kid, prev)
				validTo = e.pair.ValidTo
			}
		}
		if eligible && windowOpen(validTo, now) {
			end(&rec, validTo)
			b, err := encodeSigningRecord(rec)
			if err != nil {
				return nil, false, err
			}
			writes = append(writes, kvWrite{key: s.boot.kid, value: b, prev: prev})
			touched = true
		}
	}
	return writes, touched, nil
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
// default record, prev nil). Everything else is not found.
func (s *KVKeyStore) changeable(ctx context.Context, kid string) ([]byte, signingRecord, error) {
	data, err := s.kv.Get(ctx, signingKeysNamespace, kid)
	if errors.Is(err, spi.ErrNotFound) {
		if kid == s.boot.kid {
			return nil, defaultBootstrapRecord(kid), nil
		}
		return nil, signingRecord{}, notFound(kid)
	}
	if err != nil {
		return nil, signingRecord{}, fmt.Errorf("failed to read signing key: %w", err)
	}
	e := s.cls.classify(ctx, kid, data)
	ok := (kid == s.boot.kid && e.class == classBootstrapState && !e.deleted) ||
		(kid != s.boot.kid && (e.class == classOwned || e.class == classBroken))
	if !ok {
		return nil, signingRecord{}, notFound(kid)
	}
	var rec signingRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return nil, signingRecord{}, notFound(kid)
	}
	return data, rec, nil
}

// updateState changes the active flag or the window of one record.
func (s *KVKeyStore) updateState(ctx context.Context, kid string, change func(r *signingRecord, now time.Time)) (*KeyPair, error) {
	var out *KeyPair
	err := s.rep.mutate(func() (func(map[string]*signingEntry), bool, error) {
		prev, rec, err := s.changeable(ctx, kid)
		if err != nil {
			return nil, false, err
		}
		change(&rec, time.Now())
		data, err := encodeSigningRecord(rec)
		if err != nil {
			return nil, false, err
		}
		if err := s.rep.writeAll(ctx, []kvWrite{{key: kid, value: data, prev: prev}}); err != nil {
			return nil, true, err
		}
		e := s.cls.classify(ctx, kid, data)
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
	_, err := s.updateState(ctx, kid, func(r *signingRecord, now time.Time) {
		var validTo *time.Time
		if r.ValidTo != nil {
			if t, perr := time.Parse(time.RFC3339Nano, *r.ValidTo); perr == nil {
				validTo = &t
			}
		}
		r.Active = false
		r.ValidTo = fmtTimePtr(graceExpiry(validTo, now, graceSec))
	})
	if err == nil && kid == s.boot.kid {
		s.logRevokedBootstrap(slog.LevelWarn)
	}
	return err
}

func (s *KVKeyStore) Reactivate(ctx context.Context, kid string, from, to time.Time) (*KeyPair, error) {
	return s.updateState(ctx, kid, func(r *signingRecord, _ time.Time) {
		r.Active = true
		r.ValidFrom = fmtTime(from)
		r.ValidTo = fmtTimePtr(&to)
	})
}

// Delete removes an issued key pair (owned, broken or undecodable), or marks
// the bootstrap key deleted — terminal: no API call removes that record. An
// undecodable record at the bootstrap KID is replaced by a deleted state.
func (s *KVKeyStore) Delete(ctx context.Context, kid string) error {
	if kid != s.boot.kid {
		return s.rep.mutate(func() (func(map[string]*signingEntry), bool, error) {
			data, err := s.kv.Get(ctx, signingKeysNamespace, kid)
			if errors.Is(err, spi.ErrNotFound) {
				return nil, false, notFound(kid)
			}
			if err != nil {
				return nil, false, fmt.Errorf("failed to read signing key: %w", err)
			}
			e := s.cls.classify(ctx, kid, data)
			if e.class != classOwned && e.class != classBroken && e.class != classUndecodable {
				return nil, false, notFound(kid)
			}
			if err := s.rep.writeAll(ctx, []kvWrite{{key: kid, prev: data}}); err != nil {
				return nil, true, err
			}
			return func(recs map[string]*signingEntry) { delete(recs, kid) }, true, nil
		})
	}
	err := s.rep.mutate(func() (func(map[string]*signingEntry), bool, error) {
		data, err := s.kv.Get(ctx, signingKeysNamespace, kid)
		rec := defaultBootstrapRecord(kid)
		switch {
		case errors.Is(err, spi.ErrNotFound):
			data = nil
		case err != nil:
			return nil, false, fmt.Errorf("failed to read signing key: %w", err)
		default:
			e := s.cls.classify(ctx, kid, data)
			if e.class == classBootstrapState && e.deleted {
				return nil, false, notFound(kid)
			}
			if e.class == classBootstrapState {
				rec, _, _, _, _ = decodeSigningRecord(kid, data)
			}
		}
		rec.Deleted, rec.Active = true, false
		enc, err := encodeSigningRecord(rec)
		if err != nil {
			return nil, false, err
		}
		if err := s.rep.writeAll(ctx, []kvWrite{{key: kid, value: enc, prev: data}}); err != nil {
			return nil, true, err
		}
		e := s.cls.classify(ctx, kid, enc)
		return func(recs map[string]*signingEntry) { recs[kid] = e }, true, nil
	})
	if err == nil {
		s.logRevokedBootstrap(slog.LevelWarn)
	}
	return err
}
