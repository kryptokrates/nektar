package main

import (
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/icellan/runar/compilers/go/compiler"
)

// productionContracts are compiled into both contracts/artifacts/<Name>.json
// and internal/contracts/artifacts/<Name>.json for //go:embed in cmd/nektar.
var productionContracts = []string{
	"NektarAnchor",
	"NektarAutomaton",
	"NektarFeeVault",
	"NektarCheckpointBond",
	"DisclosureSale",
	"NektarZkClaim",
	"NektarGrothClaim",
	"ClaimVerdict",
}

// prototypeContracts are compile-checked to verify zero syntax/type/codegen
// regressions across ZK & BabyBear/Poseidon2 covenants.
var prototypeContracts = []string{
	"NektarAutomatonZk",
	"BabyBearProbe",
	"Poseidon2Stage",
	"Poseidon2Blocks",
}

// benchmarkContracts exceed Bitcoin Script's 800-item stack limit by design
// (unrolled single-tx measurement); we verify parse + typecheck + ANF IR.
var benchmarkContracts = []string{
	"Poseidon2Probe",
}

func main() {
	checkPrototypes := flag.Bool("prototypes", false, "also compile-check ZK prototype contracts")
	writeDir := flag.String("out", "artifacts", "output directory for compiled artifacts")
	embedDir := flag.String("embed", filepath.Join("..", "internal", "contracts", "artifacts"), "embedded artifacts directory to sync")
	srcDir := flag.String("src", "src", "source directory containing *.runar.go contracts")
	flag.Parse()

	if err := os.MkdirAll(*writeDir, 0o755); err != nil {
		fatalf("mkdir %s: %v", *writeDir, err)
	}
	if *embedDir != "" {
		if err := os.MkdirAll(*embedDir, 0o755); err != nil {
			fatalf("mkdir %s: %v", *embedDir, err)
		}
	}

	for _, name := range productionContracts {
		srcPath := filepath.Join(*srcDir, name+".runar.go")
		art, err := compiler.CompileFromSource(srcPath)
		if err != nil {
			fatalf("compile %s (%s): %v", name, srcPath, err)
		}
		raw, err := compiler.ArtifactToJSON(art)
		if err != nil {
			fatalf("serialize %s: %v", name, err)
		}
		outPath := filepath.Join(*writeDir, name+".json")
		if err := os.WriteFile(outPath, raw, 0o644); err != nil {
			fatalf("write %s: %v", outPath, err)
		}
		if *embedDir != "" {
			embPath := filepath.Join(*embedDir, name+".json")
			if err := os.WriteFile(embPath, raw, 0o644); err != nil {
				fatalf("write %s: %v", embPath, err)
			}
		}
		h := sha256.Sum256([]byte(art.Script))
		fmt.Printf("OK %-22s script=%5d B  sha256=%s\n", name, len(art.Script)/2, hex.EncodeToString(h[:8]))
	}

	if *checkPrototypes {
		for _, name := range prototypeContracts {
			srcPath := filepath.Join(*srcDir, name+".runar.go")
			art, err := compiler.CompileFromSource(srcPath)
			if err != nil {
				fatalf("compile prototype %s (%s): %v", name, srcPath, err)
			}
			h := sha256.Sum256([]byte(art.Script))
			fmt.Printf("OK [proto] %-14s script=%6d B  sha256=%s\n", name, len(art.Script)/2, hex.EncodeToString(h[:8]))
		}
		for _, name := range benchmarkContracts {
			srcPath := filepath.Join(*srcDir, name+".runar.go")
			if _, err := compiler.CompileSourceToIR(srcPath); err != nil {
				fatalf("ir-check benchmark %s (%s): %v", name, srcPath, err)
			}
			fmt.Printf("OK [bench] %-14s IR verified (unrolled stack benchmark)\n", name)
		}
	}
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "compile error: "+format+"\n", args...)
	os.Exit(1)
}
