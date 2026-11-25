package bstack

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/lightwebinc/bcommon/keyed"
)

const ShardKeyDomain = "nektar:bsecret:shard:v1"

// SealedEnvelope is a BRC-369 keyed content envelope (bcommon/keyed) wrapped to a borg group epoch.
type SealedEnvelope struct {
	ContentID     string `json:"contentId"`     // 32-byte hex content identifier
	KeyCommitment string `json:"keyCommitment"` // BRC-369 keyed.Commitment(k)
	Salt          string `json:"salt"`          // 4-byte BRC-369 segment salt hex
	Ciphertext    string `json:"ciphertext"`    // BRC-369 sealed segment hex
	EpochWrap     string `json:"epochWrap"`     // content key wrapped under borg group epoch symmetric key
	GroupID       string `json:"groupId"`
	Epoch         int64  `json:"epoch"`
}

// SealForEpoch encrypts plaintext under a fresh BRC-369 random content scalar k,
// and wraps k under the borg group's epoch symmetric key using bcommon/keyed.
func SealForEpoch(groupID string, epoch int64, epochSym, contentID [32]byte, plaintext []byte) (*SealedEnvelope, [32]byte, error) {
	k, err := keyed.SampleKey(rand.Reader)
	if err != nil {
		return nil, [32]byte{}, fmt.Errorf("vault: sample BRC-369 key: %w", err)
	}
	salt, err := keyed.SampleSalt(rand.Reader)
	if err != nil {
		return nil, [32]byte{}, fmt.Errorf("vault: sample salt: %w", err)
	}
	ct, err := keyed.SealSegment(k, salt, plaintext)
	if err != nil {
		return nil, [32]byte{}, fmt.Errorf("vault: seal segment: %w", err)
	}
	commit := keyed.Commitment(k)
	wrap, err := keyed.WrapToEpoch(ShardKeyDomain, epochSym, contentID, k, rand.Reader)
	if err != nil {
		return nil, [32]byte{}, fmt.Errorf("vault: wrap to epoch: %w", err)
	}
	return &SealedEnvelope{
		ContentID:     hex.EncodeToString(contentID[:]),
		KeyCommitment: hex.EncodeToString(commit[:]),
		Salt:          hex.EncodeToString(salt[:]),
		Ciphertext:    hex.EncodeToString(ct),
		EpochWrap:     hex.EncodeToString(wrap),
		GroupID:       groupID,
		Epoch:         epoch,
	}, k, nil
}

// OpenWithEpoch unwraps the BRC-369 content key using epochSym (verifying against KeyCommitment),
// then decrypts the sealed segment.
func OpenWithEpoch(env *SealedEnvelope, epochSym [32]byte) ([]byte, error) {
	cidBytes, err := hex.DecodeString(env.ContentID)
	if err != nil || len(cidBytes) != 32 {
		return nil, errors.New("vault: invalid contentId")
	}
	commitBytes, err := hex.DecodeString(env.KeyCommitment)
	if err != nil || len(commitBytes) != 32 {
		return nil, errors.New("vault: invalid keyCommitment")
	}
	saltBytes, err := hex.DecodeString(env.Salt)
	if err != nil || len(saltBytes) != keyed.SaltLen {
		return nil, errors.New("vault: invalid salt")
	}
	ct, err := hex.DecodeString(env.Ciphertext)
	if err != nil {
		return nil, err
	}
	wrap, err := hex.DecodeString(env.EpochWrap)
	if err != nil {
		return nil, err
	}
	var cid, commit [32]byte
	var salt [keyed.SaltLen]byte
	copy(cid[:], cidBytes)
	copy(commit[:], commitBytes)
	copy(salt[:], saltBytes)

	k, err := keyed.UnwrapFromEpoch(ShardKeyDomain, epochSym, cid, wrap, commit)
	if err != nil {
		return nil, fmt.Errorf("vault: BRC-369 unwrap/commitment check failed: %w", err)
	}
	return keyed.OpenSegment(k, salt, ct)
}

