#!/usr/bin/env python3
"""Shunter's example pre-flight hook: a list of named commands, each timed
and recorded, run on the assembled train before CI is spent.

Shunter runs it in its clone, at the train's head (see [hooks.preflight] in
examples/shunter.toml):

  in:  {"dir", "train", "base", "head",
        "lines": [{"ID", "Changes": [{"ID", "Head"}]}],
        "tips": {"<line>": "<its last commit in the train>"}}
  out: {"pass": true} or {"pass": false, "blamed": [...], "evidence": "..."}
       or {"pass": false, "left_out": {"<line>": "<why>"}, "evidence": "..."}

Steps run in order and stop at the first failure, each with
SHUNTER_TRAIN, SHUNTER_BASE and SHUNTER_HEAD in its environment. Each run
appends one row to `record`: time, train, step, seconds, exit code. A step
must leave the tree as it found it, or say what it may commit.

A step's "commit" commits what it changes when it passes, on top of the
train (Shunter takes the last commit as the train's head; PROTOCOL.md
§5.4): {"message": "...", "paths": [...]} or {"message": "...",
"attribute": "merge", "value": "regenerate"}. Only changed files under the
paths, or marked with the attribute's value, are committed; any other
change the step makes is undone, and said. "{train}" in the message is the
train. Later steps see the new head as SHUNTER_HEAD. For a regeneration
with nothing in conflict, and for counters the combination improves. The
committer is git's, from the hook's environment.

The usefulness rule. A step that has caught nothing (no exit but 0) in the
last `usefulness.days` days, ran at least `usefulness.min_runs` times in
them, and has a median over `usefulness.min_seconds` seconds, is skipped:
CI still runs the check. A skipped step writes no row, so after `days` of
skipping it has no rows in the window and runs again, which samples it.
Steps in `always` always run. Steps in `never` never run here. A step
named in the `recalled` file (rows: time, step, why) in the window runs
whatever its record: write a row when CI fails in what a skipped step
covers.

On a failure, blame, as the step's "blame" says ("files" when unset):
  - "none": nobody is named, and Shunter's J1 decides (its default splits
    the train in halves by line). For a step whose failure says nothing
    about which line caused it, where a script queue split the batch.
  - "bisect" (or the older "bisect": true): the step is run again at the tips of the train's
    lines, by bisection, to find the first line at whose tip it fails;
    that line is blamed (with those before it, it tipped the step). A step
    that fails at the train's base too blames nobody: trunk is broken.
  - "files": the step's output is read for the files and directories it
    names, with the blame rules in shunterhooks.py, against each line's
    files in the train (from the tips).
  Blame that names no line, or every line, is no blame: Shunter's J1
  decides.

A step's "answer" says what its failure does with the lines blame finds:
  - "blame" (the default): they are charged, and go back to their owners
    as having broken the train.
  - "leave-out": they are left out of the train, each with the step's
    output as the reason: they didn't fit this train (they don't build on
    it, or a generated part clashes with another line's). Shunter builds
    the train again without them and runs pre-flight again. A leave-out is
    not a charge. It needs a blame mode that names lines ("files" or
    "bisect"); if blame names none, the failure is unblamed, as before.

Configuration (JSON, --config FILE):

  {
    "steps": [
      {"name": "lint", "command": ["make", "lint"], "timeout": "10m"},
      {"name": "regenerate", "command": ["make", "generate"],
       "commit": {"message": "regenerate the combined train",
                  "attribute": "merge", "value": "regenerate"}},
      {"name": "ratchet", "command": ["./ratchet.sh"], "blame": "bisect",
       "commit": {"message": "record what the train improves",
                  "paths": ["counters.json"]}},
      {"name": "gate", "command": ["./gate.sh"], "blame": "none"}
    ],
    "always": ["lint"],
    "never": [],
    "record": "preflight-steps.tsv",
    "recalled": "preflight-recalled.tsv",
    "usefulness": {"days": 7, "min_runs": 100, "min_seconds": 2},
    "output_dir": "preflight-logs",
    "blame": {"patterns": [], "test_files": ["_test\\.go$"]}
  }
"""

import datetime
import os
import statistics
import subprocess
import sys
import time

# Shunter runs a hook in its clone: never write __pycache__ beside it.
sys.dont_write_bytecode = True
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import shunterhooks as sh  # noqa: E402


