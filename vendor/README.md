Vendored upstream tarballs (file: deps, do not edit).
lightwebinc-bcommon-0.34.0.tgz — bcommon ts @ v0.34.0 (tag, commit 38edefc), Apache-2.0.
lightwebinc-bbox-host-0.6.8.tgz — bbox host codec @ v0.6.8 (tag, commit 1733266), Apache-2.0.
lightwebinc-blogs-host-0.6.4.tgz — blogs host codec @ v0.6.4 (logrec/merkle/admit/anchor + tm_log/ls_log/module/retention; Apache-2.0).
lightwebinc-bfinger-host-0.8.5.tgz — bfinger host codec @ v0.8.5 (record/token/carrier/funding/keys, tm_finger, ls_finger; Apache-2.0).

Repack notes (0.34.0 refresh, 2026-11): all four tarballs were rebuilt from
their upstream tags (`npm run build && npm pack` in ts/ for bcommon, host/
for the rest). Upstream's published codec files are byte-identical to the
previous vendored versions; the bumps add only blogs host-module files and
the Go-side `keyed` (BRC-369) package, which has no TS twin yet. Two
packaging normalizations were applied at repack time so the tarballs
install cleanly under pnpm:

- blogs/bfinger upstream package.json carries bcommon in `dependencies` as
  `file:vendor/...` (unresolvable inside a tarball) — moved to
  `peerDependencies`.
- All three host packages' bcommon peer range widened to
  `^0.33.13 || ^0.34.0` so they accept bcommon 0.34.0.

Refresh: repack upstream at the new tag, replace the tgz, update SHA256SUMS.
