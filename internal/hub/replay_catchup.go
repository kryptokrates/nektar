package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/bsv-blockchain/go-sdk/overlay/lookup"
	"github.com/kryptokrates/nektar/internal/bstack"
)

// CatchUpResult summarizes one bounded BRC-24 /lookup backfill pass from an explicit sibling Hub
// (strictly separated from live BRC-149 propagation, per 1BSV Paper 008 §6 & bcommon/replay).
type CatchUpResult struct {
	Peer     string `json:"peer"`
	Fetched  int    `json:"fetched"`
	Admitted int    `json:"admitted"`
	Skipped  int    `json:"skipped"`
	LastSeq  int64  `json:"lastSeq"`
}

// CatchUpFromPeer runs a bounded, cursor-resumable BRC-24 POST /lookup catch-up walk against
// an explicit sibling peer Hub (NEKTAR_CATCHUP_PEERS). Used on startup or after a gap exceeding
// the plane's NACK repair window so offline Hubs converge cleanly before riding the live stream.
func (n *Node) CatchUpFromPeer(ctx context.Context, peerURL string, maxPages int) (CatchUpResult, error) {
	peer := strings.TrimRight(strings.TrimSpace(peerURL), "/")
	res := CatchUpResult{Peer: peer}
	if peer == "" {
		return res, nil
	}
	if maxPages <= 0 {
		maxPages = 20
	}
	cursorKey := "catchup:" + peer
	cursor := n.store.GetFedCursor(cursorKey)
	res.LastSeq = cursor

	for page := 0; page < maxPages; page++ {
		qBytes, _ := json.Marshal(bstack.MarketQuery{
			SinceSeq: cursor,
			Limit:    200,
		})
		body, _ := json.Marshal(&lookup.LookupQuestion{
			Service: bstack.LookupService,
			Query:   qBytes,
		})
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, peer+"/lookup", bytes.NewReader(body))
		if err != nil {
			return res, err
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := n.httpClient.Do(req)
		if err != nil {
			return res, fmt.Errorf("catchup %s: %w", peer, err)
		}
		raw, rErr := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
		resp.Body.Close()
		if rErr != nil {
			return res, rErr
		}
		if resp.StatusCode != http.StatusOK {
			return res, fmt.Errorf("catchup %s: HTTP %d: %s", peer, resp.StatusCode, strings.TrimSpace(string(raw)))
		}
		var ans struct {
			Type   string               `json:"type"`
			Result []bstack.MarketEvent `json:"result"`
		}
		if err := json.Unmarshal(raw, &ans); err != nil {
			return res, fmt.Errorf("catchup %s: decode lookup answer: %w", peer, err)
		}
		if len(ans.Result) == 0 {
			break
		}
		var maxSeq int64
		for _, ev := range ans.Result {
			res.Fetched++
			if ev.Seq > maxSeq {
				maxSeq = ev.Seq
			}
			if len(ev.BEEF) == 0 || ev.Topic == "" {
				res.Skipped++
				continue
			}
			if !n.loopGuard.MarkIfUnseen(ev.BEEF, ev.Topic) {
				res.Skipped++
				continue
			}
			if _, sErr := n.submitInternal(ctx, ev.BEEF, []string{ev.Topic}, true); sErr == nil {
				res.Admitted++
			} else {
				res.Skipped++
			}
		}
		if maxSeq <= cursor {
			break
		}
		cursor = maxSeq
		res.LastSeq = cursor
		_ = n.store.SetFedCursor(cursorKey, cursor)
		if len(ans.Result) < 200 {
			break
		}
	}
	return res, nil
}