def iso(t):
    return datetime.datetime.fromtimestamp(t, datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


def parse_iso(s):
    try:
        return datetime.datetime.strptime(s, "%Y-%m-%dT%H:%M:%SZ").replace(tzinfo=datetime.timezone.utc).timestamp()
    except ValueError:
        return None


def skips(cfg, base, now):
    """The steps to skip now, each with why."""
    names = [s["name"] for s in cfg.get("steps") or []]
    always = set(cfg.get("always") or [])
    out = {}
    for n in cfg.get("never") or []:
        if n not in always:
            out[n] = "listed in never"
    u = cfg.get("usefulness")
    record = sh.config_path(base, cfg.get("record", ""))
    if not u or not record or not os.path.exists(record):
        return out
    days = float(u.get("days", 7))
    min_runs = int(u.get("min_runs", 100))
    min_secs = float(u.get("min_seconds", 2))
    cut = now - days * 86400
    runs, catches, secs = {}, {}, {}
    with open(record) as f:
        for row in f:
            p = row.rstrip("\n").split("\t")
            if len(p) < 5 or not p[4].lstrip("-").isdigit():
                continue
            t = parse_iso(p[0])
            if t is None or t < cut:
                continue
            step = p[2]
            runs[step] = runs.get(step, 0) + 1
            if int(p[4]) != 0:
                catches[step] = catches.get(step, 0) + 1
            try:
                secs.setdefault(step, []).append(float(p[3]))
            except ValueError:
                pass
    recalled = set()
    rp = sh.config_path(base, cfg.get("recalled", ""))
    if rp and os.path.exists(rp):
        with open(rp) as f:
            for row in f:
                p = row.rstrip("\n").split("\t")
                t = parse_iso(p[0]) if p else None
                if len(p) >= 2 and t is not None and t >= cut:
                    recalled.add(p[1])
    for n in names:
        if n in always or n in recalled or n in out:
            continue
        r, k = runs.get(n, 0), catches.get(n, 0)
        med = statistics.median(secs[n]) if secs.get(n) else 0
        if r >= min_runs and k == 0 and med > min_secs:
            out[n] = "0 catches in %d runs over %g days, median %.0fs" % (r, days, med)
    return out


BLAME_MODES = ("none", "files", "bisect")


def blame_mode(step):
    """How a failure of the step is blamed: "none", "files" or "bisect"."""
    m = step.get("blame")
    if m is None:
        return "bisect" if step.get("bisect") else "files"
    if m not in BLAME_MODES:
        raise SystemExit("pre-flight: step %s: blame %r: one of %s" % (step.get("name"), m, ", ".join(BLAME_MODES)))
    return m


ANSWERS = ("blame", "leave-out")


def answer_mode(step):
    """What a failure of the step answers: "blame" (the default) charges
    the lines its blame finds; "leave-out" leaves them out of the train as
    lines that didn't fit it, with the evidence as the reason."""
    m = step.get("answer", "blame")
    if m not in ANSWERS:
        raise SystemExit("pre-flight: step %s: answer %r: one of %s" % (step.get("name"), m, ", ".join(ANSWERS)))
    if m == "leave-out" and blame_mode(step) == "none":
        raise SystemExit("pre-flight: step %s: answer \"leave-out\" needs a blame that names lines" % step.get("name"))
    return m


def run_step(step, doc, timeout):
    env = dict(os.environ)
    env.update({"SHUNTER_TRAIN": str(doc.get("train", "")), "SHUNTER_BASE": str(doc.get("base", "")),
                "SHUNTER_HEAD": str(doc.get("head", ""))})
    t0 = time.monotonic()
    try:
        p = subprocess.run(step["command"], cwd=doc["dir"], env=env, stdin=subprocess.DEVNULL,
                           stdout=subprocess.PIPE, stderr=subprocess.STDOUT, timeout=timeout,
                           start_new_session=True)
        rc, out = p.returncode, p.stdout
    except subprocess.TimeoutExpired as e:
        rc, out = 124, (e.output or b"") + b"\n[timed out after %ds]\n" % int(timeout)
    except OSError as e:
        rc, out = 127, str(e).encode()
    return rc, time.monotonic() - t0, out.decode("utf-8", "replace") if isinstance(out, bytes) else out


def record_row(cfg, base, train, name, secs, rc):
    path = sh.config_path(base, cfg.get("record", ""))
    if not path:
        return
    with open(path, "a") as f:
        f.write("%s\t%s\t%s\t%.1f\t%d\n" % (iso(sh.now()), train, name, secs, rc))


def tail(text, n=20):
    lines = [x for x in text.splitlines() if x.strip()]
    return "\n".join(lines[-n:])


def bisect(step, doc, order, timeout):
    """The first line at whose tip the step fails, or None, with why."""
    d, tips, head = doc["dir"], doc.get("tips") or {}, doc["head"]
    if any(not tips.get(l) for l in order):
        return None, "no tips to bisect with"

    def fails_at(commit):
        sh.git(d, "checkout", "--quiet", "--detach", "--force", commit)
        rc, _, _ = run_step(step, doc, timeout)
        sh.git(d, "reset", "--quiet", "--hard")
        sh.git(d, "clean", "-fdq")
        if rc == 124 or rc == 127:
            raise sh.GitError("the step could not run at %s (exit %d)" % (commit[:12], rc))
        return rc != 0

    try:
        if fails_at(doc["base"]):
            return None, "it fails at the train's base too: trunk is broken, not the train"
        lo, hi = 0, len(order) - 1
        while lo < hi:
            mid = (lo + hi) // 2
            if fails_at(tips[order[mid]]):
                hi = mid
            else:
                lo = mid + 1
        return order[lo], "it passes at the tip of the line before and fails at line %s's tip" % order[lo]
    except sh.GitError as e:
        return None, "bisection stopped: %s" % e
    finally:
        sh.git(d, "checkout", "--quiet", "--detach", "--force", head, check=False)
        sh.git(d, "clean", "-fdq", check=False)


def commit_spec(step):
    """A step's "commit", checked: a message, and what it may change."""
    c = step.get("commit")
    if c is None:
        return None
    if not isinstance(c, dict) or not c.get("message") or not (c.get("paths") or c.get("attribute")):
        raise SystemExit("pre-flight: step %s: commit needs a message, and paths or an attribute" % step.get("name"))
    return c


def commit_step(step, doc, untracked_before):
    """Commits what a passing step changed in the files its "commit" allows,
    on top of the train, and undoes any other change. Returns the new head,
    or None when it changed nothing allowed."""
    c, d = step["commit"], doc["dir"]
    mod, new = sh.dirty(d)
    new -= untracked_before
    changed = sorted(mod | new)
    allowed = set()
    if c.get("attribute"):
        allowed = sh.marked(d, c["attribute"], c.get("value", "set"), changed)
    prefixes = [p.rstrip("/") for p in c.get("paths") or []]

    def keep(path):
        return path in allowed or any(path == p or path.startswith(p + "/") for p in prefixes)

    staged, undone = sh.keep_only(d, mod, new, keep)
    if undone:
        sh.log("pre-flight: %s changed files it may not commit; undone: %s" % (step["name"], " ".join(undone)))
    if not staged:
        return None
    msg = c["message"].replace("{train}", str(doc.get("train", "")))
    sh.git(d, "-c", "core.hooksPath=/dev/null", "commit", "--quiet", "--no-verify", "-m", msg)
    sh.log("pre-flight: %s committed %s" % (step["name"], " ".join(staged)))
    return sh.git(d, "rev-parse", "HEAD")


def main():
    cfg, base = sh.load_config(sys.argv)
    doc = sh.read_input()
    order = [sh.line_id(l) for l in doc.get("lines") or []]
    train = str(doc.get("train", ""))
    for step in cfg.get("steps") or []:
        blame_mode(step)  # a bad configuration says nothing, before any step runs
        answer_mode(step)
        commit_spec(step)
    skipped = skips(cfg, base, sh.now())
    for n, why in sorted(skipped.items()):
        sh.log("pre-flight: %s skipped (%s; CI runs it)" % (n, why))
    for step in cfg.get("steps") or []:
        name = step["name"]
        if name in skipped:
            continue
        timeout = sh.parse_duration(step.get("timeout"), 600)
        untracked = sh.dirty(doc["dir"])[1] if step.get("commit") else set()
        rc, secs, out = run_step(step, doc, timeout)
        record_row(cfg, base, train, name, secs, rc)
        sh.log("pre-flight: %s exit %d in %.1fs" % (name, rc, secs))
        if rc == 0:
            if step.get("commit"):
                try:
                    head = commit_step(step, doc, untracked)
                except sh.GitError as e:
                    sh.log("pre-flight: %s: its commit failed: %s" % (name, e))
                    sys.exit(3)
                if head:
                    # Later steps, and a bisection's return, see the train
                    # with this commit; the lines' tips are unchanged.
                    doc["head"] = head
            continue
        od = sh.config_path(base, cfg.get("output_dir", ""))
        if od:
            os.makedirs(od, exist_ok=True)
            with open(os.path.join(od, "%s-%s.log" % (train or "train", name.replace("/", "_"))), "w") as f:
                f.write(out)
        evidence = "pre-flight step %s failed (exit %d, %.0fs):\n%s" % (name, rc, secs, tail(out))
        blamed = []
        mode = blame_mode(step)
        if mode == "none":
            evidence += "\nblame: none for this step (the judge decides)"
        elif mode == "bisect" and order:
            lid, why = bisect(step, doc, order, timeout)
            sh.log("pre-flight: bisection of %s: %s" % (name, why))
            evidence += "\nbisection: " + why
            if lid is not None:
                blamed = [lid]
        else:
            try:
                tips = doc.get("tips") or {}
                files = sh.files_of_tips(doc["dir"], doc["base"], order, tips)
                rules = sh.BlameRules(cfg.get("blame"))
                added = sh.added_of_tips(doc["dir"], doc["base"], order, tips) if rules.wants_added() else None
                blamed, why = rules.blame(out, order, files, None, added,
                                          lambda name, specs: sh.absent_at(doc["dir"], doc["base"], name, specs))
                for w in why:
                    sh.log("pre-flight blame:", w)
                if blamed:
                    evidence += "\nblame: " + "; ".join(why)
            except sh.GitError as e:
                sh.log("pre-flight: no blame: %s" % e)
        if blamed and answer_mode(step) == "leave-out":
            why = "pre-flight step %s failed with it in the train (exit %d):\n%s" % (name, rc, tail(out, 5))
            sh.answer({"pass": False, "left_out": {lid: why[:1000] for lid in blamed}, "evidence": evidence[:4000]})
        sh.answer({"pass": False, "blamed": blamed, "evidence": evidence[:4000]})
    sh.answer({"pass": True})


if __name__ == "__main__":
    main()
