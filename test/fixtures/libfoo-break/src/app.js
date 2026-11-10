'use strict'
const libfoo = require('../lib/libfoo.js')
exports.start = target => libfoo.connect(target).state
