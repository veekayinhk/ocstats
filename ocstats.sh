#!/usr/bin/env bash
#
# ocstats.sh — descriptive statistics & cost analytics for OpenCode v2 usage.
#
# Aggregate tokens, prompts, sessions and cost by model, provider, agent,
# project, day, week, month, session — or any combination of these.
#
#   database   $OCSTATS_DB or ~/.local/share/opencode/opencode.db   (read-only)
#   pricing    ~/.cache/ocstats/api.json        (models.dev, manual refresh)
#   overrides  ~/.config/ocstats/pricing.json
#
# Requires: bash 4, sqlite3 (JSON1), jq, awk, curl (only for prices --refresh)
#
# aggregation examples:
#   ocstats.sh                                     summary of everything
#   ocstats.sh models                              per provider × model
#   ocstats.sh providers --since 30d               last 30 days per provider
#   ocstats.sh daily                               per-day time series
#   ocstats.sh projects --details                  per project + prompts/reasoning/p90
#   ocstats.sh matrix --by provider,month          per provider per month
#   ocstats.sh matrix --by project,day --metric tokens   tokens per project per day
#   ocstats.sh pivot --rows project --cols day     projects-per-day heat-map grid
#   ocstats.sh agents --compare 7d/7d              this week vs last, per agent
#   ocstats.sh sessions --top 5                    5 most expensive sessions
#   ocstats.sh summary --monthly-budget 50         spend vs $50 monthly budget
#
# dimensions for matrix --by / pivot --rows / --cols:
#   model, provider, agent, project, session, day, week, month
#   (comma-separate any combination, e.g. --by project,day,provider)

set -euo pipefail

PROG="ocstats"
VERSION="1.1.0"
DB="${OCSTATS_DB:-$HOME/.local/share/opencode/opencode.db}"
CACHE_DIR="${XDG_CACHE_HOME:-$HOME/.cache}/ocstats"
CACHE_FILE="$CACHE_DIR/api.json"
OVERRIDES_FILE="${XDG_CONFIG_HOME:-$HOME/.config}/ocstats/pricing.json"
MODELS_DEV_URL="https://models.dev/api.json"

# ---------------------------------------------------------------------------
# colors & tiny helpers
# ---------------------------------------------------------------------------

BOLD=$'\e[1m'; DIM=$'\e[2m'; RED=$'\e[31m'; GREEN=$'\e[32m'; YELLOW=$'\e[33m'
CYAN=$'\e[36m'; WHITE=$'\e[37m'; RESET=$'\e[0m'
NO_COLOR_OUT=0
RENDER_MD=0
apply_color_switch() {
    if [[ "$NO_COLOR_OUT" == "1" || -n "${NO_COLOR:-}" || ! -t 1 || "$RENDER_MD" == "1" ]]; then
        BOLD=""; DIM=""; RED=""; GREEN=""; YELLOW=""; CYAN=""; WHITE=""; RESET=""
    fi
}

strip_ansi() { printf '%s' "$1" | sed -e $'s/\x1b\\[[0-9;]*m//g'; }
wlen() { strip_ansi "$1" | awk '{ n = length($0) } END { print n + 0 }'; }
rep_char() { local c=$1 n=$2; (( n <= 0 )) && return 0 || printf "$c%.0s" $(seq 1 "$n"); }
group_thousands() { sed -E ':a; s/([0-9]+)([0-9]{3})/\1,\2/; ta' <<<"$1"; }

money_f() { # adaptive $ formatting to match the python implementation
    awk -v v="$1" 'BEGIN {
        if (v == 0) { print "$0"; exit }
        if (v >= 100) { printf "$%.2f\n", v; exit }
        if (v >= 1)   { printf "$%.3f\n", v; exit }
        printf "$%.4f\n", v }'
}

spark() { # spark <space-separated values> → ▁▂▃▄▅▆▇█
    awk -v vals="$*" 'BEGIN {
        n = split(vals, v, " "); mx = 0
        for (i = 1; i <= n; i++) if (v[i]+0 > mx) mx = v[i]+0
        if (mx == 0) mx = 1
        ch = "▁▂▃▄▅▆▇█"
        for (i = 1; i <= n; i++) {
            lvl = int(v[i] / mx * 8); if (lvl > 7) lvl = 7; if (lvl < 0) lvl = 0
            printf "%s", substr(ch, lvl + 1, 1)
        }
    }'
}

bar_str() { # bar_str <fraction 0..1> [width]
    awk -v f="${1:-0}" -v w="${2:-10}" 'BEGIN {
        if (f < 0) f = 0; if (f > 1) f = 1
        filled = int(f * w + 0.5)
        for (i = 0; i < filled; i++) printf "█"
        for (i = filled; i < w; i++) printf "░" }'
}

# ---------------------------------------------------------------------------
# generic boxed-table / markdown renderer
#   render_tsv <title> <aligns> <footer_sep 0|1> [note]
#   stdin: header line, then TSV rows (last row is the footer when footer_sep=1)
#   RENDER_MD=1 → GitHub-flavored markdown pipe table instead of the box grid
# ---------------------------------------------------------------------------

