# Changelog

All notable changes to this project are documented in this file.
The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

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
