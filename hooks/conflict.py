#!/usr/bin/env python3
"""Shunter's example conflict hook: a conflict only in generated files is
resolved by regenerating them; any other conflict is a conflict.

Shunter runs it in its clone, in the middle of the replay or the merge that
conflicted (see [hooks.conflict] in examples/shunter.toml):

  in:  {"dir", "train", "line", "change", "files": ["<conflicted path>", ...]}
  out: {"resolved": true}, with every resolution staged,
       or {"resolved": false, "reason": "..."}

A file is generated when the repository marks it with a git attribute, by
default `merge=regenerate` in .gitattributes:

  api/gen/*.go      merge=regenerate
  web/src/gen/**    merge=regenerate

Every conflicted file must be marked, or nothing is resolved and the line is
left out of the train with the files named. Marked files take the train's
side ("ours", in a replay and in a merge alike), then the configured command
regenerates them from the sources as merged, and the generated files it
changed are staged. Nothing else is: a change the command makes to a file
that is not marked is undone, and said on stderr.

So that a generated file always surfaces as a conflict, and is never
silently merged as text, give the attribute's merge driver as `false` in
git's environment for Shunter's clone ([git] env in shunter.toml):

  env = ["GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=merge.regenerate.driver",
         "GIT_CONFIG_VALUE_0=false"]

Configuration (JSON, --config FILE):

  {"attribute": "merge", "value": "regenerate",
   "command": ["make", "generate"], "timeout": "10m"}
"""

import os
import subprocess
import sys

# Shunter runs a hook in its clone: never write __pycache__ beside it.
sys.dont_write_bytecode = True
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import shunterhooks as sh  # noqa: E402


def marked(d, attr, value, paths):
    """The paths whose attribute has the value."""
    if not paths:
        return set()
    out = sh.git(d, "check-attr", "-z", attr, "--", *paths)
    f = out.split("\0")
    got = set()
    for i in range(0, len(f) - 2, 3):
        if f[i + 2] == value:
            got.add(f[i])
    return got


def dirty(d):
    """Paths whose work tree differs from the index, and untracked paths."""
    mod = sh.git(d, "diff", "--name-only", "-z").split("\0")
    new = sh.git(d, "ls-files", "--others", "--exclude-standard", "-z").split("\0")
    return set(x for x in mod if x), set(x for x in new if x)


def has_stage(d, path, n):
    return subprocess.run(["git", "cat-file", "-e", ":%d:%s" % (n, path)], cwd=d,
                          stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL).returncode == 0


def main():
    cfg, base = sh.load_config(sys.argv)
    doc = sh.read_input()
    d = doc["dir"]
    attr, value = cfg.get("attribute", "merge"), cfg.get("value", "regenerate")
    files = list(doc.get("files") or [])
    if not files:
        sh.answer({"resolved": False, "reason": "no conflicted files were named"})
    gen = marked(d, attr, value, files)
    other = [f for f in files if f not in gen]
    if other:
        sh.answer({"resolved": False, "reason": "conflict in %s, not marked %s=%s: only generated files are resolved" %
                   (" ".join(other), attr, value)})
    for f in files:
        if has_stage(d, f, 2):
            sh.git(d, "checkout", "--ours", "--", f)
            sh.git(d, "add", "--", f)
        else:
            # The train's side has no such file: resolved as deleted, and
            # the command makes it again if the sources still generate it.
            sh.git(d, "rm", "--quiet", "--force", "--", f)
    _, untracked_before = dirty(d)
    cmd = cfg.get("command")
    if not cmd:
        sh.answer({"resolved": False, "reason": "no regenerate command is configured"})
    cmd = [sh.config_path(base, cmd[0]) if "/" in cmd[0] else cmd[0]] + list(cmd[1:])
    try:
        p = subprocess.run(cmd, cwd=d, stdin=subprocess.DEVNULL, stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
                           timeout=sh.parse_duration(cfg.get("timeout"), 600), start_new_session=True)
    except (subprocess.TimeoutExpired, OSError) as e:
        sh.answer({"resolved": False, "reason": "regenerating failed: %s" % e})
    if p.returncode != 0:
        tail = p.stdout.decode("utf-8", "replace").strip()[-600:]
        sh.answer({"resolved": False, "reason": "regenerating failed (exit %d): %s" % (p.returncode, tail)})
    mod, new = dirty(d)
    new -= untracked_before
    paths = sorted(mod | new)
    gen_now = marked(d, attr, value, paths) | set(files)
    undone = []
    for path in paths:
        if path in gen_now:
            sh.git(d, "add", "-A", "--", path)
            continue
        undone.append(path)
        if path in new:
            sh.git(d, "clean", "-fq", "--", path)
        else:
            sh.git(d, "checkout", "--", path)
    if undone:
        sh.log("conflict: the regenerate command changed files that are not generated; undone: %s" % " ".join(undone))
    left = sh.git(d, "diff", "--name-only", "--diff-filter=U")
    if left:
        sh.answer({"resolved": False, "reason": "still unmerged after regenerating: %s" % left.replace("\n", " ")})
    sh.log("conflict: regenerated %s" % " ".join(sorted(files)))
    sh.answer({"resolved": True})


if __name__ == "__main__":
    main()
