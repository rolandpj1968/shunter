#!/usr/bin/env python3
"""Shunter's example notify hook: delivers each notice by rules.

Shunter runs it for every notice to an owner or the judge, after writing
the notice to its events file (see [notify] in examples/shunter.toml):

  in:  {"time", "key", "to": "owner" | "judge", "line", "changes": [...],
        "state", "cause", "point", "train", "message"}
  out: nothing; exit 0 once delivered. A non-zero exit is a delivery that
       failed: Shunter runs the hook again, and escalates (J3).

"state" is the state an owner's line went to ("returned", "held",
"parked", "landed", "dropped"), or empty for advice, the line unmoved (a
conflict the runner couldn't rebase, a starving line, a void charge);
"cause" says why ("charged", "conflict", "left-out", "refused", ...).

Every rule whose "when" matches is applied, in order. A "when" key is a
value or a list of values; a key it lacks matches anything; "state": ""
matches advice. A rule may:
  - "run" commands. One that names {change} runs once per change of the
    line; any other runs once. A command that fails fails the delivery.
  - "append" a line to files, each {"file", "format"}: the owners' file a
    producer reads, a mirror of every notice.

Text in a command or a format may name {to}, {line}, {change}, {changes}
(space-separated), {state}, {kind} (the state, or "advice"), {cause},
{point}, {train}, {time}, {key} and {message}. Each is replaced as text,
never run by a shell. Commands run from the configuration's directory.

The hook runs until it succeeds once for a key, so a rule's commands must
be safe to repeat (a label added twice is one label; a comment may be
posted twice).

"dry_run", a file: commands are written to it, one JSON array a line,
instead of being run. Appends still happen. For a rehearsal: one notice of
each kind, and read what would have been done.

Configuration (JSON, --config FILE):

  {
    "rules": [
      {"when": {"to": "owner", "state": "returned", "cause": "charged"},
       "run": [["gh", "pr", "edit", "{change}", "--add-label", "wip"],
               ["gh", "pr", "comment", "{change}", "--body", "{message}"]]},
      {"when": {"to": "owner"},
       "append": [{"file": "/srv/owners/returned.tsv",
                   "format": "{time}\t{changes}\t{kind}\t{cause}\t{message}"}]},
      {"when": {"to": "judge"},
       "append": [{"file": "/srv/owners/alerts.log", "format": "{time} {point} {train}: {message}"}]}
    ],
    "timeout": "60s",
    "dry_run": ""
  }
"""

import json
import os
import subprocess
import sys

# Shunter runs a hook from its configured directory: never write __pycache__ there.
sys.dont_write_bytecode = True
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import shunterhooks as sh  # noqa: E402


def fields(doc):
    changes = [str(c) for c in doc.get("changes") or []]
    state = str(doc.get("state") or "")
    return {
        "to": str(doc.get("to") or ""), "line": str(doc.get("line") or ""),
        "changes": " ".join(changes), "state": state, "kind": state or "advice",
        "cause": str(doc.get("cause") or ""), "point": str(doc.get("point") or ""),
        "train": str(doc.get("train") or ""), "time": str(doc.get("time") or ""),
        "key": str(doc.get("key") or ""), "message": str(doc.get("message") or ""),
    }, changes


def fill(text, f):
    # One pass over the text, so that a value holding "{x}" is never filled again.
    out, i = [], 0
    while i < len(text):
        j = text.find("{", i)
        if j < 0:
            out.append(text[i:])
            break
        k = text.find("}", j)
        name = text[j + 1:k] if k > j else None
        if name is not None and name in f:
            out.append(text[i:j] + f[name])
            i = k + 1
        else:
            out.append(text[i:j + 1])
            i = j + 1
    return "".join(out)


def matches(when, f):
    for k, want in (when or {}).items():
        want = want if isinstance(want, list) else [want]
        if f.get(k, "") not in [str(w) for w in want]:
            return False
    return True


def main():
    cfg, base = sh.load_config(sys.argv)
    doc = sh.read_input()
    f, changes = fields(doc)
    timeout = sh.parse_duration(cfg.get("timeout"), 60)
    dry = sh.config_path(base, cfg.get("dry_run", ""))
    failed = []
    for rule in cfg.get("rules") or []:
        if not matches(rule.get("when"), f):
            continue
        for a in rule.get("append") or []:
            path = sh.config_path(base, a["file"])
            with open(path, "a") as out:
                out.write(fill(a.get("format", "{time} {kind} {line}: {message}"), f).replace("\n", " ") + "\n")
        for cmd in rule.get("run") or []:
            each = [dict(f, change=c) for c in changes] if any("{change}" in x for x in cmd) else [f]
            for g in each:
                argv = [fill(x, g) for x in cmd]
                if dry:
                    with open(dry, "a") as out:
                        out.write(json.dumps(argv) + "\n")
                    continue
                try:
                    p = subprocess.run(argv, cwd=base, stdin=subprocess.DEVNULL, stdout=subprocess.PIPE,
                                       stderr=subprocess.STDOUT, timeout=timeout, start_new_session=True)
                    rc, tail = p.returncode, p.stdout.decode("utf-8", "replace").strip()[-300:]
                except (subprocess.TimeoutExpired, OSError) as e:
                    rc, tail = -1, str(e)
                if rc != 0:
                    failed.append("%s: exit %d: %s" % (argv[0], rc, tail))
    for x in failed:
        sh.log("notify:", x)
    sys.exit(1 if failed else 0)


if __name__ == "__main__":
    main()
