package hub

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/bsv-blockchain/go-overlay-services/pkg/core/engine"
	"github.com/bsv-blockchain/go-overlay-services/pkg/core/gasp"
	"github.com/bsv-blockchain/go-sdk/chainhash"
	"github.com/bsv-blockchain/go-sdk/overlay"
	"github.com/bsv-blockchain/go-sdk/overlay/lookup"
	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/lightwebinc/bcommon/module"
	oespv "github.com/lightwebinc/overlay-bridge/pkg/spv"
	oemodule "github.com/lightwebinc/overlay-engine/pkg/module"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/eventbus"
	"github.com/kryptokrates/nektar/internal/fabric"
)

var (
	// ErrHubRefused classifies permanent consensus, SPV, carrier, or schema refusals (ARCH-F5:
	// federation/plane consumers skip & count poison frames instead of stalling forever).
	ErrHubRefused = errors.New("hub: submission permanently refused")

	// ErrHubTransient classifies transient chain-tracker, deadline, or storage unavailability
	// (ARCH-F5: federation/plane consumers preserve cursor and retry with backoff).
	ErrHubTransient = errors.New("hub: transient verification error")
)

const (
	maxDynamicMarketTopics = 4096
	maxInMemModOutputs     = 4096
)

type modOutputEntry struct {
	topic string
	beef  []byte
	spent bool
}

// noopChainTracker satisfies chaintracker.ChainTracker for memory/regtest Hubs where no external
// header source is configured so Lightweb modules (e.g. bstore) mount zero-config cleanly.
type noopChainTracker struct{}

func (noopChainTracker) IsValidRootForHeight(context.Context, *chainhash.Hash, uint32) (bool, error) {
	return true, nil
}

func (noopChainTracker) CurrentHeight(context.Context) (uint32, error) {
	return 1, nil
}

// DefaultAlphaSuffix10 derives a deterministic 10-character lowercase ASCII letter suffix ([a-z]{10})
// satisfying both Lightweb bbox (`boxrec.CheckOffice`) and bstore (`storerec.CheckBucket`) grammars.
func DefaultAlphaSuffix10(seed []byte, label string) string {
	h := sha256.Sum256(append(append([]byte("nektar:lightweb-id:"), seed...), []byte(label)...))
	var out [10]byte
	for i := range out {
		out[i] = 'a' + (h[i] % 26)
	}
	return string(out[:])
}

func (n *Node) initOverlayEngine(ctx context.Context, dbPath string) error {
	spvNet := oespv.NetworkAll
	switch n.net {
	case "main":
		spvNet = oespv.NetworkMain
	case "testnet":
		spvNet = oespv.NetworkTest
	}
	if n.tracker != nil {
		n.spvVerifier = &oespv.Verifier{
			Tracker: n.tracker,
			Network: spvNet,
			Timeout: 10 * time.Second,
		}
	}

	var st engine.Storage
	if dbPath != "" && dbPath != ":memory:" {
		baseDir := filepath.Join(filepath.Dir(dbPath), strings.TrimSuffix(filepath.Base(dbPath), filepath.Ext(dbPath))+"_overlay")
		blobsDir := filepath.Join(baseDir, "blobs")
		idxPath := filepath.Join(baseDir, "index.sqlite")
		oes, err := OpenOverlayStore(ctx, idxPath, blobsDir)
		if err != nil {
			return fmt.Errorf("hub: open overlay store: %w", err)
		}
		n.oeStore = oes
		n.store.SetBlobStore(oes.Blobs)
		st = oes
	}

	eng := engine.NewEngine(&engine.Config{
		Storage:      st,
		ChainTracker: n.tracker,
	})
	eng.Verifier = func(vCtx context.Context, tx *transaction.Transaction, _ *chainhash.Hash) error {
		if n.spvVerifier == nil || tx == nil || len(tx.Inputs) == 0 {
			return nil
		}
		raw, err := tx.AtomicBEEF(false)
		if err != nil {
			return fmt.Errorf("%w: %v", ErrHubRefused, err)
		}
		vd := n.spvVerifier.Verify(vCtx, raw)
		switch vd.Result {
		case oespv.ResultOK:
			return nil
		case oespv.ResultError:
			return fmt.Errorf("%w (%s): %v", ErrHubTransient, vd.Reason, vd.Err)
		default:
			return fmt.Errorf("%w (%s): %v", ErrHubRefused, vd.Reason, vd.Err)
		}
	}
	n.oeEngine = eng
	return nil
}

