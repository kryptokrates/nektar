package main

import (
	"context"
	"flag"
	"fmt"
	"strings"

	"github.com/kryptokrates/nektar/internal/discovery"
	nwallet "github.com/kryptokrates/nektar/internal/wallet"
)

func cmdDomainDocs(args []string) error {
	fs := flag.NewFlagSet("domain-docs", flag.ExitOnError)
	domain := fs.String("domain", "", "Custom DNS domain (e.g. agents.example.com)")
	handle := fs.String("handle", envOr("NEKTAR_HANDLE", "alice"), "Agent handle on the domain (default: alice)")
	netName := fs.String("network", envOr("NEKTAR_NETWORK", "testnet"), "Network (testnet|main)")
	dataDir := fs.String("data-dir", "", "Data directory override")
	pubFlag := fs.String("pub", "", "Explicit 33-byte compressed identity public key hex (defaults to local node identity)")
	hubURL := fs.String("hub-url", "", "Primary Hub URL hosting bbox & ls_finger")
	bboxOffice := fs.String("office", envOr("NEKTAR_BBOX_OFFICE", "default"), "Bbox office identifier")
	asJSON := fs.Bool("json", false, "Emit machine-readable JSON")
	_ = fs.Parse(args)

	if strings.TrimSpace(*domain) == "" {
		return fmt.Errorf("domain-docs requires --domain <fqdn> (e.g. `nektar domain-docs --domain example.com --handle alice`)")
	}

	pubHex := strings.TrimSpace(*pubFlag)
	if pubHex == "" {
		rt, err := openRuntime(context.Background(), *netName, *dataDir, DefaultLocalPort)
		if err == nil && rt != nil {
			if rt.disc != nil {
				pubHex = rt.disc.AnnouncePubHex()
				if *hubURL == "" {
					if hosts := rt.disc.ActiveHosts(); len(hosts) > 0 {
						*hubURL = hosts[0]
					}
				}
			} else if rt.mem != nil && rt.mem.Chain() != nil {
				pubHex = rt.mem.Chain().IdentityPubHex()
			}
			rt.closeFn()
		}
	}
	if pubHex == "" {
		if ephemeral, eErr := nwallet.NewEphemeralMarketSigner(); eErr == nil {
			pubHex = ephemeral.PubHex()
		}
	}
	docs, err := discovery.GenerateDomainDocs(*domain, *handle, pubHex, *hubURL, *bboxOffice)
	if err != nil {
		return err
	}
	if *asJSON {
		return printJSON(docs)
	}
	fmt.Printf("=== LightBSV Sovereign Domain Setup (%s) ===\n\n", docs.Acct)
	for _, step := range docs.SetupInstructions {
		fmt.Println("  " + step)
	}
	fmt.Printf("\n--- %s ---\n%s\n", docs.ManifestPath, string(docs.ManifestJSON))
	fmt.Printf("\n--- %s ---\n%s\n", docs.WellKnownPath, string(docs.WellKnownJSON))
	return nil
}
