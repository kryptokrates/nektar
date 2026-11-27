package lattice

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/lightwebinc/bcommon/cbor"
	"github.com/lightwebinc/bcommon/keyed"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/core"
)

const (
	// MaxLevel is the top clearance tier of the cryptographic level chain [0..MaxLevel].
	MaxLevel = 100
	// BRC369LatticeDomain domain-separates CLKA wraps inside bcommon/keyed.
	BRC369LatticeDomain = "nektar:lattice:brc369:v1"
	// DefaultDomain is the base ontological class node when a fragment specifies no class.
	DefaultDomain = "general"
)

var (
	ErrInvalidLevel     = errors.New("lattice: level outside [0, MaxLevel]")
	ErrUpwardTraversal  = errors.New("lattice: upward clearance derivation is cryptographically forbidden")
	ErrDomainAccess     = errors.New("lattice: no downward cryptographic path to target domain")
	ErrDomainCycle      = errors.New("lattice: domain privilege graph contains a cycle")
	ErrEpochMismatch    = errors.New("lattice: envelope epoch does not match clearance epoch")
	ErrShardMismatch    = errors.New("lattice: envelope shard does not match clearance shard")
	ErrManifestTampered = errors.New("lattice: manifest hash or edge token invalid")
)

// RootProvider derives the BRC-43 wallet-rooted 32-byte lattice master seed for (shard, epoch)
// without exposing raw private keys outside wallet custody.
type RootProvider func(ctx context.Context, shard string, epoch uint64) ([32]byte, error)

// DomainEdge expresses ontological dominance: Parent >= Child (holding Parent's key unlocks Child's key).
type DomainEdge struct {
	Parent string `json:"parent"`
	Child  string `json:"child"`
}

// EdgeToken is a public AEAD-encrypted transition token allowing downward traversal Parent -> Child.
type EdgeToken struct {
	Parent string `json:"parent"`
	Child  string `json:"child"`
	Nonce  string `json:"nonce"` // 12-byte hex
	CT     string `json:"ct"`    // AES-256-GCM ciphertext+tag hex wrapping Child's 32-byte secret
}

// Manifest is the public, canonical CBOR-committed lattice topology for a shard epoch.
// Publishing EdgeTokens leaks zero bits upward or laterally while enabling offline downward traversal.
type Manifest struct {
	Version      uint64      `json:"v"`
	Shard        string      `json:"shard"`
	Epoch        uint64      `json:"epoch"`
	MaxLevel     int         `json:"maxLevel"`
	Domains      []string    `json:"domains"`
	Edges        []EdgeToken `json:"edges,omitempty"`
	ManifestHash string      `json:"manifestHash"`
}

// SealedEnvelope is a BRC-369 keyed content envelope sealed under a CLKA (level, domain, epoch, hdp) node key.
// Compatible with DisclosureSale paid atomic reveals via KeyCommitment == keyed.Commitment(k).
type SealedEnvelope struct {
	Scheme        string `json:"scheme"`        // "clka-brc369-v1"
	Shard         string `json:"shard"`
	Epoch         uint64 `json:"epoch"`
	Level         int    `json:"level"`
	Domain        string `json:"domain"`
	ContentID     string `json:"contentId"`     // 32-byte hex HDP
	KeyCommitment string `json:"keyCommitment"` // 32-byte hex BRC-369 keyed.Commitment(k)
	Salt          string `json:"salt"`          // 4-byte hex BRC-369 segment salt
	Ciphertext    string `json:"ciphertext"`    // hex BRC-369 sealed segment
	NodeWrap      string `json:"nodeWrap"`      // hex content key wrapped to derived node key
}

// Clearance holds the exact cryptographic frontier keys provisioned to a principal for a shard epoch:
// ONLY LevelKey for MaxLevel (never higher) and DomainKeys for its directly granted domains.
type Clearance struct {
	Shard        string              `json:"shard"`
	Epoch        uint64              `json:"epoch"`
	MaxLevel     int                 `json:"maxLevel"`
	LevelKeyHex  string              `json:"levelKeyHex"`          // 32B hex K_{MaxLevel}
	DomainKeys   map[string]string   `json:"domainKeys,omitempty"` // domain -> 32B hex S_domain
	ManifestHash string              `json:"manifestHash,omitempty"`
}

