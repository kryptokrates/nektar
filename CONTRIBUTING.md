# contributing to nektar

bugs, market-flow ideas, overlay-object-plane work, docs — all welcome.

## ground rules

- `pnpm -r build && pnpm -r test` green before pushing; `pnpm check:deps` + `pnpm check:cruise` + `pnpm check:knip` clean
- every artifact is a signed, verifiable object — never add a claim that can't be checked
- honest limits stay honest: proofs establish recorded/unaltered/derivable — never true
- vendored third-party code lives in `src/vendor/` with license + pin noted

## market contributions that matter

- `tm_presence`/`ls_presence` artifacts (new kinds, new indexes)
- escrow/sale ops and the ZKCP rail
- object-plane transport (BRC-148/149 adapters)
- smoke coverage on live networks (regtest/ttn)