// BorgGroup models a borg-aligned access group for shared Nektar memory shards.
// Removing any member forces an epoch bump with a brand-new symmetric key (bsecret revocation pattern),
// permanently retiring custom SMT revocation shards.
type BorgGroup struct {
	GroupID      string              `json:"groupId"`
	Name         string              `json:"name"`
	CurrentEpoch int64               `json:"currentEpoch"`
	Members      map[string]string   `json:"members"` // pubHex -> role (owner|writer|reader)
	Revoked      map[string]int64    `json:"revoked"` // pubHex -> revokedAt unix
	Epochs       map[int64][32]byte  `json:"-"`       // epoch -> symmetric key (kept local/wrapped)
	EpochMembers map[int64][]string  `json:"epochMembers"`
}

// GroupPersistence persists BorgGroup state + epoch keys across restarts.
type GroupPersistence interface {
	LoadBorgGroups() ([]*BorgGroup, error)
	SaveBorgGroup(g *BorgGroup) error
}

// GroupRegistry manages borg groups & epoch keys for Nektar shards.
type GroupRegistry struct {
	mu      sync.RWMutex
	groups  map[string]*BorgGroup
	persist GroupPersistence
}

func NewGroupRegistry() *GroupRegistry {
	return &GroupRegistry{groups: make(map[string]*BorgGroup)}
}

// SetPersistence attaches a durable store, loading any previously saved groups immediately.
func (r *GroupRegistry) SetPersistence(p GroupPersistence) error {
	if p == nil {
		return nil
	}
	loaded, err := p.LoadBorgGroups()
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.persist = p
	for _, g := range loaded {
		if _, exists := r.groups[g.GroupID]; !exists {
			r.groups[g.GroupID] = g
		}
	}
	return nil
}

// CreateGroup creates a new borg group at epoch 1 with ownerPub as the initial member.
func (r *GroupRegistry) CreateGroup(groupID, name, ownerPub string) (*BorgGroup, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.groups[groupID]; exists {
		return nil, fmt.Errorf("borg: group %q already exists", groupID)
	}
	var sym [32]byte
	if _, err := rand.Read(sym[:]); err != nil {
		return nil, err
	}
	g := &BorgGroup{
		GroupID:      groupID,
		Name:         name,
		CurrentEpoch: 1,
		Members:      map[string]string{ownerPub: "owner"},
		Revoked:      make(map[string]int64),
		Epochs:       map[int64][32]byte{1: sym},
		EpochMembers: map[int64][]string{1: {ownerPub}},
	}
	if r.persist != nil {
		if err := r.persist.SaveBorgGroup(g); err != nil {
			return nil, fmt.Errorf("borg: persist group: %w", err)
		}
	}
	r.groups[groupID] = g
	return g, nil
}

// AddMember grants pubHex membership in the current epoch (deduplicated).
// Use AddMemberWithRotation when newly added members must NOT have access to prior fragments in the current epoch.
func (r *GroupRegistry) AddMember(groupID, pubHex, role string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	g, ok := r.groups[groupID]
	if !ok {
		return fmt.Errorf("borg: unknown group %q", groupID)
	}
	if role == "" {
		role = "reader"
	}
	g.Members[pubHex] = role
	delete(g.Revoked, pubHex)
	exists := false
	for _, m := range g.EpochMembers[g.CurrentEpoch] {
		if m == pubHex {
			exists = true
			break
		}
	}
	if !exists {
		g.EpochMembers[g.CurrentEpoch] = append(g.EpochMembers[g.CurrentEpoch], pubHex)
	}
	if r.persist != nil {
		if err := r.persist.SaveBorgGroup(g); err != nil {
			return fmt.Errorf("borg: persist add member: %w", err)
		}
	}
	return nil
}

