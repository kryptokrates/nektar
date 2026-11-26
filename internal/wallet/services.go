package wallet

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"sync"
	"time"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	"github.com/bsv-blockchain/go-sdk/spv"
	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/bsv-blockchain/go-wallet-toolbox/pkg/wdk"
)

// bcommonServices adapts Nektar's lightwebinc/bcommon chain stack (Headers/Testchain, Arcade,
// Settler, Chain/WoC) to go-wallet-toolbox's wdk.Services interface. Implementing wdk.Services
// directly lets Nektar use go-wallet-toolbox/pkg/wallet + pkg/storage WITHOUT importing
// go-wallet-toolbox/pkg/services (eliminating all Kubernetes, Teranode, libp2p, and AWS deps).
type bcommonServices struct {
	cfg            Config
	txCache        *txLRU
	ownedOutputSum func(txid string) (uint64, bool)
	isIncomingTx   func(txid string) bool
	loadTxFromDB   func(ctx context.Context, txid string) *transaction.Transaction
	loadBeefFromDB func(ctx context.Context, txid string) (*transaction.Beef, error)

	errMu        sync.Mutex
	lastErr      map[string]error
	plannedSpend map[string]uint64
}

var _ wdk.Services = (*bcommonServices)(nil)

func newBcommonServices(cfg Config, cache *txLRU) *bcommonServices {
	return &bcommonServices{
		cfg:          cfg,
		txCache:      cache,
		lastErr:      make(map[string]error),
		plannedSpend: make(map[string]uint64),
	}
}

func (s *bcommonServices) setPlannedSpend(txid string, externalSats uint64) {
	if txid == "" {
		return
	}
	s.errMu.Lock()
	if len(s.plannedSpend) > 512 {
		clear(s.plannedSpend)
	}
	s.plannedSpend[txid] = externalSats
	s.errMu.Unlock()
}

func (s *bcommonServices) lookupPlannedSpend(txid string) (uint64, bool) {
	s.errMu.Lock()
	defer s.errMu.Unlock()
	v, ok := s.plannedSpend[txid]
	return v, ok
}

func (s *bcommonServices) setLastErr(txid string, err error) {
	if txid == "" || err == nil {
		return
	}
	s.errMu.Lock()
	if len(s.lastErr) > 256 {
		clear(s.lastErr)
	}
	s.lastErr[txid] = err
	s.errMu.Unlock()
}

func (s *bcommonServices) takeLastErr(txid string) error {
	s.errMu.Lock()
	defer s.errMu.Unlock()
	err := s.lastErr[txid]
	delete(s.lastErr, txid)
	return err
}

func (s *bcommonServices) hasBroadcaster() bool {
	return s.cfg.Broadcast != nil || s.cfg.Broadcaster != nil
}

func (s *bcommonServices) maxDepth() int {
	if s.cfg.MaxUnprovenDepth > 0 {
		return s.cfg.MaxUnprovenDepth
	}
	return DefaultMaxUnprovenDepth
}

func (s *bcommonServices) dbLoader(ctx context.Context) func(string) *transaction.Transaction {
	if s.loadTxFromDB == nil {
		return nil
	}
	return func(txid string) *transaction.Transaction {
		return s.loadTxFromDB(ctx, txid)
	}
}

func (s *bcommonServices) CurrentHeight(ctx context.Context) (uint32, error) {
	if s.cfg.Tracker == nil {
		return 0, ErrNoChain
	}
	h, err := s.cfg.Tracker.CurrentHeight(ctx)
	if err != nil {
		return 0, err
	}
	if h == 0 && !s.cfg.InMemoryStore {
		return 0, fmt.Errorf("%w: chain tracker reported height 0", ErrNoChain)
	}
	return h, nil
}

func (s *bcommonServices) IsValidRootForHeight(ctx context.Context, root *chainhash.Hash, height uint32) (bool, error) {
	if s.cfg.Tracker == nil {
		return false, ErrNoChain
	}
	return s.cfg.Tracker.IsValidRootForHeight(ctx, root, height)
}

