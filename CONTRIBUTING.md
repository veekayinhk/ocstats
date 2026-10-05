# Contributing to ocstats

Thanks for considering a contribution! 🎉

## Setup

```sh
git clone https://github.com/YOUR_GH_USER/ocstats.git
cd ocstats
make install       # symlink the CLIs into ~/.local/bin
make test          # generates a synthetic fixture DB on first run
```

Requirements: Python 3.9+, bash, sqlite3 (JSON1), jq, awk. Go ≥1.22 only if
you touch `ocstats-go/`. No other dependencies — the Python CLI is stdlib-only
by design, and that's a feature worth keeping.

## The one rule

**A feature isn't done until it exists in all three implementations** —
`ocstats` (Python, reference), `ocstats.sh` (Bash), and `ocstats-go/` (Go) —
and `make test` passes. The suite cross-checks that all three agree on
totals, so partial implementations fail loudly.

Things that help reviews:

- Keep the Python implementation as the source of truth for behavior; port
  changes to Bash and Go in the same PR when feasible (a follow-up PR is fine
  if the scope is large — just say so in the description).
- New metrics should land in `--json` and `--csv` output too.
- Add or extend checks in `tests/compare.sh` for anything numeric.
- Match the existing style: plain functions, no dependencies, tables that
  adapt to terminal width, `—` for unknown data rather than fake zeros.

## Testing

```sh
make test                          # full cross-implementation suite
OCSTATS_DB=/path/to/db ./tests/compare.sh   # against a specific database
python3 tests/make_fixture.py      # regenerate the synthetic fixture DB
```

The fixture database is deterministic and contains only invented data
(`acme-*` projects, `acme-cloud`/`oss-local` providers). Never commit real
usage data, session titles, or project names — including in tests.

## Reporting issues

Include your `ocstats --version`, the exact command, and the output. Redact
anything personal — session titles and project names appear in some outputs.