func (n *Node) wrapEngineTopicManager(tm module.TopicManager) engine.TopicManager {
	return oemodule.TopicManager(tm)
}

func (n *Node) wrapEngineLookupService(ls module.LookupService) engine.LookupService {
	return oemodule.LookupService(ls)
}

type oeRestoreSeam struct {
	st engine.Storage
}

func (s *oeRestoreSeam) FindOutput(ctx context.Context, outpoint *transaction.Outpoint, topic string) (*module.Output, error) {
	if s.st == nil || outpoint == nil {
		return nil, nil
	}
	var tp *string
	if topic != "" {
		tp = &topic
	}
	o, err := s.st.FindOutput(ctx, outpoint, tp, nil, true)
	if err != nil || o == nil {
		return nil, err
	}
	return oemodule.Output(o)
}

func (s *oeRestoreSeam) DeleteOutput(ctx context.Context, outpoint *transaction.Outpoint, topic string) error {
	if s.st == nil || outpoint == nil {
		return nil
	}
	return s.st.DeleteOutput(ctx, outpoint, topic)
}

func (s *oeRestoreSeam) DoesAppliedTransactionExist(ctx context.Context, txid *chainhash.Hash, topic string) (bool, error) {
	if s.st == nil || txid == nil {
		return false, nil
	}
	return s.st.DoesAppliedTransactionExist(ctx, &overlay.AppliedTransaction{Txid: txid, Topic: topic})
}

func (s *oeRestoreSeam) DeleteAppliedTransaction(ctx context.Context, txid *chainhash.Hash, topic string) error {
	if s.st == nil || txid == nil {
		return nil
	}
	if d, ok := s.st.(interface {
		DeleteAppliedTransaction(context.Context, *chainhash.Hash, string) error
	}); ok {
		return d.DeleteAppliedTransaction(ctx, txid, topic)
	}
	return oemodule.ErrCannotEvict
}

func (n *Node) restoreOverlayModules(ctx context.Context) error {
	var rows []module.Output
	if n.oeStore != nil {
		for topic := range n.topics {
			if strings.HasPrefix(topic, "tm_nektar_") {
				continue
			}
			var since float64
			for {
				outs, err := n.oeStore.FindUTXOsForTopic(ctx, topic, since, 1000, true)
				if err != nil {
					return fmt.Errorf("hub: restore topic %s: %w", topic, err)
				}
				for _, o := range outs {
					r, cErr := oemodule.Output(o)
					if cErr != nil {
						return cErr
					}
					rows = append(rows, *r)
					if o.Score+1 > since {
						since = o.Score + 1
					}
				}
				if len(outs) < 1000 {
					break
				}
			}
		}
	}
	seam := &oeRestoreSeam{st: n.oeStore}
	for name, ls := range n.lookups {
		if name == bstack.LookupService {
			continue
		}
		if r, ok := ls.(module.Restorer); ok {
			if _, err := r.Restore(ctx, rows, seam); err != nil {
				return fmt.Errorf("hub: restore lookup %s: %w", name, err)
			}
		}
	}
	return nil
}

