package memory

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/chain"
	"github.com/kryptokrates/nektar/internal/core"
	"github.com/kryptokrates/nektar/internal/store"
)

// LoadOrCreateProtectedKey loads or creates a 32-byte key in SQLite wrapped at rest via BRC-43
// wallet EncryptAtRest/DecryptAtRest (transparently migrating any legacy plaintext key, failing closed
// on encryption errors so fresh keys are never persisted in plaintext SQLite/WAL).
func (s *Service) LoadOrCreateProtectedKey(name string, gen func() ([]byte, error)) ([]byte, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("memory: db unavailable")
	}
	var encBlob struct {
		EncHex string `json:"encHex"`
	}
	if ok, _ := s.db.GetJSON(name+"_enc", &encBlob); ok && encBlob.EncHex != "" {
		if s.port == nil {
			return nil, fmt.Errorf("memory: encrypted key %q requires active wallet port", name)
		}
		ct, err := hex.DecodeString(encBlob.EncHex)
		if err != nil {
			return nil, fmt.Errorf("memory: decode encrypted key %q: %w", name, err)
		}
		pt, err := s.port.DecryptAtRest(context.Background(), name, ct)
		if err != nil || len(pt) != 32 {
			return nil, fmt.Errorf("memory: decrypt protected key %q: %w", name, err)
		}
		return pt, nil
	}
	// Check for legacy plaintext key to migrate (never create a new plaintext key if s.port != nil).
	var legacyHex string
	if ok, _ := s.db.GetJSON(name, &legacyHex); ok && len(legacyHex) == 64 {
		if raw, err := hex.DecodeString(legacyHex); err == nil && len(raw) == 32 {
			if s.port != nil {
				ct, encErr := s.port.EncryptAtRest(context.Background(), name, raw)
				if encErr != nil {
					core.ZeroBytes(raw)
					return nil, fmt.Errorf("memory: migrate key %q at rest: %w", name, encErr)
				}
				if err := s.db.SetJSON(name+"_enc", map[string]string{"encHex": hex.EncodeToString(ct)}); err != nil {
					core.ZeroBytes(raw)
					return nil, err
				}
				_ = s.db.SetJSON(name, map[string]string{"migrated": "encrypted"})
			}
			return raw, nil
		}
	}
	if s.port != nil {
		created, err := gen()
		if err != nil {
			return nil, err
		}
		if len(created) != 32 {
			core.ZeroBytes(created)
			return nil, fmt.Errorf("memory: generated key %q must be 32 bytes", name)
		}
		ct, encErr := s.port.EncryptAtRest(context.Background(), name, created)
		if encErr != nil {
			core.ZeroBytes(created)
			return nil, fmt.Errorf("memory: encrypt new key %q at rest: %w", name, encErr)
		}
		if err := s.db.SetJSON(name+"_enc", map[string]string{"encHex": hex.EncodeToString(ct)}); err != nil {
			core.ZeroBytes(created)
			return nil, err
		}
		return created, nil
	}
	return s.db.LoadOrCreateStableKey(name, gen)
}

type dbGroupStore struct {
	db   *store.DB
	port *chain.Port
}

type sealedEpochsBlob struct {
	EncHex string `json:"encHex"`
}

func (g *dbGroupStore) LoadBorgGroups() ([]*bstack.BorgGroup, error) {
	rows, err := g.db.ListBorgGroups()
	if err != nil {
		return nil, err
	}
	out := make([]*bstack.BorgGroup, 0, len(rows))
	for _, r := range rows {
		bg := &bstack.BorgGroup{
			GroupID:      r.GroupID,
			Name:         r.Name,
			CurrentEpoch: r.CurrentEpoch,
			Members:      make(map[string]string),
			Revoked:      make(map[string]int64),
			Epochs:       make(map[int64][32]byte),
			EpochMembers: make(map[int64][]string),
		}
		_ = json.Unmarshal(r.MembersJSON, &bg.Members)
		_ = json.Unmarshal(r.RevokedJSON, &bg.Revoked)
		_ = json.Unmarshal(r.EpochMembersJSON, &bg.EpochMembers)

		epJSON := r.EpochsJSON
		var wrapped sealedEpochsBlob
		if json.Unmarshal(r.EpochsJSON, &wrapped) == nil && wrapped.EncHex != "" {
			if g.port == nil {
				return nil, fmt.Errorf("borg: encrypted epoch keys for group %q require active wallet port", r.GroupID)
			}
			ct, dErr := hex.DecodeString(wrapped.EncHex)
			if dErr != nil {
				return nil, fmt.Errorf("borg: decode encrypted epochs for group %q: %w", r.GroupID, dErr)
			}
			pt, decErr := g.port.DecryptAtRest(context.Background(), "borg-group:"+r.GroupID, ct)
			if decErr != nil {
				return nil, fmt.Errorf("borg: decrypt epochs for group %q: %w", r.GroupID, decErr)
			}
			epJSON = pt
			defer core.ZeroBytes(pt)
		}
		var hexEpochs map[int64]string
		if json.Unmarshal(epJSON, &hexEpochs) == nil {
			for ep, hx := range hexEpochs {
				if raw, decErr := hex.DecodeString(hx); decErr == nil && len(raw) == 32 {
					var k [32]byte
					copy(k[:], raw)
					core.ZeroBytes(raw)
					bg.Epochs[ep] = k
				}
			}
		}
		out = append(out, bg)
	}
	return out, nil
}

func (g *dbGroupStore) SaveBorgGroup(bg *bstack.BorgGroup) error {
	memJSON, _ := json.Marshal(bg.Members)
	revJSON, _ := json.Marshal(bg.Revoked)
	epMemJSON, _ := json.Marshal(bg.EpochMembers)
	hexEpochs := make(map[int64]string, len(bg.Epochs))
	for ep, k := range bg.Epochs {
		hexEpochs[ep] = hex.EncodeToString(k[:])
	}
	epJSON, _ := json.Marshal(hexEpochs)
	defer core.ZeroBytes(epJSON)
	persistEpochs := epJSON
	if g.port != nil {
		ct, err := g.port.EncryptAtRest(context.Background(), "borg-group:"+bg.GroupID, epJSON)
		if err != nil {
			// Fail closed: never fall back to persisting plaintext Borg epoch symmetric keys in SQLite.
			return fmt.Errorf("borg: encrypt epoch keys at rest for group %q: %w", bg.GroupID, err)
		}
		persistEpochs, _ = json.Marshal(sealedEpochsBlob{EncHex: hex.EncodeToString(ct)})
	}
	return g.db.SaveBorgGroup(store.BorgGroupRecord{
		GroupID:          bg.GroupID,
		Name:             bg.Name,
		CurrentEpoch:     bg.CurrentEpoch,
		MembersJSON:      memJSON,
		RevokedJSON:      revJSON,
		EpochsJSON:       persistEpochs,
		EpochMembersJSON: epMemJSON,
	})
}
