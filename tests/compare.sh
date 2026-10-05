#!/usr/bin/env bash
#
# tests/compare.sh — cross-implementation consistency check.
#
# Verifies that the Python and Bash implementations (and the Go binary when
# built) agree on headline totals for a fixed filter window, and that JSON
# output is valid.  Exits non-zero on any mismatch beyond tolerance.

set -euo pipefail

DIR="$(cd "$(dirname "$0")/.." && pwd)"
PY="$DIR/ocstats"
SH="$DIR/ocstats.sh"
GO_BIN="$DIR/ocstats-bin"
TOL="0.02"   # relative tolerance for cost comparisons

# database selection: $OCSTATS_DB wins; otherwise a deterministic synthetic
# fixture is generated on first run so the suite needs no real usage data
FIXTURE="$DIR/tests/fixture.db"
if [[ -n "${OCSTATS_DB:-}" ]]; then
    export OCSTATS_DB
    echo "database: $OCSTATS_DB (from \$OCSTATS_DB)"
    SINCE="${OCSTATS_SINCE:-$(date -d '-14 days' +%F)}"
else
    if [[ ! -f "$FIXTURE" ]]; then
        python3 "$DIR/tests/make_fixture.py" "$FIXTURE"
    fi
    export OCSTATS_DB="$FIXTURE"
    echo "database: $FIXTURE (synthetic fixture — regenerate: python3 tests/make_fixture.py)"
    SINCE="$(date -d '-14 days' +%F)"
fi

for tool in jq sqlite3 awk; do
    command -v "$tool" >/dev/null || { echo "compare.sh requires: $tool"; exit 1; }
done

fail=0
ok()   { echo "  ok    $1"; }
bad()  { echo "  FAIL  $1"; fail=1; }

jqval() { jq -r "$1"; }

# --- field extractor: totals from `summary --json` --------------------------
py_tot=$("$PY" summary --json --since "$SINCE")
sh_tot=$("$SH" summary --json --since "$SINCE")

extract() { # extract <json> <path>
    printf '%s' "$1" | jqval "$2"
}

echo "python vs bash · summary totals (since $SINCE):"
for field in .totals.messages .totals.sessions .totals.tokens.input \
             .totals.tokens.output .totals.tokens.cache_read; do
    a=$(extract "$py_tot" "$field")
    b=$(extract "$sh_tot" "$field")
    if [[ "$a" == "$b" ]]; then ok "$field = $a"; else bad "$field: python=$a bash=$b"; fi
done

for field in .totals.cost.reported .totals.cost.effective; do
    a=$(extract "$py_tot" "$field")
    b=$(extract "$sh_tot" "$field")
    if awk -v a="$a" -v b="$b" -v t="$TOL" \
        'BEGIN{ d = a - b; if (d < 0) d = -d; exit !(d <= t * (a > b ? a : b)) }'; then
        ok "$field ≈ $a"
    else
        bad "$field: python=$a bash=$b"
    fi
done

# --- group invariants --------------------------------------------------------
echo "invariants:"
# matrix(provider) totals must equal providers totals
mat=$("$PY" matrix --by provider --json --limit 0)
prov=$("$PY" providers --json --limit 0)
m_sum=$(printf '%s' "$mat" | jq '[.rows[].cost_eff] | add')
p_sum=$(printf '%s' "$prov" | jq '[.rows[].cost_eff] | add')
if awk -v a="$m_sum" -v b="$p_sum" 'BEGIN{ d = a - b; if (d < 0) d = -d; exit !(d < 0.001) }'; then
    ok "matrix(provider) Σ $(printf '%.4f' "$m_sum") == providers Σ $(printf '%.4f' "$p_sum")"
else
    bad "matrix Σ $m_sum != providers Σ $p_sum"
fi

# messages sum across models == summary messages
mdl_sum=$("$PY" models --json --since "$SINCE" --limit 0 | jq '[.rows[].n] | add')
msg_n=$(extract "$py_tot" '.totals.messages')
[[ "$mdl_sum" == "$msg_n" ]] && ok "models msgs Σ $mdl_sum == summary msgs $msg_n" \
                          || bad "models Σ $mdl_sum != summary $msg_n"

# --- bash json validity ------------------------------------------------------
echo "bash json validity:"
for cmd in "models" "providers" "daily" "matrix --by provider,month"; do
    if "$SH" $cmd --json --since "$SINCE" --limit 3 | jq -e '.rows | type == "array"' >/dev/null 2>&1; then
        ok "$cmd --json parses"
    else
        bad "$cmd --json invalid"
    fi
done

# --- new output modes & features ---------------------------------------------
echo "output modes & analytics features:"
# csv parses and rows match json count (csv always emits all rows)
n_json=$("$PY" models --json --since "$SINCE" --limit 0 | jq '.rows | length')
n_csv=$("$PY" models --csv --since "$SINCE" --limit 0 | tail -n +2 | grep -c .)
[[ "$n_json" == "$n_csv" ]] && ok "python --csv rows ($n_csv) == --json rows" \
                        || bad "csv rows $n_csv != json rows $n_json"

