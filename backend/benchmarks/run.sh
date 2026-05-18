#!/usr/bin/env bash
# Run one benchmark and record everything needed to reproduce and compare it.
#
# A throughput number on its own is not evidence. What makes a before/after
# defensible is knowing the dataset size, the configuration, and where the CPU
# and the database locks were while the number was being produced — so this
# samples all of that during the run rather than reporting it afterwards, when
# the interesting state has already drained away.
#
#   benchmarks/run.sh <label> [loadgen flags...]
#
#   benchmarks/run.sh stage3-after -mode=charge -n=1500 -c=100 -dupes=1
#   benchmarks/run.sh ramp-500     -mode=charge -n=3000 -rate=500 -c=600 -dupes=1
#
# Writes benchmarks/results/<label>.md.

set -uo pipefail
cd "$(dirname "$0")/.."

LABEL="${1:?usage: run.sh <label> [loadgen flags...]}"
shift
OUT="benchmarks/results/${LABEL}.md"
SAMPLES=$(mktemp)

psql() { docker compose exec -T postgres psql -U payflow -d payflow -t -A -c "$1" 2>/dev/null; }

# ---------------------------------------------------------------- preflight
for p in 8080 8081 8090; do
  code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 3 "localhost:$p/healthz" 2>/dev/null)
  [ "$code" = "200" ] || { echo "service on :$p is not healthy ($code) — aborting"; exit 1; }
done

DATASET=$(psql "SELECT (SELECT count(*) FROM charges) || ' charges, '
                   || (SELECT count(*) FROM ledger_entries) || ' ledger entries, '
                   || (SELECT count(*) FROM merchants) || ' merchants'")
GIT=$(git rev-parse --short HEAD 2>/dev/null)
DIRTY=$(git diff --quiet 2>/dev/null && echo "clean" || echo "uncommitted changes")

echo "  dataset : $DATASET"
echo "  code    : $GIT ($DIRTY)"
echo "  running : $*"

# ------------------------------------------------------------- sample loop
# Backgrounded so it captures the system UNDER load. Sampling before and after
# would miss exactly the lock waits and connection saturation we care about.
(
  while true; do
    api_rss=$(powershell -Command "[math]::Round((Get-Process pf-api -EA SilentlyContinue).WorkingSet64/1MB,1)" 2>/dev/null | tr -d '\r')
    pg=$(docker stats --no-stream --format '{{.CPUPerc}}' payflow-postgres-1 2>/dev/null | tr -d '%\r')
    conns=$(psql "SELECT count(*) FILTER (WHERE state='active') || '/' || count(*) FROM pg_stat_activity WHERE datname='payflow'")
    waits=$(psql "SELECT count(*) FROM pg_stat_activity WHERE datname='payflow' AND wait_event_type='Lock'")
    slow=$(psql "SELECT coalesce(max(round(extract(epoch from (now()-query_start))*1000)),0) FROM pg_stat_activity WHERE datname='payflow' AND state='active'")
    echo "${api_rss:-0} ${pg:-0} ${conns:-0/0} ${waits:-0} ${slow:-0}" >> "$SAMPLES"
    sleep 2
  done
) &
SAMPLER=$!
trap 'kill $SAMPLER 2>/dev/null' EXIT

# ------------------------------------------------------------------- run it
RESULT=$(go run ./benchmarks/loadgen "$@" 2>&1)
kill $SAMPLER 2>/dev/null

# ---------------------------------------------------------- worst offenders
TOP=$(psql "SELECT '- ' || round(mean_exec_time::numeric,2) || ' ms mean, ' || calls || ' calls: '
                 || left(regexp_replace(query,'\s+',' ','g'), 90)
            FROM pg_stat_statements
            WHERE calls > 10 ORDER BY total_exec_time DESC LIMIT 5" \
      || echo "- pg_stat_statements not enabled")

# -------------------------------------------------------------- aggregate
read -r PG_MAX RSS_MAX CONN_MAX WAIT_MAX SLOW_MAX <<< "$(
  awk '{ if ($2>a) a=$2; if ($1>b) b=$1; if ($4>d) d=$4; if ($5>e) e=$5;
         split($3,c,"/"); if (c[1]>f) f=c[1] }
       END { printf "%.0f %.1f %d %d %.0f", a, b, f, d, e }' "$SAMPLES" 2>/dev/null
)"

# ----------------------------------------------------------------- write it
mkdir -p benchmarks/results
{
  echo "# Benchmark — $LABEL"
  echo
  echo "\`$(date -u '+%Y-%m-%d %H:%M UTC')\` · code \`$GIT\` ($DIRTY)"
  echo
  echo "## Configuration"
  echo
  echo "| | |"
  echo "|---|---|"
  echo "| Command | \`loadgen $*\` |"
  echo "| Dataset | $DATASET |"
  echo "| Hardware | $(nproc) cores, load generator on the same machine |"
  echo "| DB_MAX_CONNS | ${DB_MAX_CONNS:-50} |"
  echo "| Acquirer | simulator, ~24ms p50, 95/3/1.5/0.5 success/decline/timeout/error |"
  echo
  echo "## Result"
  echo
  echo '```'
  echo "$RESULT" | grep -vE '^\s*[0-9]+ / [0-9]+' | sed '/^$/d'
  echo '```'
  echo
  echo "## System under load"
  echo
  echo "| | Peak |"
  echo "|---|---|"
  echo "| Postgres CPU | ${PG_MAX:-?}% |"
  echo "| API resident memory | ${RSS_MAX:-?} MB |"
  echo "| Active DB connections | ${CONN_MAX:-?} |"
  echo "| Queries **waiting on a lock** | ${WAIT_MAX:-?} |"
  echo "| Longest running query | ${SLOW_MAX:-?} ms |"
  echo
  echo "## Most expensive statements"
  echo
  echo "$TOP"
} > "$OUT"

rm -f "$SAMPLES"
echo
echo "$RESULT" | grep -E 'throughput|latency|ok |failed'
echo
echo "  wrote $OUT"
