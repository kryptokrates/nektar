// ClaimVerdict — GOAL-v9 §4.5 consumer hook: the covenant UTXO that
// MamemZkClaim.settle emits (output[0]). Its STATE is the settled
// verdict {receiptHash, journalRoot, queryId, payeePkh} — readable
// on-chain by indexers and verifiable off-chain against the settle tx.
// Consumers gate on it by pinning its outpoint as input1 (prevouts
// check, same pin shape as proveStage); when it is spent, THIS script
// enforces that the tx pays `payeePkh` — the settled prover can't be
// tricked into burning their verdict for free, and a sale/consumer tx
// can't execute without consuming the real verdict.
//   authorize: outputs = [pay→payeePkh (paySats), change] (shared
//   change with the co-spending consumer, like the stage co-spend).
import {
  StatefulSmartContract,
  assert,
  cat, toByteString,
  type ByteString
} from 'runar-lang'

export class ClaimVerdict extends StatefulSmartContract {
  receiptHash: ByteString
  journalRoot: ByteString
  queryId: ByteString
  payeePkh: ByteString

  constructor (receiptHash: ByteString, journalRoot: ByteString, queryId: ByteString, payeePkh: ByteString) {
    super(receiptHash, journalRoot, queryId, payeePkh)
    this.receiptHash = receiptHash
    this.journalRoot = journalRoot
    this.queryId = queryId
    this.payeePkh = payeePkh
  }

  /** spend the verdict — requires output[0] to pay payeePkh ≥ 1 sat.
   *  The consumer contract enumerates the same output list, so both
   *  hashOutputs checks pass on the shared tx. `paySats` is caller-set;
   *  the consumer's own price check bounds it. */
  public authorize (paySats: bigint) {
    assert(paySats >= 1n)
    this.addRawOutput(paySats, cat(toByteString('76a914'), cat(this.payeePkh, toByteString('88ac'))))
  }
}