timeout 90 "$SH" models --csv --since "$SINCE" --limit 3 >/tmp/.ocstats_csv.$$ 2>&1 \
    && grep -q "prompts" /tmp/.ocstats_csv.$$ \
    && ok "bash --csv includes prompts column" || bad "bash --csv missing prompts"
rm -f /tmp/.ocstats_csv.$$

# markdown shape: header + alignment row + TOTAL
mdout=$("$PY" providers --md --limit 3)
echo "$mdout" | grep -q '^| ' && echo "$mdout" | grep -q '\*\*TOTAL\*\*' \
    && ok "python --md renders pipe table with TOTAL" || bad "python --md malformed"

# details columns present
"$PY" models --details --limit 1 --width 300 --no-color | grep -q "Last seen" \
    && ok "python --details shows extended stats" || bad "python --details missing columns"
timeout 90 "$SH" models --details --limit 1 --no-color >/tmp/.ocstats_det.$$ 2>&1 \
    && grep -q "Last seen" /tmp/.ocstats_det.$$ \
    && ok "bash --details shows extended stats" || bad "bash --details missing columns"
rm -f /tmp/.ocstats_det.$$

# top/bottom sanity: same command, different slices (first data row after the header border)
top1=$("$PY" providers --top 1 --no-color | awk '/^├/{getline; print; exit}' | sed 's/^│ *//; s/ *│.*//')
bot1=$("$PY" providers --bottom 1 --no-color | awk '/^├/{getline; print; exit}' | sed 's/^│ *//; s/ *│.*//')
[[ -n "$top1" && -n "$bot1" && "$top1" != "$bot1" ]] && ok "--top/--bottom differ ($top1 vs $bot1)" \
                                               || bad "--top='$top1' --bottom='$bot1'"

# pivot reconciles with matrix totals (same metric basis: cost)
pv_grand=$("$PY" pivot --rows project --cols day --metric cost --json --since "$SINCE" | jq -r '.grand_total')
mx_eff=$("$PY" matrix --by project --json --since "$SINCE" --limit 0 | jq '[.rows[].cost_eff] | add')
if awk -v a="$pv_grand" -v b="$mx_eff" 'BEGIN{ d=a-b; if(d<0)d=-d; exit !(d < 0.5) }'; then
    ok "pivot cost grand $(printf '%.4f' "$pv_grand") ≈ matrix Σ $(printf '%.4f' "$mx_eff")"
else
    bad "pivot $pv_grand != matrix $mx_eff"
fi
timeout 90 "$SH" pivot --rows project --cols day --since "$SINCE" --no-color >/dev/null 2>&1 \
    && ok "bash pivot runs" || bad "bash pivot failed"

# prompts total matches direct SQL count (local midnight, same as the tool)
lo_ms=$(( $(date -d "$SINCE" +%s) * 1000 ))
sql_prompts=$(sqlite3 "file:${OCSTATS_DB}?mode=ro" \
    "SELECT COUNT(*) FROM session_message WHERE type='user' AND time_created >= $lo_ms;")
py_prompts=$("$PY" summary --json --since "$SINCE" | jq -r '.totals.prompts')
[[ "$sql_prompts" == "$py_prompts" ]] && ok "prompts total == SQL count ($py_prompts)" \
                                   || bad "prompts: tool=$py_prompts sql=$sql_prompts"

# compare: current 7d window rows equal an explicit --since 7d run
cmp_eff=$("$PY" providers --compare 7d/7d --json --limit 0 2>/dev/null | jq '[.rows[].cost_eff] | add')
plain_eff=$("$PY" providers --since 7d --json --limit 0 | jq '[.rows[].cost_eff] | add')
[[ -n "$cmp_eff" ]] && awk -v a="$cmp_eff" -v b="$plain_eff" 'BEGIN{d=a-b;if(d<0)d=-d;exit !(d<0.01)}' \
    && ok "compare window == explicit range" || bad "compare $cmp_eff vs $plain_eff"

# budget block appears
"$PY" summary --monthly-budget 50 --no-color --since "$SINCE" | grep -q "Pace" \
    && ok "python budget block renders" || bad "python budget missing"
timeout 90 "$SH" summary --monthly-budget 50 --no-color --since "$SINCE" >/tmp/.ocstats_bud.$$ 2>&1 \
    && grep -q "Pace" /tmp/.ocstats_bud.$$ \
    && ok "bash budget block renders" || bad "bash budget missing"
rm -f /tmp/.ocstats_bud.$$

# --- go binary (only when built) ---------------------------------------------
if [[ -x "$GO_BIN" ]]; then
    echo "go binary:"
    go_tot=$("$GO_BIN" summary --json --since "$SINCE")
    for field in .totals.messages .totals.sessions .totals.tokens.input; do
        a=$(extract "$py_tot" "$field")
        c=$(extract "$go_tot" "$field")
        [[ "$a" == "$c" ]] && ok "$field = $a" || bad "$field: python=$a go=$c"
    done
else
    echo "go binary: skipped (not built — run: make build)"
fi

echo
if ((fail)); then
    echo "RESULT: FAILURES DETECTED"
    exit 1
fi
echo "RESULT: all checks passed"
