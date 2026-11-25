package memory

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"codeberg.org/TauCeti/mangle-go/analysis"
	"codeberg.org/TauCeti/mangle-go/engine"
	"codeberg.org/TauCeti/mangle-go/factstore"
	"codeberg.org/TauCeti/mangle-go/parse"
	cedar "github.com/cedar-policy/cedar-go"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/chain"
	"github.com/kryptokrates/nektar/internal/contracts"
	"github.com/kryptokrates/nektar/internal/policy"
	"github.com/kryptokrates/nektar/internal/reasoning"
	"github.com/kryptokrates/nektar/internal/store"
)

func newBenchService(b *testing.B) *Service {
	b.Helper()
	ctx := context.Background()
	db, err := store.Open(":memory:")
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = db.Close() })
	port, err := chain.Open(ctx, chain.Config{Network: "memory", DataDir: b.TempDir()})
	if err != nil {
		b.Fatal(err)
	}
	svc, err := New(db, port, bstack.NewGroupRegistry(), bstack.NewMemoryObjectStore(), Config{})
	if err != nil {
		b.Fatal(err)
	}
	return svc
}

func BenchmarkRememberUnanchored(b *testing.B) {
	ctx := context.Background()
	svc := newBenchService(b)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := svc.Remember(ctx, RememberInput{
			Shard: "bench",
			Text:  fmt.Sprintf("fragment payload #%d with verifiable provenance", i),
		}); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkAnchorAppend(b *testing.B) {
	ctx := context.Background()
	svc := newBenchService(b)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := svc.Remember(ctx, RememberInput{
			Shard: "anchor-bench",
			Text:  fmt.Sprintf("anchored fact #%d", i),
		}); err != nil {
			b.Fatal(err)
		}
		if _, err := svc.AnchorShard(ctx, "anchor-bench"); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkVerifyFragment16Leaf(b *testing.B) {
	ctx := context.Background()
	svc := newBenchService(b)
	for i := 0; i < 16; i++ {
		if _, err := svc.Remember(ctx, RememberInput{
			Shard: "spv",
			Text:  fmt.Sprintf("leaf %d", i),
		}); err != nil {
			b.Fatal(err)
		}
	}
	if _, err := svc.AnchorShard(ctx, "spv"); err != nil {
		b.Fatal(err)
	}
	bundle, vr, err := svc.Prove(ctx, "spv", 7)
	if err != nil || !vr.OK || !vr.Settled {
		b.Fatalf("setup Prove failed: vr=%+v err=%v", vr, err)
	}
	tracker := svc.Chain().Tracker()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		res := contracts.VerifyFragment(*bundle, tracker)
		if !res.OK || !res.Settled {
			b.Fatalf("verify failed: %+v", res)
		}
	}
}

func BenchmarkCedarAuthorizeParseAndEval(b *testing.B) {
	p := policy.Principal{Type: "Agent", ID: "alice", Level: 5}
	r := policy.Resource{Type: "Fragment", ID: "ops#1", Level: 3, Shard: "ops", Class: "general"}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		d, err := policy.Authorize(policy.DefaultPolicySet, p, "read", r)
		if err != nil || !d.Allow {
			b.Fatalf("authorize failed: %+v %v", d, err)
		}
	}
}

func BenchmarkCedarAuthorizeCompiled(b *testing.B) {
	ps, err := cedar.NewPolicySetFromBytes("default.cedar", []byte(policy.DefaultPolicySet))
	if err != nil {
		b.Fatal(err)
	}
	pUID := cedar.NewEntityUID("Agent", "alice")
	aUID := cedar.NewEntityUID("Action", "read")
	rUID := cedar.NewEntityUID("Fragment", "ops#1")
	entities := cedar.EntityMap{
		pUID: {UID: pUID, Attributes: cedar.NewRecord(cedar.RecordMap{"level": cedar.Long(5)})},
		rUID: {UID: rUID, Attributes: cedar.NewRecord(cedar.RecordMap{"level": cedar.Long(3), "shard": cedar.String("ops"), "class": cedar.String("general")})},
	}
	req := cedar.Request{Principal: pUID, Action: aUID, Resource: rUID, Context: cedar.NewRecord(nil)}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		dec, _ := ps.IsAuthorized(entities, req)
		if dec != cedar.Allow {
			b.Fatal("expected allow")
		}
	}
}

func BenchmarkMangleDatalog601Facts(b *testing.B) {
	// 2 rules + 601 EDB facts (25 disconnected chains of 24 edges + 1 fact -> ~7,500+ derived facts,
	// matching v0.3 BENCHMARKS.md "2 rules, 601 EDB facts").
	var sb strings.Builder
	count := 0
	for c := 0; c < 25; c++ {
		for n := 0; n < 24; n++ {
			fmt.Fprintf(&sb, "edge(\"c%d_n%d\", \"c%d_n%d\").\n", c, n, c, n+1)
			count++
		}
	}
	sb.WriteString("edge(\"extra_a\", \"extra_b\").\n")
	count++
	sb.WriteString("reach(X, Y) :- edge(X, Y).\n")
	sb.WriteString("reach(X, Z) :- edge(X, Y), reach(Y, Z).\n")
	prog := sb.String()
	// Warmup & verify fact count.
	derived, err := reasoning.EvalDatalog(prog)
	if err != nil || len(derived) < 7000 {
		b.Fatalf("setup EvalDatalog: derived=%d err=%v (edb=%d)", len(derived), err, count)
	}
	// Measure both full text parse+eval AND pure indexed Mangle evaluation (matching TS AST bench).
	b.Run("parse+eval", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			out, err := reasoning.EvalDatalog(prog)
			if err != nil || len(out) == 0 {
				b.Fatal(err)
			}
		}
	})
	unit, err := parse.Unit(strings.NewReader(prog))
	if err != nil {
		b.Fatal(err)
	}
	info, err := analysis.AnalyzeOneUnit(unit, nil)
	if err != nil {
		b.Fatal(err)
	}
	b.Run("eval-only", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			fs := factstore.NewMultiIndexedInMemoryStore()
			for _, cl := range unit.Clauses {
				if len(cl.Premises) == 0 {
					fs.Add(cl.Head)
				}
			}
			if err := engine.EvalProgram(info, fs); err != nil {
				b.Fatal(err)
			}
		}
	})
}

func BenchmarkBRC369SealAndOpen(b *testing.B) {
	var sym, cid [32]byte
	sym[0] = 0x42
	cid[0] = 0x99
	pt := []byte("Confidential agent memory payload sealed under BRC-369 epoch key.")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		env, _, err := bstack.SealForEpoch("g1", 1, sym, cid, pt)
		if err != nil {
			b.Fatal(err)
		}
		if _, err := bstack.OpenWithEpoch(env, sym); err != nil {
			b.Fatal(err)
		}
	}
}