// ShardState persists per-shard lattice epoch & domain DAG topology (never persisting raw keys!).
type ShardState struct {
	Shard       string       `json:"shard"`
	Epoch       uint64       `json:"epoch"`
	DomainEdges []DomainEdge `json:"domainEdges,omitempty"`
}

// Persistence optionally persists shard epoch and domain DAG structure across restarts.
type Persistence interface {
	LoadLatticeStates() ([]ShardState, error)
	SaveLatticeState(st ShardState) error
}

func normDomain(d string) string {
	s := strings.ToLower(strings.TrimSpace(d))
	if s == "" {
		return DefaultDomain
	}
	return s
}

func clampLevel(l int) (int, error) {
	if l < 0 || l > MaxLevel {
		return 0, fmt.Errorf("%w: got %d", ErrInvalidLevel, l)
	}
	return l, nil
}

// deriveTopLevelKey derives K_{MaxLevel} from the BRC-43 shard epoch root seed.
func deriveTopLevelKey(root [32]byte, shard string, epoch uint64) ([32]byte, error) {
	var out [32]byte
	salt := core.DSHA256(core.Concat([]byte("nektar:lattice:level-salt:v1"), core.Field([]byte(shard)), core.U64LE(epoch)))
	b, err := hkdf.Key(sha256.New, root[:], salt, fmt.Sprintf("nektar:lattice:top:%d", MaxLevel), 32)
	if err != nil {
		return out, err
	}
	copy(out[:], b)
	core.ZeroBytes(b)
	return out, nil
}

// DeriveLevelKey steps strictly DOWNWARD along the one-way HKDF-SHA256 clearance chain
// from (fromLevel, fromKey) to toLevel. Any attempt at upward traversal (toLevel > fromLevel)
// fails closed immediately with ErrUpwardTraversal.
func DeriveLevelKey(fromKey [32]byte, fromLevel, toLevel int, shard string, epoch uint64) ([32]byte, error) {
	var zero [32]byte
	if _, err := clampLevel(fromLevel); err != nil {
		return zero, err
	}
	if _, err := clampLevel(toLevel); err != nil {
		return zero, err
	}
	if toLevel > fromLevel {
		return zero, fmt.Errorf("%w: cannot derive level %d from clearance level %d", ErrUpwardTraversal, toLevel, fromLevel)
	}
	cur := fromKey
	if fromLevel == toLevel {
		return cur, nil
	}
	salt := core.DSHA256(core.Concat(core.TagLatticeDown, core.Field([]byte(shard)), core.U64LE(epoch)))
	for lvl := fromLevel - 1; lvl >= toLevel; lvl-- {
		info := fmt.Sprintf("nektar:lattice:down:v1:%d", lvl)
		next, err := hkdf.Key(sha256.New, cur[:], salt, info, 32)
		if err != nil {
			core.Zero32Array(&cur)
			return zero, err
		}
		copy(cur[:], next)
		core.ZeroBytes(next)
	}
	return cur, nil
}

// deriveDomainSecret derives an independent 256-bit secret S_domain from (root, shard, epoch, domain).
// Two sibling domains share ZERO cryptographic correlation without an explicit EdgeToken in Manifest.
func deriveDomainSecret(root [32]byte, shard string, epoch uint64, domain string) ([32]byte, error) {
	var out [32]byte
	d := normDomain(domain)
	salt := core.DSHA256(core.Concat([]byte("nektar:lattice:domain-salt:v1"), core.Field([]byte(shard)), core.U64LE(epoch)))
	b, err := hkdf.Key(sha256.New, root[:], salt, "nektar:lattice:domain:v1:"+d, 32)
	if err != nil {
		return out, err
	}
	copy(out[:], b)
	core.ZeroBytes(b)
	return out, nil
}

func edgeAAD(shard string, epoch uint64, parent, child string) []byte {
	return core.Concat(
		[]byte("nektar:lattice:edge:v1"),
		core.Field([]byte(shard)),
		core.U64LE(epoch),
		core.Field([]byte(normDomain(parent))),
		core.Field([]byte(normDomain(child))),
	)
}

