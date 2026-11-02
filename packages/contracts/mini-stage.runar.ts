import { StatefulSmartContract, assert } from 'runar-lang'
export class MiniStage extends StatefulSmartContract {
  a: bigint
  b: bigint
  round: bigint
  constructor (a: bigint, b: bigint, round: bigint) {
    super(a, b, round)
    this.a = a; this.b = b; this.round = round
  }
  public step () {
    let la = this.a
    let lb = this.b
    la = (la * lb) % 2013265921n
    lb = (lb + la) % 2013265921n
    this.a = la
    this.b = lb
    this.addOutput(1n, la, lb, this.round + 1n)
  }
}