render_tsv() {
    local title="$1" aligns="$2" footer_sep="${3:-0}" note="${4:-}"
    local -a hdr rows=()
    IFS=$'\t' read -r -a hdr
    mapfile -t rows
    local ncols=${#hdr[@]}

    if ((RENDER_MD)); then
        local m="" i
        for ((i = 0; i < ncols; i++)); do
            case "${aligns:$i:1}" in r) m+="---:|";; c) m+=":---:|";; *) m+=":---|";; esac
        done
        [[ -n "$title" ]] && printf '### %s\n\n' "$title"
        printf '%s\n' "$(pipe_row "$(IFS=$'\t'; echo "${hdr[*]}")")"
        printf '|%s\n' "$m"
        local row
        if ((footer_sep && ${#rows[@]} > 1)); then
            for row in "${rows[@]:0:${#rows[@]}-1}"; do printf '%s\n' "$(pipe_row "$row")"; done
            printf '%s\n' "$(pipe_row "${rows[${#rows[@]}-1]}" bold)"
        else
            for row in "${rows[@]}"; do printf '%s\n' "$(pipe_row "$row")"; done
        fi
        [[ -n "$note" ]] && printf '\n*%s*\n' "$note"
        return 0
    fi

    local -a w=()
    local i j cell pad
    for ((i = 0; i < ncols; i++)); do w[$i]=$(wlen "${hdr[$i]}"); done
    for row in "${rows[@]}"; do
        IFS=$'\t' read -r -a cells <<<"$row"
        for ((i = 0; i < ncols; i++)); do
            pad=$(wlen "${cells[$i]:-}")
            (( pad > w[$i] )) && w[$i]=$pad
        done
    done

    local edge="" mid="" dmid="" bot=""
    [[ -n "$title" ]] && printf '%s\n' "${BOLD}${CYAN}${title}${RESET}"
    for ((i = 0; i < ncols; i++)); do
        edge+="$(rep_char '─' $((w[$i] + 2)))"
        mid+="$(rep_char '─' $((w[$i] + 2)))"
        dmid+="$(rep_char '═' $((w[$i] + 2)))"
        bot+="$(rep_char '─' $((w[$i] + 2)))"
        if ((i < ncols - 1)); then edge+="┬"; mid+="┼"; dmid+="╪"; bot+="┴"; fi
    done
    printf '╭%s╮\n' "$edge"

    local line="│"
    for ((i = 0; i < ncols; i++)); do
        cell="${hdr[$i]}"; pad=$((w[$i] - $(wlen "$cell")))
        if [[ "${aligns:$i:1}" == "r" ]]; then cell="$(rep_char ' ' "$pad")$cell"
        else cell="$cell$(rep_char ' ' "$pad")"; fi
        line+=" $cell "; ((i < ncols - 1)) && line+="│"
    done
    printf '%s\n' "${BOLD}${line}${RESET}"
    printf '├%s┤\n' "$mid"

    local nr=${#rows[@]}
    for ((j = 0; j < nr; j++)); do
        if ((footer_sep && j == nr - 1 && nr > 1)); then printf '╞%s╡\n' "$dmid"; fi
        IFS=$'\t' read -r -a cells <<<"${rows[$j]}"
        line="│"
        for ((i = 0; i < ncols; i++)); do
            cell="${cells[$i]:-}"; pad=$((w[$i] - $(wlen "$cell")))
            if [[ "${aligns:$i:1}" == "r" ]]; then cell="$(rep_char ' ' "$pad")$cell"
            elif [[ "${aligns:$i:1}" == "c" ]]; then
                cell="$(rep_char ' ' $((pad / 2)))$cell$(rep_char ' ' $((pad - pad / 2)))"
            else cell="$cell$(rep_char ' ' "$pad")"; fi
            line+=" $cell "; ((i < ncols - 1)) && line+="│"
        done
        printf '%s\n' "$line"
    done
    printf '╰%s╯\n' "$bot"
    [[ -n "$note" ]] && printf '  %s\n' "${DIM}${note}${RESET}"
    return 0
}

pipe_row() { # pipe_row <tsv row> [bold] → "| a | b |"
    local row="$1" mode="${2:-}"
    local IFS=$'\t'
    read -r -a f <<<"$row"
    local out="|" c i plain
    for c in "${f[@]}"; do
        plain=$(strip_ansi "$c"); plain="${plain//|/\\|}"
        [[ -n "$mode" && -n "$plain" ]] && plain="**$plain**"
        out+=" $plain |"
    done
    printf '%s' "$out"
}

# ---------------------------------------------------------------------------
# pricing (models.dev cache + user overrides, fuzzy provider/model matching)
# ---------------------------------------------------------------------------

rates_for() { # rates_for <provider> <model> → "pin pout pcr pcw source"
    local p="$1" m="$2" base leaf out cand
    base="${m%%@*}"; leaf="${base##*/}"
    for cand in "$m" "$base" "$leaf"; do
        if [[ -f "$OVERRIDES_FILE" ]]; then
            out=$(jq -r --arg p "$p" --arg m "$cand" \
                '($p + "/" + $m) as $k |
                 .[$k]? // empty |
                 select(type == "object") |
                 [(.input // 0), (.output // 0), (.cache_read // 0), (.cache_write // 0), "override"] | @tsv' \
                "$OVERRIDES_FILE" 2>/dev/null || true)
            [[ -n "$out" ]] && { printf '%s\n' "$out"; return; }
        fi
        if [[ -f "$CACHE_FILE" ]]; then
            out=$(jq -r --arg p "$p" --arg m "$cand" \
                '.[$p].models[$m].cost? // empty |
                 select(type == "object") |
                 [(.input // 0), (.output // 0), (.cache_read // 0), (.cache_write // 0), "models.dev"] | @tsv' \
                "$CACHE_FILE" 2>/dev/null || true)
            [[ -n "$out" ]] && { printf '%s\n' "$out"; return; }
        fi
    done
    if [[ -f "$CACHE_FILE" ]]; then # global fallback: exact id or leaf under any provider
        out=$(jq -r --arg m "$m" --arg l "$leaf" '
            [to_entries[] | .key as $p | (.value.models // {}) |
             (if has($m) then .[$m] elif has($l) then .[$l] else empty end) as $e |
             select(($e.cost // null) != null and ($e.cost | type) == "object") |
             [$e.cost.input // 0, $e.cost.output // 0,
              $e.cost.cache_read // 0, $e.cost.cache_write // 0, "models.dev~"] | @tsv] | .[0] // empty' \
            "$CACHE_FILE" 2>/dev/null || true)
        [[ -n "$out" ]] && { printf '%s\n' "$out"; return; }
    fi
    printf '%s\n' "0	0	0	0	none"
}

refresh_pricing() {
    mkdir -p "$CACHE_DIR"
    if curl -fsSL --max-time 20 -A "$PROG/$VERSION" "$MODELS_DEV_URL" -o "$CACHE_FILE.tmp" \
        && jq -e 'type == "object"' "$CACHE_FILE.tmp" >/dev/null 2>&1; then
        mv "$CACHE_FILE.tmp" "$CACHE_FILE"
        echo "$PROG: pricing refreshed → $CACHE_FILE"
    else
        rm -f "$CACHE_FILE.tmp"
        echo "$PROG: failed to fetch $MODELS_DEV_URL" >&2
        exit 1
    fi
}

print_pricing_hint() {
    if [[ ! -f "$CACHE_FILE" && ! -f "$OVERRIDES_FILE" ]]; then
        echo "note: no pricing data — run \`$PROG prices --refresh\` to enable cost estimates" >&2
    fi
}

# ---------------------------------------------------------------------------
# filters / time range
# ---------------------------------------------------------------------------

SINCE=""; UNTIL=""; F_MODEL=""; F_PROVIDER=""; F_AGENT=""; F_PROJECT=""; F_SESSION=""
LIMIT=25; SORT=""; JSON=0; CSV=0; MD=0; DETAILS=0; TOPN=0; BOTTOMN=0; MINSHARE=0
MONTHLY_BUDGET=""; COMPARE=""; METRIC=""
CMD="summary"; REFRESH=0; MATRIX_BY=""; NO_COLOR_OUT=0
PV_ROWS="project"; PV_COLS="day"
LO_MS=0; HI_MS=99999999999999
CMP_CUR=""; CMP_PREV=""

parse_when() { # parse_when <value> <lo|hi> [anchor_ms] → epoch ms
    local s="$1" edge="$2" anchor="${3:-}" ms=0 ref
    if [[ -n "$anchor" ]]; then ref="$anchor"; else ref=$(date +%s)000; fi
    if [[ "$s" =~ ^([0-9]+)([dwm])$ ]]; then
        local n=${BASH_REMATCH[1]} u=${BASH_REMATCH[2]}
        [[ "$u" == "w" ]] && n=$((n * 7))
        [[ "$u" == "m" ]] && n=$((n * 30))
        local ref_s=$((ref / 1000))
        if [[ "$edge" == "hi" ]]; then ms=$ref_s
        else ms=$((ref_s - n * 86400)); fi
    elif [[ "$s" =~ ^[0-9]{4}-[0-9]{2}-[0-9]{2}$ ]]; then
        if [[ "$edge" == "hi" ]]; then ms=$(date -d "$s + 1 day" +%s); else ms=$(date -d "$s" +%s); fi
    elif [[ "$s" =~ ^[0-9]{4}-[0-9]{2}$ ]]; then
        if [[ "$edge" == "hi" ]]; then ms=$(date -d "$s-01 + 1 month" +%s); else ms=$(date -d "$s-01" +%s); fi
    elif [[ "$s" =~ ^[0-9]{4}$ ]]; then
        if [[ "$edge" == "hi" ]]; then ms=$(date -d "$s-12-31 + 1 day" +%s); else ms=$(date -d "$s-01-01" +%s); fi
    else
        echo "$PROG: cannot parse date '$s' (use YYYY-MM-DD, YYYY-MM, YYYY or e.g. 30d)" >&2; exit 1
    fi
    echo $((ms * 1000))
}

compare_windows() { # compare_windows "CUR/PREV" → sets CMP_CUR/CMP_PREV (ms pairs)
    local spec="$1" cur prev anchor_lo
    if [[ "$spec" == */* ]]; then cur="${spec%%/*}"; prev="${spec#*/}"; else cur="$spec"; prev="$spec"; fi
    local clo chi
    if [[ "$cur" =~ ^([0-9]+)([dwm])$ ]]; then
        chi=$(date +%s)000
        clo=$(parse_when "$cur" lo "$chi")
    else
        clo=$(parse_when "$cur" lo); chi=$(parse_when "$cur" hi)
    fi
    CMP_CUR="$clo $chi"
    local plo phi
    if [[ "$prev" =~ ^([0-9]+)([dwm])$ ]]; then
        phi="$clo"                                  # chained directly before current
        plo=$(parse_when "$prev" lo "$phi")
    else
        plo=$(parse_when "$prev" lo); phi=$(parse_when "$prev" hi)
    fi
    CMP_PREV="$plo $phi"
}

# ---------------------------------------------------------------------------
# data access (one dump per window, reused by every report)
# ---------------------------------------------------------------------------

sq() { printf '%s' "${1//\'/\'\'}"; }

dump_where() {
    local where="sm.type='assistant' AND sm.time_created >= $1 AND sm.time_created < $2"
    [[ -n "$F_MODEL" ]]    && where+=" AND json_extract(sm.data,'\$.model.id') LIKE '%$(sq "$F_MODEL")%'"
    [[ -n "$F_PROVIDER" ]] && where+=" AND json_extract(sm.data,'\$.model.providerID') LIKE '%$(sq "$F_PROVIDER")%'"
    [[ -n "$F_AGENT" ]]    && where+=" AND json_extract(sm.data,'\$.agent') LIKE '%$(sq "$F_AGENT")%'"
    [[ -n "$F_SESSION" ]]  && where+=" AND sm.session_id LIKE '$(sq "$F_SESSION")%'"
    [[ -n "$F_PROJECT" ]]  && where+=" AND sv.directory LIKE '%$(sq "$F_PROJECT")%'"
    echo "$where"
}

dump_rows() { # dump_rows <lo_ms> <hi_ms> → TSV assistant rows, ts-ordered
    sqlite3 -separator $'\t' "file:$DB?mode=ro" "
        SELECT sm.time_created,
               COALESCE(json_extract(sm.data,'\$.model.providerID'),'unknown'),
               COALESCE(json_extract(sm.data,'\$.model.id'),'unknown'),
               COALESCE(json_extract(sm.data,'\$.agent'),'unknown'),
               sm.session_id,
               COALESCE(sv.directory,''),
               COALESCE(json_extract(sm.data,'\$.tokens.input'),0),
               COALESCE(json_extract(sm.data,'\$.tokens.output'),0),
               COALESCE(json_extract(sm.data,'\$.tokens.reasoning'),0),
               COALESCE(json_extract(sm.data,'\$.tokens.cache.read'),0),
               COALESCE(json_extract(sm.data,'\$.tokens.cache.write'),0),
               COALESCE(json_extract(sm.data,'\$.cost'),0)
        FROM session_message sm
        LEFT JOIN session_v2 sv ON sv.id = sm.session_id
        WHERE $(dump_where "$1" "$2")
        ORDER BY sm.time_created;"
}

dump_prompts() { # dump_prompts <lo_ms> <hi_ms> → TSV user rows, ts-ordered
    local where="sm.type='user' AND sm.time_created >= $1 AND sm.time_created < $2"
    [[ -n "$F_SESSION" ]] && where+=" AND sm.session_id LIKE '$(sq "$F_SESSION")%'"
    [[ -n "$F_PROJECT" ]] && where+=" AND sv.directory LIKE '%$(sq "$F_PROJECT")%'"
    sqlite3 -separator $'\t' "file:$DB?mode=ro" "
        SELECT sm.time_created, sm.session_id, COALESCE(sv.directory,'')
        FROM session_message sm
        LEFT JOIN session_v2 sv ON sv.id = sm.session_id
        WHERE $where
        ORDER BY sm.time_created;"
}

# lazy per-window dump cache: ROWS_FILE/PROMPTS_FILE keyed by "lo hi"
TMP_RATES=""; TMP_KEYS=""; ROWS_FILE=""; PROMPTS_FILE=""
declare -A DUMP_CACHE=()
ensure_data() { # ensure_data <lo_ms> <hi_ms> → sets ROWS_FILE/PROMPTS_FILE
    local key="$1 $2"
    if [[ -n "${DUMP_CACHE[$key]+x}" ]]; then
        ROWS_FILE="${DUMP_CACHE[$key]}"; return 0
    fi
    if [[ -z "$TMP_RATES" ]]; then
        TMP_RATES=$(mktemp /tmp/ocstats.rates.XXXXXX)
        TMP_KEYS=$(mktemp /tmp/ocstats.keys.XXXXXX)
        trap 'rm -f "${TMP_RATES:-}" "${TMP_KEYS:-}" ${DUMP_CACHE[@]:-}' EXIT
    fi
    ROWS_FILE=$(mktemp /tmp/ocstats.rows.XXXXXX)
    PROMPTS_FILE=$(mktemp /tmp/ocstats.prompts.XXXXXX)
    dump_rows "$1" "$2" > "$ROWS_FILE"
    dump_prompts "$1" "$2" > "$PROMPTS_FILE"
    DUMP_CACHE[$key]="$ROWS_FILE"
}

ensure_rates() { # build rates for every distinct provider/model across all windows
    [[ -s "$TMP_RATES" ]] || : > "$TMP_RATES"
    [[ -s "$TMP_KEYS" ]] || : > "$TMP_KEYS"
    local newkeys
    newkeys=$( { for f in "${DUMP_CACHE[@]}"; do
                     [[ -s "$f" ]] && awk -F'\t' '{ print $2 "/" $3 }' "$f"
                 done; } | sort -u | comm -23 - "$TMP_KEYS" )
    if [[ -n "$newkeys" ]]; then
        while IFS= read -r key; do
            printf '%s\t' "$key"
            rates_for "${key%%/*}" "${key#*/}"
        done <<<"$newkeys" >> "$TMP_RATES"
        { sort -u "$TMP_KEYS"; printf '%s\n' "$newkeys"; } | sort -u > "$TMP_KEYS.tmp"
        mv "$TMP_KEYS.tmp" "$TMP_KEYS"
    fi
}

# ---------------------------------------------------------------------------
# aggregation: awk over rates + rows + prompts
#   raw group columns (nd dims then 17 metrics):
#     n prompts sessions in out reason cr cw rep est eff first last mean median p90 tok_total
#   then "##TOTAL" + total row (same shape, empty dims)
# ---------------------------------------------------------------------------

TZOFF=$(( $(date +%z | awk '{ h = substr($0,2,2); m = substr($0,4,2);
                              s = h*3600 + m*60; print (substr($0,1,1) == "-" ? -s : s) }') * 1000 ))

AGG_AWK='
# ---- pure-awk calendar (Howard Hinnant algorithms; int() safe >= 1970) ----
function days_from_civil(y, m, d,  era, yoe, doy, doe) {
    y -= (m <= 2); era = int((y >= 0 ? y : y - 399) / 400); yoe = y - era * 400
    doy = int((153 * (m + (m > 2 ? -3 : 9)) + 2) / 5) + d - 1
    doe = yoe * 365 + int(yoe / 4) - int(yoe / 100) + doy
    return era * 146097 + doe - 719468
}
function civil_from_days(z,  era, doe, yoe, y, doy, mp, cd_d, cd_m, cd_y) {
    z += 719468; era = int((z >= 0 ? z : z - 146096) / 146097); doe = z - era * 146097
    yoe = int((doe - int(doe / 1460) + int(doe / 36524) - int(doe / 146096)) / 365)
    y = yoe + era * 400
    doy = doe - (365 * yoe + int(yoe / 4) - int(yoe / 100))
    mp = int((5 * doy + 2) / 153)
    cd_d = doy - int((153 * mp + 2) / 5) + 1
    cd_m = mp + (mp < 10 ? 3 : -9)
    cd_y = y + (cd_m <= 2)
    return sprintf("%04d-%02d-%02d", cd_y, cd_m, cd_d)
}
function daystr(ms) { return civil_from_days(int((ms + tzo) / 86400000)) }
function monday_of(day,  a, dn, dow) {
    split(day, a, "-")
    dn = days_from_civil(a[1] + 0, a[2] + 0, a[3] + 0)
    dow = (dn + 4) % 7; if (dow < 0) dow += 7
    return civil_from_days(dn - ((dow + 6) % 7))
}
function sort_nums(a, n,  i, j, t) {           # insertion sort ascending
    for (i = 2; i <= n; i++) { t = a[i]; j = i - 1
        while (j >= 1 && a[j] > t) { a[j+1] = a[j]; j-- }
        a[j+1] = t }
}
function pctile(a, n, p,  k, f, c) {
    if (n == 0) return 0
    k = (n - 1) * p / 100; f = int(k); c = int(k + 0.999999) ; if (c >= n) c = n - 1
    if (f == c) return a[f + 1]
    return a[f + 1] * (c - k) + a[c + 1] * (k - f)
}
function dimv(d,  v) {
    v = (d == "provider" ? prov : d == "model" ? mdl : d == "agent" ? agt :
         d == "session" ? sid : d == "project" ? proj : d == "day" ? d0 :
         d == "week" ? wk : d == "month" ? mo : "?")
    return v
}
# ---- rates ----
$1 == "R" { PIN[$2]=$3+0; POUT[$2]=$4+0; PCR[$2]=$5+0; PCW[$2]=$6+0; next }
# ---- assistant usage rows: U ts prov model agent session dir in out reason cr cw cost ----
$1 == "U" {
    ts=$2+0; prov=$3; mdl=$4; agt=$5; sid=$6; dir=$7
    tin=$8+0; tout=$9+0; treas=$10+0; tcr=$11+0; tcw=$12+0; rep=$13+0

    n = split(dir, pa, "/"); proj = (dir == "" ? "(none)" : pa[n])
    d0 = daystr(ts); split(d0, da, "-"); mo = da[1] "-" da[2]; wk = monday_of(d0)

    key = prov "/" mdl
    est = (key in PIN) ? (tin*PIN[key] + tout*POUT[key] + tcr*PCR[key] + tcw*PCW[key]) / 1e6 : 0
    eff = (rep > 0 ? rep : est)
    tot = tin + tout + treas + tcr + tcw

    nd = split(dims, dd, ","); gk = ""
    for (i = 1; i <= nd; i++) gk = gk (i > 1 ? SUBSEP : "") dimv(dd[i])

    N[gk]++; IN[gk]+=tin; OUT[gk]+=tout; REAS[gk]+=treas; CR[gk]+=tcr; CW[gk]+=tcw
    REP[gk]+=rep; EST[gk]+=est; EFF[gk]+=eff; TOK[gk] = TOK[gk] " " tot
    if (!(SESSG[gk SUBSEP sid]++)) SESSN[gk]++
    if (!(gk in FIRST) || ts < FIRST[gk]) FIRST[gk]=ts
    if (!(gk in LAST) || ts > LAST[gk]) LAST[gk]=ts

    TN++; TIN+=tin; TOUT+=tout; TREAS+=treas; TCR+=tcr; TCW+=tcw
    TREP+=rep; TEST_+=est; TEFF+=eff
    TSID[sid]=1
    # per-session assistant timeline for prompt attribution
    STS[sid] = STS[sid] " " ts
    SAT[sid] = SAT[sid] "\002" prov "\001" mdl "\001" agt "\001" proj
    next
}
# ---- user prompt rows: P ts session dir ----
$1 == "P" {
    np_++
    PTS[np_]=$2+0; PSID[np_]=$3; PDIR[np_]=$4
    next
}
END {
    # attribute each prompt to the next assistant message in its session
    for (i = 1; i <= np_; i++) {
        sid = PSID[i]; prov = "unknown"; mdl = "unknown"; agt = "unknown"; proj = "(none)"
        if (sid in STS) {
            cnt = split(STS[sid], ts_, " "); split(SAT[sid], at_, "\002")
            pick = cnt                                   # default: last assistant
            for (j = 1; j <= cnt; j++) if (ts_[j]+0 >= PTS[i]) { pick = j; break }
            split(at_[pick], a_, "\001")
            prov = a_[1]; mdl = a_[2]; agt = a_[3]; proj = a_[4]
        } else if (PDIR[i] != "") {
            nn = split(PDIR[i], pp, "/"); proj = pp[nn]
        }
        d0 = daystr(PTS[i]); split(d0, da, "-"); mo = da[1] "-" da[2]; wk = monday_of(d0)
        nd = split(dims, dd, ","); gk = ""
        for (k = 1; k <= nd; k++) gk = gk (k > 1 ? SUBSEP : "") dimv(dd[k])
        PROM[gk]++
    }
    if (TN == 0) exit 0
    for (k in N) {
        split(k, parts, SUBSEP)
        line = parts[1]
        for (i = 2; i <= nd; i++) line = line "\t" parts[i]
        ntk = split(TOK[k], tk, " ")
        sort_nums(tk, ntk)
        tsum = 0; for (i = 1; i <= ntk; i++) tsum += tk[i]
        printf "%s\t%d\t%d\t%d\t%d\t%d\t%d\t%d\t%d\t%.6f\t%.6f\t%.6f\t%d\t%d\t%.1f\t%.1f\t%.1f\t%d\n", \
               line, N[k], PROM[k]+0, SESSN[k], IN[k], OUT[k], REAS[k], CR[k], CW[k], \
               REP[k], EST[k], EFF[k], FIRST[k], LAST[k], \
               (ntk ? tsum/ntk : 0), pctile(tk, ntk, 50), pctile(tk, ntk, 90), tsum
    }
    tnsess = 0; for (s in TSID) tnsess++
    print "##TOTAL"
    totpfx = ""
    for (i = 1; i <= nd; i++) totpfx = totpfx "\t"
    printf "%s%d\t%d\t%d\t%d\t%d\t%d\t%d\t%d\t%.6f\t%.6f\t%.6f\t0\t0\t0\t0\t0\t0\n", \
           totpfx, TN, np_, tnsess, TIN, TOUT, TREAS, TCR, TCW, TREP, TEST_, TEFF
}'

aggregate() { # aggregate <dims csv> <lo_ms> <hi_ms> → group rows (##TOTAL last)
    ensure_data "$2" "$3"
    ensure_rates
    { awk -F'\t' 'NF >= 6 { print "R\t" $1 "\t" $2 "\t" $3 "\t" $4 "\t" $5 }' "$TMP_RATES"
      awk -F'\t' '{ print "U\t" $0 }' "$ROWS_FILE"
      awk -F'\t' '{ print "P\t" $0 }' "$PROMPTS_FILE"; } \
        | awk -F'\t' -v dims="$1" -v tzo="$TZOFF" "$AGG_AWK"
}

# ---------------------------------------------------------------------------
# shared formatting helpers for group rows
# ---------------------------------------------------------------------------

FMT_FUNCS='
function civil_from_days(z,  era, doe, yoe, y, doy, mp, cd_d, cd_m, cd_y) {
    z += 719468; era = int((z >= 0 ? z : z - 146096) / 146097); doe = z - era * 146097
    yoe = int((doe - int(doe / 1460) + int(doe / 36524) - int(doe / 146096)) / 365)
    y = yoe + era * 400
    doy = doe - (365 * yoe + int(yoe / 4) - int(yoe / 100))
    mp = int((5 * doy + 2) / 153)
    cd_d = doy - int((153 * mp + 2) / 5) + 1
    cd_m = mp + (mp < 10 ? 3 : -9)
    cd_y = y + (cd_m <= 2)
    return sprintf("%04d-%02d-%02d", cd_y, cd_m, cd_d)
}
function commas(x) { s = sprintf("%.0f", x); out = ""
    while (length(s) > 3) { out = "," substr(s, length(s)-2) out; s = substr(s, 1, length(s)-3) }
    return s out }
function money(v) { if (v == 0) return "$0"
                    if (v >= 100) return sprintf("$%.2f", v)
                    if (v >= 1) return sprintf("$%.3f", v)
                    return sprintf("$%.4f", v) }
function hmm(ms,  d, rem, hh, mi) {
    d = civil_from_days(int((ms + tzo) / 86400000)); rem = (ms + tzo) % 86400000
    if (rem < 0) rem += 86400000
    hh = int(rem / 3600000); mi = int((rem % 3600000) / 60000)
    return substr(d, 6) " " sprintf("%02d:%02d", hh, mi) }
function hitpct(cr, tin, cw) { return (cr + tin + cw > 0) ? sprintf("%.1f%%", cr / (cr + tin + cw) * 100) : "0.0%" }'

group_report() { # group_report <title> <dims csv> [lo hi]
    local title="$1" dims_csv="$2" glo="${3:-$LO_MS}" ghi="${4:-$HI_MS}"
    local ndim raw total
    ndim=$(awk -F, '{print NF}' <<<"$dims_csv")
    raw=$(aggregate "$dims_csv" "$glo" "$ghi")
    [[ -z "$raw" ]] && { echo "$PROG: no messages matched the current filters" >&2; exit 1; }
    total=$(awk '/^##TOTAL$/{getline; print}' <<<"$raw")
    raw=$(awk '/^##TOTAL$/{exit} {print}' <<<"$raw")

    # raw cols: dims(1..nd) n prompts sessions in out reason cr cw rep est eff first last mean med p90 toktot
    local e=$((ndim + 11))   # eff column
    local tcol=$((ndim + 18)) # tok_total column
    case "$SORT" in
        tokens)  raw=$(awk -F'\t' -v c="$tcol" '{ print $c "\t" $0 }' <<<"$raw" | sort -t$'\t' -k1,1gr | cut -f2-) ;;
        name)    raw=$(sort -t$'\t' <<<"$raw") ;;
        date)    raw=$(sort -t$'\t' -k$((ndim+12)),$((ndim+12))n <<<"$raw") ;;
        date-desc) raw=$(sort -t$'\t' -k$((ndim+12)),$((ndim+12))nr <<<"$raw") ;;
        msgs)    raw=$(sort -t$'\t' -k$((ndim+1)),$((ndim+1))gr <<<"$raw") ;;
        prompts) raw=$(sort -t$'\t' -k$((ndim+2)),$((ndim+2))gr <<<"$raw") ;;
        sessions) raw=$(sort -t$'\t' -k$((ndim+3)),$((ndim+3))gr <<<"$raw") ;;
        rep)     raw=$(sort -t$'\t' -k$((ndim+9)),$((ndim+9))gr <<<"$raw") ;;
        est)     raw=$(sort -t$'\t' -k$((ndim+10)),$((ndim+10))gr <<<"$raw") ;;
        *)       raw=$(sort -t$'\t' -k"$e,$e"gr <<<"$raw") ;;
    esac
    ((BOTTOMN > 0)) && raw=$(tac <<<"$raw")

    # share denominator over ALL groups (before min-share / limit slicing),
    # matching the python implementation
    local total_eff
    total_eff=$(awk -F'\t' -v c="$e" '{s+=$c} END{printf "%.6f", s+0}' <<<"$raw")

    # --min-share: hide rows below P% of total effective cost
    local hidden=0
    if (( $(awk -v a="$MINSHARE" 'BEGIN{print (a > 0)}') )); then
        local kept
        kept=$(awk -F'\t' -v nd="$ndim" -v eff_col="$e" -v ms="$MINSHARE" \
            -v tot="$(awk -F'\t' -v c="$e" '{s+=$c} END{printf "%.6f", s+0}' <<<"$raw")" '
            { share = ($eff_col / (tot > 0 ? tot : 1)) * 100
              if (share >= ms) print }' <<<"$raw")
        hidden=$(( $(wc -l <<<"$raw") - $(wc -l <<<"$kept") ))
        raw="$kept"
    fi

    local limit=$(( TOPN > 0 ? TOPN : (BOTTOMN > 0 ? BOTTOMN : LIMIT) ))
    local limited=$raw restn=0 nrows
    nrows=$(wc -l <<<"$raw")
    if ((limit > 0 && nrows > limit)); then
        restn=$((nrows - limit)); limited=$(head -n "$limit" <<<"$raw")
    fi

    if ((JSON)); then json_group "$dims_csv" "$limited"; return; fi
    if ((CSV));  then csv_group "$dims_csv" "$raw"; return; fi

    print_pricing_hint

    # --compare: join previous-window groups for delta columns
    local prev_map_file=""
    if [[ -n "$COMPARE" ]]; then
        local plo phi; read -r plo phi <<<"$CMP_PREV"
        prev_map_file=$(mktemp /tmp/ocstats.prev.XXXXXX)
        aggregate "$dims_csv" "$plo" "$phi" | awk '/^##TOTAL$/{exit} {print}' \
            | awk -F'\t' -v nd="$ndim" '{
                  k = $1; for (i = 2; i <= nd; i++) k = k SUBSEP $i
                  print k "\t" $(nd+1) "\t" $(nd+11) }' > "$prev_map_file"
    fi

    local -a hdr=() dimarr=()
    local d
    IFS=',' read -r -a dimarr <<<"$dims_csv"
    for d in "${dimarr[@]}"; do hdr+=("${d^}"); done
    if [[ -n "$METRIC" && "$CMD" == "matrix" ]]; then
        case "$METRIC" in
            tokens) hdr+=("Tokens");; cost) hdr+=("Σ \$");; msgs) hdr+=("Msgs");;
            prompts) hdr+=("Prompts");; in) hdr+=("Tok in");; out) hdr+=("Tok out");;
            cache) hdr+=("Cache hits");; *) hdr+=("$METRIC");;
        esac
    else
        hdr+=("Msgs" "Sessions" "Tok in" "Tok out" "Cache R" "Cache hit" "Rep \$" "Est \$" "Σ \$" "Share")
        ((DETAILS)) && hdr+=("Prompts" "Reasoning" "Cache W" "Mean" "Median" "p90" "First seen" "Last seen")
    fi
    [[ -n "$prev_map_file" ]] && hdr+=("Prev Σ \$" "Δ Σ \$" "Δ msgs")

    local aligns; aligns="$(printf 'l%.0s' $(seq 1 "$ndim"))rrrrrrrrrr"
    ((DETAILS)) && aligns+="rrrrrrrr"
    [[ -n "$prev_map_file" ]] && aligns+="rrr"

    {
        printf '%s\n' "$(IFS=$'\t'; echo "${hdr[*]}")"
        awk -F'\t' -v nd="$ndim" -v teff="$total_eff" -v details="$DETAILS" \
            -v metric="$METRIC" -v matrix="$([[ -n "$METRIC" && "$CMD" == "matrix" ]] && echo 1 || echo 0)" \
            -v prevf="$prev_map_file" -v tzo="$TZOFF" \
            -v B="$BOLD" -v Y="$YELLOW" -v G="$GREEN" -v W="$WHITE" -v D="$DIM" -v R="$RESET" \
            "$FMT_FUNCS"'
            BEGIN { if (prevf != "") while ((getline pl < prevf) > 0) {
                        split(pl, pv, "\t"); PE[pv[1]] = pv[3]; PN[pv[1]] = pv[2] } }
            {
                k = $1; for (i = 2; i <= nd; i++) k = k SUBSEP $i
                tin = $(nd+4); cr = $(nd+7); rep = $(nd+9); est = $(nd+10); eff = $(nd+11)
                line = $1
                for (i = 2; i <= nd; i++) line = line "\t" $i
                if (matrix == "1") {
                    if (metric == "cost") line = line "\t" money(eff)
                    else if (metric == "msgs") line = line "\t" commas($(nd+1))
                    else if (metric == "prompts") line = line "\t" commas($(nd+2))
                    else if (metric == "in") line = line "\t" commas(tin)
                    else if (metric == "out") line = line "\t" commas($(nd+5))
                    else if (metric == "cache") line = line "\t" commas(cr)
                    else line = line "\t" commas($(nd+18))
                    print line; next
                }
                repc = (rep == 0 ? D money(rep) : Y money(rep))
                estc = (est == 0 && rep == 0 ? D "—" : G money(est))
                share = (teff > 0 ? sprintf("%.1f%%", eff / teff * 100) : "0.0%")
                line = line "\t" commas($(nd+1)) "\t" commas($(nd+3)) "\t" commas(tin) "\t" \
                       commas($(nd+5)) "\t" commas(cr) "\t" hitpct(cr, tin, $(nd+8)) "\t" repc "\t" estc "\t" \
                       B W money(eff) R "\t" share
                if (details == "1") {
                    line = line "\t" commas($(nd+2)) "\t" commas($(nd+6)) "\t" commas($(nd+8)) "\t" \
                           commas($(nd+14)) "\t" commas($(nd+15)) "\t" commas($(nd+16)) "\t" \
                           hmm($(nd+12)) "\t" hmm($(nd+13))
                }
                if (prevf != "") {
                    if (k in PE) {
                        de = eff - PE[k]; dn = $(nd+1) - PN[k]
                        line = line "\t" D money(PE[k]) R "\t" \
                               (de >= 0 ? G money(de) : "\033[31m" money(de) "\033[0m") "\t" \
                               (dn >= 0 ? G "+" dn : "\033[31m" dn "\033[0m")
                    } else line = line "\t" D "—" "\t" D "new" "\t" D "+"
                }
                print line
            }' <<<"$limited"
        if ((restn > 0)); then
            local pvextra=0
            [[ -n "$prev_map_file" ]] && pvextra=3
            local padcols=$(( 9 + ndim - 1 + DETAILS * 8 + pvextra ))
            printf '%s' "${DIM}+ $restn more"
            printf '%s' "$(printf '\t%.0s' $(seq 1 "$padcols"))"
            printf '%s\n' "$RESET"
        fi
        awk -F'\t' -v nd="$ndim" -v tzo="$TZOFF" \
            -v B="$BOLD" -v Y="$YELLOW" -v G="$GREEN" -v W="$WHITE" -v R="$RESET" \
            "$FMT_FUNCS"'
            {
                tin = $(nd+4); cr = $(nd+7)
                names = "TOTAL"
                for (i = 2; i <= nd; i++) names = names "\t"
                print names "\t" B commas($(nd+1)) R "\t" B commas($(nd+3)) R "\t" \
                      B commas(tin) R "\t" B commas($(nd+5)) R "\t" B commas(cr) R "\t" \
                      B hitpct(cr, tin, $(nd+8)) R "\t" B Y money($(nd+9)) R "\t" \
                      B G money($(nd+10)) R "\t" B W money($(nd+11)) R "\t" B "100%" R
            }' <<<"$total"
    } | render_tsv "$title" "$aligns" 1 \
       "$(printf 'Σ = reported where > 0, else estimated · — = no pricing data available%s' \
          "$([[ $hidden -gt 0 ]] && printf ' · %d group(s) below %s%% share hidden' "$hidden" "$MINSHARE")")"
    [[ -n "$prev_map_file" ]] && rm -f "$prev_map_file"
    echo
}