func sealEdgeToken(shard string, epoch uint64, parent, child string, parentKey, childKey [32]byte) (EdgeToken, error) {
	block, err := aes.NewCipher(parentKey[:])
	if err != nil {
		return EdgeToken{}, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return EdgeToken{}, err
	}
	var nonce [12]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return EdgeToken{}, err
	}
	aad := edgeAAD(shard, epoch, parent, child)
	ct := gcm.Seal(nil, nonce[:], childKey[:], aad)
	return EdgeToken{
		Parent: normDomain(parent),
		Child:  normDomain(child),
		Nonce:  hex.EncodeToString(nonce[:]),
		CT:     hex.EncodeToString(ct),
	}, nil
}

func openEdgeToken(shard string, epoch uint64, tok EdgeToken, parentKey [32]byte) ([32]byte, error) {
	var out [32]byte
	nonce, err := hex.DecodeString(tok.Nonce)
	if err != nil || len(nonce) != 12 {
		return out, ErrManifestTampered
	}
	ct, err := hex.DecodeString(tok.CT)
	if err != nil {
		return out, ErrManifestTampered
	}
	block, err := aes.NewCipher(parentKey[:])
	if err != nil {
		return out, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return out, err
	}
	pt, err := gcm.Open(nil, nonce, ct, edgeAAD(shard, epoch, tok.Parent, tok.Child))
	if err != nil || len(pt) != 32 {
		return out, ErrManifestTampered
	}
	copy(out[:], pt)
	core.ZeroBytes(pt)
	return out, nil
}

// ComputeManifestHash computes the canonical CBOR DSHA256 digest over a Manifest's topology and edge tokens.
func ComputeManifestHash(m Manifest) (string, error) {
	domVals := make([]cbor.Value, len(m.Domains))
	for i, d := range m.Domains {
		domVals[i] = d
	}
	edgeVals := make([]cbor.Value, len(m.Edges))
	for i, e := range m.Edges {
		edgeVals[i] = cbor.Map{
			{Key: uint64(0), Val: e.Parent},
			{Key: uint64(1), Val: e.Child},
			{Key: uint64(2), Val: e.Nonce},
			{Key: uint64(3), Val: e.CT},
		}
	}
	h, err := core.HashCBOR([]byte("nektar:lattice:manifest:v1"), cbor.Map{
		{Key: uint64(0), Val: uint64(1)},
		{Key: uint64(1), Val: m.Shard},
		{Key: uint64(2), Val: m.Epoch},
		{Key: uint64(3), Val: uint64(m.MaxLevel)},
		{Key: uint64(4), Val: domVals},
		{Key: uint64(5), Val: edgeVals},
	})
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(h), nil
}

// deriveFragmentNodeKey binds level key + domain secret + canonical CBOR fragment context (shard, epoch, level, domain, hdp).
func deriveFragmentNodeKey(levelKey, domainKey [32]byte, shard string, epoch uint64, level int, domain string, contentID [32]byte) ([32]byte, error) {
	var out [32]byte
	ctxCBOR, err := core.EncodeCBOR(cbor.Map{
		{Key: uint64(0), Val: shard},
		{Key: uint64(1), Val: epoch},
		{Key: uint64(2), Val: uint64(level)},
		{Key: uint64(3), Val: normDomain(domain)},
	})
	if err != nil {
		return out, err
	}
	ikm := make([]byte, 64)
	copy(ikm[:32], levelKey[:])
	copy(ikm[32:], domainKey[:])
	defer core.ZeroBytes(ikm)
	b, err := hkdf.Key(sha256.New, ikm, contentID[:], "nektar:lattice:frag:v1:"+hex.EncodeToString(ctxCBOR), 32)
	if err != nil {
		return out, err
	}
	copy(out[:], b)
	core.ZeroBytes(b)
	return out, nil
}