// AddMemberWithRotation grants pubHex membership AND rotates to a fresh epoch with a new
// symmetric key so newly added members cannot read prior-epoch ciphertext (backward secrecy).
func (r *GroupRegistry) AddMemberWithRotation(groupID, pubHex, role string) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	g, ok := r.groups[groupID]
	if !ok {
		return 0, fmt.Errorf("borg: unknown group %q", groupID)
	}
	if role == "" {
		role = "reader"
	}
	var nextSym [32]byte
	if _, err := rand.Read(nextSym[:]); err != nil {
		return 0, err
	}
	g.Members[pubHex] = role
	delete(g.Revoked, pubHex)
	g.CurrentEpoch++
	g.Epochs[g.CurrentEpoch] = nextSym
	members := make([]string, 0, len(g.Members))
	for m := range g.Members {
		members = append(members, m)
	}
	g.EpochMembers[g.CurrentEpoch] = members
	if r.persist != nil {
		if err := r.persist.SaveBorgGroup(g); err != nil {
			return 0, fmt.Errorf("borg: persist add member with rotation: %w", err)
		}
	}
	return g.CurrentEpoch, nil
}

// RevokeMember removes pubHex from groupID and IMMEDIATELY rotates to a fresh epoch
// with a new 32-byte symmetric key (enforcing borg/bsecret forward secrecy).
func (r *GroupRegistry) RevokeMember(groupID, pubHex string) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	g, ok := r.groups[groupID]
	if !ok {
		return 0, fmt.Errorf("borg: unknown group %q", groupID)
	}
	if _, member := g.Members[pubHex]; !member {
		return 0, fmt.Errorf("borg: %s is not a member of %q", pubHex, groupID)
	}
	delete(g.Members, pubHex)
	g.Revoked[pubHex] = time.Now().Unix()
	var nextSym [32]byte
	if _, err := rand.Read(nextSym[:]); err != nil {
		return 0, err
	}
	g.CurrentEpoch++
	g.Epochs[g.CurrentEpoch] = nextSym
	remaining := make([]string, 0, len(g.Members))
	for m := range g.Members {
		remaining = append(remaining, m)
	}
	g.EpochMembers[g.CurrentEpoch] = remaining
	if r.persist != nil {
		if err := r.persist.SaveBorgGroup(g); err != nil {
			return 0, fmt.Errorf("borg: persist revoke: %w", err)
		}
	}
	return g.CurrentEpoch, nil
}

// EpochKey returns the epoch symmetric key ONLY if pubHex is currently an active, non-revoked
// member of the group AND was authorized for the requested epoch.
func (r *GroupRegistry) EpochKey(groupID, pubHex string, epoch int64) ([32]byte, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	g, ok := r.groups[groupID]
	if !ok {
		return [32]byte{}, fmt.Errorf("borg: unknown group %q", groupID)
	}
	if _, active := g.Members[pubHex]; !active {
		return [32]byte{}, fmt.Errorf("borg: %s is not an active member of %q", pubHex, groupID)
	}
	if _, revoked := g.Revoked[pubHex]; revoked {
		return [32]byte{}, fmt.Errorf("borg: %s has been revoked from %q", pubHex, groupID)
	}
	if epoch == 0 {
		epoch = g.CurrentEpoch
	}
	// Check membership at the requested epoch.
	allowed := false
	for _, m := range g.EpochMembers[epoch] {
		if m == pubHex {
			allowed = true
			break
		}
	}
	if !allowed {
		return [32]byte{}, fmt.Errorf("borg: %s is not authorized for group %q epoch %d", pubHex, groupID, epoch)
	}
	sym, ok := g.Epochs[epoch]
	if !ok {
		return [32]byte{}, fmt.Errorf("borg: unknown epoch %d", epoch)
	}
	return sym, nil
}

// GetGroup returns a redacted copy of the group summary (without exposing raw Epochs keys).
func (r *GroupRegistry) GetGroup(groupID string) (*BorgGroup, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	g, ok := r.groups[groupID]
	if !ok {
		return nil, false
	}
	cp := &BorgGroup{
		GroupID:      g.GroupID,
		Name:         g.Name,
		CurrentEpoch: g.CurrentEpoch,
		Members:      make(map[string]string, len(g.Members)),
		Revoked:      make(map[string]int64, len(g.Revoked)),
		EpochMembers: make(map[int64][]string, len(g.EpochMembers)),
	}
	for k, v := range g.Members {
		cp.Members[k] = v
	}
	for k, v := range g.Revoked {
		cp.Revoked[k] = v
	}
	for ep, ms := range g.EpochMembers {
		cp.EpochMembers[ep] = append([]string(nil), ms...)
	}
	return cp, true
}
