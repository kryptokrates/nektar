// ClaimVerdict — GOAL-v9 §4.5 hardened consumer hook.
// F-21: authorize() requires payeePkh signature so third parties cannot destroy a
// pinned ClaimVerdict UTXO standalone for 1 satoshi and DoS DisclosureSale.buyGated.
import {
  StatefulSmartContract,
  assert, checkSig, hash160,
  cat, toByteString,
  type Sig, type PubKey, type ByteString
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

  public authorize (sig: Sig, pubKey: PubKey, paySats: bigint) {
    assert(hash160(pubKey) === this.payeePkh)
    assert(checkSig(sig, pubKey))
    assert(paySats >= 1n)
    this.addRawOutput(paySats, cat(toByteString('76a914'), cat(this.payeePkh, toByteString('88ac'))))
  }
}