func (n *Node) recordModuleOutput(ctx context.Context, topic string, txid *chainhash.Hash, vout uint32, beef *transaction.Beef, rawBEEF []byte, ancillary []*chainhash.Hash) {
	op := transaction.Outpoint{Txid: *txid, Index: vout}
	key := op.String()
	n.mu.Lock()
	if n.modOutputs == nil {
		n.modOutputs = make(map[string]modOutputEntry)
	}
	if _, exists := n.modOutputs[key]; !exists {
		if len(n.modOutOrder) >= maxInMemModOutputs {
			evict := n.modOutOrder[0]
			n.modOutOrder = n.modOutOrder[1:]
			delete(n.modOutputs, evict)
		}
		n.modOutOrder = append(n.modOutOrder, key)
	}
	n.modOutputs[key] = modOutputEntry{
		topic: topic,
		beef:  append([]byte(nil), rawBEEF...),
	}
	n.mu.Unlock()

	if n.oeStore != nil {
		_ = n.oeStore.InsertOutputs(ctx, topic, txid, []uint32{vout}, nil, beef, ancillary)
		_ = n.oeStore.InsertAppliedTransaction(ctx, &overlay.AppliedTransaction{Txid: txid, Topic: topic})
	}
}

func (n *Node) findModuleOutpointTopic(ctx context.Context, op transaction.Outpoint) (topic string, spent bool, found bool) {
	key := op.String()
	n.mu.RLock()
	if ent, ok := n.modOutputs[key]; ok {
		n.mu.RUnlock()
		return ent.topic, ent.spent, true
	}
	n.mu.RUnlock()
	if n.oeStore != nil {
		if o, err := n.oeStore.FindOutput(ctx, &op, nil, nil, false); err == nil && o != nil {
			return o.Topic, o.Spent, true
		}
	}
	return "", false, false
}

func (n *Node) markModuleOutpointSpent(ctx context.Context, op transaction.Outpoint, topic string) {
	key := op.String()
	n.mu.Lock()
	if ent, ok := n.modOutputs[key]; ok {
		ent.spent = true
		n.modOutputs[key] = ent
	}
	n.mu.Unlock()
	if n.oeStore != nil && topic != "" {
		_ = n.oeStore.DeleteOutput(ctx, &op, topic)
	}
}

func (n *Node) hydrateLookupFormulas(ctx context.Context, ans *lookup.LookupAnswer) *lookup.LookupAnswer {
	if ans == nil || ans.Type != lookup.AnswerTypeFormula {
		return ans
	}
	outs := make([]*lookup.OutputListItem, 0, len(ans.Formulas))
	for _, f := range ans.Formulas {
		if f.Outpoint == nil {
			continue
		}
		key := f.Outpoint.String()
		var raw []byte
		n.mu.RLock()
		if ent, ok := n.modOutputs[key]; ok && len(ent.beef) > 0 {
			raw = ent.beef
		}
		n.mu.RUnlock()
		if len(raw) == 0 && n.oeStore != nil {
			if o, err := n.oeStore.FindOutput(ctx, f.Outpoint, nil, nil, true); err == nil && o != nil && o.Beef != nil {
				if ab, aErr := o.Beef.AtomicBytes(&o.Outpoint.Txid); aErr == nil {
					raw = ab
				}
			}
		}
		if len(raw) > 0 {
			outs = append(outs, &lookup.OutputListItem{
				Beef:        raw,
				OutputIndex: f.Outpoint.Index,
			})
		}
	}
	return &lookup.LookupAnswer{
		Type:    lookup.AnswerTypeOutputList,
		Outputs: outs,
	}
}

// OverlayEngine returns the embedded LightBSV go-overlay-services Engine.
func (n *Node) OverlayEngine() *engine.Engine {
	if n == nil {
		return nil
	}
	return n.oeEngine
}

// FabricServer returns the embedded BRC-223 topic interest & BRC-149 unicast distributor server (when enabled).
func (n *Node) FabricServer() *fabric.HubInterestServer {
	if n == nil {
		return nil
	}
	return n.fabricSrv
}

// EventBus returns the Hub's attached in-process EventBus (if configured).
func (n *Node) EventBus() *eventbus.Bus {
	if n == nil {
		return nil
	}
	n.mu.RLock()
	defer n.mu.RUnlock()
	return n.eventBus
}

