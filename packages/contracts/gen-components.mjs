// Stage 0c components: emit each Poseidon2 building block as its own method —
// mExt, mInt, one full round, one partial round — each under the 800-deep
// scheduler limit, giving exact per-component bytes/ops.
import { readFileSync, writeFileSync } from 'node:fs'
const { rc, di } = JSON.parse(readFileSync('poseidon2-consts.json', 'utf8'))
const P = 2013265921n
const M = ' % 2013265921n'
const C = 24
const v = i => `s${i}`
const L = []
const set = (i,e) => L.push(`    ${v(i)} = (${e})${M}`)

function emitMExt (ind) {
  const I = ' '.repeat(ind)
  for (let b=0;b<6;b++){
    const i=b*4
    L.push(`${I}const b${b}t0 = (${v(i)} + ${v(i+1)})${M}`)
    L.push(`${I}const b${b}t1 = (${v(i+2)} + ${v(i+3)})${M}`)
    L.push(`${I}const b${b}t2 = (2n * ${v(i+1)} + b${b}t1)${M}`)
    L.push(`${I}const b${b}t3 = (2n * ${v(i+3)} + b${b}t0)${M}`)
    L.push(`${I}const b${b}t4 = (4n * b${b}t1 + b${b}t3)${M}`)
    L.push(`${I}const b${b}t5 = (4n * b${b}t0 + b${b}t2)${M}`)
    L.push(`${I}const b${b}o0 = (b${b}t3 + b${b}t5)${M}`)
    L.push(`${I}const b${b}o1 = b${b}t5`)
    L.push(`${I}const b${b}o2 = (b${b}t2 + b${b}t4)${M}`)
    L.push(`${I}const b${b}o3 = b${b}t4`)
  }
  for(let j=0;j<4;j++) L.push(`${I}const mtmp${j} = (${[0,1,2,3,4,5].map(b=>`b${b}o${j}`).join(' + ')})${M}`)
  for(let i=0;i<C;i++){ const b=Math.floor(i/4), j=i%4; set(i, `b${b}o${j} + mtmp${j}`) }
}
function emitMInt (ind) {
  const I = ' '.repeat(ind)
  L.push(`${I}const msum = (${v(0)} + ${[...Array(23)].map((_,i)=>v(i+1)).join(' + ')})${M}`)
  for (let i=0;i<C;i++) set(i, `msum + 0x${di[i].toString(16)}n * ${v(i)}`)
}
function emitSboxAll (ind) {
  const I = ' '.repeat(ind)
  for(let i=0;i<C;i++){
    L.push(`${I}qt = (${v(i)} * ${v(i)})${M}`)
    L.push(`${I}wt = (qt * qt)${M}`)
    L.push(`${I}zt = (wt * qt)${M}`)
    set(i, `zt * ${v(i)}`)
  }
}
const args24 = [...Array(C)].map((_,i)=>`x${i}: bigint`).join(', ')
const exp8 = [...Array(8)].map((_,i)=>`e${i}: bigint`).join(', ')
const asserts = `    assert(${[...Array(8)].map((_,i)=>`${v(i)} === e${i}`).join(' && ')})`
const decls = ['    let qt = 0n; let wt = 0n; let zt = 0n', ...[...Array(C)].map((_,i)=>`    let ${v(i)} = x${i}`)]

const hdr = [
`// Poseidon2Blocks — GOAL-v9 Stage 0c: per-component measurements of the`,
`// risc0-zkp 3.0.5 poseidon2_mix building blocks. Generated file.`,
`import { SmartContract, assert } from 'runar-lang'`,
``,
`export class Poseidon2Blocks extends SmartContract {`,
`  constructor () { super() }`,
]
L.push(...hdr)

// method 1: mExt alone
L.push(`  public mExt (${args24}, ${exp8}) {`)
L.push(...decls); emitMExt(4); L.push(asserts); L.push(`  }`)

// method 2: mInt alone
L.push(`  public mInt (${args24}, ${exp8}) {`)
L.push(...decls); emitMInt(4); L.push(asserts); L.push(`  }`)

// method 3: sboxesFull alone (24 sbox7)
L.push(`  public sboxesFull (${args24}, ${exp8}) {`)
L.push(...decls); emitSboxAll(4); L.push(asserts); L.push(`  }`)

// method 4: one full round = +24 RC + 24 sbox + mExt  (round 0 consts)
L.push(`  public fullRound (${args24}, ${exp8}) {`)
L.push(...decls)
for(let i=0;i<C;i++) set(i,`${v(i)} + 0x${rc[i].toString(16)}n`)
emitSboxAll(4); emitMExt(4); L.push(asserts); L.push(`  }`)

// method 5: one partial round = +1 RC + 1 sbox + mInt (round 4 const)
L.push(`  public partialRound (${args24}, ${exp8}) {`)
L.push(...decls)
set(0, `${v(0)} + 0x${rc[4*C].toString(16)}n`)
L.push(`    qt = (${v(0)} * ${v(0)})${M}`); L.push(`    wt = (qt * qt)${M}`); L.push(`    zt = (wt * qt)${M}`); set(0,`zt * ${v(0)}`)
emitMInt(4); L.push(asserts); L.push(`  }`)

L.push(`}`)
writeFileSync('src/Poseidon2Blocks.runar.ts', L.join('\n'))
console.log('emitted lines:', L.length)
