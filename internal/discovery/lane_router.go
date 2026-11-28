package discovery

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

type hubTopicCacheEntry struct {
	topics    map[string]bool
	fetchedAt time.Time
}

// hubLaneRouter caches each Hub's mounted topics (from GET /health) with a 60s TTL
// so BuildAndSubmitMarketAd routes WantAd/OfferAd strictly to the Hub(s) owning those topic lanes
// instead of broadcasting redundantly to every sharded Hub.
type hubLaneRouter struct {
	mu      sync.RWMutex
	entries map[string]hubTopicCacheEntry
}

func newHubLaneRouter() *hubLaneRouter {
	return &hubLaneRouter{
		entries: make(map[string]hubTopicCacheEntry),
	}
}

// SetHostTopics explicitly populates the mounted topic set for hostURL (useful for tests & prewarming).
func (r *hubLaneRouter) SetHostTopics(hostURL string, topics []string) {
	if r == nil {
		return
	}
	hu := normalizeHostURL(hostURL)
	if hu == "" {
		return
	}
	m := make(map[string]bool, len(topics))
	for _, t := range topics {
		if t = strings.TrimSpace(t); t != "" {
			m[t] = true
		}
	}
	r.mu.Lock()
	r.entries[hu] = hubTopicCacheEntry{topics: m, fetchedAt: time.Now()}
	r.mu.Unlock()
}

// FilterHostsForTopics returns only the hosts from hosts that mount at least one topic in topics.
// If any host has no cached topic map yet, it fetches /health once (bounded 500ms); if no host matches
// (e.g. legacy Hub not reporting mountedTopics), it falls back safely to all hosts.
func (r *hubLaneRouter) FilterHostsForTopics(ctx context.Context, client *http.Client, hosts []string, topics []string) []string {
	if r == nil || len(hosts) <= 1 || len(topics) == 0 {
		return hosts
	}
	now := time.Now()
	const ttl = 60 * time.Second

	var matched []string
	var unknown []string
	for _, rawHost := range hosts {
		hu := normalizeHostURL(rawHost)
		if hu == "" {
			continue
		}
		r.mu.RLock()
		ent, ok := r.entries[hu]
		r.mu.RUnlock()
		if !ok || now.Sub(ent.fetchedAt) > ttl {
			fetched := r.fetchHostTopics(ctx, client, hu)
			if fetched != nil {
				r.mu.Lock()
				r.entries[hu] = hubTopicCacheEntry{topics: fetched, fetchedAt: time.Now()}
				r.mu.Unlock()
				ent = hubTopicCacheEntry{topics: fetched, fetchedAt: now}
				ok = true
			}
		}
		if !ok || len(ent.topics) == 0 {
			unknown = append(unknown, rawHost)
			continue
		}
		hit := false
		for _, t := range topics {
			if ent.topics[t] {
				hit = true
				break
			}
		}
		if hit {
			matched = append(matched, rawHost)
		}
	}
	if len(matched) > 0 {
		return append(matched, unknown...)
	}
	return hosts
}

func (r *hubLaneRouter) fetchHostTopics(ctx context.Context, client *http.Client, hostURL string) map[string]bool {
	if client == nil {
		return nil
	}
	fCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(fCtx, http.MethodGet, strings.TrimRight(hostURL, "/")+"/health", nil)
	if err != nil {
		return nil
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	var payload struct {
		MountedTopics []string `json:"mountedTopics"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 256<<10)).Decode(&payload); err != nil {
		return nil
	}
	if len(payload.MountedTopics) == 0 {
		return nil
	}
	m := make(map[string]bool, len(payload.MountedTopics))
	for _, t := range payload.MountedTopics {
		if t = strings.TrimSpace(t); t != "" {
			m[t] = true
		}
	}
	return m
}
