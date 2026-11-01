// V0 spike probe 2 (v2): DisclosureSale must be StatefulSmartContract —
// addRawOutput is only available there. buy() is a terminal spend: the
// outputs are [P2PKH payment to seller, compiler change output to buyer].
import {
  StatefulSmartContract,
  assert, checkSig, extractLocktime, extractAmount,
  sha256, hash160, cat, toByteString,
  type PubKey, type Sig, type Ripemd160, type Sha256, type ByteString
} from 'runar-lang'

export class ProbeSale extends StatefulSmartContract {
  readonly sellerPkh: ByteString
  readonly price: bigint
  readonly hWrapped: Sha256
  readonly refundAfter: bigint
  // one mutable field so the contract is stateful (dummy — terminal methods
  // never continue it)
  alive: bigint

  constructor (sellerPkh: ByteString, price: bigint, hWrapped: Sha256, refundAfter: bigint) {
    super(1n)
    this.sellerPkh = sellerPkh
    this.price = price
    this.hWrapped = hWrapped
    this.refundAfter = refundAfter
    this.alive = 1n
  }

  private payScript (): ByteString {
    // OP_DUP OP_HASH160 <20B> OP_EQUALVERIFY OP_CHECKSIG
    return cat(toByteString('76a914'), cat(this.sellerPkh, toByteString('88ac')))
  }

  public buy (wrapped: ByteString, _changePKH: ByteString, _changeAmount: bigint) {
    // the spend itself reveals the payload — bound to the committed hash
    assert(sha256(wrapped) === this.hWrapped)
    // payment output ≥ price to the seller + buyer change (unconstrained —
    // the buyer builds and funds the tx)
    this.addRawOutput(this.price, this.payScript())
  }

  public refund (sig: Sig, pubKey: PubKey) {
    assert(hash160(pubKey) === this.sellerPkh)
    assert(checkSig(sig, pubKey))
    assert(extractLocktime(this.txPreimage) >= this.refundAfter)
  }
}