func (s *bcommonServices) ChainHeaderByHeight(ctx context.Context, height uint32) (*wdk.ChainBlockHeader, error) {
	if s.cfg.HeaderBytes != nil {
		raw, err := s.cfg.HeaderBytes(ctx, height)
		if err != nil {
			if !s.cfg.InMemoryStore {
				return nil, fmt.Errorf("wallet services: fetch header %d: %w", height, err)
			}
		} else if len(raw) == wdk.BlockHeaderLength {
			base, pErr := wdk.ChainBaseBlockHeaderFromBytes(raw)
			if pErr != nil {
				if !s.cfg.InMemoryStore {
					return nil, fmt.Errorf("wallet services: parse header %d: %w", height, pErr)
				}
			} else {
				hash, hErr := base.CalculateHash()
				if hErr == nil {
					return &wdk.ChainBlockHeader{
						ChainBaseBlockHeader: *base,
						Height:               uint(height),
						Hash:                 hash.String(),
					}, nil
				}
			}
		} else if !s.cfg.InMemoryStore {
			return nil, fmt.Errorf("wallet services: header %d invalid length %d", height, len(raw))
		}
	}
	if s.cfg.InMemoryStore {
		// Deterministic synthetic header fallback strictly for ephemeral in-memory testchain.
		return syntheticChainBlockHeader(height), nil
	}
	return nil, fmt.Errorf("%w: no raw block header source configured for height %d", ErrNoChain, height)
}

func (s *bcommonServices) FindChainTipHeader(ctx context.Context) (*wdk.ChainBlockHeader, error) {
	h, err := s.CurrentHeight(ctx)
	if err != nil {
		return nil, err
	}
	return s.ChainHeaderByHeight(ctx, h)
}

func (s *bcommonServices) NLockTimeIsFinal(ctx context.Context, txOrLockTime any) (bool, error) {
	const locktimeThreshold = uint32(500_000_000)
	var lockTime uint32
	switch v := txOrLockTime.(type) {
	case *transaction.Transaction:
		if v == nil {
			return true, nil
		}
		allFinal := true
		for _, in := range v.Inputs {
			if in.SequenceNumber != 0xffffffff {
				allFinal = false
				break
			}
		}
		if allFinal {
			return true, nil
		}
		lockTime = v.LockTime
	case uint32:
		lockTime = v
	case int:
		if v <= 0 {
			return true, nil
		}
		lockTime = uint32(v)
	default:
		return true, nil
	}
	if lockTime == 0 {
		return true, nil
	}
	if lockTime >= locktimeThreshold {
		// Consensus BIP113/BSV rule: timestamp locktime is final iff lockTime < 11-block Median Time Past.
		if s.cfg.MedianTimePast != nil {
			mtp, err := s.cfg.MedianTimePast(ctx)
			if err != nil {
				return false, fmt.Errorf("wallet services: median time past: %w", err)
			}
			return lockTime < mtp, nil
		}
		if mp, ok := s.cfg.Tracker.(spv.MedianTimePastProvider); ok {
			mtp, err := mp.MedianTimePast(ctx)
			if err != nil {
				return false, fmt.Errorf("wallet services: median time past: %w", err)
			}
			return lockTime < mtp, nil
		}
		if s.cfg.InMemoryStore {
			return int64(lockTime) < time.Now().Unix(), nil
		}
		return false, fmt.Errorf("%w: no MedianTimePast header source configured for timestamp lockTime %d", ErrNoChain, lockTime)
	}
	h, err := s.CurrentHeight(ctx)
	if err != nil {
		return false, err
	}
	return lockTime <= h, nil
}

func syntheticChainBlockHeader(height uint32) *wdk.ChainBlockHeader {
	var prev uint32
	if height > 0 {
		prev = height - 1
	}
	return &wdk.ChainBlockHeader{
		ChainBaseBlockHeader: wdk.ChainBaseBlockHeader{
			Version:      1,
			PreviousHash: syntheticHashHex("block", prev),
			MerkleRoot:   syntheticHashHex("root", height),
			Time:         uint32(time.Now().Unix()),
			Bits:         0x1d00ffff,
			Nonce:        height,
		},
		Height: uint(height),
		Hash:   syntheticHashHex("block", height),
	}
}

func syntheticHashHex(tag string, height uint32) string {
	var buf [16]byte
	copy(buf[:], tag)
	binary.LittleEndian.PutUint32(buf[12:], height)
	sum := sha256.Sum256(buf[:])
	return hex.EncodeToString(sum[:])
}