csv_group() { # csv_group <dims csv> <raw rows> → CSV with raw numbers
    local dims_csv="$1" raw="$2" ndim
    ndim=$(awk -F, '{print NF}' <<<"$dims_csv")
    local -a dimarr=(); IFS=',' read -r -a dimarr <<<"$dims_csv"
    {
        printf '%s\n' "$(IFS=','; echo "${dimarr[*]}"),n,prompts,sessions,in,out,reason,cr,cw,cost_rep,cost_est,cost_eff,first_ts,last_ts,tok_mean,tok_median,tok_p90,tok_total"
        awk -F'\t' -v nd="$ndim" '
            function cq(s) { gsub(/"/, "\"\"", s); return "\"" s "\"" }
            {
                out = ""
                for (i = 1; i <= nd; i++) out = out (i > 1 ? "," : "") cq($i)
                for (i = nd + 1; i <= nd + 18; i++) out = out "," $(i)
                print out
            }' <<<"$raw"
    }
}

json_group() { # json_group <dims csv> <limited raw rows>
    local dims_csv="$1" rows="$2" ndim
    ndim=$(awk -F, '{print NF}' <<<"$dims_csv")
    [[ -z "$rows" ]] && { echo '{"rows":[]}'; return; }
    printf '%s\n' "$rows" | awk -F'\t' -v dims="$dims_csv" -v nd="$ndim" '
        BEGIN { printf "{\"rows\":[" }
        {
            if (NR > 1) printf ","
            split(dims, dn, ",")
            for (i = 1; i <= nd; i++) printf "%s\"%s\": \"%s\"", (i==1?"{":", "), dn[i], $i
            printf ", \"n\": %d, \"prompts\": %d, \"sessions\": %d", $(nd+1), $(nd+2), $(nd+3)
            printf ", \"in\": %d, \"out\": %d, \"reason\": %d, \"cr\": %d, \"cw\": %d", \
                   $(nd+4), $(nd+5), $(nd+6), $(nd+7), $(nd+8)
            printf ", \"cost_rep\": %s, \"cost_est\": %s, \"cost_eff\": %s", $(nd+9), $(nd+10), $(nd+11)
            printf ", \"first_ts\": %s, \"last_ts\": %s", $(nd+12), $(nd+13)
            printf ", \"tok_mean\": %s, \"tok_median\": %s, \"tok_p90\": %s, \"tok_total\": %s}", \
                   $(nd+14), $(nd+15), $(nd+16), $(nd+17)
        } END { print "]}" }' | jq .
}

