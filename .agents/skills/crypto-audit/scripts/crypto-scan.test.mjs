import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';

import { scanGoSource, scanRunarSource, scanDirectory } from './crypto-scan.mjs';

test('Go scanner flags unchecked rand.Read, ad-hoc KDF, AAD delimiter, unframed concat, and variable-time secret compare', () => {
  const sample = `
package sample

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
)

func genToken() []byte {
	var b [32]byte
	_, _ = rand.Read(b[:])
	return b[:]
}

func deriveBoxAEADKey(shared, a, b []byte) [32]byte {
	h := sha256.New()
	h.Write(shared)
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

func BoxAAD(office, deal string) []byte {
	return []byte("v1|" + office + "|" + deal)
}

func signDelim(a, b string) []byte {
	s := fmt.Sprintf("%s|%s", a, b)
	return DSHA256([]byte(s))
}

func unframed(a, b string) []byte {
	return DSHA256(Concat(Tag, []byte(a), []byte(b)))
}

func checkTok(presentedToken, expectedToken []byte) bool {
	return bytes.Equal(presentedToken, expectedToken)
}
`;

  const { inventory, leads } = scanGoSource('internal/sample/bad.go', sample);
  assert.ok(inventory);
  assert.deepEqual(inventory.primitives, ['crypto/rand', 'crypto/sha256']);

  const rules = new Set(leads.map((l) => l.rule));
  assert.ok(rules.has('key/unchecked-rng'), 'expected key/unchecked-rng');
  assert.ok(rules.has('crypto/adhoc-kdf'), 'expected crypto/adhoc-kdf');
  assert.ok(rules.has('crypto/aad-ambiguity'), 'expected crypto/aad-ambiguity');
  assert.ok(rules.has('hash/delimiter-collision'), 'expected hash/delimiter-collision');
  assert.ok(rules.has('hash/unframed-concat'), 'expected hash/unframed-concat');
  assert.ok(rules.has('ct/variable-time-secret-cmp'), 'expected ct/variable-time-secret-cmp');
});

test('Go scanner produces zero leads on hardened Go crypto patterns', () => {
  const safe = `
package sample

import (
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
)

func genToken() ([]byte, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return nil, err
	}
	return b[:], nil
}

func deriveBoxAEADKey(shared, salt, info []byte) ([]byte, error) {
	return hkdf.Key(sha256.New, shared, salt, string(info), 32)
}

func checkTok(presentedToken, expectedToken []byte) bool {
	return subtle.ConstantTimeCompare(presentedToken, expectedToken) == 1
}
`;

  const { leads } = scanGoSource('internal/sample/good.go', safe);
  assert.equal(leads.length, 0, `expected 0 leads, got: ${JSON.stringify(leads)}`);
});

test('Rúnar scanner flags BabyBear OP_MOD without range guard, incomplete Fiat-Shamir, unpinned TODO, and 1-byte length prefix', () => {
  const sample = `
// STAGE-1b TODO: bind the leaf's stage-script template hash on-chain
import { StatefulSmartContract, assert, sha256, ecMulGen, ecAdd, ecMul, cat, num2bin, len } from 'runar-lang'

export class SampleZk extends StatefulSmartContract {
  public stepZk (commitR: Point, s: bigint, e: bigint, from: bigint, sym: bigint, to: bigint, outHash: ByteString) {
    const challenge: ByteString = sha256(cat(commitR, num2bin(to, 8n)))
    assert(challenge == num2bin(e, 32n))
    assert(ecMulGen(s) == ecAdd(commitR, ecMul(this.delegateP, e)))
    const e1 = cat(num2bin(len(outHash), 1n), outHash)
    const x = (from * sym) % 2013265921n
    assert(x === 0n)
  }
}
`;

  const { inventory, leads } = scanRunarSource('contracts/src/SampleZk.runar.ts', sample);
  assert.ok(inventory);
  assert.ok(inventory.primitives.includes('ecMulGen'));
  assert.ok(inventory.primitives.includes('2013265921n'));

  const rules = new Set(leads.map((l) => l.rule));
  assert.ok(rules.has('zk/unpinned-vk-or-stage'), 'expected zk/unpinned-vk-or-stage');
  assert.ok(rules.has('zk/fiat-shamir-incomplete'), 'expected zk/fiat-shamir-incomplete');
  assert.ok(rules.has('script/signed-mod-neg'), 'expected script/signed-mod-neg');
  assert.ok(rules.has('script/length-prefix-overflow'), 'expected script/length-prefix-overflow');
});

test('Rúnar scanner accepts guarded BabyBear arithmetic and full Fiat-Shamir transcript', () => {
  const safe = `
import { StatefulSmartContract, assert, sha256, ecMulGen, ecAdd, ecMul, cat, num2bin } from 'runar-lang'

export class SafeZk extends StatefulSmartContract {
  public stepZk (commitR: Point, s: bigint, e: bigint, from: bigint, sym: bigint, to: bigint, outHash: ByteString) {
    assert(from >= 0n && from < 2013265921n)
    const challenge: ByteString = sha256(cat(commitR, num2bin(from, 8n), num2bin(sym, 8n), num2bin(to, 8n), outHash))
    assert(challenge == num2bin(e, 32n))
    assert(ecMulGen(s) == ecAdd(commitR, ecMul(this.delegateP, e)))
    const x = (from * 2n) % 2013265921n
    assert(x === 0n)
  }
}
`;

  const { leads } = scanRunarSource('contracts/src/SafeZk.runar.ts', safe);
  assert.equal(leads.length, 0, `expected 0 leads, got: ${JSON.stringify(leads)}`);
});

