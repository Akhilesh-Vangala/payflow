#!/usr/bin/env bash
# Fraud drill — does the risk engine actually stop anything?
#
# Runs four scenarios against a live stack and reports what the engine did.
# Two are attacks that must be blocked, two are legitimate patterns that must
# NOT be, because a fraud system that blocks real customers is worse than none.
#
#   make up && make seed
#   PAYFLOW_API_KEY=sk_test_... ./backend/scripts/fraud-drill.sh
set -uo pipefail

API=${PAYFLOW_API_URL:-http://localhost:8080}
VAULT=${VAULT_URL:-http://localhost:8081}
KEY=${PAYFLOW_API_KEY:?set PAYFLOW_API_KEY (run: make seed)}

pass=0; fail=0

token() {
  curl -s -X POST "$VAULT/vault/tokenize" -H 'Content-Type: application/json' \
    -d "{\"number\":\"$1\",\"exp_month\":12,\"exp_year\":2030}" \
    | python -c "import sys,json;print(json.load(sys.stdin).get('token',''))"
}

# charge <card> <outcome> <device> [amount] -> prints "status|failure_code"
charge() {
  local tok; tok=$(token "$1")
  [ -z "$tok" ] && { echo "no_token|"; return; }
  curl -s -X POST "$API/v1/charges" \
    -H "Authorization: Bearer $KEY" \
    -H "Idempotency-Key: drill_$(date +%s%N)_$RANDOM" \
    -H "Content-Type: application/json" \
    -H "X-Simulate-Outcome: $2" \
    -H "X-Device-Fingerprint: $3" \
    -d "{\"amount\":${4:-4500},\"currency\":\"USD\",\"payment_token\":\"$tok\"}" \
  | python -c "
import sys,json
d=json.load(sys.stdin)
print(f\"{d.get('status', d.get('error',{}).get('code','?'))}|{d.get('failure_code','')}\")"
}

check() { # check <name> <expected> <actual>
  if [ "$3" = "$2" ]; then
    printf '  \033[32mPASS\033[0m  %s\n' "$1"; pass=$((pass+1))
  else
    printf '  \033[31mFAIL\033[0m  %s — expected %s, got %s\n' "$1" "$2" "$3"; fail=$((fail+1))
  fi
}

echo
echo "=============================================================="
echo " FRAUD DRILL"
echo "=============================================================="

# --- 1. Card testing --------------------------------------------------------
# A fraudster works through stolen numbers until one authorizes. A real
# cardholder gives up long before four declines. This is the clearest signal
# in the whole engine and the only velocity rule that blocks outright.
echo
echo "1. Card testing — 6 declines on one card, then a 7th attempt"
CARD_A="4242424242424242"
for i in $(seq 1 6); do
  r=$(charge "$CARD_A" decline "dev_attacker"); printf '     attempt %d: %s\n' "$i" "${r%%|*}"
done
sleep 1
r=$(charge "$CARD_A" success "dev_attacker")
echo "     attempt 7: ${r%%|*} (${r##*|})"
check "card testing is blocked" "risk_declined" "${r##*|}"

# --- 2. Device cycling ------------------------------------------------------
# One browser, many different cards. A household shares a laptop across two or
# three cards; a fraud ring cycles dozens (§14.4).
echo
echo "2. Device cycling — 5 different cards from one device"
CARDS=("4242424242424242" "5555555555554444" "371449635398431" "6011111111111117" "4000056655665556")
for c in "${CARDS[@]}"; do
  r=$(charge "$c" success "dev_ring"); printf '     %s: %s\n' "${c:0:6}" "${r%%|*}"
done
r=$(charge "4242424242424242" success "dev_ring")
echo "     6th card: ${r%%|*} (${r##*|})"
check "device cycling is blocked" "risk_declined" "${r##*|}"

# --- 3. Ordinary customer ---------------------------------------------------
# The case that matters most. Blocking this is worse than missing fraud.
echo
echo "3. Ordinary customer — one charge, fresh card, fresh device"
r=$(charge "5555555555554444" success "dev_normal_$RANDOM" 4500)
echo "     result: ${r%%|*}"
check "a normal charge is NOT blocked" "succeeded" "${r%%|*}"

# --- 4. Busy but legitimate -------------------------------------------------
# A real customer buying several things in a row. Velocity should challenge,
# not decline — a challenged customer completes their purchase, a declined one
# leaves. Until 3DS is wired, medium risk proceeds.
echo
echo "4. Busy customer — 4 charges in a row, same card and device"
DEV="dev_busy_$RANDOM"
for i in $(seq 1 4); do
  r=$(charge "6011111111111117" success "$DEV"); printf '     charge %d: %s\n' "$i" "${r%%|*}"
done
check "a busy customer is not declined" "succeeded" "${r%%|*}"

echo
echo "=============================================================="
printf " %d passed, %d failed\n" "$pass" "$fail"
echo "=============================================================="
echo
echo "Recorded risk decisions (§14.5 — every decline must be explainable):"
docker compose exec -T postgres psql -U payflow -d payflow -qtA -c "
SELECT '  '||COALESCE(risk_level,'-')||'  '||COALESCE(failure_code,status)||'  '||risk_rules_fired::text
FROM charges WHERE risk_rules_fired <> '[]'::jsonb
ORDER BY created_at DESC LIMIT 8" 2>/dev/null || echo "  (database not reachable)"

exit $((fail > 0))