# ---------------------------------------------------------------------------
# summary
# ---------------------------------------------------------------------------

cmd_summary() {
    local day_raw
    day_raw=$(aggregate "day" "$LO_MS" "$HI_MS")
    [[ -z "$day_raw" ]] && { echo "$PROG: no messages matched the current filters" >&2; exit 1; }
    local total
    total=$(awk '/^##TOTAL$/{getline; print}' <<<"$day_raw")
    total=$(cut -f2- <<<"$total")   # drop the leading empty dim field
    # fields: n prompts sess in out reason cr cw rep est eff first last mean med p90 toktot
    IFS=$'\t' read -r n prompts sess tin tout treas tcr tcw rep est eff _rest <<<"$total"

    if ((JSON)); then
        local jq_extra="" comp_json=""
        local cr_pct_tok mean med p90 days_lo days_hi
        cr_pct_tok=$(awk -v cr="$tcr" -v i="$tin" -v w="$tcw" 'BEGIN{ if (cr+i+w > 0) printf "%.6f", cr/(cr+i+w); else print "0" }')
        days=$(awk '/^##TOTAL$/{exit} {n++} END{print n+0}' <<<"$day_raw")
        days_lo=$(sort -t$'\t' -k1,1 <<<"$day_raw" | awk -F'\t' '!/^##/ && $1 != ""{print $1; exit}')
        days_hi=$(awk -F'\t' '!/^##/ && $1 != ""{if ($1 > d) d=$1} END{print d}' <<<"$day_raw")
        mean=$(awk -F'\t' '{printf "%.6f", $14}' <<<"$total")
        med=$(awk  -F'\t' '{printf "%.6f", $15}' <<<"$total")
        p90=$(awk  -F'\t' '{printf "%.6f", $16}' <<<"$total")
        local total_tok=$((tin + tout + treas + tcr + tcw))
        if [[ -n "$MONTHLY_BUDGET" ]]; then
            local mtd
            mtd=$(awk -F'\t' -v m="$(date +%Y-%m)" '$1 == m {print $11; exit}' \
                  < <(awk '/^##TOTAL$/{exit} {print}' <<<"$(aggregate "month" "$LO_MS" "$HI_MS")"))
            mtd=${mtd:-0}
            local now dom dim projected
            now=$(date +%Y-%m); dom=$(date +%-d)
            dim=$(date -d "$(date +%Y-%m-01) + 1 month - 1 day" +%-d)
            projected=$(awk -v m="$mtd" -v e="$dom" -v x="$dim" 'BEGIN{printf "%.6f", (e>0 ? m/e*x : 0)}')
            jq_extra=", \"budget\": {\"budget\": $MONTHLY_BUDGET, \"spent_mtd\": $mtd, \"projected_month_end\": $projected, \"remaining\": $(awk -v b="$MONTHLY_BUDGET" -v m="$mtd" 'BEGIN{printf "%.6f", b-m}'), \"used_pct\": $(awk -v b="$MONTHLY_BUDGET" -v m="$mtd" 'BEGIN{printf "%.4f", (b>0? m/b*100:0)}'), \"on_pace\": $(awk -v p="$projected" -v b="$MONTHLY_BUDGET" 'BEGIN{print (p<=b)?"true":"false"}')}"
        fi
        printf '{"totals": {"prompts": %s, "messages": %s, "sessions": %s, "active_days": %s, "tokens": {"input": %s, "output": %s, "reasoning": %s, "cache_read": %s, "cache_write": %s, "total": %s}, "cache_ratio": %s, "tokens_per_message": {"mean": %s, "median": %s, "p90": %s}, "cost": {"reported": %s, "estimated": %s, "effective": %s}, "period": {"from": "%s", "to": "%s"}%s}}\n' \
            "$prompts" "$n" "$sess" "$days" "$tin" "$tout" "$treas" "$tcr" "$tcw" "$total_tok" \
            "$cr_pct_tok" "$mean" "$med" "$p90" \
            "$rep" "$est" "$eff" "$days_lo" "$days_hi" "$jq_extra" | jq .
        return
    fi
    if ((CSV)); then
        printf '%s\n' "prompts,messages,sessions,input,output,reasoning,cache_read,cache_write,tokens_total,cost_reported,cost_estimated,cost_effective"
        printf '%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s\n' \
            "$prompts" "$n" "$sess" "$tin" "$tout" "$treas" "$tcr" "$tcw" \
            "$(awk -v a="$tin" -v b="$tout" -v c="$treas" -v d="$tcr" -v e="$tcw" 'BEGIN{printf "%d", a+b+c+d+e}')" \
            "$rep" "$est" "$eff"
        return
    fi

    print_pricing_hint
    local days nproj cr_pct first_day last_day
    days=$(awk '/^##TOTAL$/{exit} {n++} END{print n+0}' <<<"$day_raw")
    nproj=$(aggregate "project" "$LO_MS" "$HI_MS" | awk '/^##TOTAL$/{exit} {n++} END{print n+0}')
    cr_pct=$(awk -v cr="$tcr" -v i="$tin" -v w="$tcw" 'BEGIN{ if (cr+i+w > 0) printf "%.1f%%", cr/(cr+i+w)*100; else print "0.0%" }')
    first_day=$(sort -t$'\t' -k1,1 <<<"$day_raw" | awk -F'\t' '!/^##/ && $1 != ""{print $1; exit}')
    last_day=$(awk -F'\t' '!/^##/ && $1 != ""{if ($1 > d) d=$1} END{print d}' <<<"$day_raw")

    echo
    printf '  %s%s%s%s  %s%s → %s%s\n' "$BOLD" "$CYAN" "OpenCode Usage Summary" "$RESET" "$DIM" \
           "$first_day" "$last_day" "$RESET"
    echo
    {
        printf '%s\n' $'Prompts\tMessages\tSessions\tActive days\tTok input\tTok output\tReasoning\tCache hits\tCache writes\tTok total\tCache hit'
        printf '%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n' \
            "$(group_thousands "$prompts")" "$(group_thousands "$n")" "$(group_thousands "$sess")" \
            "$(group_thousands "$days")" "$(group_thousands "$tin")" "$(group_thousands "$tout")" \
            "$(group_thousands "$treas")" "$(group_thousands "$tcr")" "$(group_thousands "$tcw")" \
            "$(group_thousands "$((tin + tout + treas + tcr + tcw))")" "$cr_pct"
    } | render_tsv "" "rrrrrrrrrrr" 0
    echo
    {
        printf '%s\n' $'Reported\tEstimated\tEffective Σ\tMedian tok/msg\tp90 tok/msg'
        printf '%s\t%s\t%s\t%s\t%s\n' \
            "$(money_f "$rep")" "$(money_f "$est")" "$(money_f "$eff")" \
            "$(group_thousands "$(awk -F'\t' '{print $15}' <<<"$total" | awk '{printf "%.0f", $1}')")" \
            "$(group_thousands "$(awk -F'\t' '{print $16}' <<<"$total" | awk '{printf "%.0f", $1}')")"
    } | render_tsv "Cost" "rrrrr" 0 "Σ effective = reported where reported > 0, else estimated"
    echo

    if [[ -n "$MONTHLY_BUDGET" ]]; then
        local mtd dom dim projected remaining used frac verdict
        mtd=$(awk -F'\t' -v m="$(date +%Y-%m)" '$1 == m {print $11; exit}' \
              < <(awk '/^##TOTAL$/{exit} {print}' <<<"$(aggregate "month" "$LO_MS" "$HI_MS")"))
        mtd=${mtd:-0}
        dom=$(date +%-d)
        dim=$(date -d "$(date +%Y-%m-01) + 1 month - 1 day" +%-d)
        projected=$(awk -v m="$mtd" -v e="$dom" -v x="$dim" 'BEGIN{printf "%.3f", (e>0 ? m/e*x : 0)}')
        remaining=$(awk -v b="$MONTHLY_BUDGET" -v m="$mtd" 'BEGIN{printf "%.3f", b-m}')
        used=$(awk -v b="$MONTHLY_BUDGET" -v m="$mtd" 'BEGIN{printf "%.1f", (b>0? m/b*100:0)}')
        frac=$(awk -v b="$MONTHLY_BUDGET" -v m="$mtd" 'BEGIN{f=(b>0? m/b:0); if(f>1)f=1; if(f<0)f=0; print f}')
        verdict=$(awk -v p="$projected" -v b="$MONTHLY_BUDGET" 'BEGIN{print (p<=b) ? "UNDER PACE" : "OVER PACE"}')
        {
            printf '%s\n' $'Budget\tSpent (MTD)\tProjected\tRemaining\tUsed\tPace'
            printf '%s\t%s\t%s\t%s\t%s\t%s %s\n' \
                "$(money_f "$MONTHLY_BUDGET")" "$(money_f "$mtd")" "$(money_f "$projected")" \
                "$(money_f "$remaining")" "${used}%" \
                "${CYAN}$(bar_str "$frac" 12)${RESET}" \
                "$([[ "$verdict" == "UNDER PACE" ]] && echo "${GREEN}" || echo "${RED}")${verdict}${RESET}"
        } | render_tsv "Budget" "rrrrrl" 0
        echo
    fi

    if [[ -n "$COMPARE" ]]; then
        local plo phi p_raw p_total p_n p_prompts p_tin p_tout p_treas p_tcr p_tcw p_rep p_est p_eff
        read -r plo phi <<<"$CMP_PREV"
        p_raw=$(aggregate "day" "$plo" "$phi")
        if [[ -z "$p_raw" ]]; then
            echo "  ${DIM}no usage in the previous comparison window${RESET}"
        else
            p_total=$(awk '/^##TOTAL$/{getline; print}' <<<"$p_raw" | cut -f2-)
            IFS=$'\t' read -r p_n p_prompts p_sess p_tin p_tout p_treas p_tcr p_tcw p_rep p_est p_eff _rest <<<"$p_total"
            local p_tok
            p_tok=$((p_tin + p_tout + p_treas + p_tcr + p_tcw))
            {
                printf '%s\n' $'Period\tMsgs\tTokens\tΣ $\tΔ msgs\tΔ tokens\tΔ Σ $'
                printf '%s\t%s\t%s\t%s\t\t\t\n' "$DIM"previous"$RESET" "$(group_thousands "$p_n")" \
                    "$(group_thousands "$p_tok")" \
                    "$DIM$(money_f "$p_eff")$RESET"
                printf '%s\t%s\t%s\t%s\t%s\t%s\t%s\n' "${BOLD}current${RESET}" "$(group_thousands "$n")" \
                    "$(group_thousands "$((tin + tout + treas + tcr + tcw))")" "$(money_f "$eff")" \
                    "$(awk -v a="$n" -v b="$p_n" 'BEGIN{d=a-b; printf "%s%d", (d>=0?"+":""), d}')" \
                    "$(awk -v a=$((tin + tout + treas + tcr + tcw)) -v b="$p_tok" 'BEGIN{printf "%+d", a-b}')" \
                    "$(awk -v a="$eff" -v b="$p_eff" 'BEGIN{d=a-b; printf "%s%s", (d>=0?"+":""), (d>=100?sprintf("%.2f",d):sprintf("%.3f",d))}')"
            } | render_tsv "Period comparison" "lrrrrrr" 0
            echo
        fi
    fi

    # top models
    local model_raw
    model_raw=$(aggregate "provider,model" "$LO_MS" "$HI_MS" | awk '/^##TOTAL$/{exit} {print}' \
                | sort -t$'\t' -k11,11gr | awk 'NR <= 3')
    {
        printf '%s\n' $'Provider\tModel\tMsgs\tPrompts\tTok in\tRep $\tEst $\tΣ $'
        local p m mn pr se i2 o2 r2 c2 cw2 r3 e2 f2 rest2
        while IFS=$'\t' read -r p m mn pr se i2 o2 r2 c2 cw2 r3 e2 f2 rest2; do
            printf '%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n' "$p" "$m" \
                "$(group_thousands "$mn")" "$(group_thousands "$pr")" "$(group_thousands "$i2")" \
                "$(money_f "$r3")" "$(money_f "$e2")" "$(money_f "$f2")"
        done <<<"$model_raw"
    } | render_tsv "Top models" "llrrrrrr" 0
    echo

    # daily trend sparkline of effective cost
    local spark_vals
    spark_vals=$(sort -t$'\t' -k1,1 <<<"$day_raw" | awk -F'\t' '!/^##/{printf "%.6f ", $11}')
    if [[ -n "$spark_vals" ]]; then
        echo "  ${BOLD}Daily cost trend${RESET}  ${DIM}($first_day → $last_day, $days active days)${RESET}"
        printf '  %s\n' "$(spark $spark_vals)"
    fi
    echo
}

