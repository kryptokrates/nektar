// GOAL-v9 Stage 0d: emit Poseidon2Stage.runar.ts — the atomic verifier
// primitive: a stateful covenant carrying the 24-cell state in the UTXO,
// performing ONE round per spend, emitting continuation state.
import { readFileSync, writeFileSync } from 'node:fs'
const { rc, di } = JSON.parse(readFileSync('poseidon2-consts.json', 'utf8'))
const P='2013265921n', C=24
const v=i=>`s${i}`, M=' % 2013265921n'
const L=[]
const set=(i,e,pre='this.')=>L.push(`    ${pre}${v(i)} = (${e})${M}`)
function emitMExt(pre='this.'){
  for(let b=0;b<6;b++){const i=b*4
    L.push(`    const b${b}t0 = (${pre}${v(i)} + ${pre}${v(i+1)})${M}`)
    L.push(`    const b${b}t1 = (${pre}${v(i+2)} + ${pre}${v(i+3)})${M}`)
    L.push(`    const b${b}t2 = (2n * ${pre}${v(i+1)} + b${b}t1)${M}`)
    L.push(`    const b${b}t3 = (2n * ${pre}${v(i+3)} + b${b}t0)${M}`)
    L.push(`    const b${b}t4 = (4n * b${b}t1 + b${b}t3)${M}`)
    L.push(`    const b${b}t5 = (4n * b${b}t0 + b${b}t2)${M}`)
    L.push(`    const b${b}o0 = (b${b}t3 + b${b}t5)${M}`)
    L.push(`    const b${b}o1 = b${b}t5`)
    L.push(`    const b${b}o2 = (b${b}t2 + b${b}t4)${M}`)
    L.push(`    const b${b}o3 = b${b}t4`)}
  for(let j=0;j<4;j++)L.push(`    const mtmp${j} = (${[0,1,2,3,4,5].map(b=>`b${b}o${j}`).join(' + ')})${M}`)
  for(let i=0;i<C;i++){const b=Math.floor(i/4),j=i%4;set(i,`b${b}o${j} + mtmp${j}`,pre)}
}
function emitMInt(pre='this.'){
  L.push(`    const msum = (${pre}${v(0)} + ${[...Array(23)].map((_,i)=>pre+v(i+1)).join(' + ')})${M}`)
  for(let i=0;i<C;i++)set(i,`msum + 0x${di[i].toString(16)}n * ${pre}${v(i)}`,pre)
}
function emitSboxAll(pre='this.'){for(let i=0;i<C;i++){
  L.push(`    qt = (${pre}${v(i)} * ${pre}${v(i)})${M}`)
  L.push(`    wt = (qt * qt)${M}`)
  L.push(`    zt = (wt * qt)${M}`)
  set(i,`zt * ${pre}${v(i)}`,pre)}}
const sargs=[...Array(C)].map((_,i)=>`s${i}: bigint`).join(', ')
const rcargs=[...Array(C)].map((_,i)=>`rc${i}: bigint`).join(', ')
const stateRefs=[...Array(C)].map((_,i)=>`this.s${i}`).join(', ')
L.push(`// Poseidon2Stage — GOAL-v9 Stage 0d: ONE round of poseidon2_mix as a`)
L.push(`// stateful covenant step. 24 cells + round live in the UTXO (never the`)
L.push(`// stack). Round constants are constructor-baked per stage instance.`)
L.push(`import { StatefulSmartContract, assert } from 'runar-lang'`)
L.push(`export class Poseidon2Stage extends StatefulSmartContract {`)
for(let i=0;i<C;i++)L.push(`  s${i}: bigint`)
L.push(`  round: bigint`)
L.push(`  constructor (${sargs}, round: bigint) {`)
L.push(`    super(${[...Array(C)].map((_,i)=>`s${i}`).join(', ')}, round)`)
for(let i=0;i<C;i++)L.push(`    this.s${i} = s${i}`)
L.push(`    this.round = round`)
L.push(`  }`)
// methods compute into plain locals l0..l23 (ANF-friendly), then write
// this.sX back once — the stateful covenant emits continuation state.
function emitMethod (name, kind) {
  L.push(`  public ${name} () {`)
  L.push(`    let qt = 0n; let wt = 0n; let zt = 0n`)
  for (let i=0;i<C;i++) L.push(`    let ls${i} = this.s${i}`)
  if (kind === 'full') {
    for (let i=0;i<C;i++) L.push(`    ls${i} = (ls${i} + ` + `0x${rc[0*C+i].toString(16)}n` + `)${M}`)
    emitSboxAll('l'); emitMExt('l')
  } else {
    L.push(`    ls0 = (ls0 + ` + `0x${rc[4*C].toString(16)}n` + `)${M}`)
    L.push(`    qt = (ls0 * ls0)${M}`); L.push(`    wt = (qt * qt)${M}`); L.push(`    zt = (wt * qt)${M}`); L.push(`    ls0 = (zt * ls0)${M}`)
    emitMInt('l')
  }
  for (let i=0;i<C;i++) L.push(`    this.s${i} = ls${i}`)
  const outs = [...Array(C)].map((_,i)=>`ls${i}`).join(', ')
  L.push(`    this.addOutput(1n, ${outs}, this.round + 1n)`)
  L.push(`  }`)
}
emitMethod('fullStep', 'full')
emitMethod('partialStep', 'partial')
L.push(`}`)
writeFileSync('src/Poseidon2Stage.runar.ts', L.join('\n'))
console.log('emitted lines:', L.length)
