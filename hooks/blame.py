#!/usr/bin/env python3
"""Shunter's example blame hook: which lines of a failed train are to blame.

Shunter runs it when CI fails on a train's head, with the train's lines and
the check runs (see the [hooks.blame] section of examples/shunter.toml):

  in:  {"train", "head", "lines": [{"id", "changes": [{"ID", "Head"}], "text"}],
        "checks": [{"Name", "Conclusion", "URL", "Summary", ...}],
        "evidence"}
  out: {"class": "deterministic", "blamed": [...], "evidence": "..."}
       or {"class": "flaky" | "no-verdict", "why": "..."}

The failure's text is the evidence, the check runs' names and summaries,
and the output of `log_command` (the failed jobs' logs, say), which reads
the same input on its stdin. Each line's files are its own "files", when
Shunter or a wrapper gives them, or else what its last change's head
changes since it left trunk, read with git in `git_dir`.

A line's "text" is its changes' titles and descriptions, as Shunter reads
them from the forge; "description" patterns match against it.

The rules are BlameRules's, in shunterhooks.py. A failure that names no
line, or every line of several, answers blamed: [] and Shunter's J1 decides
(its default splits the train in halves by line). Nothing is judged on what
could not be read: a log or a file list that can't be read answers
"no-verdict", and the train's lines go back unjudged.

Configuration (JSON, --config FILE):

  {
    "git_dir": "/var/lib/shunter/clone",      # a clone with the changes
    "trunk_ref": "refs/remotes/origin/main",
    "fetch": ["origin"],                       # fetch a head that is missing
    "log_command": ["./fetch-failed-logs"],    # optional
    "log_timeout": "2m",
    "patterns": [{"regex": "^FAIL\\s+example.com/mod/(\\S+)\\s", "kind": "dir", "subdirs": false},
                 {"regex": "below its floor", "kind": "prefix", "path": "locales"},
                 {"regex": "issue #(\\d+) is still listed as open", "kind": "description",
                  "describes": "(closes|fixes) #{}\\b"}],
    "test_files": ["_test\\.go$"],
    "timing": ["panic: test timed out"], "deterministic": ["Error Trace:"],
    "flaky": ["connection reset"]
  }
"""

import json
import os
import re
import subprocess
import sys

# Shunter runs a hook in its clone: never write __pycache__ beside it.
sys.dont_write_bytecode = True
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import shunterhooks as sh  # noqa: E402


def failure_text(cfg, base, doc):
    parts = [doc.get("evidence") or ""]
    for c in doc.get("checks") or []:
        if c.get("Conclusion") not in (None, "", "success", "skipped", "neutral"):
            parts.append("%s: %s %s" % (c.get("Name", ""), c.get("Conclusion", ""), c.get("Summary", "")))
    cmd = cfg.get("log_command")
    if cmd:
        cmd = [sh.config_path(base, cmd[0]) if "/" in cmd[0] else cmd[0]] + list(cmd[1:])
        p = subprocess.run(cmd, input=_json(doc), stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True,
                           timeout=sh.parse_duration(cfg.get("log_timeout"), 120))
        if p.returncode != 0:
            raise sh.GitError("log_command exited %d: %s" % (p.returncode, p.stderr.strip()[-300:]))
        parts.append(p.stdout)
    return "\n".join(parts)


def _json(doc):
    return json.dumps(doc)


def line_files(cfg, base, doc, order):
    out = {}
    need = []
    for l in doc.get("lines") or []:
        lid = sh.line_id(l)
        if "files" in l:
            out[lid] = set(l["files"] or [])
        else:
            need.append(l)
    if not need:
        return out
    gd = sh.config_path(base, cfg.get("git_dir", ""))
    if not gd:
        raise sh.GitError("the lines carry no files and no git_dir is configured")
    trunk = cfg.get("trunk_ref", "refs/remotes/origin/main")
    for l in need:
        ch = sh.line_changes(l)
        if not ch or not ch[-1]["head"]:
            raise sh.GitError("line %s has no head" % sh.line_id(l))
        head = ch[-1]["head"]
        if not has_commit(gd, head):
            for remote in cfg.get("fetch") or []:
                sh.git(gd, "fetch", "--quiet", "--no-tags", remote, head, check=False)
        mb = sh.git(gd, "merge-base", trunk, head)
        out[sh.line_id(l)] = sh.files_between(gd, mb, head)
    return out


def has_commit(gd, sha):
    return subprocess.run(["git", "cat-file", "-e", sha + "^{commit}"], cwd=gd,
                          stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL).returncode == 0


def main():
    cfg, base = sh.load_config(sys.argv, default={})
    doc = sh.read_input()
    order = [sh.line_id(l) for l in doc.get("lines") or []]
    try:
        rules = sh.BlameRules(cfg)
    except (ValueError, KeyError, re.error) as e:
        raise SystemExit("blame: the configuration: %s" % e)
    try:
        text = failure_text(cfg, base, doc)
        files = line_files(cfg, base, doc, order)
    except (sh.GitError, OSError, subprocess.SubprocessError) as e:
        sh.answer({"class": "no-verdict", "why": "blame could not read what it judges by: %s" % e})
    if rules.kind(text) == "flaky":
        sh.answer({"class": "flaky", "why": "the failure shows only timing or connection errors"})
    texts = {sh.line_id(l): l.get("text") or "" for l in doc.get("lines") or []}
    blamed, why = rules.blame(text, order, files, texts)
    for w in why:
        sh.log("blame:", w)
    ev = (doc.get("evidence") or "a check failed").strip()
    if blamed:
        ev = ("%s; blame: %s" % (ev, "; ".join(why)))[:2000]
    sh.answer({"class": "deterministic", "blamed": blamed, "evidence": ev})


if __name__ == "__main__":
    main()
