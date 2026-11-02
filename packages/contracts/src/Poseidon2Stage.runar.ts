// Poseidon2Stage — GOAL-v9 Stage 0d: ONE round of poseidon2_mix as a
// stateful covenant step. 24 cells + round live in the UTXO (never the
// stack). Round constants are constructor-baked per stage instance.
import { StatefulSmartContract, assert } from 'runar-lang'
export class Poseidon2Stage extends StatefulSmartContract {
  s0: bigint
  s1: bigint
  s2: bigint
  s3: bigint
  s4: bigint
  s5: bigint
  s6: bigint
  s7: bigint
  s8: bigint
  s9: bigint
  s10: bigint
  s11: bigint
  s12: bigint
  s13: bigint
  s14: bigint
  s15: bigint
  s16: bigint
  s17: bigint
  s18: bigint
  s19: bigint
  s20: bigint
  s21: bigint
  s22: bigint
  s23: bigint
  round: bigint
  constructor (s0: bigint, s1: bigint, s2: bigint, s3: bigint, s4: bigint, s5: bigint, s6: bigint, s7: bigint, s8: bigint, s9: bigint, s10: bigint, s11: bigint, s12: bigint, s13: bigint, s14: bigint, s15: bigint, s16: bigint, s17: bigint, s18: bigint, s19: bigint, s20: bigint, s21: bigint, s22: bigint, s23: bigint, round: bigint) {
    super(s0, s1, s2, s3, s4, s5, s6, s7, s8, s9, s10, s11, s12, s13, s14, s15, s16, s17, s18, s19, s20, s21, s22, s23, round)
    this.s0 = s0
    this.s1 = s1
    this.s2 = s2
    this.s3 = s3
    this.s4 = s4
    this.s5 = s5
    this.s6 = s6
    this.s7 = s7
    this.s8 = s8
    this.s9 = s9
    this.s10 = s10
    this.s11 = s11
    this.s12 = s12
    this.s13 = s13
    this.s14 = s14
    this.s15 = s15
    this.s16 = s16
    this.s17 = s17
    this.s18 = s18
    this.s19 = s19
    this.s20 = s20
    this.s21 = s21
    this.s22 = s22
    this.s23 = s23
    this.round = round
  }
  public fullStep () {
    let qt = 0n; let wt = 0n; let zt = 0n
    let ls0 = this.s0
    let ls1 = this.s1
    let ls2 = this.s2
    let ls3 = this.s3
    let ls4 = this.s4
    let ls5 = this.s5
    let ls6 = this.s6
    let ls7 = this.s7
    let ls8 = this.s8
    let ls9 = this.s9
    let ls10 = this.s10
    let ls11 = this.s11
    let ls12 = this.s12
    let ls13 = this.s13
    let ls14 = this.s14
    let ls15 = this.s15
    let ls16 = this.s16
    let ls17 = this.s17
    let ls18 = this.s18
    let ls19 = this.s19
    let ls20 = this.s20
    let ls21 = this.s21
    let ls22 = this.s22
    let ls23 = this.s23
    ls0 = (ls0 + 0xfa20c37n) % 2013265921n
    ls1 = (ls1 + 0x795bb97n) % 2013265921n
    ls2 = (ls2 + 0x12c60b9cn) % 2013265921n
    ls3 = (ls3 + 0xeabd88en) % 2013265921n
    ls4 = (ls4 + 0x96485can) % 2013265921n
    ls5 = (ls5 + 0x7093527n) % 2013265921n
    ls6 = (ls6 + 0x1b1d4e50n) % 2013265921n
    ls7 = (ls7 + 0x30a01acen) % 2013265921n
    ls8 = (ls8 + 0x3bd86f5an) % 2013265921n
    ls9 = (ls9 + 0x69af7c28n) % 2013265921n
    ls10 = (ls10 + 0x3f94775fn) % 2013265921n
    ls11 = (ls11 + 0x731560e8n) % 2013265921n
    ls12 = (ls12 + 0x465a0ecdn) % 2013265921n
    ls13 = (ls13 + 0x574ef807n) % 2013265921n
    ls14 = (ls14 + 0x62fd4870n) % 2013265921n
    ls15 = (ls15 + 0x52ccfe44n) % 2013265921n
    ls16 = (ls16 + 0x14772b14n) % 2013265921n
    ls17 = (ls17 + 0x4dedf371n) % 2013265921n
    ls18 = (ls18 + 0x260acd7cn) % 2013265921n
    ls19 = (ls19 + 0x1f51dc58n) % 2013265921n
    ls20 = (ls20 + 0x75125532n) % 2013265921n
    ls21 = (ls21 + 0x686a4d7bn) % 2013265921n
    ls22 = (ls22 + 0x54bac179n) % 2013265921n
    ls23 = (ls23 + 0x31947706n) % 2013265921n
    qt = (ls0 * ls0) % 2013265921n
    wt = (qt * qt) % 2013265921n
    zt = (wt * qt) % 2013265921n
    ls0 = (zt * ls0) % 2013265921n
    qt = (ls1 * ls1) % 2013265921n
    wt = (qt * qt) % 2013265921n
    zt = (wt * qt) % 2013265921n
    ls1 = (zt * ls1) % 2013265921n
    qt = (ls2 * ls2) % 2013265921n
    wt = (qt * qt) % 2013265921n
    zt = (wt * qt) % 2013265921n
    ls2 = (zt * ls2) % 2013265921n
    qt = (ls3 * ls3) % 2013265921n
    wt = (qt * qt) % 2013265921n
    zt = (wt * qt) % 2013265921n
    ls3 = (zt * ls3) % 2013265921n
    qt = (ls4 * ls4) % 2013265921n
    wt = (qt * qt) % 2013265921n
    zt = (wt * qt) % 2013265921n
    ls4 = (zt * ls4) % 2013265921n
    qt = (ls5 * ls5) % 2013265921n
    wt = (qt * qt) % 2013265921n
    zt = (wt * qt) % 2013265921n
    ls5 = (zt * ls5) % 2013265921n
    qt = (ls6 * ls6) % 2013265921n
    wt = (qt * qt) % 2013265921n
    zt = (wt * qt) % 2013265921n
    ls6 = (zt * ls6) % 2013265921n
    qt = (ls7 * ls7) % 2013265921n
    wt = (qt * qt) % 2013265921n
    zt = (wt * qt) % 2013265921n
    ls7 = (zt * ls7) % 2013265921n
    qt = (ls8 * ls8) % 2013265921n
    wt = (qt * qt) % 2013265921n
    zt = (wt * qt) % 2013265921n
    ls8 = (zt * ls8) % 2013265921n
    qt = (ls9 * ls9) % 2013265921n
    wt = (qt * qt) % 2013265921n
    zt = (wt * qt) % 2013265921n
    ls9 = (zt * ls9) % 2013265921n
    qt = (ls10 * ls10) % 2013265921n
    wt = (qt * qt) % 2013265921n
    zt = (wt * qt) % 2013265921n
    ls10 = (zt * ls10) % 2013265921n
    qt = (ls11 * ls11) % 2013265921n
    wt = (qt * qt) % 2013265921n
    zt = (wt * qt) % 2013265921n
    ls11 = (zt * ls11) % 2013265921n
    qt = (ls12 * ls12) % 2013265921n
    wt = (qt * qt) % 2013265921n
    zt = (wt * qt) % 2013265921n
    ls12 = (zt * ls12) % 2013265921n
    qt = (ls13 * ls13) % 2013265921n
    wt = (qt * qt) % 2013265921n
    zt = (wt * qt) % 2013265921n
    ls13 = (zt * ls13) % 2013265921n
    qt = (ls14 * ls14) % 2013265921n
    wt = (qt * qt) % 2013265921n
    zt = (wt * qt) % 2013265921n
    ls14 = (zt * ls14) % 2013265921n
    qt = (ls15 * ls15) % 2013265921n
    wt = (qt * qt) % 2013265921n
    zt = (wt * qt) % 2013265921n
    ls15 = (zt * ls15) % 2013265921n
    qt = (ls16 * ls16) % 2013265921n
    wt = (qt * qt) % 2013265921n
    zt = (wt * qt) % 2013265921n
    ls16 = (zt * ls16) % 2013265921n
    qt = (ls17 * ls17) % 2013265921n
    wt = (qt * qt) % 2013265921n
    zt = (wt * qt) % 2013265921n
    ls17 = (zt * ls17) % 2013265921n
    qt = (ls18 * ls18) % 2013265921n
    wt = (qt * qt) % 2013265921n
    zt = (wt * qt) % 2013265921n
    ls18 = (zt * ls18) % 2013265921n
    qt = (ls19 * ls19) % 2013265921n
    wt = (qt * qt) % 2013265921n
    zt = (wt * qt) % 2013265921n
    ls19 = (zt * ls19) % 2013265921n
    qt = (ls20 * ls20) % 2013265921n
    wt = (qt * qt) % 2013265921n
    zt = (wt * qt) % 2013265921n
    ls20 = (zt * ls20) % 2013265921n
    qt = (ls21 * ls21) % 2013265921n
    wt = (qt * qt) % 2013265921n
    zt = (wt * qt) % 2013265921n
    ls21 = (zt * ls21) % 2013265921n
    qt = (ls22 * ls22) % 2013265921n
    wt = (qt * qt) % 2013265921n
    zt = (wt * qt) % 2013265921n
    ls22 = (zt * ls22) % 2013265921n
    qt = (ls23 * ls23) % 2013265921n
    wt = (qt * qt) % 2013265921n
    zt = (wt * qt) % 2013265921n
    ls23 = (zt * ls23) % 2013265921n
    const b0t0 = (ls0 + ls1) % 2013265921n
    const b0t1 = (ls2 + ls3) % 2013265921n
    const b0t2 = (2n * ls1 + b0t1) % 2013265921n
    const b0t3 = (2n * ls3 + b0t0) % 2013265921n
    const b0t4 = (4n * b0t1 + b0t3) % 2013265921n
    const b0t5 = (4n * b0t0 + b0t2) % 2013265921n
    const b0o0 = (b0t3 + b0t5) % 2013265921n
    const b0o1 = b0t5
    const b0o2 = (b0t2 + b0t4) % 2013265921n
    const b0o3 = b0t4
    const b1t0 = (ls4 + ls5) % 2013265921n
    const b1t1 = (ls6 + ls7) % 2013265921n
    const b1t2 = (2n * ls5 + b1t1) % 2013265921n
    const b1t3 = (2n * ls7 + b1t0) % 2013265921n
    const b1t4 = (4n * b1t1 + b1t3) % 2013265921n
    const b1t5 = (4n * b1t0 + b1t2) % 2013265921n
    const b1o0 = (b1t3 + b1t5) % 2013265921n
    const b1o1 = b1t5
    const b1o2 = (b1t2 + b1t4) % 2013265921n
    const b1o3 = b1t4
    const b2t0 = (ls8 + ls9) % 2013265921n
    const b2t1 = (ls10 + ls11) % 2013265921n
    const b2t2 = (2n * ls9 + b2t1) % 2013265921n
    const b2t3 = (2n * ls11 + b2t0) % 2013265921n
    const b2t4 = (4n * b2t1 + b2t3) % 2013265921n
    const b2t5 = (4n * b2t0 + b2t2) % 2013265921n
    const b2o0 = (b2t3 + b2t5) % 2013265921n
    const b2o1 = b2t5
    const b2o2 = (b2t2 + b2t4) % 2013265921n
    const b2o3 = b2t4
    const b3t0 = (ls12 + ls13) % 2013265921n
    const b3t1 = (ls14 + ls15) % 2013265921n
    const b3t2 = (2n * ls13 + b3t1) % 2013265921n
    const b3t3 = (2n * ls15 + b3t0) % 2013265921n
    const b3t4 = (4n * b3t1 + b3t3) % 2013265921n
    const b3t5 = (4n * b3t0 + b3t2) % 2013265921n
    const b3o0 = (b3t3 + b3t5) % 2013265921n
    const b3o1 = b3t5
    const b3o2 = (b3t2 + b3t4) % 2013265921n
    const b3o3 = b3t4
    const b4t0 = (ls16 + ls17) % 2013265921n
    const b4t1 = (ls18 + ls19) % 2013265921n
    const b4t2 = (2n * ls17 + b4t1) % 2013265921n
    const b4t3 = (2n * ls19 + b4t0) % 2013265921n
    const b4t4 = (4n * b4t1 + b4t3) % 2013265921n
    const b4t5 = (4n * b4t0 + b4t2) % 2013265921n
    const b4o0 = (b4t3 + b4t5) % 2013265921n
    const b4o1 = b4t5
    const b4o2 = (b4t2 + b4t4) % 2013265921n
    const b4o3 = b4t4
    const b5t0 = (ls20 + ls21) % 2013265921n
    const b5t1 = (ls22 + ls23) % 2013265921n
    const b5t2 = (2n * ls21 + b5t1) % 2013265921n
    const b5t3 = (2n * ls23 + b5t0) % 2013265921n
    const b5t4 = (4n * b5t1 + b5t3) % 2013265921n
    const b5t5 = (4n * b5t0 + b5t2) % 2013265921n
    const b5o0 = (b5t3 + b5t5) % 2013265921n
    const b5o1 = b5t5
    const b5o2 = (b5t2 + b5t4) % 2013265921n
    const b5o3 = b5t4
    const mtmp0 = (b0o0 + b1o0 + b2o0 + b3o0 + b4o0 + b5o0) % 2013265921n
    const mtmp1 = (b0o1 + b1o1 + b2o1 + b3o1 + b4o1 + b5o1) % 2013265921n
    const mtmp2 = (b0o2 + b1o2 + b2o2 + b3o2 + b4o2 + b5o2) % 2013265921n
    const mtmp3 = (b0o3 + b1o3 + b2o3 + b3o3 + b4o3 + b5o3) % 2013265921n
    ls0 = (b0o0 + mtmp0) % 2013265921n
    ls1 = (b0o1 + mtmp1) % 2013265921n
    ls2 = (b0o2 + mtmp2) % 2013265921n
    ls3 = (b0o3 + mtmp3) % 2013265921n
    ls4 = (b1o0 + mtmp0) % 2013265921n
    ls5 = (b1o1 + mtmp1) % 2013265921n
    ls6 = (b1o2 + mtmp2) % 2013265921n
    ls7 = (b1o3 + mtmp3) % 2013265921n
    ls8 = (b2o0 + mtmp0) % 2013265921n
    ls9 = (b2o1 + mtmp1) % 2013265921n
    ls10 = (b2o2 + mtmp2) % 2013265921n
    ls11 = (b2o3 + mtmp3) % 2013265921n
    ls12 = (b3o0 + mtmp0) % 2013265921n
    ls13 = (b3o1 + mtmp1) % 2013265921n
    ls14 = (b3o2 + mtmp2) % 2013265921n
    ls15 = (b3o3 + mtmp3) % 2013265921n
    ls16 = (b4o0 + mtmp0) % 2013265921n
    ls17 = (b4o1 + mtmp1) % 2013265921n
    ls18 = (b4o2 + mtmp2) % 2013265921n
    ls19 = (b4o3 + mtmp3) % 2013265921n
    ls20 = (b5o0 + mtmp0) % 2013265921n
    ls21 = (b5o1 + mtmp1) % 2013265921n
    ls22 = (b5o2 + mtmp2) % 2013265921n
    ls23 = (b5o3 + mtmp3) % 2013265921n
    this.s0 = ls0
    this.s1 = ls1
    this.s2 = ls2
    this.s3 = ls3
    this.s4 = ls4
    this.s5 = ls5
    this.s6 = ls6
    this.s7 = ls7
    this.s8 = ls8
    this.s9 = ls9
    this.s10 = ls10
    this.s11 = ls11
    this.s12 = ls12
    this.s13 = ls13
    this.s14 = ls14
    this.s15 = ls15
    this.s16 = ls16
    this.s17 = ls17
    this.s18 = ls18
    this.s19 = ls19
    this.s20 = ls20
    this.s21 = ls21
    this.s22 = ls22
    this.s23 = ls23
    this.addOutput(1n, ls0, ls1, ls2, ls3, ls4, ls5, ls6, ls7, ls8, ls9, ls10, ls11, ls12, ls13, ls14, ls15, ls16, ls17, ls18, ls19, ls20, ls21, ls22, ls23, this.round + 1n)
  }
  public partialStep () {
    let qt = 0n; let wt = 0n; let zt = 0n
    let ls0 = this.s0
    let ls1 = this.s1
    let ls2 = this.s2
    let ls3 = this.s3
    let ls4 = this.s4
    let ls5 = this.s5
    let ls6 = this.s6
    let ls7 = this.s7
    let ls8 = this.s8
    let ls9 = this.s9
    let ls10 = this.s10
    let ls11 = this.s11
    let ls12 = this.s12
    let ls13 = this.s13
    let ls14 = this.s14
    let ls15 = this.s15
    let ls16 = this.s16
    let ls17 = this.s17
    let ls18 = this.s18
    let ls19 = this.s19
    let ls20 = this.s20
    let ls21 = this.s21
    let ls22 = this.s22
    let ls23 = this.s23
    ls0 = (ls0 + 0x1da78ec2n) % 2013265921n
    qt = (ls0 * ls0) % 2013265921n
    wt = (qt * qt) % 2013265921n
    zt = (wt * qt) % 2013265921n
    ls0 = (zt * ls0) % 2013265921n
    const msum = (ls0 + ls1 + ls2 + ls3 + ls4 + ls5 + ls6 + ls7 + ls8 + ls9 + ls10 + ls11 + ls12 + ls13 + ls14 + ls15 + ls16 + ls17 + ls18 + ls19 + ls20 + ls21 + ls22 + ls23) % 2013265921n
    ls0 = (msum + 0x409133f0n * ls0) % 2013265921n
    ls1 = (msum + 0x1667a8a1n * ls1) % 2013265921n
    ls2 = (msum + 0x6a6c7b6n * ls2) % 2013265921n
    ls3 = (msum + 0x6f53160en * ls3) % 2013265921n
    ls4 = (msum + 0x273b11d1n * ls4) % 2013265921n
    ls5 = (msum + 0x3176c5dn * ls5) % 2013265921n
    ls6 = (msum + 0x72f9bbf9n * ls6) % 2013265921n
    ls7 = (msum + 0x73ceba91n * ls7) % 2013265921n
    ls8 = (msum + 0x5cdef81dn * ls8) % 2013265921n
    ls9 = (msum + 0x1393285n * ls9) % 2013265921n
    ls10 = (msum + 0x46daee06n * ls10) % 2013265921n
    ls11 = (msum + 0x65d7ba6n * ls11) % 2013265921n
    ls12 = (msum + 0x52d72d6fn * ls12) % 2013265921n
    ls13 = (msum + 0x5dd05e0n * ls13) % 2013265921n
    ls14 = (msum + 0x3bab4b63n * ls14) % 2013265921n
    ls15 = (msum + 0x6ada3842n * ls15) % 2013265921n
    ls16 = (msum + 0x2fc5fbecn * ls16) % 2013265921n
    ls17 = (msum + 0x770d61b0n * ls17) % 2013265921n
    ls18 = (msum + 0x5715aae9n * ls18) % 2013265921n
    ls19 = (msum + 0x3ef0e90n * ls19) % 2013265921n
    ls20 = (msum + 0x75b6c770n * ls20) % 2013265921n
    ls21 = (msum + 0x242adf5fn * ls21) % 2013265921n
    ls22 = (msum + 0xd0ca4cn * ls22) % 2013265921n
    ls23 = (msum + 0x36c0e388n * ls23) % 2013265921n
    this.s0 = ls0
    this.s1 = ls1
    this.s2 = ls2
    this.s3 = ls3
    this.s4 = ls4
    this.s5 = ls5
    this.s6 = ls6
    this.s7 = ls7
    this.s8 = ls8
    this.s9 = ls9
    this.s10 = ls10
    this.s11 = ls11
    this.s12 = ls12
    this.s13 = ls13
    this.s14 = ls14
    this.s15 = ls15
    this.s16 = ls16
    this.s17 = ls17
    this.s18 = ls18
    this.s19 = ls19
    this.s20 = ls20
    this.s21 = ls21
    this.s22 = ls22
    this.s23 = ls23
    this.addOutput(1n, ls0, ls1, ls2, ls3, ls4, ls5, ls6, ls7, ls8, ls9, ls10, ls11, ls12, ls13, ls14, ls15, ls16, ls17, ls18, ls19, ls20, ls21, ls22, ls23, this.round + 1n)
  }
}