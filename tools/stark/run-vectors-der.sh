#!/bin/sh
# run every JSONL case through the derivation guest executor; print a summary
pass=0; fail=0
while IFS= read -r line; do
  name=$(printf '%s' "$line" | sed -n 's/.*"case":"\([^"]*\)".*/\1/p')
  expect=$(printf '%s' "$line" | sed -n 's/.*"expect":"\([^"]*\)".*/\1/p')
  res=$(printf '%s' "$line" | /work/target/release/nektar-stark execute-der 2>/dev/null)
  root=$(printf '%s' "$res" | sed -n 's/.*"policyRoot":"\([0-9a-f]*\)".*/\1/p')
  case "$expect" in
    refuse) if printf '%s' "$res" | grep -q '"ok":false'; then pass=$((pass+1)); else fail=$((fail+1)); echo "FAIL $name (accepted)"; fi ;;
    *) if [ "$root" = "$expect" ]; then pass=$((pass+1)); else fail=$((fail+1)); echo "FAIL $name got=$root"; fi ;;
  esac
done
echo "vectors-der: pass=$pass fail=$fail"