// SetEventBus attaches an in-process EventBus so local agents wake in <1ms on Hub admissions.
func (n *Node) SetEventBus(bus *eventbus.Bus) {
	if n == nil {
		return
	}
	n.mu.Lock()
	n.eventBus = bus
	n.mu.Unlock()
}

// SetHandleResolver attaches a BRC-169 `/.well-known/metanet-handles/resolve` handler for Tier 1 handles.
func (n *Node) SetHandleResolver(fn http.HandlerFunc) {
	if n == nil {
		return
	}
	n.mu.Lock()
	n.handleResolver = fn
	n.mu.Unlock()
}

// PoisonFramesSkipped returns the count of permanently invalid BRC-149 / catch-up frames skipped via ARCH-F5.
func (n *Node) PoisonFramesSkipped() uint64 {
	if n == nil {
		return 0
	}
	return n.poisonSkipped.Load()
}

// DefaultBboxOffice returns this Hub's mounted Lightweb bbox office identifier (`<name>_<10letters>`).
func (n *Node) DefaultBboxOffice() string {
	if n == nil {
		return ""
	}
	n.mu.RLock()
	defer n.mu.RUnlock()
	return n.defaultBboxOffice
}

// DefaultBstoreBucket returns this Hub's mounted Lightweb bstore bucket identifier (`<name>_<10letters>`).
func (n *Node) DefaultBstoreBucket() string {
	if n == nil {
		return ""
	}
	n.mu.RLock()
	defer n.mu.RUnlock()
	return n.defaultBstoreBucket
}