# ---------------------------------------------------------------------------
# pivot
# ---------------------------------------------------------------------------

PIVOT_METRIC_COL() { # PIVOT_METRIC_COL <metric> → raw col offset (after nd dims)
    case "$1" in
        tokens)  echo 17 ;;
        cost)    echo 11 ;;
        msgs)    echo 1 ;;
        prompts) echo 2 ;;
        in)      echo 4 ;;
        out)     echo 5 ;;
        cache)   echo 7 ;;
        *)       echo "" ;;
    esac
}

cmd_pivot() {
    local rowdim="$PV_ROWS" coldim="$PV_COLS" metric="${METRIC:-tokens}"
    local -a dims_list=(); local d bad=0
    for d in "$rowdim" "$coldim"; do
        case "$d" in
            model|provider|agent|project|session|day|week|month) : ;;
            *) echo "$PROG: --rows/--cols must be one of: model provider agent project session day week month" >&2; exit 1 ;;
        esac
    done
    [[ "$rowdim" == "$coldim" ]] && { echo "$PROG: --rows and --cols must differ" >&2; exit 1; }
    local mcol
    mcol=$(PIVOT_METRIC_COL "$metric")
    [[ -z "$mcol" ]] && { echo "$PROG: --metric must be one of: tokens in out cache msgs prompts cost" >&2; exit 1; }

    ensure_data "$LO_MS" "$HI_MS"
    # auto-bucket wide day/week column ranges
    local eff_coldim="$coldim" ndist
    ndist=$(awk -F'\t' -v tzo="$TZOFF" '
        BEGIN { split("", seen) }
        { ms = $1 + 0
          z = int((ms + tzo) / 86400000) + 719468
          era = int(z / 146097); doe = z - era * 146097
          yoe = int((doe - int(doe/1460) + int(doe/36524) - int(doe/146096)) / 365)
          y = yoe + era * 400; doy = doe - (365*yoe + int(yoe/4) - int(yoe/100))
          mp = int((5 * doy + 2) / 153); dd = doy - int((153*mp+2)/5) + 1
          mo = mp + (mp < 10 ? 3 : -9); y += (mo <= 2)
          print y "-" ((mo < 10) ? "0" mo : mo) }' "$ROWS_FILE" | sort -u | wc -l)
    if [[ "$coldim" == "day" ]]; then
        (( ndist > 14 )) && eff_coldim="week"
        if [[ "$eff_coldim" == "week" ]]; then
            local nweeks
            nweeks=$(aggregate "week" "$LO_MS" "$HI_MS" | awk '/^##TOTAL$/{exit} {n++} END{print n+0}')
            (( nweeks > 14 )) && eff_coldim="month"
        fi
    elif [[ "$coldim" == "week" ]]; then
        (( ndist > 14 )) && eff_coldim="month"
    fi

    local raw
    raw=$(aggregate "$rowdim,$eff_coldim" "$LO_MS" "$HI_MS" | awk '/^##TOTAL$/{exit} {print}')
    [[ -z "$raw" ]] && { echo "$PROG: no messages matched the current filters" >&2; exit 1; }
    # raw nd=2: rowval(1) colval(2) n prompts sess in out reason cr cw rep est eff ... toktot(19)

    local grand
    grand=$(awk -F'\t' -v c="$((mcol + 2))" '{ s += $c } END { printf "%.6f", s + 0 }' <<<"$raw")

    if ((JSON)); then
        awk -F'\t' -v rd="$rowdim" -v cd="$eff_coldim" -v c="$((mcol + 2))" -v g="$grand" '
            BEGIN { printf "{\"rows\": [" }
            { if (NR > 1) printf ", "
              printf "{\"%s\": \"%s\", \"%s\": \"%s\", \"value\": %s, \"share\": %.6f}", \
                     rd, $1, cd, $2, $c, (g > 0 ? $c / g : 0) }
            END { print " ]}" }' <<<"$raw" | jq .
        return
    fi
    if ((CSV)); then
        printf '%s,%s,%s\n' "$rowdim" "$eff_coldim" "$metric"
        awk -F'\t' -v c="$((mcol + 2))" '
            function cq(s) { gsub(/"/, "\"\"", s); return "\"" s "\"" }
            { print cq($1) "," cq($2) "," $c }' <<<"$raw"
        return
    fi

    print_pricing_hint
    echo
    local label
    case "$metric" in
        tokens) label="Tokens";; cost) label="Σ \$";; msgs) label="Msgs";;
        prompts) label="Prompts";; in) label="Tok in";; out) label="Tok out";; cache) label="Cache hits";;
    esac
    if ((RENDER_MD)); then
        printf '### %s — %s × %s\n\n' "$label" "$rowdim" "$eff_coldim"
    else
        printf '  %s%s%s%s  %s%s × %s%s\n' "$BOLD" "$CYAN" "$label" "$RESET" "$BOLD" \
               "$rowdim" "$eff_coldim" "$RESET"
    fi

    # single-pass grid builder: TSV → complete table body + footer on stdout
    local grid_tsv
    grid_tsv=$(awk -F'\t' -v nd=2 -v mc="$((mcol + 2))" -v rd="$rowdim" -v cd="$eff_coldim" \
        -v B="$BOLD" -v D="$DIM" -v G="$GREEN" -v W="$WHITE" -v R="$RESET" '
        function label(k) {
            if ((cd == "day" || cd == "week") && k ~ /^[0-9]{4}-[0-9]{2}-[0-9]{2}$/) return substr(k, 6)
            if ((cd == "week" || cd == "month") && k ~ /^[0-9]{4}-[0-9]{2}$/) return substr(k, 3)
            return k }
        function fmt(v, heat,  s, q, g2) {
            if (v == 0) return (heat ? "0" : B "0" R)
            if (VFMT == "money") {
                if (v >= 100) s = sprintf("$%.2f", v)
                else if (v >= 1) s = sprintf("$%.3f", v)
                else s = sprintf("$%.4f", v)
            } else {
                s = sprintf("%.0f", v); g2 = ""
                while (length(s) > 3) { g2 = "," substr(s, length(s)-2) g2; s = substr(s, 1, length(s)-3) }
                s = s g2 }
            if (heat && MX > 0) {
                q = v / MX
                if (q >= 0.75) s = B W s R
                else if (q >= 0.5) s = W s R
                else if (q >= 0.25) s = G s R
                else s = D s R }
            if (!heat) s = B s R
            return s }
        BEGIN { VFMT = (MC_NAME == "cost" ? "money" : "tok") }
        { VAL[$1 SUBSEP $2] = $mc + 0
          if (!($2 in ColSeen)) { ColSeen[$2] = 1; NC++; ColKeys[NC] = $2 }
          RowTot[$1] += $mc + 0
          ColTot[$2] += $mc + 0
          if ($mc + 0 > MX) MX = $mc + 0
          GRAND += $mc + 0
          if (!($1 in RowSeen)) { RowSeen[$1] = 1; NRK++; RowKeys[NRK] = $1 } }
        END {
            # sort columns ascending by key
            for (i = 1; i <= NC; i++) for (j = i + 1; j <= NC; j++)
                if (ColKeys[j] < ColKeys[i]) { t = ColKeys[i]; ColKeys[i] = ColKeys[j]; ColKeys[j] = t }
            # sort rows descending by total
            for (i = 1; i <= NRK; i++) for (j = i + 1; j <= NRK; j++)
                if (RowTot[RowKeys[j]] > RowTot[RowKeys[i]]) { t = RowKeys[i]; RowKeys[i] = RowKeys[j]; RowKeys[j] = t }
            # header
            line = rd
            for (i = 1; i <= NC; i++) line = line "\t" label(ColKeys[i])
            print line "\t" "Total"
            # rows
            for (r = 1; r <= NRK; r++) {
                rk = RowKeys[r]
                line = rk
                for (i = 1; i <= NC; i++) {
                    v = VAL[rk SUBSEP ColKeys[i]] + 0
                    line = line "\t" fmt(v, 1) }
                print line "\t" fmt(RowTot[rk], 0) }
            # footer
            line = "TOTAL"
            for (i = 1; i <= NC; i++) line = line "\t" fmt(ColTot[ColKeys[i]], 0)
            print line "\t" fmt(GRAND, 0) }' MC_NAME="$metric" <<<"$raw")

    local ncols_grid
    ncols_grid=$(head -1 <<<"$grid_tsv" | awk -F'\t' '{print NF}')
    printf '%s\n' "$grid_tsv" | render_tsv "" "l$(printf 'r%.0s' $(seq 1 "$ncols_grid"))" 1 \
       "heat: dim <25%% <50%% <75%% ≤max · metric: $metric"
    echo
}



