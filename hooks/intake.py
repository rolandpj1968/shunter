#!/usr/bin/env python3
"""The intake: offers each new line of a producers' lines file to Shunter,
and writes each offer's answer to a file of its own (PROTOCOL.md §5.2).

    python3 -B hooks/intake.py --config intake.json [--every 30s]

Like linecheck.py, it is not a hook: it calls Shunter. The lines file has
one line a row, "<name> <change>... [alone]", appended by producers; '#'
starts a comment. Each name is offered once: `shunt queue <change>...
[--alone]`, which answers only after the gate has (Shunter's own checks,
then the repository's gate hook, [hooks.gate]). The answer goes to
"<answers>/<name><suffix>":

    QUEUED <the row's changes, as written>
    REFUSED
    <the gate's reason>

A name with an answer file is not offered again: a producer offers again
under a new name. A call that fails for any other reason (the daemon is
down, the forge can't be read) writes nothing, and the row is tried again
next run.

Configuration:

    {
      "shunt": ["shunt"],
      "socket": "/var/lib/shunter/shunter.sock",
      "lines": "/srv/producers/offers.list",
      "answers": "/srv/producers/lines",
      "suffix": ".done",
      "drop": ["b"],
      "owner": "^(.+)-[0-9]+[a-z]?$",
      "actor": "intake"
    }

- **drop**: tokens of a row that are neither changes nor "alone" (a marker
  an older format carries), dropped before the offer and kept in QUEUED.
- **owner**: a regular expression on the name whose first group is the
  producer who offers, and so the line's owner (SHUNTER_ACTOR); a name it
  doesn't match is offered as **actor** (default "intake").

Exit status: 0, or 1 when a row could not be offered this run. Standard
library only.
"""

import os
import re
import subprocess
import sys
import time

sys.dont_write_bytecode = True
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import shunterhooks as sh  # noqa: E402


def rows(path):
    try:
        with open(path) as f:
            text = f.read()
    except FileNotFoundError:
        return []
    out = []
    for raw in text.splitlines():
        toks = raw.split("#", 1)[0].split()
        if len(toks) >= 2:
            out.append((toks[0], toks[1:]))
    return out


def write_answer(path, text):
    tmp = path + ".tmp"
    with open(tmp, "w") as f:
        f.write(text if text.endswith("\n") else text + "\n")
    os.replace(tmp, path)


def offer(cfg, name, toks):
    """Offers one row; returns the answer to write, or None to try again."""
    drop = set(cfg.get("drop", []))
    alone = "alone" in toks
    changes = [t for t in toks if t != "alone" and t not in drop]
    spec = " ".join(t for t in toks if t != "alone")
    if not changes or any(not re.fullmatch(r"[A-Za-z0-9._/-]+", c) for c in changes):
        return "REFUSED\n%s: no changes to offer in %r" % (name, " ".join(toks))
    actor = cfg.get("actor") or "intake"
    m = re.match(cfg["owner"], name) if cfg.get("owner") else None
    if m and m.group(1):
        actor = m.group(1)
    env = dict(os.environ, SHUNTER_ACTOR=actor)
    if cfg.get("socket"):
        env["SHUNTER_SOCKET"] = cfg["socket"]
    args = list(cfg.get("shunt", ["shunt"])) + ["queue"] + changes + (["--alone"] if alone else [])
    try:
        p = subprocess.run(args, env=env, stdin=subprocess.DEVNULL, stdout=subprocess.PIPE,
                           stderr=subprocess.PIPE, text=True, timeout=300)
    except (OSError, subprocess.TimeoutExpired) as e:
        sh.log("intake: %s: %s" % (name, e))
        return None
    if p.returncode == 0:
        return "QUEUED %s" % spec
    # The CLI's own prefix ("shunter: ") is not the producer's to read.
    msg = re.sub(r"^shunter: ", "", (p.stderr or p.stdout).strip(), count=1)
    # Only the gate's and the queue's own refusals are answers: "connection
    # refused" from a daemon that is down is not.
    if not re.match(r"(line \S+ refused at the gate: |refused: )", msg):
        sh.log("intake: %s: not offered, tried again next run: %s" % (name, msg[:500]))
        return None
    return "REFUSED\n%s" % msg


def once(cfg, base):
    answers = sh.config_path(base, cfg["answers"])
    suffix = cfg.get("suffix", ".done")
    os.makedirs(answers, exist_ok=True)
    ok = True
    seen = set()
    for name, toks in rows(sh.config_path(base, cfg["lines"])):
        if name in seen or not re.fullmatch(r"[A-Za-z0-9._-]+", name):
            continue
        seen.add(name)
        path = os.path.join(answers, name + suffix)
        if os.path.exists(path):
            continue
        a = offer(cfg, name, toks)
        if a is None:
            ok = False
            continue
        write_answer(path, a)
        sh.log("intake: %s: %s" % (name, a.splitlines()[0]))
    return ok


def main():
    cfg, base = sh.load_config(sys.argv)
    for k in ("lines", "answers"):
        if not cfg.get(k):
            raise SystemExit("intake: the configuration names no %s" % k)
    every = None
    for i, a in enumerate(sys.argv):
        if a == "--every" and i + 1 < len(sys.argv):
            every = sh.parse_duration(sys.argv[i + 1])
    while True:
        ok = once(cfg, base)
        if every is None:
            sys.exit(0 if ok else 1)
        time.sleep(every)


if __name__ == "__main__":
    main()
