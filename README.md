# ocstats

Beautiful terminal analytics for [OpenCode v2](https://opencode.ai/v2/docs/) usage —
aggregate tokens, prompts, sessions and cost by **model, provider, agent,
project, day, week, month, session**, or any combination of these.

```
  OpenCode Usage Summary  2026-07-01 → 2026-09-15  · all time

╭─────────┬──────────┬──────────┬───────────┬───────────┬────────────┬─────────────┬────────────┬───────────┬──────────┬─────────╮
│ Prompts │ Messages │ Sessions │ Projects  │   Tok in  │  Tok out   │  Cache hits │  Tok total │ Cache hit │  Rep $   │   Σ $   │
├─────────┼──────────┼──────────┼───────────┼───────────┼────────────┼─────────────┼────────────┼───────────┼──────────┼─────────┤
│     412 │    1,248 │       42 │        19 │ 32,411,882│  1,004,552 │ 118,220,140 │ 33,220,981 │     78.1%│  $18.04  │ $19.88  │
╰─────────┴──────────┴──────────┴───────────┴───────────┴────────────┴─────────────┴────────────┴───────────┴──────────┴─────────╯

Cost
╭──────────┬───────────┬─────────────╮
│ Reported │ Estimated │ Effective Σ │
├──────────┼───────────┼─────────────┤
│  $18.04  │  $17.61   │   $19.88    │
╰──────────┴───────────┴─────────────╯
```

It reads OpenCode v2's local SQLite database **read-only** — it never writes,
locks, or interferes with a running OpenCode. No API keys are used; the only
optional network call is `prices --refresh`.

## Why

OpenCode's own `/costs` gives per-session totals. `ocstats` answers the
questions that view can't:

- *Which model did I spend the most on this month — and is it trending up?*
- *How many prompts and tokens did project X consume per day?*
- *What's my cache-hit ratio, and which agent burns output tokens?*
- *Am I on pace to blow through my monthly budget?*

## Three implementations, one CLI

| File | Stack | Build |
|---|---|---|
| `ocstats` | Python 3.9+, **stdlib only** — reference implementation | none |
| `ocstats.sh` | Bash + `sqlite3` + `jq` + `awk` | none |
| `ocstats-go/` → `ocstats-bin` | Go (pure-Go SQLite driver, no cgo) | `make build` |

All three share the same subcommands, filters, pricing cache and override
file, and agree on totals (verified by `make test`). Differences: the Python
version applies models.dev **context tiers** (>200k pricing); Bash and Go use
base rates. `weekly` labels are Monday-of-week dates.

## Install

Requirements: Python 3.9+ (the reference CLI has zero dependencies).
The Bash variant additionally needs `sqlite3` (with JSON1), `jq` and `awk`.

```sh
git clone https://github.com/YOUR_GH_USER/ocstats.git
cd ocstats
make install          # symlinks ocstats + ocstats.sh into ~/.local/bin
ocstats               # done
```

Optional compiled build (needs Go ≥1.22; downloads the driver once):

```sh
make build            # → ocstats-bin
```

## Commands

```
ocstats                     # summary dashboard (default command)
ocstats models              # per provider × model
ocstats providers           # per provider
ocstats agents              # per agent (build/plan/explore/…)
ocstats projects            # per project directory
ocstats sessions            # top sessions by effective cost
ocstats daily | weekly | monthly
ocstats matrix --by provider,month     # any dimension combination
ocstats matrix --by model,day --metric cost   # focused single-metric view
ocstats pivot --rows project --cols day       # cross-tab grid with heat shading
ocstats prices [--refresh]  # resolved pricing table; --refresh re-fetches
```

Dimensions for `matrix --by`, `pivot --rows/--cols`: `model provider agent
project session day week month` — combine freely (`month,model`,
`provider,agent`, projects-per-day, …).

### Daily usage by model

```sh
ocstats matrix --by day,model --sort date-desc     # one row per (day, model)
ocstats pivot  --rows model --cols day             # models as rows, days as columns
```

### Pivot — projects per day at a glance

```
ocstats pivot --rows project --cols day --since 30d
ocstats pivot --rows model --cols provider --metric cost
ocstats pivot --rows agent --cols month --metric msgs
```

Renders a grid with per-cell values, heat shading (dim → green → white → bold
by quartile), row/column totals, and automatic bucketing to week/month when a
day range is too wide (>14 columns). Metrics: `tokens in out cache msgs
prompts cost`. Works with `--json`, `--csv`, `--md` too. Totals reconcile
with `matrix --by <rows>,<cols>`.

### Full token & activity detail

The summary dashboard breaks usage down completely — **prompts** (user
messages, attributed to the assistant message that answers them), messages,
sessions, input/output/reasoning tokens, cache hits (reads), cache writes,
token total and cache-hit ratio. Group tables expose the same detail via
`--details` (Prompts, Reasoning, Cache W, mean/median/p90 tokens per message,
first/last seen), and `--json` / `--csv` always carry every field.

## Filters

```
--since 2026-09-01 | --since 30d | --since 2026-09 | --until 2026-09-30
--model gemini --provider google --agent build --project "acme-api"
--session ses_f513…
```

Substring matches are case-insensitive. Combine anything:

```sh
ocstats models --since 30d --provider google --sort tokens --limit 10
ocstats matrix --by month,provider --since 2026-08 --json | jq .
```

## Cost model

- **Rep $** — cost as reported by the provider in the database (often `$0`
  for subscription/free tiers).
- **Est $** — tokens × [models.dev](https://models.dev) rates
  ($/1M input, output, cache read, cache write).
- **Σ (effective)** — reported where reported > 0, else estimated. This is
  what shares and sorting use by default.
- `—` — no pricing data exists for that provider/model.

Pricing lives in `~/.cache/ocstats/api.json` (refreshed **only** when you run
`ocstats prices --refresh`; everything else works offline from the cache).
User overrides win over models.dev — useful for subscription plans:

```jsonc
// ~/.config/ocstats/pricing.json   ($ per 1M tokens)
{
  "my-subscription/my-model":  { "input": 0.60, "output": 2.20, "cache_read": 0.06 },
  "your-provider/your-model":  { "input": 0.22, "output": 0.88, "cache_read": 0.02 }
}
```

Model lookup is fuzzy: exact id → variant-stripped (`@default`) → leaf
(`vendor/model` → `model`) → any provider. The `prices` command shows which
source each rate came from (`override`, `models.dev`, `models.dev~`, `none`).

## Output options

```
--json            machine-readable (same rows/metrics as the tables)
--csv             RFC-4180 CSV with raw numbers (spreadsheets, pandas)
--md              GitHub-flavored markdown tables (issues, PRs, notes)
--details         extended stats: prompts, reasoning, cache W, mean/median/p90
                  tokens per message, first/last seen
--limit N         cap rows (0 = all; default 25) — the "+ N more" row keeps totals honest
--top N           top N groups by the current sort
--bottom N        bottom N groups by the current sort
--min-share P     hide groups below P% share of effective cost (totals stay complete)
--sort KEY        cost (default) | rep | est | tokens | msgs | sessions | name | date | date-desc
--no-color        disable ANSI (also automatic when piped, or via NO_COLOR=1)
--width N         force table width (default: terminal width, 110 fallback)
```

Precedence: `--json` > `--csv` > `--md` > text table. CSV/JSON always carry
the full metric set regardless of `--details`; CSV is not limited by
`--limit`/`--top` (machine consumers get everything).

## Budget & projections

```sh
ocstats summary --monthly-budget 50
```

Adds a Budget block: month-to-date effective spend, month-end projection
(MTD ÷ days elapsed × days in month), remaining, % used, a progress bar and
an UNDER/OVER PACE verdict. Also included in `--json` output as a `budget`
object.

## Period comparison

```sh
ocstats providers --compare 7d/7d        # last 7 days vs the 7 before it
ocstats models    --compare 2026-09/2026-08
ocstats summary   --compare 30d/30d
```

Every grouping command gains **Prev Σ $, Δ Σ $, Δ msgs** columns (current
window vs previous window, same filters), and `summary` renders a two-period
comparison table. Relative windows (`7d`) chain back-to-back from today;
absolute specs (`2026-09`, `2026-09-01`) are calendar ranges. `--since` /
`--until` are ignored while comparing.

## Environment

| Variable | Meaning |
|---|---|
| `OCSTATS_DB` | path to the OpenCode database |
| `NO_COLOR` | disable color (same as `--no-color`) |
| `XDG_CACHE_HOME` | pricing cache location (`…/ocstats/api.json`) |
| `XDG_CONFIG_HOME` | overrides location (`…/ocstats/pricing.json`) |

## How it works

- Opens `~/.local/share/opencode/opencode.db` **read-only** (`mode=ro`).
- Reads the **v2 tables only**: `session_message` (`type='assistant'` for
  usage, `type='user'` for prompts) joined to `session_v2` for the project
  directory — legacy rows are never double-counted.
- Cost estimates come from [models.dev](https://models.dev) public pricing
  (tier-aware in the Python implementation), cached locally and overridable.

## Testing

```sh
make test
```

The suite generates a deterministic synthetic fixture database on first run
(`tests/make_fixture.py`), then cross-checks the Python and Bash
implementations: headline totals, matrix invariants, CSV/JSON/markdown shape,
pivot reconciliation, prompt counts vs raw SQL, budget and comparison
windows. The Go binary is exercised automatically once built.

## Contributing

PRs welcome — see [CONTRIBUTING.md](CONTRIBUTING.md). The one rule that
matters: a feature isn't done until it exists in **all three** implementations
and `make test` passes.

## License

[MIT](LICENSE) © 2026 ocstats contributors