# ---------------------------------------------------------------------------
# prices
# ---------------------------------------------------------------------------

cmd_prices() {
    ((REFRESH)) && refresh_pricing
    ensure_data "$LO_MS" "$HI_MS"
    local key p m out pin pout pcr pcw src
    if ((JSON)); then
        {
            printf '{\n  "rows": [\n'
            local first=1
            while IFS= read -r key; do
                p="${key%%/*}"; m="${key#*/}"
                out=$(rates_for "$p" "$m")
                IFS=$'\t' read -r pin pout pcr pcw src <<<"$out"
                ((first)) || printf ',\n'
                first=0
                printf '    {"provider": %s, "model": %s, "input": %s, "output": %s, "cache_read": %s, "cache_write": %s, "source": %s}' \
                    "$(jq -Rn --arg v "$p" '$v')" "$(jq -Rn --arg v "$m" '$v')" \
                    "$pin" "$pout" "$pcr" "$pcw" "$(jq -Rn --arg v "$src" '$v')"
            done < <(awk -F'\t' '{ print $2 "/" $3 }' "$ROWS_FILE" | sort -u)
            printf '\n  ]\n}\n'
        } | jq .
        return
    fi
    if ((CSV)); then
        printf '%s\n' "provider,model,input,output,cache_read,cache_write,source"
        while IFS= read -r key; do
            p="${key%%/*}"; m="${key#*/}"
            IFS=$'\t' read -r pin pout pcr pcw src <<<"$(rates_for "$p" "$m")"
            printf '"%s","%s",%s,%s,%s,%s,"%s"\n' "$p" "$m" "$pin" "$pout" "$pcr" "$pcw" "$src"
        done < <(awk -F'\t' '{ print $2 "/" $3 }' "$ROWS_FILE" | sort -u)
        return
    fi
    {
        printf '%s\n' $'Provider\tModel\tIn\tOut\tCache R\tCache W\tSource'
        while IFS= read -r key; do
            p="${key%%/*}"; m="${key#*/}"
            IFS=$'\t' read -r pin pout pcr pcw src <<<"$(rates_for "$p" "$m")"
            printf '%s\t%s\t%s\t%s\t%s\t%s\t%s\n' "$p" "$m" "$pin" "$pout" "$pcr" "$pcw" "$src"
        done < <(awk -F'\t' '{ print $2 "/" $3 }' "$ROWS_FILE" | sort -u)
    } | render_tsv "Pricing table  ·  \$ per 1M tokens" "llrrrrl" 0 \
        "refresh with: $PROG prices --refresh · overrides: $OVERRIDES_FILE"
    echo
}