test('Rúnar scanner flags vulnerable .runar.go covenants and passes hardened .runar.go covenants', () => {
  const badGo = `
// TODO: pin verifier script hash on-chain
package contracts

import runar "github.com/icellan/runar/packages/runar-go"

type BadZk struct {
	runar.StatefulSmartContract
	DelegateP runar.Point
}

func (c *BadZk) StepZk(commitR runar.Point, s runar.Bigint, from runar.Bigint, sym runar.Bigint, to runar.Bigint, outHash runar.ByteString) {
	lt := runar.ExtractLocktime(c.TxPreimage)
	runar.Assert(lt >= 10)
	challenge := runar.Sha256(runar.Cat(commitR, runar.Num2Bin(to, 8)))
	e := runar.Bin2Num(challenge)
	runar.Assert(runar.EcMulGen(s) == runar.EcAdd(commitR, runar.EcMul(c.DelegateP, e)))
	e1 := runar.Cat(runar.Num2Bin(runar.Len(outHash), 1), outHash)
	_ = e1
	x := (from * sym) % 2013265921
	runar.Assert(x == 0)
}
`;
  const { inventory, leads } = scanRunarSource('contracts/src/BadZk.runar.go', badGo);
  assert.ok(inventory);
  assert.ok(inventory.primitives.includes('EcMulGen'));
  assert.ok(inventory.primitives.includes('2013265921'));

  const rules = new Set(leads.map((l) => l.rule));
  assert.ok(rules.has('zk/unpinned-vk-or-stage'), 'expected zk/unpinned-vk-or-stage');
  assert.ok(rules.has('zk/fiat-shamir-incomplete'), 'expected zk/fiat-shamir-incomplete');
  assert.ok(rules.has('script/signed-mod-neg'), 'expected script/signed-mod-neg');
  assert.ok(rules.has('script/length-prefix-overflow'), 'expected script/length-prefix-overflow');
  assert.ok(rules.has('script/locktime-sequence-bypass'), 'expected script/locktime-sequence-bypass');

  const safeGo = `
package contracts

import runar "github.com/icellan/runar/packages/runar-go"

type SafeZk struct {
	runar.StatefulSmartContract
	DelegateP runar.Point
}

func (c *SafeZk) StepZk(commitR runar.Point, s runar.Bigint, from runar.Bigint, sym runar.Bigint, to runar.Bigint, outHash runar.ByteString) {
	lt := runar.ExtractLocktime(c.TxPreimage)
	runar.Assert(runar.ExtractSequence(c.TxPreimage) < 4294967295)
	runar.Assert(lt < 500000000)
	runar.Assert(from >= 0 && from < 2013265921)
	runar.Assert(runar.Len(outHash) <= 75)
	stepBytes := runar.Cat(runar.Num2Bin(from, 8), runar.Cat(runar.Num2Bin(sym, 8), runar.Cat(runar.Num2Bin(to, 8), outHash)))
	challenge := runar.Sha256(runar.Cat(commitR, stepBytes))
	e := runar.Bin2Num(runar.Cat(challenge, runar.HexToBytes("00")))
	runar.Assert(runar.EcMulGen(s) == runar.EcAdd(commitR, runar.EcMul(c.DelegateP, e)))
	e1 := runar.Cat(runar.Num2Bin(runar.Len(outHash), 1), outHash)
	_ = e1
	x := (from * 2) % 2013265921
	runar.Assert(x == 0)
}
`;
  const safeRes = scanRunarSource('contracts/src/SafeZk.runar.go', safeGo);
  assert.equal(safeRes.leads.length, 0, `expected 0 leads on safe .runar.go, got: ${JSON.stringify(safeRes.leads)}`);
});

test('scanDirectory returns deterministic schema and skips *_test.go by default', () => {
  const tmp = fs.mkdtempSync(path.join(os.tmpdir(), 'crypto-scan-'));
  try {
    fs.mkdirSync(path.join(tmp, 'internal'), { recursive: true });
    fs.writeFileSync(
      path.join(tmp, 'internal', 'prod.go'),
      'package p\nimport "crypto/rand"\nfunc f(){ var b [8]byte; _, _ = rand.Read(b[:]) }\n'
    );
    fs.writeFileSync(
      path.join(tmp, 'internal', 'prod_test.go'),
      'package p\nimport "crypto/md5"\nvar _ = md5.New\n'
    );

    const resDefault = scanDirectory(tmp, { probe: false });
    assert.equal(resDefault.schemaVersion, 1);
    assert.equal(resDefault.scannedFiles.go, 1);
    assert.equal(resDefault.leads.length, 1);
    assert.equal(resDefault.leads[0].rule, 'key/unchecked-rng');

    const resWithTests = scanDirectory(tmp, { includeTests: true, probe: false });
    assert.equal(resWithTests.scannedFiles.go, 2);
    assert.equal(resWithTests.leads.length, 2);
  } finally {
    fs.rmSync(tmp, { recursive: true, force: true });
  }
});
