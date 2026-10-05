# Security Policy

## Scope

`ocstats` is a local, read-only analytics tool:

- It opens the OpenCode SQLite database with `mode=ro` and never writes to it.
- It performs **no** network calls except `ocstats prices --refresh`, which
  downloads public pricing data from `https://models.dev/api.json`.
- It contains **no** telemetry, accounts, or API keys.

## Reporting a vulnerability

Please open a GitHub security advisory ("Report a vulnerability" on the
Security tab) rather than a public issue. Include the version
(`ocstats --version`), the affected implementation (Python/Bash/Go), and a
reproduction.

## What is out of scope

- The contents of your local OpenCode database. `ocstats` reads it like any
  other local process running as your user; secure your machine.
- Rates fetched from models.dev (public, community-maintained data).