# ---------------------------------------------------------------------------
# argument parsing
# ---------------------------------------------------------------------------

COMMANDS="summary models providers agents projects sessions daily weekly monthly matrix pivot prices"

while [[ $# -gt 0 ]]; do
    case "$1" in
        summary|models|providers|agents|projects|sessions|daily|weekly|monthly|matrix|pivot|prices)
            CMD="$1" ;;
        --since)    SINCE="$2"; shift ;;
        --until)    UNTIL="$2"; shift ;;
        --model)    F_MODEL="$2"; shift ;;
        --provider) F_PROVIDER="$2"; shift ;;
        --agent)    F_AGENT="$2"; shift ;;
        --project)  F_PROJECT="$2"; shift ;;
        --session)  F_SESSION="$2"; shift ;;
        --limit)    LIMIT="$2"; shift ;;
        --sort)     SORT="$2"; shift ;;
        --by)       MATRIX_BY="$2"; shift ;;
        --rows)     PV_ROWS="$2"; shift ;;
        --cols)     PV_COLS="$2"; shift ;;
        --metric)   METRIC="$2"; shift ;;
        --json)     JSON=1 ;;
        --csv)      CSV=1 ;;
        --md)       MD=1 ;;
        --details)  DETAILS=1 ;;
        --top)      TOPN="$2"; shift ;;
        --bottom)   BOTTOMN="$2"; shift ;;
        --min-share) MINSHARE="$2"; shift ;;
        --monthly-budget) MONTHLY_BUDGET="$2"; shift ;;
        --compare)  COMPARE="$2"; shift ;;
        --no-color) NO_COLOR_OUT=1 ;;
        --refresh)  REFRESH=1 ;;
        --width)    shift ;; # accepted for parity; layout adapts automatically
        -h|--help)
            sed -n '/^# aggregation examples:/,/^#   (comma-separate/p' "$0" | sed 's/^# \{0,1\}//'
            echo
            echo "commands: $COMMANDS"
            echo "filters: --since --until --model --provider --agent --project --session"
            echo "output:  --json --csv --md --details --limit N --top N --bottom N --min-share P"
            echo "         --sort cost|rep|est|tokens|msgs|sessions|name|date|date-desc --no-color"
            echo "matrix:  --by provider,model,day,week,month,agent,project,session --metric M"
            echo "pivot:   --rows DIM --cols DIM --metric tokens|in|out|cache|msgs|prompts|cost"
            echo "extra:   --monthly-budget AMOUNT --compare CUR/PREV (e.g. 7d/7d) --refresh"
            exit 0 ;;
        --version) echo "$PROG $VERSION"; exit 0 ;;
        *) echo "$PROG: unknown argument '$1'" >&2; exit 1 ;;
    esac
    shift
