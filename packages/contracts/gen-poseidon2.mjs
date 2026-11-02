// GOAL-v9 Stage 0c codegen: emit Poseidon2Probe.runar.ts implementing one
// complete risc0-zkp 3.0.5 poseidon2_mix (CELLS=24, 8 full + 21 partial
// rounds, x^7 sbox, M_ext/M_int) in canonical BabyBear arithmetic — rep-
// equivalent to risc0's Montgomery internals (field ops commute with reps).
import { readFileSync, writeFileSync } from 'node:fs'
const { rc, di } = JSON.parse(readFileSync('poseidon2-consts.json', 'utf8'))
const P = 2013265921n
const M = ' % 2013265921n'
const C = 24, HF = 4, HP = 21

// ---------- JS reference ----------
const sbox = x => { const x2 = x*x%P, x4 = x2*x2%P, x6 = x4*x2%P; return x6*x%P }
function mExt (s) {
  const t = [0n,0n,0n,0n], out = []
  for (let b = 0; b < 6; b++) {
    const x = s.slice(b*4, b*4+4)
    const t0=(x[0]+x[1])%P, t1=(x[2]+x[3])%P, t2=(2n*x[1]+t1)%P, t3=(2n*x[3]+t0)%P
    const t4=(4n*t1+t3)%P, t5=(4n*t0+t2)%P, t6=(t3+t5)%P, t7=(t2+t4)%P
    const o=[t6,t5,t7,t4]
    for (let j=0;j<4;j++){ t[j]=(t[j]+o[j])%P; out[b*4+j]=o[j] }
  }
  for (let i=0;i<C;i++) out[i]=(out[i]+t[i%4])%P
  return out
}
function mInt (s) {
  const sum = s.reduce((a,x)=>(a+x)%P,0n)
  return s.map((v,i)=>(sum+BigInt(di[i])*v)%P)
}
function mix (s0) {
  let s = mExt(s0)
  for (let r=0;r<HF;r++){ for(let i=0;i<C;i++)s[i]=(s[i]+BigInt(rc[r*C+i]))%P; s=s.map(sbox); s=mExt(s) }
  for (let r=HF;r<HF+HP;r++){ s[0]=(s[0]+BigInt(rc[r*C]))%P; s[0]=sbox(s[0]); s=mInt(s) }
  for (let r=HF+HP;r<2*HF+HP;r++){ for(let i=0;i<C;i++)s[i]=(s[i]+BigInt(rc[r*C+i]))%P; s=s.map(sbox); s=mExt(s) }
  return s
}
const input = Array.from({length:C},(_,i)=>BigInt(i))          // state [0..23]
const expected = mix(input)
console.log('reference mix([0..23]) cells 0..7 :', expected.slice(0,8).map(x=>'0x'+x.toString(16)).join(','))

// ---------- Rúnar emitter ----------
const L = []
const v = i => `s${i}`
const set = (i,e) => L.push(`    ${v(i)} = (${e})${M}`)
function emitMExt () {
  for (let b=0;b<6;b++){
    const i=b*4
    L.push(`    const b${b}t0 = (${v(i)} + ${v(i+1)})${M}`)
    L.push(`    const b${b}t1 = (${v(i+2)} + ${v(i+3)})${M}`)
    L.push(`    const b${b}t2 = (2n * ${v(i+1)} + b${b}t1)${M}`)
    L.push(`    const b${b}t3 = (2n * ${v(i+3)} + b${b}t0)${M}`)
    L.push(`    const b${b}t4 = (4n * b${b}t1 + b${b}t3)${M}`)
    L.push(`    const b${b}t5 = (4n * b${b}t0 + b${b}t2)${M}`)
    L.push(`    const b${b}o0 = (b${b}t3 + b${b}t5)${M}`)
    L.push(`    const b${b}o1 = b${b}t5`)
    L.push(`    const b${b}o2 = (b${b}t2 + b${b}t4)${M}`)
    L.push(`    const b${b}o3 = b${b}t4`)
  }
  // tmp sums + final accumulation — do explicitly per cell to dodge arrays:
  // recompute: tmp_j = sum over b of o_j ; s_i = s_i + tmp_{i%4}
  // emit tmp sums first then assign
  for(let j=0;j<4;j++){
    L.push(`    const mtmp${j} = (${[0,1,2,3,4,5].map(b=>`b${b}o${j}`).join(' + ')})${M}`)
  }
  for(let i=0;i<C;i++){
    const b=Math.floor(i/4), j=i%4
    set(i, `b${b}o${j} + mtmp${j}`)
  }
}
function emitMInt () {
  L.push(`    const msum = (${v(0)} + ${[...Array(23)].map((_,i)=>v(i+1)).join(' + ')})${M}`)
  for (let i=0;i<C;i++) set(i, `msum + 0x${di[i].toString(16)}n * ${v(i)}`)
}
function emitSbox (i) {
  L.push(`    qt = (${v(i)} * ${v(i)})${M}`)
  L.push(`    wt = (qt * qt)${M}`)
  L.push(`    zt = (wt * qt)${M}`)
  set(i, `zt * ` + v(i))
}
function emitSboxAll () { for(let i=0;i<C;i++) emitSbox(i) }
function emitSbox0 () { emitSbox(0) }
const args = [...Array(C)].map((_,i)=>`x${i}: bigint`).join(', ')
const expArgs = [...Array(8)].map((_,i)=>`e${i}: bigint`).join(', ')
L.push(`// Poseidon2Probe — GOAL-v9 Stage 0c: ONE complete risc0-zkp 3.0.5`)
L.push(`// poseidon2_mix permutation in Script. Generated — do not edit by hand.`)
L.push(`import { SmartContract, assert } from 'runar-lang'`)
L.push(``)
L.push(`export class Poseidon2Probe extends SmartContract {`)
L.push(`  constructor () { super() }`)
L.push(`  public mix (${args}, ${expArgs}) {`)
L.push(`    let qt = 0n; let wt = 0n; let zt = 0n`)
for (let i=0;i<C;i++) L.push(`    let ${v(i)} = x${i}`)
emitMExt()
for (let r=0;r<HF;r++){ for(let i=0;i<C;i++) set(i,`${v(i)} + 0x${rc[r*C+i].toString(16)}n`); emitSboxAll(); emitMExt() }
for (let r=HF;r<HF+HP;r++){ set(0,`${v(0)} + 0x${rc[r*C].toString(16)}n`); emitSbox0(); emitMInt() }
for (let r=HF+HP;r<2*HF+HP;r++){ for(let i=0;i<C;i++) set(i,`${v(i)} + 0x${rc[r*C+i].toString(16)}n`); emitSboxAll(); emitMExt() }
L.push(`    assert(` + [...Array(8)].map((_,i) => v(i) + ' === e' + i).join(' && ') + `)`)
L.push(`  }`)
L.push(`}`)
writeFileSync('src/Poseidon2Probe.runar.ts', L.join('\n'))
writeFileSync('poseidon2-vector.json', JSON.stringify({ input: input.map(String), expected: expected.map(String) }))
console.log('emitted src/Poseidon2Probe.runar.ts lines:', L.length)
