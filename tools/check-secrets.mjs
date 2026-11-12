// pnpm check:secrets — scan the working tree for committed secrets.
// Prefers a `gitleaks` binary on PATH; falls back to the pinned docker image
// (same image the CI hygiene job uses). Scans `dir` (working tree) — CI
// additionally runs `gitleaks git` over history.
import { spawnSync } from 'node:child_process'

const IMAGE = 'zricethezav/gitleaks@sha256:c00b6bd0aeb3071cbcb79009cb16a60dd9e0a7c60e2be9ab65d25e6bc8abbb7f'

const onPath = spawnSync('gitleaks', ['--version'], { stdio: 'pipe' }).status === 0
const [cmd, args] = onPath
  ? ['gitleaks', ['dir']]
  : ['docker', ['run', '--rm', '-v', `${process.cwd()}:/repo`, '-w', '/repo', IMAGE, 'dir']]

if (!onPath) console.log('gitleaks not on PATH — via docker (CI uses the same image)')
const r = spawnSync(cmd, args, { stdio: 'inherit' })
process.exit(r.status ?? 1)
