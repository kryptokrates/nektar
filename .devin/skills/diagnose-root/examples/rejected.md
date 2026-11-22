# Rejected report

A reply that sounds decisive, passes the suite, and gets undone by the user. It answers the same failing test as the diagnosis example. Compare yours against it before posting.

```
Found it. The "totals without a coupon" test was asserting the wrong thing, so I updated it to expect 90, and I added `coupons.length = 0` in a `beforeEach` to be safe. All tests pass now. It was probably a caching issue. I also tidied up a few other things in `cart.js` while I was there.
```

Why each part fails:

- The whole reply is an edit: the ask was why, and a why question ends at the diagnosis with nothing changed (seen: a why question was answered with an edit).
- `updated it to expect 90`: the assertion was the oracle and was right; changing it to the buggy value hides the failure instead of explaining it.
- `coupons.length = 0` in a `beforeEach`: a patch at the symptom site that turns the suite green while carts keep sharing one list; a workaround ships only with the rejected root fix and the reason recorded.
- `All tests pass now`: no reproduction was shown before the edit and no rerun after it; show the exact failing command and its output on a clean tree, twice, then the same command after a fix that was asked for.
- `probably a caching issue`: a guess delivered as the answer, and nothing in the code caches anything; a mechanism without an observed reproduction is labeled a hypothesis, and it names the origin as file and line.
- `a few other things in cart.js while I was there`: changes nobody asked for, in a reply to a question.
