#!/usr/bin/env python3
"""
make_fixture.py — generate a deterministic synthetic OpenCode-style fixture
database for the ocstats test suite.

The database mirrors the small slice of the OpenCode v2 schema that ocstats
reads (session_v2 + session_message) and contains only invented data:
projects, providers and models are fictional (acme-api, acme-cloud, …).

Usage:  python3 tests/make_fixture.py [output_path]     (default tests/fixture.db)
"""

import json
import random
import sqlite3
import sys
import time
from datetime import datetime, timedelta
from pathlib import Path

SEED = 20260101
DAYS = 21                      # messages spread over the last N days
SESSIONS_PER_PROJECT = 4

PROJECTS = [
    "/home/dev/acme-api",
    "/home/dev/data-pipeline",
    "/home/dev/docs-site",
    "/home/dev/ml-experiments",
]

# (provider, model, $/1M in, $/1M out, $/1M cache-read) — cost=None → free tier
MODELS = [
    ("acme-cloud", "acme-fast", 0.50, 2.00, 0.05, None),
    ("acme-cloud", "acme-pro", 3.00, 12.00, 0.30, None),
    ("oss-local", "llama-4-70b", 0.0, 0.0, 0.0, None),
    ("my-subscription", "private-model", 0.0, 0.0, 0.0, None),
]

AGENTS = ["build", "build", "build", "plan", "explore", "general"]


def make_db(path: str) -> dict:
    rng = random.Random(SEED)
    p = Path(path)
    p.parent.mkdir(parents=True, exist_ok=True)
    if p.exists():
        p.unlink()

    conn = sqlite3.connect(str(p))
    conn.executescript(
        """
        CREATE TABLE session_v2 (
            id TEXT PRIMARY KEY, project_id TEXT NOT NULL, directory TEXT,
            title TEXT, slug TEXT, time_created INTEGER NOT NULL
        );
        CREATE TABLE session_message (
            id TEXT PRIMARY KEY, session_id TEXT NOT NULL, type TEXT NOT NULL,
            seq INTEGER NOT NULL, time_created INTEGER NOT NULL, data TEXT NOT NULL
        );
        CREATE INDEX sm_session_time_idx ON session_message (session_id, time_created);
        """
    )

    now = time.time()
    day_start = int(datetime.combine(datetime.now().date(), datetime.min.time())
                    .timestamp() * 1000)
    stats = {"sessions": 0, "assistant": 0, "prompts": 0, "cost": 0.0}

    for proj_i, directory in enumerate(PROJECTS):
        project_id = f"proj_{proj_i:02d}"
        for s in range(SESSIONS_PER_PROJECT):
            sid = f"ses_fixture{proj_i:02d}{s:02d}"
            provider, model, pin, pout, pcr, _free = MODELS[rng.randrange(len(MODELS))]
            slug = f"fixture-{proj_i:02d}-{s:02d}"
            title = f"Fixing the {rng.choice(['parser', 'query', 'build', 'docs', 'tests'])}"
            start_day = rng.randrange(0, DAYS)
            t0 = day_start - start_day * 86_400_000 + rng.randrange(3_600_000, 60_000_000)
            conn.execute(
                "INSERT INTO session_v2 (id, project_id, directory, title, slug, time_created) "
                "VALUES (?,?,?,?,?,?)",
                (sid, project_id, directory, title, slug, t0))
            stats["sessions"] += 1

            seq = 0
            turns = rng.randrange(8, 25)
            ts = t0
            for _ in range(turns):
                agent = rng.choice(AGENTS)
                # user prompt first, then the assistant reply
                p_ts = ts
                conn.execute(
                    "INSERT INTO session_message VALUES (?,?,?,?,?,?)",
                    (f"msg_{sid}_{seq}_u", sid, "user", seq, p_ts, json.dumps({
                        "agent": agent,
                        "model": {"providerID": provider, "id": model},
                    })))
                seq += 1
                stats["prompts"] += 1

                a_ts = p_ts + rng.randrange(2_000, 90_000)
                tin = rng.randrange(400, 9_000)
                tout = rng.randrange(40, 900)
                treas = rng.choice([0, 0, rng.randrange(10, 400)])
                tcr = int(tin * rng.uniform(1.0, 4.0)) if rng.random() < 0.8 else 0
                tcw = 0
                cost = 0.0
                if pin or pout:  # paid model → reported cost from fixed rates
                    cost = round((tin * pin + tout * pout + tcr * pcr) / 1_000_000, 6)
                    stats["cost"] += cost
                conn.execute(
                    "INSERT INTO session_message VALUES (?,?,?,?,?,?)",
                    (f"msg_{sid}_{seq}_a", sid, "assistant", seq, a_ts, json.dumps({
                        "agent": agent,
                        "model": {"providerID": provider, "id": model},
                        "tokens": {"input": tin, "output": tout, "reasoning": treas,
                                   "cache": {"read": tcr, "write": tcw}},
                        "cost": cost,
                    })))
                seq += 1
                stats["assistant"] += 1
                ts = a_ts + rng.randrange(1_000, 400_000)
                if ts > now * 1000:
                    break

    conn.commit()
    conn.close()
    return stats


if __name__ == "__main__":
    out = sys.argv[1] if len(sys.argv) > 1 else str(
        Path(__file__).resolve().parent / "fixture.db")
    s = make_db(out)
    print(f"fixture: {out} · {s['sessions']} sessions · {s['prompts']} prompts · "
          f"{s['assistant']} assistant messages · ${s['cost']:.2f} reported")
