'use strict'
// libfoo 3.0 — connect() was renamed to open()
exports.open = target => ({ target, state: 'open' })