// SealWithKeys encrypts plaintext under a fresh BRC-369 content scalar k wrapped to the CLKA node key.
func SealWithKeys(shard string, epoch uint64, level int, domain string, contentID [32]byte, levelKey, domainKey [32]byte, plaintext []byte) (*SealedEnvelope, [32]byte, error) {
	if _, err := clampLevel(level); err != nil {
		return nil, [32]byte{}, err
	}
	dom := normDomain(domain)
	nodeKey, err := deriveFragmentNodeKey(levelKey, domainKey, shard, epoch, level, dom, contentID)
	if err != nil {
		return nil, [32]byte{}, err
	}
	defer core.Zero32Array(&nodeKey)

	k, err := keyed.SampleKey(rand.Reader)
	if err != nil {
		return nil, [32]byte{}, fmt.Errorf("lattice: sample BRC-369 key: %w", err)
	}
	salt, err := keyed.SampleSalt(rand.Reader)
	if err != nil {
		core.Zero32Array(&k)
		return nil, [32]byte{}, fmt.Errorf("lattice: sample salt: %w", err)
	}
	ct, err := keyed.SealSegment(k, salt, plaintext)
	if err != nil {
		core.Zero32Array(&k)
		return nil, [32]byte{}, fmt.Errorf("lattice: seal segment: %w", err)
	}
	commit := keyed.Commitment(k)
	wrap, err := keyed.WrapToEpoch(BRC369LatticeDomain, nodeKey, contentID, k, rand.Reader)
	if err != nil {
		core.Zero32Array(&k)
		return nil, [32]byte{}, fmt.Errorf("lattice: wrap to node key: %w", err)
	}
	return &SealedEnvelope{
		Scheme:        "clka-brc369-v1",
		Shard:         shard,
		Epoch:         epoch,
		Level:         level,
		Domain:        dom,
		ContentID:     hex.EncodeToString(contentID[:]),
		KeyCommitment: hex.EncodeToString(commit[:]),
		Salt:          hex.EncodeToString(salt[:]),
		Ciphertext:    hex.EncodeToString(ct),
		NodeWrap:      hex.EncodeToString(wrap),
	}, k, nil
}

// OpenWithClearance decrypts a CLKA SealedEnvelope completely offline using ONLY the caller's
// Clearance bundle and the public Manifest — cryptographically enforcing no upward level traversal
// and no unauthorized lateral/upward domain traversal.
func OpenWithClearance(env *SealedEnvelope, clr *Clearance, manifest *Manifest) ([]byte, error) {
	if env == nil || clr == nil {
		return nil, errors.New("lattice: envelope and clearance are required")
	}
	if env.Shard != clr.Shard {
		return nil, ErrShardMismatch
	}
	if env.Epoch != clr.Epoch {
		return nil, ErrEpochMismatch
	}
	if env.Level > clr.MaxLevel {
		return nil, fmt.Errorf("%w: fragment level %d > clearance maxLevel %d", ErrUpwardTraversal, env.Level, clr.MaxLevel)
	}
	lkBytes, err := hex.DecodeString(strings.TrimSpace(clr.LevelKeyHex))
	if err != nil || len(lkBytes) != 32 {
		return nil, errors.New("lattice: invalid clearance levelKeyHex")
	}
	var clrLevelKey [32]byte
	copy(clrLevelKey[:], lkBytes)
	defer core.Zero32Array(&clrLevelKey)

	fragLevelKey, err := DeriveLevelKey(clrLevelKey, clr.MaxLevel, env.Level, env.Shard, env.Epoch)
	if err != nil {
		return nil, err
	}
	defer core.Zero32Array(&fragLevelKey)

	domKey, err := resolveDomainKeyFromClearance(env.Shard, env.Epoch, normDomain(env.Domain), clr, manifest)
	if err != nil {
		return nil, err
	}
	defer core.Zero32Array(&domKey)

	return openWithNodeKeys(env, fragLevelKey, domKey)
}

