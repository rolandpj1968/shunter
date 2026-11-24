#!/usr/bin/env python3
"""The line-check worker (PROTOCOL.md §5.4): claims one waiting line's check
from Shunter, runs the site's check on it, and answers with its verdict.

Shunter never runs a line check itself. This worker asks it for one:

    python3 -B hooks/linecheck.py --config linecheck.json [--every 30s]

One run is one claim: `shunt claim --lease L --version V`, which prints
"<line> <trunk> <head>..." or nothing when no line is due. The site's check
runs on that key, and the worker answers `shunt checked <line> <trunk>
<head>... --action pass|fail|conflict|infra --version V`. With --every it
runs again after each pause, for ever. Run one worker for each slot the
checks may use, each with its own actor: a claim is the actor's, and no
other worker is given the line while it stands.

Configuration:

    {
      "shunt": ["shunt"],
      "socket": "/var/lib/shunter/shunter.sock",
      "actor": "line-check-1",
      "command": ["./scripts/check-line"],
      "version_files": ["scripts/check-line"],
      "lease": "3h",
      "timeout": "2h50m"
    }

- **command** is the site's check. It reads one JSON document on stdin,
  {"line", "trunk", "heads", "version"}, checks the line's heads merged
  with trunk, and prints one JSON answer:
  {"verdict": "pass" | "fail" | "conflict" | "infra", "reason": "..."}.
  A fail's reason is its evidence (I6); a conflict's is the files in
  conflict; an infra's says why there is no verdict. A check that crashes,
  runs past `timeout`, prints no JSON, or answers a fail or a conflict with
  no reason has judged nothing: the worker answers infra.
- **infra is no verdict.** Shunter ends the claim at once, records nothing
  of the line, tells the judge, and gives the line to no worker asking for
  any line for a while ([deadlines] unanswered). A trunk that itself fails
  is infra too: the line can't be judged on it (J2 is the judge's).
- **version** is the check's version (I14). Without it, it is a hash of
  `command` and the contents of `version_files`, so a change to the check
  is a new version. When Shunter's line check is at another version, the
  worker declares its own (`shunt checker line-check V`; "declare": false
  turns that off), which makes every green of the old version stop
  counting. Run every worker of a queue at one version.
- **lease** is how long the claim stands (`shunt claim --lease`); **timeout**
  bounds the check, and must be under the lease, or a slow check's verdict
  comes after its claim has been given to another worker.
- **socket** is SHUNTER_SOCKET for the worker's calls (default: the
  environment's).
- **actor** is SHUNTER_ACTOR for the worker's calls (default: the
  environment's, else "line-check").

Exit status: 0 when the run answered, or found nothing due; 1 when Shunter
refused or could not be reached. Standard library only.
"""

import hashlib
import json
import os
import signal
import subprocess
import sys
import time

sys.dont_write_bytecode = True
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import shunterhooks as sh  # noqa: E402

VERDICTS = ("pass", "fail", "conflict", "infra")
REASON_MAX = 4000


class ShuntError(Exception):
    pass


def shunt(cfg, *args):
    """Runs the shunt CLI and returns its stdout; raises on a refusal."""
    env = dict(os.environ)
    env["SHUNTER_ACTOR"] = cfg.get("actor") or env.get("SHUNTER_ACTOR") or "line-check"
    if cfg.get("socket"):
        env["SHUNTER_SOCKET"] = cfg["socket"]
    p = subprocess.run(list(cfg.get("shunt", ["shunt"])) + list(args), env=env,
                       stdin=subprocess.DEVNULL, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                       text=True, timeout=120)
    if p.returncode != 0:
        raise ShuntError((p.stderr or p.stdout).strip()[:1000] or "exit %d" % p.returncode)
    return p.stdout.strip()


def version(cfg, base):
    if cfg.get("version"):
        return str(cfg["version"])
    h = hashlib.sha256(json.dumps(cfg.get("command", [])).encode())
    for f in cfg.get("version_files", []):
        with open(sh.config_path(base, f), "rb") as fh:
            h.update(fh.read())
    return h.hexdigest()[:12]


def run_check(cfg, base, doc):
    """Runs the site's check; returns (verdict, reason)."""
    timeout = sh.parse_duration(cfg.get("timeout"), 3600)
    try:
        p = subprocess.Popen(cfg["command"], cwd=base, stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                             stderr=sys.stderr, start_new_session=True)
    except OSError as e:
        return "infra", "the check could not start: %s" % e
    try:
        out, _ = p.communicate(json.dumps(doc).encode(), timeout=timeout)
    except subprocess.TimeoutExpired:
        # The check's whole process group: a script's children too.
        try:
            os.killpg(p.pid, signal.SIGKILL)
        except OSError:
            pass
        p.communicate()
        return "infra", "the check ran past its timeout (%ds)" % int(timeout)
    try:
        a = json.loads(out.decode(errors="replace").strip().splitlines()[-1])
    except (ValueError, IndexError):
        return "infra", "the check printed no JSON answer (exit %d)" % p.returncode
    v = a.get("verdict") if isinstance(a, dict) else None
    reason = str(a.get("reason") or "").strip() if isinstance(a, dict) else ""
    if v not in VERDICTS:
        return "infra", "the check answered no verdict (%r)" % (v,)
    if v in ("fail", "conflict") and not reason:
        return "infra", "the check answered %s with no reason: nothing is judged on what isn't stated" % v
    if v == "infra" and not reason:
        reason = "the check's infrastructure failed"
    return v, reason[-REASON_MAX:]


def claim(cfg, ver, declared):
    lease = cfg.get("lease")
    args = ["claim", "--version", ver] + (["--lease", lease] if lease else [])
    try:
        return shunt(cfg, *args)
    except ShuntError as e:
        if declared or cfg.get("declare", True) is False or "the line check is at version" not in str(e):
            raise
    sh.log("linecheck: Shunter's line check is at another version; declaring %s" % ver)
    shunt(cfg, "checker", "line-check", ver)
    return claim(cfg, ver, True)


def once(cfg, base):
    ver = version(cfg, base)
    got = claim(cfg, ver, False)
    if not got:
        sh.log("linecheck: no line is due a check")
        return
    parts = got.split()
    line, trunk, heads = parts[0], parts[1], parts[2:]
    sh.log("linecheck: checking %s at %s on trunk %s" % (line, " ".join(heads), trunk))
    verdict, reason = run_check(cfg, base, {"line": line, "trunk": trunk, "heads": heads, "version": ver})
    args = ["checked", line, trunk] + heads + ["--action", verdict, "--version", ver]
    if reason:
        args += ["--reason", reason]
    shunt(cfg, *args)
    sh.log("linecheck: %s %s%s" % (line, verdict, (": " + reason.splitlines()[0][:200]) if reason and verdict != "pass" else ""))


def main():
    cfg, base = sh.load_config(sys.argv)
    every = None
    for i, a in enumerate(sys.argv):
        if a == "--every" and i + 1 < len(sys.argv):
            every = sh.parse_duration(sys.argv[i + 1])
    if "command" not in cfg:
        raise SystemExit("linecheck: the configuration names no command")
    while True:
        try:
            once(cfg, base)
            code = 0
        except (ShuntError, subprocess.TimeoutExpired, OSError) as e:
            sh.log("linecheck: shunt: %s" % e)
            code = 1
        if every is None:
            sys.exit(code)
        time.sleep(every)


if __name__ == "__main__":
    main()
