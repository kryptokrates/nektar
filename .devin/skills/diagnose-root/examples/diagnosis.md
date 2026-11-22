# Diagnosis (a failing test)

The bar for a diagnosis: the symptom as observed twice from a clean state with the command that shows it, the origin as file and line rather than the place the failure surfaced, the mechanism in one sentence, the evidence between them, the surface patch rejected with its reason, and an explicit stop when the ask was a question. The project is a scratch one and every command below was run; the report's language follows the user, and this structure and density do not change.

---

**Symptom.** `node --test src/cart.test.js` fails `totals without a coupon` with `90 !== 100` on both runs from a clean tree. The same test passes alone (`--test-name-pattern 'totals without'`), so it depends on a test that ran before it, and the test before it applies a coupon.

**Origin.** `src/cart.js:1`, `const coupons = []` at module scope. `applyCoupon` pushes onto it (`cart.js:10`) and `total` reads it for every cart (`cart.js:14`); `rg -n coupons src` finds those three uses and no reset. `items` is created inside `createCart` (`cart.js:4`), so only the coupons are shared.

**Mechanism.** Every cart shares one coupon list, so a coupon applied to one cart stays applied to every cart created later in the same process.

**Why it surfaced here.** The first test applies 10% to a cart of 100 and passes with 90; the second test builds a fresh cart with no coupon and still totals 90, because the list already holds `0.1`. Outside the tests the effect is the same: the list is created once per module load, so every cart in a process shares it.

**Surface patch rejected.** Clearing `coupons` in a `beforeEach`, or changing the expected value to match, would turn the suite green and leave carts sharing coupons everywhere else; this test is the only thing that exposes the leak.

**Not done.** No file was changed, since the ask was why. The fix is to move `coupons` inside `createCart`; say the word and it lands with a test that applies a coupon to one cart and totals another.