func openWithNodeKeys(env *SealedEnvelope, levelKey, domainKey [32]byte) ([]byte, error) {
	cidBytes, err := hex.DecodeString(env.ContentID)
	if err != nil || len(cidBytes) != 32 {
		return nil, errors.New("lattice: invalid contentId")
	}
	commitBytes, err := hex.DecodeString(env.KeyCommitment)
	if err != nil || len(commitBytes) != 32 {
		return nil, errors.New("lattice: invalid keyCommitment")
	}
	saltBytes, err := hex.DecodeString(env.Salt)
	if err != nil || len(saltBytes) != keyed.SaltLen {
		return nil, errors.New("lattice: invalid salt")
	}
	ct, err := hex.DecodeString(env.Ciphertext)
	if err != nil {
		return nil, err
	}
	wrap, err := hex.DecodeString(env.NodeWrap)
	if err != nil {
		return nil, err
	}
	var cid, commit [32]byte
	var salt [keyed.SaltLen]byte
	copy(cid[:], cidBytes)
	copy(commit[:], commitBytes)
	copy(salt[:], saltBytes)

	nodeKey, err := deriveFragmentNodeKey(levelKey, domainKey, env.Shard, env.Epoch, env.Level, env.Domain, cid)
	if err != nil {
		return nil, err
	}
	defer core.Zero32Array(&nodeKey)

	k, err := keyed.UnwrapFromEpoch(BRC369LatticeDomain, nodeKey, cid, wrap, commit)
	if err != nil {
		return nil, fmt.Errorf("lattice: BRC-369 unwrap/commitment check failed: %w", err)
	}
	defer core.Zero32Array(&k)
	return keyed.OpenSegment(k, salt, ct)
}

func resolveDomainKeyFromClearance(shard string, epoch uint64, target string, clr *Clearance, manifest *Manifest) ([32]byte, error) {
	var zero [32]byte
	if hx, ok := clr.DomainKeys[target]; ok {
		b, err := hex.DecodeString(strings.TrimSpace(hx))
		if err != nil || len(b) != 32 {
			return zero, errors.New("lattice: invalid clearance domain key hex")
		}
		var out [32]byte
		copy(out[:], b)
		return out, nil
	}
	if manifest == nil || len(manifest.Edges) == 0 {
		return zero, fmt.Errorf("%w: target domain %q", ErrDomainAccess, target)
	}
	if manifest.Shard != shard || manifest.Epoch != epoch {
		return zero, ErrManifestTampered
	}
	adj := make(map[string][]EdgeToken)
	for _, e := range manifest.Edges {
		adj[normDomain(e.Parent)] = append(adj[normDomain(e.Parent)], e)
	}
	// BFS downward from principal's granted frontier domains using public AEAD edge tokens.
	visited := make(map[string][32]byte, len(clr.DomainKeys))
	queue := make([]string, 0, len(clr.DomainKeys))
	defer func() {
		for k, v := range visited {
			core.Zero32Array(&v)
			delete(visited, k)
		}
	}()
	for dom, hx := range clr.DomainKeys {
		b, err := hex.DecodeString(strings.TrimSpace(hx))
		if err != nil || len(b) != 32 {
			continue
		}
		var dk [32]byte
		copy(dk[:], b)
		nd := normDomain(dom)
		visited[nd] = dk
		queue = append(queue, nd)
	}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		curKey := visited[cur]
		for _, tok := range adj[cur] {
			child := normDomain(tok.Child)
			if _, seen := visited[child]; seen {
				continue
			}
			childKey, err := openEdgeToken(shard, epoch, tok, curKey)
			if err != nil {
				return zero, err
			}
			if child == target {
				return childKey, nil
			}
			visited[child] = childKey
			queue = append(queue, child)
		}
	}
	return zero, fmt.Errorf("%w: target domain %q", ErrDomainAccess, target)
}

// SealClearanceForRecipient encrypts a Clearance bundle end-to-end to recipientPubHex
// via ephemeral secp256k1 ECDH + HKDF-SHA256 + AES-256-GCM (bstack.SealBboxBody).
func SealClearanceForRecipient(recipientPubHex string, clr *Clearance) (json.RawMessage, error) {
	if clr == nil {
		return nil, errors.New("lattice: nil clearance")
	}
	raw, err := json.Marshal(clr)
	if err != nil {
		return nil, err
	}
	defer core.ZeroBytes(raw)
	aad := core.Concat([]byte("nektar:lattice:clearance:v1"), core.Field([]byte(clr.Shard)), core.U64LE(clr.Epoch), core.Field([]byte(recipientPubHex)))
	return bstack.SealBboxBody(recipientPubHex, aad, raw)
}

