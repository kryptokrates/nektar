import { StatefulSmartContract, assert, hash160, checkSig, cat, toByteString, type Sig, type PubKey, type ByteString, extractLocktime, extractSequence, extractAmount } from 'runar-lang'

// NektarCheckpointBond — hardened SLA checkpoint bond covenant.
// Enforces extractSequence < 0xffffffff and block-height locktime domain (< 500_000_000)
// on commit, seize, and close so consensus nLockTime cannot be bypassed via SEQUENCE_FINAL
// or 1985 timestamp confusion.
export class NektarCheckpointBond extends StatefulSmartContract {
  operatorPkh: ByteString
  bondReleaseHeight: bigint
  maxFee: bigint
  epochLen: bigint
  n: bigint
  lastRoot: ByteString
  deadline: bigint
  seized: bigint

  constructor (operatorPkh: ByteString, bondReleaseHeight: bigint, maxFee: bigint, epochLen: bigint) {
    super(operatorPkh, bondReleaseHeight, maxFee, epochLen)
    this.operatorPkh = operatorPkh
    this.bondReleaseHeight = bondReleaseHeight
    this.maxFee = maxFee
    this.epochLen = epochLen
    this.n = 0n
    this.lastRoot = toByteString('')
    this.deadline = bondReleaseHeight
    this.seized = 0n
  }

  private p2pkh (pkh: ByteString): ByteString {
    return cat(toByteString('76a914'), cat(pkh, toByteString('88ac')))
  }

  public commit (sig: Sig, pubKey: PubKey, newRoot: ByteString, bondSats: bigint, newDeadline: bigint) {
    assert(hash160(pubKey) === this.operatorPkh)
    assert(checkSig(sig, pubKey))
    assert(this.seized == 0n)
    const lt = extractLocktime(this.txPreimage)
    assert(extractSequence(this.txPreimage) < 4294967295n)
    assert(lt < 500000000n)
    assert(lt < this.deadline)
    assert(newDeadline - lt == this.epochLen)
    assert(newDeadline < 500000000n)
    assert(bondSats >= 1n)
    this.n += 1n
    this.lastRoot = newRoot
    this.deadline = newDeadline
    assert(extractAmount(this.txPreimage) - bondSats <= this.maxFee + 1n)
    this.addOutput(bondSats, this.operatorPkh, this.bondReleaseHeight, this.maxFee, this.epochLen, this.n, this.lastRoot, this.deadline, this.seized)
  }

  public seize (payTo: ByteString, payoutSats: bigint) {
    assert(this.seized == 0n)
    const lt = extractLocktime(this.txPreimage)
    assert(extractSequence(this.txPreimage) < 4294967295n)
    assert(lt < 500000000n)
    assert(lt >= this.deadline)
    assert(payoutSats >= 1n)
    assert(extractAmount(this.txPreimage) - payoutSats <= this.maxFee + 1n)
    this.seized = 1n
    this.addRawOutput(payoutSats, payTo)
    this.addOutput(1n, this.operatorPkh, this.bondReleaseHeight, this.maxFee, this.epochLen, this.n, this.lastRoot, this.deadline, this.seized)
  }

  public close (sig: Sig, pubKey: PubKey, payoutSats: bigint) {
    assert(hash160(pubKey) === this.operatorPkh)
    assert(checkSig(sig, pubKey))
    const lt = extractLocktime(this.txPreimage)
    assert(extractSequence(this.txPreimage) < 4294967295n)
    assert(lt < 500000000n)
    assert(lt >= this.bondReleaseHeight)
    assert(payoutSats >= 1n)
    assert(extractAmount(this.txPreimage) - payoutSats <= this.maxFee + 1n)
    this.addRawOutput(payoutSats, this.p2pkh(this.operatorPkh))
  }
}
