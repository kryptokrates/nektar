// proofstore.ts — the public proof store extracted from NektarMemory: proofs
// committed inside receipts are public artifacts (a ZK proof reveals
// nothing). Local content-addressed files under <dataDir>/proofs/<sha256>
// plus the bstore object plane (blob key IS chirp/<commit>, so a verifier
// resolves from any gateway copy and self-verifies on the digest).
import { join } from 'node:path'
import { readFileSync } from 'node:fs'
import { toHex, sha256, writeFileAtomic, nektarEnv } from '@nektar/core'
import { storeChirp, type S3ObjectStore } from '@nektar/store'

export interface ProofStoreDeps {
  dataDir: () => string
  objectStore: () => S3ObjectStore | undefined
  persistState: () => void
  metricsInc: (name: string) => void
}

export class ProofStore {
  /** commit → {endpoint,bucket} locator (the object key is derivable:
   *  `chirp/<commit>`). Persisted via snapshot/restore into state.json. */
  readonly stores = new Map<string, { endpoint: string; bucket: string }>()

  constructor(private deps: ProofStoreDeps) {}

  private dir(): string {
    return join(this.deps.dataDir(), 'proofs')
  }

  /** store a proof artifact; returns its sha256 commitment hex */
  put(bytes: Uint8Array): string {
    const commit = toHex(sha256(bytes))
    // atomic: a torn proof file would read as a FAILED verification — the
    // watcher would challenge an honest claim and put its bond at risk
    writeFileAtomic(join(this.dir(), commit), Buffer.from(bytes))
    // stock object plane: the same bytes land content-addressed in bstore —
    // for single-chunk content the blob key IS chirp/<commit>, so a verifier
    // resolves the proof from any gateway copy and self-verifies on the
    // digest; /v1/proof stays the compatibility fallback.
    const st = this.deps.objectStore()
    if (st !== undefined) {
      // record the locator synchronously — the blob key IS chirp/<commit>,
      // so any holder of {endpoint,bucket}+digest fetches and self-verifies
      this.stores.set(commit, { endpoint: st.endpoint, bucket: st.bucket })
      void storeChirp(st, bytes)
        .then(() => this.deps.persistState())
        .catch(() => this.deps.metricsInc('nektar_proof_store_errors_total'))
    }
    return commit
  }

  /** fetch a stored proof by commitment; integrity-checked — a corrupted or
   *  foreign file can never masquerade as the committed artifact. */
  get(commitHex: string): Uint8Array | undefined {
    try {
      const b = readFileSync(join(this.dir(), commitHex))
      const u = new Uint8Array(b)
      return toHex(sha256(u)) === commitHex ? u : undefined
    } catch {
      return undefined
    }
  }

  /** the bstore locator a proof was PUT under (endpoint+bucket — the object
   *  key is derivable: `chirp/<commit>`); receipts carry it in proofStore so
   *  a remote verifier needs no origin call. */
  ref(commitHex: string): { endpoint: string; bucket: string } | undefined {
    return this.stores.get(commitHex)
  }

  /** proof bytes by commitment — local dir first, then the store locator a
   *  receipt carried (`chirp/<commit>` unsigned, then signed when this
   *  instance has a store configured). Digest-checked: wrong bytes never
   *  verify as the committed proof. */
  async fetch(commitHex: string, store?: { endpoint?: string; bucket?: string }): Promise<Uint8Array | undefined> {
    const local = this.get(commitHex)
    if (local !== undefined) return local
    const key = `chirp/${commitHex}`
    const own = this.deps.objectStore()
    const endpoint = store?.endpoint ?? this.stores.get(commitHex)?.endpoint ?? own?.endpoint
    const bucket = store?.bucket ?? this.stores.get(commitHex)?.bucket ?? own?.bucket
    if (endpoint !== undefined && bucket !== undefined) {
      try {
        const { getObjectUnsigned } = await import('@nektar/store')
        const bytes = await getObjectUnsigned(endpoint, bucket, key)
        return toHex(sha256(bytes)) === commitHex ? bytes : undefined
      } catch {
        /* fall through to the signed path */
      }
    }
    if (own !== undefined) {
      try {
        const bytes = await own.get(key, bucket)
        return toHex(sha256(bytes)) === commitHex ? bytes : undefined
      } catch {
        return undefined
      }
    }
    return undefined
  }
}
