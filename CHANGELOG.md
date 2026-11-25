# Changelog

All notable changes to this project are documented in this file.
The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

## [1.2.3] - 2026-11-25

### Added
- **Total token column**: `daily`, `weekly`, `monthly` and every group table
  (`models`, `providers`, `agents`, `projects`, `sessions`, `matrix`, …) now
  show a **Tokens** column alongside the input/output/cache breakdown.
- **Speed (Tok/s)**: group tables now show output + reasoning tokens divided by
  the model's streaming window (`time.firstToken` → `time.streamed`) — the
  same definition OpenCode uses. Rows without streaming timestamps render
  `—` but still count toward token and cost totals.
- `--sort speed` for any group table, and a `tok_per_sec` field in `--json` /
  `--csv` output across Python, Bash and Go.
- The synthetic fixture now emits streaming timestamps, and the test suite
  checks daily token reconciliation plus cross-implementation Tok/s parity.

### Fixed
- Bash `--sort tokens` and the focused token metric read the correct raw
  column (off-by-one), and CSV output no longer appends an empty field.

## [1.2.2] - 2026-11-15

### Changed
- **Session tables include `Project` column**: `ocstats sessions`, `matrix --by session`, and `pivot --rows session` now always display the `Project` column right alongside `Session` across Python, Bash, and Go (and pin `Project` at top priority so it is never dropped on narrower terminals).
- **Default ordering is `date-desc`**: Output tables across commands now order by `date-desc` (most recent activity first) by default; use `--sort cost` (or `tokens`, `msgs`, `name`, `date`) to sort by other keys.

## [1.2.1] - 2026-11-04

### Changed
- **SQLite index routing & query optimizations** (Python, Bash, Go):
  - Route `--session` and `--project` queries through `session_v2` and
    `session_message_session_type_seq_idx (session_id, type, seq)` (~70× faster
    on filtered runs).
  - Route bounded `--since`/`--until` queries through
    `session_message_time_created_idx` and use sequential scans for unbounded
    all-time runs instead of random B-tree lookups.
  - Extract scalar usage fields inside SQLite C (`json_extract`) in a single
    pass over `session_message` instead of copying and parsing ~243 MB of
    `"content"` arrays in Python/Go.
  - Preload `session_v2` metadata, memoize per-row cost estimates and
    calendar-day date decompositions, and enable read-only SQLite `mmap_size`
    and memory temp-store PRAGMAs.
- **Bash (`ocstats.sh`) speedups (~15–35× faster)**:
  - Resolve all distinct `provider/model` pricing keys in a single batched
    `jq` invocation instead of spawning per-model `jq` subprocesses.
  - Replace per-cell subshells in `render_tsv` with a single-pass `awk`
    boxed-table renderer.

## [1.2.0] - 2026-10-20

### Added
- `heatmap` (alias `calendar`) command: 7×W calendar heatmap with quartile
  intensity blocks (`·░▒▓█`), current & longest streaks, busiest weekday,
  `--by model|provider|agent|project` dominant-series coloring (and ASCII
  symbols when colorless), and `--split` stacked mini-calendars per top
  series.
- `chart` command: stacked horizontal ASCII/Unicode time-series bar chart
  over `--period day|week|month|hour|dow` segmented by `--by DIM` using
  distinct block shades (`█▓▒░▪`) + ANSI colors.
- `punchcard` command: 7×24 (`Mon`–`Sun` × `00`–`23`h) circadian activity
  grid with hourly mix sparkline, peak hour/slot stats, and optional
  `--by DIM` dominant-series coloring.
- `hour` (`00`–`23`) and `dow` (`Mon`–`Sun`) dimensions usable in `matrix`
  and `pivot` too.

### Fixed
- Filtering by `--model`, `--provider`, or `--agent` now preserves attributed
  user prompt counts across all three implementations (previously filtered
  user rows before turn-attribution in Python, and unattributed prompts in
  Bash/Go).

## [1.1.0] - 2026-10-05

### Added
- `pivot` command: cross-tab heat-map grid for any two dimensions
  (e.g. projects per day) with row/column totals and automatic
  day→week→month bucketing for wide ranges.
- `--csv` and `--md` output modes (RFC-4180 CSV, GitHub-flavored markdown),
  with precedence `--json` > `--csv` > `--md` > text.
- `--details`: extended stats columns — prompts, reasoning tokens, cache
  writes, mean/median/p90 tokens per message, first/last seen.
- `--top N` / `--bottom N` / `--min-share P` row slicing; `--min-share`
  keeps footer totals complete and notes hidden groups.
- `--monthly-budget AMOUNT`: budget block with month-to-date spend,
  month-end projection, remaining, progress bar and UNDER/OVER PACE verdict.
- `--compare CUR/PREV`: period-over-period deltas (Prev Σ $, Δ Σ $, Δ msgs)
  on every grouping command plus a comparison table in `summary`.
- **Prompts** metric: user messages counted and attributed to the assistant
  message that answers them; shown in summary, `--details`, JSON and CSV.
- Full token breakdown everywhere: input, output, reasoning, cache reads,
  cache writes, total, cache-hit ratio.
- `matrix --metric` single-metric focus view.
- Synthetic fixture database for tests (`tests/make_fixture.py`) so the suite
  runs without real usage data; CI workflow.

## [1.0.0] - 2026-10-03

### Added
- Initial CLI: `summary`, `models`, `providers`, `agents`, `projects`,
  `sessions`, `daily`/`weekly`/`monthly`, `matrix --by`, `prices [--refresh]`.
- Aggregations across model, provider, agent, project, session, day, week,
  month — and arbitrary combinations.
- Cost analytics: provider-reported cost plus estimates from models.dev
  pricing (tier-aware in Python) with local cache and user overrides.
- Filters (`--since/--until`, `--model`, `--provider`, `--agent`,
  `--project`, `--session`), `--json`, `--limit`, `--sort`, `--no-color`.
- Boxed tables with ANSI colors (TTY-aware), unicode bars, sparklines and
  adaptive column widths.
- Three implementations: Python (reference), Bash, Go.
