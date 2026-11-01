// V0 spike probe 1: can a stateful method body reference the implicit
// _changePKH / _changeAmount / _newAmount params and txPreimage?
import {
  StatefulSmartContract,
  assert, checkSig, hash160, num2bin, extractLocktime, extractAmount,
  type PubKey, type Sig, type type ByteString
} from 'runar-lang'

export class ProbeVault extends StatefulSmartContract {
  spentInWindow: bigint
  windowStart: bigint
  destPkh: ByteString
  ownerPkh: ByteString
  perTxCap: bigint
  windowCap: bigint
  windowBlocks: bigint
  maxFee: bigint

  constructor (destPkh: ByteString, ownerPkh: ByteString, perTxCap: bigint, windowCap: bigint, windowBlocks: bigint, maxFee: bigint) {
    super(0n, 0n)
    this.spentInWindow = 0n
    this.windowStart = 0n
    this.destPkh = destPkh
    this.ownerPkh = ownerPkh
    this.perTxCap = perTxCap
    this.windowCap = windowCap
    this.windowBlocks = windowBlocks
    this.maxFee = maxFee
  }

  public spend (sig: Sig, pubKey: PubKey, payAmount: bigint) {
    assert(hash160(pubKey) === this.ownerPkh)
    assert(checkSig(sig, pubKey))
    const lt = extractLocktime(this.txPreimage)
    const rolled = lt >= this.windowStart + this.windowBlocks
    const newSpent = rolled ? payAmount : this.spentInWindow + payAmount
    assert(newSpent <= this.windowCap)
    assert(payAmount <= this.perTxCap)
    // the change output IS the payment: it must go to destPkh at payAmount
    assert(_changePKH === this.destPkh)
    assert(_changeAmount === payAmount)
    assert(extractAmount(this.txPreimage) - _newAmount - _changeAmount <= this.maxFee)
    this.spentInWindow = newSpent
    this.windowStart = rolled ? lt : this.windowStart
  }
}
