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
	"github.com/kryptokrates/nektar/internal/lattice"
	"github.com/kryptokrates/nektar/internal/store"
)

type dbLatticeStore struct {
	db *store.DB
}

func (l *dbLatticeStore) LoadLatticeStates() ([]lattice.ShardState, error) {
	rows, err := l.db.LoadLatticeStates()
	if err != nil {
		return nil, err
	}
	out := make([]lattice.ShardState, len(rows))
	for i, r := range rows {
		edges := make([]lattice.DomainEdge, len(r.DomainEdges))
		for j, e := range r.DomainEdges {
			edges[j] = lattice.DomainEdge{Parent: e.Parent, Child: e.Child}
		}
		out[i] = lattice.ShardState{
			Shard:       r.Shard,
			Epoch:       r.Epoch,
			DomainEdges: edges,
		}
	}
	return out, nil
}

func (l *dbLatticeStore) SaveLatticeState(st lattice.ShardState) error {
	edges := make([]store.LatticeDomainEdge, len(st.DomainEdges))
	for i, e := range st.DomainEdges {
		edges[i] = store.LatticeDomainEdge{Parent: e.Parent, Child: e.Child}
	}
	return l.db.SaveLatticeState(store.LatticeShardState{
		Shard:       st.Shard,
		Epoch:       st.Epoch,
		DomainEdges: edges,
	})
}

// LoadOrCreateProtectedKey loads or creates a 32-byte key in SQLite wrapped at rest via BRC-43
// wallet EncryptAtRest/DecryptAtRest (transparently migrating any legacy plaintext key, failing closed
// on read or encryption errors so existing keys are never overwritten and fresh keys are never
// persisted in plaintext SQLite/WAL). Serialized under s.keyMu to eliminate concurrent TOCTOU races.
func (s *Service) LoadOrCreateProtectedKey(name string, gen func() ([]byte, error)) ([]byte, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("memory: db unavailable")
	}
	if name == "" || gen == nil {
		return nil, errors.New("memory: key name and generator are required")
	}
	s.keyMu.Lock()
	defer s.keyMu.Unlock()

	var encBlob struct {
		EncHex string `json:"encHex"`
	}
	ok, err := s.db.GetJSON(name+"_enc", &encBlob)
	if err != nil {
		return nil, fmt.Errorf("memory: read encrypted key %q: %w", name, err)
	}
	if ok {
		if encBlob.EncHex == "" {
			return nil, fmt.Errorf("memory: corrupt encrypted key %q (empty encHex)", name)
		}
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
	return nil, fmt.Errorf("memory: chain port required for encrypted key storage (%s)", name)
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

		var wrapped sealedEpochsBlob
		if err := json.Unmarshal(r.EpochsJSON, &wrapped); err != nil || wrapped.EncHex == "" {
			return nil, fmt.Errorf("borg: epochs for group %q must be sealed at rest (encHex required)", r.GroupID)
		}
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
		defer core.ZeroBytes(pt)
		var hexEpochs map[int64]string
		if json.Unmarshal(pt, &hexEpochs) == nil {
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
	if g.port == nil {
		return fmt.Errorf("borg: wallet port required to encrypt epoch keys for group %q", bg.GroupID)
	}
	ct, err := g.port.EncryptAtRest(context.Background(), "borg-group:"+bg.GroupID, epJSON)
	if err != nil {
		// Fail closed: never fall back to persisting plaintext Borg epoch symmetric keys in SQLite.
		return fmt.Errorf("borg: encrypt epoch keys at rest for group %q: %w", bg.GroupID, err)
	}
	persistEpochs, _ := json.Marshal(sealedEpochsBlob{EncHex: hex.EncodeToString(ct)})
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
