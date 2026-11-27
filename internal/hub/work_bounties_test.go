package hub

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/chain"
	"github.com/kryptokrates/nektar/internal/core"
	"github.com/kryptokrates/nektar/internal/memory"
	"github.com/kryptokrates/nektar/internal/store"
)

func TestHubZeroSatWorkBountiesAndSponsoredBounties(t *testing.T) {
	ctx := context.Background()
	hubDB, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer hubDB.Close()
	hubPort, err := chain.Open(ctx, chain.Config{Network: "memory", DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer hubPort.Close()
	hubMem, err := memory.New(hubDB, hubPort, nil, nil, memory.Config{})
	if err != nil {
		t.Fatal(err)
	}

	node, err := NewNode(Config{Network: "memory"})
	if err != nil {
		t.Fatal(err)
	}
	defer node.Close()

	if _, err := ArmHubLiquidity(ctx, node, hubMem, LiquidityConfig{
		SeedStarterShards: false,
		ArmBounties:       true,
		BountyRewardSats:  100,
	}); err != nil {
		t.Fatalf("ArmHubLiquidity: %v", err)
	}
	node.SetWorkCaps(100, 1000)

	srv := httptest.NewServer(node.Handler())
	defer srv.Close()

	// Fresh 0-sat worker identity.
	workerKey, _ := ec.NewPrivateKey()
	workerPub := strings.ToLower(hex.EncodeToString(workerKey.PubKey().Compressed()))
	workerPort, err := chain.Open(ctx, chain.Config{Network: "memory", DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer workerPort.Close()
	workerAddr, err := workerPort.FundAddress()
	if err != nil {
		t.Fatal(err)
	}

	// Track A: List 0-sat work bounties, audit one, submit proof, verify AtomicBEEF payout!
	resp, err := http.Get(srv.URL + "/work-bounties")
	if err != nil {
		t.Fatalf("GET /work-bounties: %v", err)
	}
	var listResp struct {
		Tasks []WorkTask `json:"tasks"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&listResp); err != nil {
		t.Fatalf("decode work tasks: %v", err)
	}
	resp.Body.Close()
	if len(listResp.Tasks) == 0 {
		t.Fatal("expected active 0-sat work bounty tasks")
	}

	task := listResp.Tasks[0]
	digest, count, err := ComputeWorkAuditDigest(task.TaskID, task.Kind, task.ChallengeHex, task.Payload)
	if err != nil {
		t.Fatalf("ComputeWorkAuditDigest: %v", err)
	}
	wSub := WorkSubmission{
		TaskID:        task.TaskID,
		WorkerPub:     workerPub,
		WorkerAddress: workerAddr,
		AuditDigest:   digest,
		VerifiedCount: count,
		TS:            time.Now().Unix(),
	}
	wSub.Sig, err = bstack.SignArtifact(workerKey, wSub.Signable())
	if err != nil {
		t.Fatalf("SignArtifact work sub: %v", err)
	}
	body, _ := json.Marshal(wSub)
	pResp, err := http.Post(srv.URL+"/work-bounties/submit", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /work-bounties/submit: %v", err)
	}
	if pResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK on work bounty submit, got %d", pResp.StatusCode)
	}
	var payout WorkPayoutResult
	if err := json.NewDecoder(pResp.Body).Decode(&payout); err != nil {
		t.Fatalf("decode work payout: %v", err)
	}
	pResp.Body.Close()
	if !payout.Accepted || payout.TxID == "" || payout.BEEFHex == "" || payout.RewardSats != task.RewardSats {
		t.Fatalf("unexpected work payout: %+v", payout)
	}

	// Replay of same task MUST fail!
	rResp, _ := http.Post(srv.URL+"/work-bounties/submit", "application/json", bytes.NewReader(body))
	if rResp.StatusCode == http.StatusOK {
		t.Fatal("expected replayed work bounty task to be rejected")
	}
	rResp.Body.Close()

	// Track B: Hub-sponsored 0-sat knowledge bounty (unanchored signed fragment -> PII scrub -> Hub anchors -> pays net of fee!).
	prov := core.Provenance{Source: "agent", Time: time.Now().Unix(), ModelID: "test/v1"}
	cleanText := "Go 1.27 modernc.org/sqlite prevents SQLITE_BUSY under concurrent writers when SetMaxOpenConns(1) and WAL are enabled."
	provSig, err := workerKey.Sign(core.ProvenanceDigest([]byte(cleanText), prov))
	if err != nil {
		t.Fatal(err)
	}
	prov.Signature = provSig.Serialize()
	provBytes := core.SerializeProvenance(prov)
	hdpHex := hex.EncodeToString(core.ComputeHDPV2([]byte(cleanText), prov))

	bSub := BountySubmission{
		BountyID:      "bounty-starter-sponsored",
		SellerPub:     workerPub,
		ProvenancePub: workerPub,
		SellerAddress: workerAddr,
		Shard:         "market-ops",
		Seq:           1,
		HDP:           hdpHex,
		Text:          cleanText,
		ProvenanceHex: hex.EncodeToString(provBytes),
		TS:            time.Now().Unix(),
	}
	bSub.Sig, _ = bstack.SignArtifact(workerKey, bSub.Signable(hdpHex))
	canon, _ := core.CanonicalJSON(bSub.Signable(hdpHex))
	oSig, _ := workerKey.Sign(core.SHA256(canon))
	bSub.OwnerSig = hex.EncodeToString(oSig.Serialize())

	bBody, _ := json.Marshal(bSub)
	bResp, err := http.Post(srv.URL+"/bounties/submit", "application/json", bytes.NewReader(bBody))
	if err != nil {
		t.Fatalf("POST /bounties/submit sponsored: %v", err)
	}
	if bResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK on sponsored bounty submit, got %d", bResp.StatusCode)
	}
	var bOut BountyPayoutResult
	if err := json.NewDecoder(bResp.Body).Decode(&bOut); err != nil {
		t.Fatal(err)
	}
	bResp.Body.Close()
	// 100 reward - 5 anchor fee reserve = 95 net sats!
	if !bOut.Accepted || bOut.RewardSats != 95 || bOut.TxID == "" || bOut.BEEFHex == "" {
		t.Fatalf("expected sponsored bounty payout of 95 net sats + BEEF, got %+v", bOut)
	}

	// Secret-bearing submission to sponsored bounty MUST be rejected fail-closed by PII scrubber!
	badText := "Never leak sk-proj-1234567890abcdef1234567890abcdef1234567890abcdef on the public market."
	badProv := core.Provenance{Source: "agent", Time: time.Now().Unix(), ModelID: "test/v1"}
	badPSig, _ := workerKey.Sign(core.ProvenanceDigest([]byte(badText), badProv))
	badProv.Signature = badPSig.Serialize()
	badProvBytes := core.SerializeProvenance(badProv)
	badHDP := hex.EncodeToString(core.ComputeHDPV2([]byte(badText), badProv))
	badSub := BountySubmission{
		BountyID:      "bounty-starter-sponsored",
		SellerPub:     workerPub,
		ProvenancePub: workerPub,
		SellerAddress: workerAddr,
		Shard:         "market-ops",
		Seq:           2,
		HDP:           badHDP,
		Text:          badText,
		ProvenanceHex: hex.EncodeToString(badProvBytes),
		TS:            time.Now().Unix(),
	}
	badSub.Sig, _ = bstack.SignArtifact(workerKey, badSub.Signable(badHDP))
	badCanon, _ := core.CanonicalJSON(badSub.Signable(badHDP))
	badOSig, _ := workerKey.Sign(core.SHA256(badCanon))
	badSub.OwnerSig = hex.EncodeToString(badOSig.Serialize())
	badBody, _ := json.Marshal(badSub)
	badResp, _ := http.Post(srv.URL+"/bounties/submit", "application/json", bytes.NewReader(badBody))
	if badResp.StatusCode == http.StatusOK {
		t.Fatal("expected secret-containing sponsored bounty submission to be blocked fail-closed")
	}
	badResp.Body.Close()
}
