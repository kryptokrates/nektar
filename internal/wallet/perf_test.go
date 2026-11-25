package wallet

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lightwebinc/bcommon/testchain"
)

func percentile(durs []time.Duration, pct float64) time.Duration {
	if len(durs) == 0 {
		return 0
	}
	cp := append([]time.Duration(nil), durs...)
	sort.Slice(cp, func(i, j int) bool { return cp[i] < cp[j] })
	idx := int(float64(len(cp)-1) * pct)
	if idx < 0 {
		idx = 0
	}
	if idx >= len(cp) {
		idx = len(cp) - 1
	}
	return cp[idx]
}

// TestPerfMixedReadWriteLoad instruments wallet hot paths under simultaneous high-concurrency
// writes (CreateAction/Send + background FuelKeeper) and dashboard/API reads (Balance, ListUTXOs,
// ListRecentActions) after accumulating hundreds of historical transactions in wallet.sqlite.
func TestPerfMixedReadWriteLoad(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping heavy perf soak in -short mode")
	}
	ctx := context.Background()
	tc := testchain.New(1)
	var tcMu sync.Mutex

	dir := t.TempDir()
	prof := ThroughputFuelProfile()
	prof.FuelTarget = 128
	prof.FuelSats = 1000
	prof.LowWaterPct = 40
	prof.MaxFanOutOutputs = 128

	goroutinesStart := runtime.NumGoroutine()
	eng := newTestEngine(t, tc, &tcMu, dir, &prof)

	initialSats := seedTestEngine(t, tc, &tcMu, eng, 8)
	if _, err := eng.FuelKeeper().WarmFuel(ctx, 128, 1000); err != nil {
		t.Fatalf("WarmFuel: %v", err)
	}

	bgCtx, bgCancel := context.WithCancel(ctx)
	eng.StartBackground(bgCtx)

	const (
		writers     = 32
		txsPerWrite = 16 // 512 writes
		readers     = 12
		sendEach    = 200
	)
	destAddr, _ := randomTestAddress(t)

	var (
		writeLatMu sync.Mutex
		writeLats  = make([]time.Duration, 0, writers*txsPerWrite)
		balLatMu   sync.Mutex
		balLats    = make([]time.Duration, 0, 512)
		utxoLatMu  sync.Mutex
		utxoLats   = make([]time.Duration, 0, 512)
		actLatMu   sync.Mutex
		actLats    = make([]time.Duration, 0, 512)

		writeOK   atomic.Int64
		writeErrs atomic.Int64
		readOK    atomic.Int64
		readErrs  atomic.Int64
		totalSent atomic.Uint64
		firstErr  atomic.Value
	)

	stopReaders := make(chan struct{})
	var readerWG sync.WaitGroup
	for r := 0; r < readers; r++ {
		readerWG.Add(1)
		go func(rid int) {
			defer readerWG.Done()
			for {
				select {
				case <-stopReaders:
					return
				default:
				}
				t0 := time.Now()
				switch rid % 3 {
				case 0:
					_, err := eng.Balance(ctx)
					d := time.Since(t0)
					if err != nil {
						readErrs.Add(1)
					} else {
						readOK.Add(1)
						balLatMu.Lock()
						balLats = append(balLats, d)
						balLatMu.Unlock()
					}
				case 1:
					_, _, err := eng.ListUTXOs(ctx, "all", false, 100, 0)
					d := time.Since(t0)
					if err != nil {
						readErrs.Add(1)
					} else {
						readOK.Add(1)
						utxoLatMu.Lock()
						utxoLats = append(utxoLats, d)
						utxoLatMu.Unlock()
					}
				case 2:
					_, _, err := eng.ListRecentActions(ctx, 25, 0)
					d := time.Since(t0)
					if err != nil {
						readErrs.Add(1)
					} else {
						readOK.Add(1)
						actLatMu.Lock()
						actLats = append(actLats, d)
						actLatMu.Unlock()
					}
				}
				time.Sleep(5 * time.Millisecond)
			}
		}(r)
	}

	var memBefore runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&memBefore)

	start := time.Now()
	var writeWG sync.WaitGroup
	for w := 0; w < writers; w++ {
		writeWG.Add(1)
		go func() {
			defer writeWG.Done()
			for i := 0; i < txsPerWrite; i++ {
				var err error
				var res *SendResult
				t0 := time.Now()
				for retry := 0; retry < 5; retry++ {
					res, err = eng.Send(ctx, SendArgs{
						ToAddress:   destAddr,
						Satoshis:    sendEach,
						Description: "mixed perf load tx",
					})
					if err == nil {
						break
					}
					time.Sleep(10 * time.Millisecond)
				}
				d := time.Since(t0)
				if err != nil {
					writeErrs.Add(1)
					firstErr.CompareAndSwap(nil, err.Error())
				} else {
					writeOK.Add(1)
					totalSent.Add(res.Satoshis)
					writeLatMu.Lock()
					writeLats = append(writeLats, d)
					writeLatMu.Unlock()
				}
			}
		}()
	}
	writeWG.Wait()
	elapsed := time.Since(start)
	close(stopReaders)
	readerWG.Wait()

	// Post-load isolated micro-measurements on a 500+ tx database!
	const probeN = 20
	var postBal, postUTXO, postAct []time.Duration
	for i := 0; i < probeN; i++ {
		t0 := time.Now()
		_, _ = eng.Balance(ctx)
		postBal = append(postBal, time.Since(t0))

		t0 = time.Now()
		_, _, _ = eng.ListUTXOs(ctx, "all", false, 100, 0)
		postUTXO = append(postUTXO, time.Since(t0))

		t0 = time.Now()
		_, _, _ = eng.ListRecentActions(ctx, 25, 0)
		postAct = append(postAct, time.Since(t0))
	}

	var memAfter runtime.MemStats
	runtime.ReadMemStats(&memAfter)

	var dbSize, walSize int64
	if st, err := os.Stat(filepath.Join(dir, "wallet.sqlite")); err == nil {
		dbSize = st.Size()
	}
	if st, err := os.Stat(filepath.Join(dir, "wallet.sqlite-wal")); err == nil {
		walSize = st.Size()
	}

	finalBal, err := eng.Balance(ctx)
	if err != nil {
		t.Fatalf("final Balance: %v", err)
	}
	allTxs, _, _ := eng.ListRecentActions(ctx, 2000, 0)
	var allFees uint64
	for _, r := range allTxs {
		if r.IsOutgoing && r.Status != StatusFailed && r.Status != StatusAborted {
			allFees += r.FeeSats
		}
	}

	bgCancel()
	_ = eng.Close()
	time.Sleep(50 * time.Millisecond)
	goroutinesEnd := runtime.NumGoroutine()

	writeTPS := float64(writeOK.Load()) / elapsed.Seconds()
	readQPS := float64(readOK.Load()) / elapsed.Seconds()

	t.Logf("=== MIXED LOAD PERF REPORT (512 writes + %d concurrent reads in %v) ===", readOK.Load(), elapsed.Round(time.Millisecond))
	t.Logf("  Writes : %d OK / %d err | %.1f TPS | p50=%v p95=%v p99=%v max=%v",
		writeOK.Load(), writeErrs.Load(), writeTPS,
		percentile(writeLats, 0.50).Round(time.Microsecond),
		percentile(writeLats, 0.95).Round(time.Microsecond),
		percentile(writeLats, 0.99).Round(time.Microsecond),
		percentile(writeLats, 1.00).Round(time.Microsecond))
	t.Logf("  Reads  : %d OK / %d err | %.1f QPS", readOK.Load(), readErrs.Load(), readQPS)
	t.Logf("    Under-load Balance           : n=%d p50=%v p95=%v p99=%v", len(balLats), percentile(balLats, 0.50).Round(time.Microsecond), percentile(balLats, 0.95).Round(time.Microsecond), percentile(balLats, 0.99).Round(time.Microsecond))
	t.Logf("    Under-load ListUTXOs(100)    : n=%d p50=%v p95=%v p99=%v", len(utxoLats), percentile(utxoLats, 0.50).Round(time.Microsecond), percentile(utxoLats, 0.95).Round(time.Microsecond), percentile(utxoLats, 0.99).Round(time.Microsecond))
	t.Logf("    Under-load ListRecentAct(25) : n=%d p50=%v p95=%v p99=%v", len(actLats), percentile(actLats, 0.50).Round(time.Microsecond), percentile(actLats, 0.95).Round(time.Microsecond), percentile(actLats, 0.99).Round(time.Microsecond))
	t.Logf("  Post-500tx isolated reads:")
	t.Logf("    Balance()           : p50=%v p95=%v", percentile(postBal, 0.50).Round(time.Microsecond), percentile(postBal, 0.95).Round(time.Microsecond))
	t.Logf("    ListUTXOs(100)      : p50=%v p95=%v", percentile(postUTXO, 0.50).Round(time.Microsecond), percentile(postUTXO, 0.95).Round(time.Microsecond))
	t.Logf("    ListRecentAct(25)   : p50=%v p95=%v", percentile(postAct, 0.50).Round(time.Microsecond), percentile(postAct, 0.95).Round(time.Microsecond))
	t.Logf("  Memory & Disk: HeapInuse=%.1fMB TotalAllocDelta=%.1fMB SQLite=%.2fMB WAL=%.2fMB Goroutines=%d->%d",
		float64(memAfter.HeapInuse)/(1024*1024),
		float64(memAfter.TotalAlloc-memBefore.TotalAlloc)/(1024*1024),
		float64(dbSize)/(1024*1024),
		float64(walSize)/(1024*1024),
		goroutinesStart, goroutinesEnd)

	if writeErrs.Load() > 0 {
		t.Fatalf("write errors: %d (first: %v)", writeErrs.Load(), firstErr.Load())
	}
	if readErrs.Load() > 0 {
		t.Fatalf("read errors: %d", readErrs.Load())
	}
	if finalBal.TotalSats+totalSent.Load()+allFees != initialSats {
		t.Fatalf("conservation violation: rem(%d) + sent(%d) + fees(%d) = %d != initial(%d)",
			finalBal.TotalSats, totalSent.Load(), allFees, finalBal.TotalSats+totalSent.Load()+allFees, initialSats)
	}
	if goroutinesEnd > goroutinesStart+8 {
		t.Fatalf("possible goroutine leak: start=%d end=%d", goroutinesStart, goroutinesEnd)
	}
	_ = fmt.Sprintf("ok")
}
