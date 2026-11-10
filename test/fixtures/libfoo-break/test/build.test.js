'use strict'
const { test } = require('node:test')
const assert = require('node:assert/strict')
const { start } = require('../src/app.js')
test('app starts against libfoo 3.0', () => { assert.equal(start('db'), 'open') })
