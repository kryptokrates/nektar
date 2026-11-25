//go:build ignore

// Run with: go run ./docs/formal/run.go
// Model-checks docs/formal/AORG.tla with TLC inside Docker (no local Java or Node.js needed).
// Pass: AORG.cfg finds no error AND AORG_mutant.cfg violates AppendOnly.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
)

const (
	jarURL    = "https://github.com/tlaplus/tlaplus/releases/download/v1.7.4/tla2tools.jar"
	jarSHA256 = "936a262061c914694dfd669a543be24573c45d5aa0ff20a8b96b23d01e050e88"
)

func main() {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		fatalf("cannot resolve script directory")
	}
	here := filepath.Dir(thisFile)
	jarPath := filepath.Join(here, ".tools", "tla2tools.jar")

	if _, err := os.Stat(jarPath); os.IsNotExist(err) {
		if err := os.MkdirAll(filepath.Dir(jarPath), 0o755); err != nil {
			fatalf("mkdir: %v", err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, jarURL, nil)
		if err != nil {
			fatalf("request tla2tools.jar: %v", err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			fatalf("download tla2tools.jar: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			fatalf("download tla2tools.jar: HTTP %d", resp.StatusCode)
		}
		raw, err := io.ReadAll(resp.Body)
		if err != nil {
			fatalf("read tla2tools.jar: %v", err)
		}
		if err := os.WriteFile(jarPath, raw, 0o644); err != nil {
			fatalf("write tla2tools.jar: %v", err)
		}
	}

	raw, err := os.ReadFile(jarPath)
	if err != nil {
		fatalf("read jar: %v", err)
	}
	sum := sha256.Sum256(raw)
	if hex.EncodeToString(sum[:]) != jarSHA256 {
		fatalf("tla2tools.jar sha256 mismatch: got %x", sum)
	}

	implOK, implOut := runTLC(here, "AORG.cfg")
	mutOK, mutOut := runTLC(here, "AORG_mutant.cfg")

	statesRe := regexp.MustCompile(`(\d+) distinct states found`)
	states := "?"
	if m := statesRe.FindStringSubmatch(implOut); len(m) == 2 {
		states = m[1]
	}
	passedImpl := implOK && strings.Contains(implOut, "No error has been found")
	caughtMut := !mutOK && strings.Contains(mutOut, "Invariant AppendOnly is violated")

	fmt.Printf("AORG.cfg: %s (%s distinct states)\n", statusWord(passedImpl), states)
	fmt.Printf("AORG_mutant.cfg: %s\n", statusWord(caughtMut))
	if !passedImpl || !caughtMut {
		fmt.Println(tail(implOut, 1500))
		fmt.Println(tail(mutOut, 1500))
		os.Exit(1)
	}
}

func runTLC(specDir, cfg string) (bool, string) {
	ctx, cancel := context.WithTimeout(context.Background(), 110*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", "run", "--rm",
		"-v", specDir+":/spec",
		"-w", "/spec",
		"eclipse-temurin:21-jre",
		"java", "-cp", ".tools/tla2tools.jar", "tlc2.TLC",
		"-workers", "2", "-deadlock", "-config", cfg, "AORG.tla",
	)
	cmd.Env = append(os.Environ(), "MSYS_NO_PATHCONV=1")
	out, err := cmd.CombinedOutput()
	return err == nil, string(out)
}

func statusWord(ok bool) string {
	if ok {
		return "PASS"
	}
	return "FAIL"
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