// OpenSealedClearance decrypts a Clearance bundle addressed to recipientKey.
func OpenSealedClearance(recipientKey *ec.PrivateKey, shard string, epoch uint64, sealed json.RawMessage) (*Clearance, error) {
	if recipientKey == nil {
		return nil, errors.New("lattice: nil recipient key")
	}
	recPub := hex.EncodeToString(recipientKey.PubKey().Compressed())
	aad := core.Concat([]byte("nektar:lattice:clearance:v1"), core.Field([]byte(shard)), core.U64LE(epoch), core.Field([]byte(recPub)))
	pt, err := bstack.OpenBboxBody(recipientKey, aad, sealed)
	if err != nil {
		return nil, err
	}
	defer core.ZeroBytes(pt)
	var clr Clearance
	if err := json.Unmarshal(pt, &clr); err != nil {
		return nil, err
	}
	return &clr, nil
}

// Manager coordinates per-shard CLKA epochs, domain privilege DAGs, sealing/opening, and clearance provisioning.
type Manager struct {
	mu      sync.RWMutex
	rootFn  RootProvider
	persist Persistence
	states  map[string]*ShardState
}

// NewManager creates a CLKA Manager backed by a BRC-43 RootProvider.
func NewManager(rootFn RootProvider) *Manager {
	return &Manager{
		rootFn: rootFn,
		states: make(map[string]*ShardState),
	}
}

// SetPersistence attaches durable epoch/DAG persistence and loads existing shard states.
func (m *Manager) SetPersistence(p Persistence) error {
	if p == nil {
		return nil
	}
	loaded, err := p.LoadLatticeStates()
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.persist = p
	for _, st := range loaded {
		cp := st
		if cp.Epoch == 0 {
			cp.Epoch = 1
		}
		m.states[cp.Shard] = &cp
	}
	return nil
}

func (m *Manager) getOrInitStateLocked(shard string) *ShardState {
	st, ok := m.states[shard]
	if !ok {
		st = &ShardState{Shard: shard, Epoch: 1}
		m.states[shard] = st
	}
	return st
}

// CurrentEpoch returns the active lattice epoch for shard (defaults to 1).
func (m *Manager) CurrentEpoch(shard string) uint64 {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if st, ok := m.states[shard]; ok && st.Epoch > 0 {
		return st.Epoch
	}
	return 1
}

// SetDomainHierarchy configures the ontological privilege DAG (Parent >= Child) for shard,
// rejecting cycles before persisting.
func (m *Manager) SetDomainHierarchy(shard string, edges []DomainEdge) error {
	if err := validateDAG(edges); err != nil {
		return err
	}
	norm := make([]DomainEdge, 0, len(edges))
	seen := make(map[DomainEdge]bool, len(edges))
	for _, e := range edges {
		ne := DomainEdge{Parent: normDomain(e.Parent), Child: normDomain(e.Child)}
		if !seen[ne] {
			seen[ne] = true
			norm = append(norm, ne)
		}
	}
	sort.Slice(norm, func(i, j int) bool {
		if norm[i].Parent != norm[j].Parent {
			return norm[i].Parent < norm[j].Parent
		}
		return norm[i].Child < norm[j].Child
	})

	m.mu.Lock()
	defer m.mu.Unlock()
	cur := m.getOrInitStateLocked(shard)
	next := ShardState{Shard: shard, Epoch: cur.Epoch, DomainEdges: norm}
	if m.persist != nil {
		if err := m.persist.SaveLatticeState(next); err != nil {
			return err
		}
	}
	m.states[shard] = &next
	return nil
}

