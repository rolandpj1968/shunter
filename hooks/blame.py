#!/usr/bin/env python3
"""Shunter's example blame hook: which lines of a failed train are to blame.

Shunter runs it when CI fails on a train's head, with the train's lines and
the check runs (see the [hooks.blame] section of examples/shunter.toml):

  in:  {"train", "head", "lines": [{"id", "changes": [{"ID", "Head"}], "text"}],
        "checks": [{"Name", "Conclusion", "URL", "Summary", ...}],
        "evidence", "force_class"}
  out: {"class": "deterministic", "blamed": [...], "evidence": "..."}
       or {"class": "flaky" | "no-verdict", "why": "..."}

The failure's text is the evidence, the check runs' names and summaries,
and the output of `log_command` (the failed jobs' logs, say), which reads
the same input on its stdin. Each line's files are its own "files", when
Shunter or a wrapper gives them, or else what its last change's head
changes since it left trunk, read with git in `git_dir`.

"force_class", when Shunter sets it, is the class the failure is: today
only "deterministic", when a known flake's or a timing failure's one rerun
failed again (PROTOCOL.md §5.5). The hook then doesn't ask whether the
failure is a flake: it only names the lines, by the rules below, and
answers that class. A log or a file list that can't be read still answers
"no-verdict", which Shunter reads as naming nobody.

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
    "flaky": ["connection reset"],
    "seeded": ["\\b(?:[A-Z][A-Z0-9_]*_)?SEED=\\S+"]  # the default
  }

A failure whose text names a seed ("seeded") is deterministic whatever else
it shows, and its evidence leads with the seeds, so the failure can be
filed and reproduced: a rerun draws another seed, and its pass proves
nothing (PROTOCOL.md §5.5).
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


def line_added(cfg, base, doc):
    """Each line's added lines (its "added", or read with git as its files
    are), and the test of a name new in the train (against trunk_ref; with
    no git_dir, every name is new)."""
    gd = sh.config_path(base, cfg.get("git_dir", ""))
    trunk = cfg.get("trunk_ref", "refs/remotes/origin/main")
    out = {}
    for l in doc.get("lines") or []:
        lid = sh.line_id(l)
        if "added" in l:
            out[lid] = l["added"] or ""
            continue
        if not gd:
            raise sh.GitError("line %s carries no added lines and no git_dir is configured" % lid)
        head = sh.line_changes(l)[-1]["head"]
        out[lid] = sh.added_between(gd, sh.git(gd, "merge-base", trunk, head), head)
    if not gd:
        return out, None
    return out, lambda name, specs: sh.absent_at(gd, trunk, name, specs)


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
    force = doc.get("force_class") or ""
    if force not in ("", "deterministic"):
        raise SystemExit("blame: force_class %r: only \"deterministic\" is known" % force)
    if not force and rules.kind(text) == "flaky":
        sh.answer({"class": "flaky", "why": "the failure shows only timing or connection errors"})
    texts = {sh.line_id(l): l.get("text") or "" for l in doc.get("lines") or []}
    added, is_new = None, None
    if rules.wants_added():
        try:
            added, is_new = line_added(cfg, base, doc)
        except (sh.GitError, OSError, subprocess.SubprocessError) as e:
            sh.answer({"class": "no-verdict", "why": "blame could not read the lines' added lines: %s" % e})
    blamed, why = rules.blame(text, order, files, texts, added, is_new)
    for w in why:
        sh.log("blame:", w)
    ev = (doc.get("evidence") or "a check failed").strip()
    seeds = rules.seeds(text)
    if seeds:
        ev = "seeded, never a flake (reproduce with %s); %s" % (" ".join(seeds), ev)
    if blamed:
        ev = "%s; blame: %s" % (ev, "; ".join(why))
    ev = ev[:2000]
    sh.answer({"class": "deterministic", "blamed": blamed, "evidence": ev})


if __name__ == "__main__":
    main()
