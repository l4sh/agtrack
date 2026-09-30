#!/usr/bin/env python3
"""Regenerate python_cases.json from the reference Python CLI's argparse.

    PYTHONDONTWRITEBYTECODE=1 python3 gen_python_cases.py [AGTRACK_PY_DIR]

Reads cases.txt (one shell-quoted argv per line, '#' comments) and records,
for each, what agtracklib.cli.build_parser() does with it: the namespace, the
usage error (prog + message), a help request (prog) or the version text.
Nothing touches a database: only the parser is exercised.
"""
import argparse
import contextlib
import io
import json
import os
import shlex
import sys

here = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, sys.argv[1] if len(sys.argv) > 1 else os.environ["AGTRACK_PY_SRC"])
from agtracklib.cli import build_parser  # noqa: E402


class Err(Exception):
    pass


class Help(Exception):
    pass


def error(self, message):
    raise Err(self.prog, message)


def print_help(self, file=None):
    raise Help(self.prog)


argparse.ArgumentParser.error = error
argparse.ArgumentParser.print_help = print_help

out = []
with open(os.path.join(here, "cases.txt")) as f:
    for line in f:
        line = line.strip()
        if not line or line.startswith("#"):
            continue
        argv = shlex.split(line)
        case = {"argv": argv}
        stdout = io.StringIO()
        try:
            with contextlib.redirect_stdout(stdout):
                ns = build_parser().parse_args(argv)
            d = vars(ns)
            d.pop("func", None)
            typed = [d.pop("noun")]
            if "verb" in d:
                v = d.pop("verb")
                if v is not None:
                    typed.append(v)
            case["typed"] = typed
            case["values"] = d
        except Err as e:
            case["error"] = {"prog": e.args[0], "message": e.args[1]}
        except Help as e:
            case["help"] = e.args[0]
        except SystemExit as e:
            case["exit"] = e.code
            case["stdout"] = stdout.getvalue()
        out.append(case)

with open(os.path.join(here, "python_cases.json"), "w") as f:
    json.dump(out, f, indent=1)
    f.write("\n")
print(f"{len(out)} cases", file=sys.stderr)