func validateDAG(edges []DomainEdge) error {
	adj := make(map[string][]string)
	nodes := make(map[string]bool)
	for _, e := range edges {
		p, c := normDomain(e.Parent), normDomain(e.Child)
		if p == c {
			return fmt.Errorf("%w: self-loop on %q", ErrDomainCycle, p)
		}
		adj[p] = append(adj[p], c)
		nodes[p] = true
		nodes[c] = true
	}
	// Standard 3-color DFS cycle check (0=unvisited, 1=visiting, 2=done).
	color := make(map[string]int, len(nodes))
	var dfs func(u string) error
	dfs = func(u string) error {
		color[u] = 1
		for _, v := range adj[u] {
			if color[v] == 1 {
				return fmt.Errorf("%w: %q -> %q", ErrDomainCycle, u, v)
			}
			if color[v] == 0 {
				if err := dfs(v); err != nil {
					return err
				}
			}
		}
		color[u] = 2
		return nil
	}
	for u := range nodes {
		if color[u] == 0 {
			if err := dfs(u); err != nil {
				return err
			}
		}
	}
	return nil
}

// RotateEpoch bumps the shard's lattice epoch, yielding brand-new BRC-43 root keys and
// revoking all prior-epoch clearance bundles for future writes (forward secrecy).
func (m *Manager) RotateEpoch(shard string) (uint64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cur := m.getOrInitStateLocked(shard)
	next := ShardState{
		Shard:       shard,
		Epoch:       cur.Epoch + 1,
		DomainEdges: append([]DomainEdge(nil), cur.DomainEdges...),
	}
	if m.persist != nil {
		if err := m.persist.SaveLatticeState(next); err != nil {
			return 0, err
		}
	}
	m.states[shard] = &next
	return next.Epoch, nil
}

// BuildManifest constructs the public, canonical CBOR-hashed Manifest with AEAD edge tokens for (shard, epoch).
func (m *Manager) BuildManifest(ctx context.Context, shard string, epoch uint64) (*Manifest, error) {
	m.mu.RLock()
	st := m.states[shard]
	var edges []DomainEdge
	if st != nil {
		edges = append([]DomainEdge(nil), st.DomainEdges...)
		if epoch == 0 {
			epoch = st.Epoch
		}
	}
	m.mu.RUnlock()
	if epoch == 0 {
		epoch = 1
	}
	root, err := m.rootFn(ctx, shard, epoch)
	if err != nil {
		return nil, err
	}
	defer core.Zero32Array(&root)

	domSet := map[string]bool{DefaultDomain: true}
	for _, e := range edges {
		domSet[normDomain(e.Parent)] = true
		domSet[normDomain(e.Child)] = true
	}
	domains := make([]string, 0, len(domSet))
	for d := range domSet {
		domains = append(domains, d)
	}
	sort.Strings(domains)

	toks := make([]EdgeToken, 0, len(edges))
	for _, e := range edges {
		pk, err := deriveDomainSecret(root, shard, epoch, e.Parent)
		if err != nil {
			return nil, err
		}
		ck, err := deriveDomainSecret(root, shard, epoch, e.Child)
		if err != nil {
			core.Zero32Array(&pk)
			return nil, err
		}
		tok, err := sealEdgeToken(shard, epoch, e.Parent, e.Child, pk, ck)
		core.Zero32Array(&pk)
		core.Zero32Array(&ck)
		if err != nil {
			return nil, err
		}
		toks = append(toks, tok)
	}
	man := &Manifest{
		Version:  1,
		Shard:    shard,
		Epoch:    epoch,
		MaxLevel: MaxLevel,
		Domains:  domains,
		Edges:    toks,
	}
	h, err := ComputeManifestHash(*man)
	if err != nil {
		return nil, err
	}
	man.ManifestHash = h
	return man, nil
}

// SealFragment encrypts plaintext for (shard, level, domain, contentID) under the shard's current CLKA epoch.
func (m *Manager) SealFragment(ctx context.Context, shard string, level int, domain string, contentID [32]byte, plaintext []byte) (*SealedEnvelope, [32]byte, error) {
	if _, err := clampLevel(level); err != nil {
		return nil, [32]byte{}, err
	}
	epoch := m.CurrentEpoch(shard)
	root, err := m.rootFn(ctx, shard, epoch)
	if err != nil {
		return nil, [32]byte{}, err
	}
	defer core.Zero32Array(&root)

	topKey, err := deriveTopLevelKey(root, shard, epoch)
	if err != nil {
		return nil, [32]byte{}, err
	}
	defer core.Zero32Array(&topKey)

	lvlKey, err := DeriveLevelKey(topKey, MaxLevel, level, shard, epoch)
	if err != nil {
		return nil, [32]byte{}, err
	}
	defer core.Zero32Array(&lvlKey)

	domKey, err := deriveDomainSecret(root, shard, epoch, domain)
	if err != nil {
		return nil, [32]byte{}, err
	}
	defer core.Zero32Array(&domKey)

	return SealWithKeys(shard, epoch, level, domain, contentID, lvlKey, domKey, plaintext)
}

