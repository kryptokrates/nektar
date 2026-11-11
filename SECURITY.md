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
- payment tooling (quotes, spend caps)

## honest model

artifacts verify signatures + provenance — they establish
recorded/unaltered/derivable, not truth. Domain tags are claims; receipts are
the anchor. Overlay hosts are replaceable carriers of signed artifacts —
a hostile host can withhold but never forge; readers verify every answer
locally.

Supported: latest release tag (`v0.3.x` line).
