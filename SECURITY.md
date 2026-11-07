# security

**Report privately** — open a GitHub Security Advisory on this repo
(Security → Advisories → Report a vulnerability). Do not file public issues
for exploitable findings.

## scope

- the signing/verification model for artifacts (ads, catalogs, wants, offers,
  settlements, reputation cards)
- the escrow/covenant rail (BIP-199 disclosure sale, ZKCP)
- key handling (`keys.json`/`fee.key.json` encrypted at rest,
  `NEKTAR_KEY_PASSPHRASE`)
- payment tooling (quotes, `peerCall`, spend caps)

## honest model

artifacts verify signatures + provenance — they establish
recorded/unaltered/derivable, not truth. Domain tags are claims; receipts are
the anchor. The rendezvous is a thin signed index — a hostile index can
withhold, never forge.

Supported: latest release tag (`v0.3.x` line).