// OpenFragmentForPrincipal decrypts env strictly after deriving ONLY the key allowed by principalLevel
// (and verifying principalLevel >= env.Level before touching keys — for owner/local daemon calls).
// External/sub-agent callers with a provisioned Clearance should call OpenWithClearance directly.
func (m *Manager) OpenFragmentForPrincipal(ctx context.Context, env *SealedEnvelope, principalLevel int) ([]byte, error) {
	if env == nil {
		return nil, errors.New("lattice: nil envelope")
	}
	if principalLevel > MaxLevel {
		principalLevel = MaxLevel
	}
	if principalLevel < env.Level {
		return nil, fmt.Errorf("%w: principal level %d < fragment level %d", ErrUpwardTraversal, principalLevel, env.Level)
	}
	root, err := m.rootFn(ctx, env.Shard, env.Epoch)
	if err != nil {
		return nil, err
	}
	defer core.Zero32Array(&root)

	topKey, err := deriveTopLevelKey(root, env.Shard, env.Epoch)
	if err != nil {
		return nil, err
	}
	defer core.Zero32Array(&topKey)

	// Derive principal's clearance key FIRST, then step downward to env.Level.
	princKey, err := DeriveLevelKey(topKey, MaxLevel, principalLevel, env.Shard, env.Epoch)
	if err != nil {
		return nil, err
	}
	defer core.Zero32Array(&princKey)

	fragKey, err := DeriveLevelKey(princKey, principalLevel, env.Level, env.Shard, env.Epoch)
	if err != nil {
		return nil, err
	}
	defer core.Zero32Array(&fragKey)

	domKey, err := deriveDomainSecret(root, env.Shard, env.Epoch, env.Domain)
	if err != nil {
		return nil, err
	}
	defer core.Zero32Array(&domKey)

	return openWithNodeKeys(env, fragKey, domKey)
}

// ProvisionClearance derives ONLY the frontier keys (K_{maxLevel}, {S_d for d in domains})
// for shard's current epoch — NEVER exposing keys above maxLevel or outside granted domains.
func (m *Manager) ProvisionClearance(ctx context.Context, shard string, maxLevel int, domains []string) (*Clearance, *Manifest, error) {
	if _, err := clampLevel(maxLevel); err != nil {
		return nil, nil, err
	}
	epoch := m.CurrentEpoch(shard)
	root, err := m.rootFn(ctx, shard, epoch)
	if err != nil {
		return nil, nil, err
	}
	defer core.Zero32Array(&root)

	topKey, err := deriveTopLevelKey(root, shard, epoch)
	if err != nil {
		return nil, nil, err
	}
	defer core.Zero32Array(&topKey)

	lvlKey, err := DeriveLevelKey(topKey, MaxLevel, maxLevel, shard, epoch)
	if err != nil {
		return nil, nil, err
	}
	defer core.Zero32Array(&lvlKey)

	if len(domains) == 0 {
		domains = []string{DefaultDomain}
	}
	domKeys := make(map[string]string, len(domains))
	for _, d := range domains {
		nd := normDomain(d)
		sk, err := deriveDomainSecret(root, shard, epoch, nd)
		if err != nil {
			return nil, nil, err
		}
		domKeys[nd] = hex.EncodeToString(sk[:])
		core.Zero32Array(&sk)
	}
	man, err := m.BuildManifest(ctx, shard, epoch)
	if err != nil {
		return nil, nil, err
	}
	clr := &Clearance{
		Shard:        shard,
		Epoch:        epoch,
		MaxLevel:     maxLevel,
		LevelKeyHex:  hex.EncodeToString(lvlKey[:]),
		DomainKeys:   domKeys,
		ManifestHash: man.ManifestHash,
	}
	return clr, man, nil
}