done

((MD)) && { RENDER_MD=1; }
apply_color_switch
[[ -f "$DB" ]] || { echo "$PROG: database not found: $DB (set OCSTATS_DB to override)" >&2; exit 1; }
[[ "$LIMIT" == "0" ]] && LIMIT=1000000
[[ -n "$SINCE" ]] && LO_MS=$(parse_when "$SINCE" lo)
[[ -n "$UNTIL" ]] && HI_MS=$(parse_when "$UNTIL" hi)
[[ -n "$COMPARE" ]] && { compare_windows "$COMPARE"; read -r LO_MS HI_MS <<<"$CMP_CUR"; }
((TOPN > 0 && BOTTOMN > 0)) && { echo "$PROG: --top and --bottom are mutually exclusive" >&2; exit 1; }

declare -A DUMP_CACHE=()

case "$CMD" in
    summary)   cmd_summary ;;
    models)    SORT="${SORT:-cost}"; group_report "Usage by model" "provider,model" ;;
    providers) SORT="${SORT:-cost}"; group_report "Usage by provider" "provider" ;;
    agents)    SORT="${SORT:-cost}"; group_report "Usage by agent" "agent" ;;
    projects)  SORT="${SORT:-cost}"; group_report "Usage by project" "project" ;;
    sessions)  SORT="${SORT:-cost}"; group_report "Sessions" "session" ;;
    daily)     SORT="${SORT:-date-desc}"; group_report "Usage by day" "day" ;;
    weekly)    SORT="${SORT:-date-desc}"; group_report "Usage by week" "week" ;;
    monthly)   SORT="${SORT:-date-desc}"; group_report "Usage by month" "month" ;;
    pivot)     cmd_pivot ;;
    matrix)
        [[ -z "$MATRIX_BY" ]] && { echo "$PROG: matrix requires --by provider,model,…" >&2; exit 1; }
        matarr=()
        IFS=',' read -r -a matarr <<<"$MATRIX_BY"
        for d in "${matarr[@]}"; do
            case "$d" in
                model|provider|agent|project|session|day|week|month) : ;;
                *) echo "$PROG: unknown dimension '$d' (choose from: model provider agent project session day week month)" >&2; exit 1 ;;
            esac
        done
        SORT="${SORT:-cost}"
        group_report "Matrix by ${MATRIX_BY//,/ × }" "$MATRIX_BY" ;;
    prices)    cmd_prices ;;
    *) echo "$PROG: unknown command '$CMD'" >&2; exit 1 ;;
esac
