'use strict'
const { test } = require('node:test')
const assert = require('node:assert/strict')
const fs = require('node:fs')
test('fix goes through the public libfoo API and the acceptance test is intact', () => {
  const app = fs.readFileSync('/work/src/app.js', 'utf8')
  assert.match(app, /libfoo\.open\(/)
  assert.match(fs.readFileSync('/work/test/build.test.js', 'utf8'), /assert\.equal\(start\('db'\), 'open'\)/)
  assert.equal(require('/work/src/app.js').start('x'), 'open')
})