// resolveTopicManager looks up a mounted topic manager or dynamically instantiates a semantic/ontology
// MarketTopicManager (`tm_nektar_<net>_dom_<slug>` / `tm_nektar_<net>_ont_<id16>`) on demand.
func (n *Node) resolveTopicManager(topic string) (module.TopicManager, bool) {
	n.mu.RLock()
	tm, ok := n.topics[topic]
	if ok {
		n.mu.RUnlock()
		return tm, true
	}
	// Compute ownerLane BEFORE ever escalating to an exclusive write lock so non-owned
	// dynamic semantic/ontology topics reject lock-free under RLock!
	domPrefix := "tm_nektar_" + n.net + "_dom_"
	ontPrefix := "tm_nektar_" + n.net + "_ont_"
	var ownerLane string
	if strings.HasPrefix(topic, domPrefix) {
		ownerLane = bstack.LaneOf(n.net, strings.TrimPrefix(topic, domPrefix))
	} else if strings.HasPrefix(topic, ontPrefix) {
		rest := strings.TrimPrefix(topic, ontPrefix)
		if len(rest) > 0 {
			ownerLane = "tm_nektar_" + n.net + "_" + rest[:1]
		}
	}
	if ownerLane != "" {
		if _, owns := n.topics[ownerLane]; !owns {
			n.mu.RUnlock()
			return nil, false
		}
	}
	n.mu.RUnlock()

	dyn := bstack.DynamicTopicForMarket(n.net, topic)
	if dyn == nil {
		return nil, false
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if existing, exists := n.topics[topic]; exists {
		return existing, true
	}
	if ownerLane != "" {
		if _, owns := n.topics[ownerLane]; !owns {
			return nil, false
		}
	}
	if len(n.topics) >= maxDynamicMarketTopics {
		return dyn, true
	}
	n.topics[topic] = dyn
	if n.oeEngine != nil {
		n.oeEngine.RegisterTopicManager(topic, n.wrapEngineTopicManager(dyn))
	}
	return dyn, true
}

func (n *Node) verifyBEEFWithSPV(ctx context.Context, rawBEEF []byte, tx *transaction.Transaction) error {
	if len(tx.Inputs) == 0 || n.tracker == nil {
		return nil
	}
	if n.spvVerifier == nil {
		return nil
	}
	vd := n.spvVerifier.Verify(ctx, rawBEEF)
	switch vd.Result {
	case oespv.ResultOK:
		return nil
	case oespv.ResultError:
		return fmt.Errorf("%w: tracker unavailable (%s): %v", ErrHubTransient, vd.Reason, vd.Err)
	default:
		return fmt.Errorf("%w: SPV verification refused (%s): %v", ErrHubRefused, vd.Reason, vd.Err)
	}
}

func (n *Node) handleWellKnownResolve(w http.ResponseWriter, r *http.Request) {
	n.mu.RLock()
	fn := n.handleResolver
	n.mu.RUnlock()
	if fn != nil {
		fn(w, r)
		return
	}
	writeJSON(w, http.StatusNotFound, map[string]string{"error": "handle resolver not configured on this hub"})
}

// handleManifestJSON serves a strict BRC-180 + BRC-68 + BRC-169 `/manifest.json` document compatible
// with Lightweb's `bcommon/resolve.ParseManifest` and `FetchManifest`.
func (n *Node) handleManifestJSON(w http.ResponseWriter, r *http.Request) {
	scheme := "https"
	if r.TLS == nil && !strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "http"
	}
	host := strings.TrimSpace(r.Host)
	if host == "" {
		host = "127.0.0.1"
	}
	baseURL := scheme + "://" + host
	if strings.HasSuffix(r.URL.Path, "/hub/manifest.json") {
		baseURL += "/hub"
	}

	n.mu.RLock()
	overlays := make(map[string]string, len(n.topics)+len(n.lookups))
	for name := range n.topics {
		overlays[name] = baseURL
	}
	for name := range n.lookups {
		overlays[name] = baseURL
	}
	hasFinger := n.lookups["ls_finger"] != nil
	hasResolver := n.handleResolver != nil
	bboxOffice := n.defaultBboxOffice
	bstoreBucket := n.defaultBstoreBucket
	n.mu.RUnlock()

	metanet := map[string]any{
		"overlays": overlays,
	}
	if pub := n.PayeeIdentityKey(); pub != "" {
		metanet["trust"] = map[string]any{
			"name":      "nektar-hub",
			"publicKey": pub,
		}
	}
	if hasFinger || hasResolver || bboxOffice != "" {
		handles := map[string]any{
			"version": "1.0",
			"resolve": baseURL + "/.well-known/metanet-handles/resolve",
		}
		if bboxOffice != "" {
			handles["messagebox"] = bboxOffice
		}
		metanet["handles"] = handles
	}
	if bstoreBucket != "" {
		metanet["bstore"] = map[string]any{
			"defaultBucket": bstoreBucket,
		}
	}
	if n.fabricSrv != nil {
		metanet["brc223"] = map[string]any{
			"udpAddr": n.fabricSrv.UDPAddr().String(),
		}
	}

	writeJSONWithETag(w, r, map[string]any{
		"metanet": metanet,
	})
}

func (n *Node) handleRequestForeignGASP(w http.ResponseWriter, r *http.Request) {
	if n.oeEngine == nil || n.oeEngine.Storage == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "GASP requires disk-backed overlay storage"})
		return
	}
	topic := strings.TrimSpace(r.Header.Get("X-BSV-Topic"))
	var req struct {
		GraphID  *transaction.Outpoint `json:"graphID"`
		TxID     *chainhash.Hash       `json:"txid"`
		OutIndex uint32                `json:"outputIndex"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil || req.TxID == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid GASP foreign node request"})
		return
	}
	outpoint := &transaction.Outpoint{Txid: *req.TxID, Index: req.OutIndex}
	node, err := n.oeEngine.ProvideForeignGASPNode(r.Context(), req.GraphID, outpoint, topic)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, node)
}

func (n *Node) handleProvideForeignGASP(w http.ResponseWriter, _ *http.Request) {
	var ack gasp.InitialResponse
	writeJSON(w, http.StatusOK, ack)
}
