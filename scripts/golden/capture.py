#!/usr/bin/env python3
"""Captures the golden parity corpus by running the real Python agtrack CLI.

Reads scenarios.py, replays each one against a fresh scratch database through
`pyagtrack` (stdlib only; no third-party deps), and writes
testdata/golden/<scenario>.json. Running it twice must produce byte-identical
files -- that determinism is the whole point of the corpus.

Usage:
    python3 scripts/golden/capture.py                 # capture every scenario
    python3 scripts/golden/capture.py NAME [NAME ...]  # capture just these
"""

from __future__ import annotations

import datetime
import json
import os
import shutil
import subprocess
import sys
import tempfile
from pathlib import Path

HERE = Path(__file__).resolve().parent
REPO_ROOT = HERE.parent.parent
PYAGTRACK = HERE / "pyagtrack"
TESTDATA = REPO_ROOT / "testdata" / "golden"

sys.path.insert(0, str(HERE))
from scenarios import SCENARIOS  # noqa: E402

# Every scenario's clock starts here; step k of a scenario reads at
# BASE + k minutes. Scenarios never share a database, so reusing one base
# across all of them is fine and keeps the corpus easy to reason about.
BASE = datetime.datetime(2026, 9, 1, 12, 0, 0, tzinfo=datetime.timezone.utc)


def _now_for_step(k: int) -> str:
    instant = BASE + datetime.timedelta(minutes=k)
    return instant.strftime("%Y-%m-%dT%H:%M:%SZ")


def _make_cwd(kind: str, tmp: Path) -> Path:
    d = tmp / f"cwd-{kind}"
    d.mkdir()
    if kind == "repo":
        subprocess.run(["git", "init", "-q", "-b", "main"], cwd=d, check=True)
        subprocess.run(
            ["git", "remote", "add", "origin", "git@github.com:acme/widgets.git"],
            cwd=d, check=True,
        )
    return d


def run_scenario(scenario: dict) -> dict:
    name = scenario["name"]
    actor = scenario.get("actor")
    default_cwd_kind = scenario.get("cwd", "plain")

    with tempfile.TemporaryDirectory(prefix=f"agtrack-golden-{name}-") as tmp_s:
        tmp = Path(tmp_s)
        home = tmp / "home"
        home.mkdir()
        db_path = tmp / "db" / "agtrack.db"
        db_path.parent.mkdir()
        cwd_dirs = {
            "plain": _make_cwd("plain", tmp),
        }
        if any(s.get("cwd", default_cwd_kind) == "repo" for s in scenario["steps"]) or (
            default_cwd_kind == "repo"
        ):
            cwd_dirs["repo"] = _make_cwd("repo", tmp)

        base_env = {
            "PATH": os.environ.get("PATH", ""),
            "TZ": "UTC",
            "LC_ALL": "C.UTF-8",
            "HOME": str(home),
            "AGTRACK_DB": str(db_path),
        }
        if actor:
            base_env["AGTRACK_ACTOR"] = actor
        if scenario.get("format"):
            base_env["AGTRACK_FORMAT"] = scenario["format"]

        out_steps = []
        for k, step in enumerate(scenario["steps"]):
            cwd_kind = step.get("cwd", default_cwd_kind)
            env = dict(base_env)
            extra_env = step.get("env", {})
            env.update(extra_env)
            env["AGTRACK_NOW"] = _now_for_step(k)
            stdin = step.get("stdin", "")

            result = subprocess.run(
                [sys.executable, str(PYAGTRACK), *step["argv"]],
                cwd=cwd_dirs[cwd_kind],
                env=env,
                input=stdin,
                capture_output=True,
                text=True,
                timeout=30,
            )
            out_steps.append({
                "argv": step["argv"],
                "cwd": cwd_kind,
                "now": env["AGTRACK_NOW"],
                "env": extra_env,
                "stdin": stdin,
                "stdout": result.stdout,
                "stderr": result.stderr,
                "exit": result.returncode,
                "normalize": step.get("normalize", []),
                "tags": step.get("tags", ["parity"]),
            })

    return {"scenario": name, "actor": actor, "steps": out_steps}


def main(argv: list[str]) -> int:
    wanted = set(argv) or None
    TESTDATA.mkdir(parents=True, exist_ok=True)

    seen = set()
    total_steps = 0
    for scenario in SCENARIOS:
        name = scenario["name"]
        if name in seen:
            print(f"capture: duplicate scenario name: {name}", file=sys.stderr)
            return 1
        seen.add(name)
        if wanted is not None and name not in wanted:
            continue
        data = run_scenario(scenario)
        total_steps += len(data["steps"])
        out_path = TESTDATA / f"{name}.json"
        with out_path.open("w") as f:
            json.dump(data, f, indent=1, sort_keys=True)
            f.write("\n")
        print(f"captured {name}: {len(data['steps'])} steps -> {out_path}")

    if wanted is None:
        print(f"total: {len(SCENARIOS)} scenarios, {total_steps} steps")
    return 0


if __name__ == "__main__":
    raise SystemExit(main(sys.argv[1:]))
