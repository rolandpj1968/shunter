"""Shared code of Shunter's example hooks: the JSON contract, git, durations,
and the blame rules that the pre-flight and blame hooks both use.

A hook reads one JSON document on stdin and answers one on stdout. Anything
else it prints goes to stderr, which Shunter keeps, bounded, for its log.
A hook that crashes, hangs or prints no JSON has said nothing: Shunter treats
that as no verdict, never as a charge against a line.

Standard library only, Python 3.8 or later.
"""

import json
import os
import re
import subprocess
import sys
import time

__all__ = [
    "load_config", "read_input", "answer", "log", "git", "GitError",
    "parse_duration", "now", "line_id", "line_changes", "BlameRules",
]


def log(*args):
    """Writes to stderr: the hook's log, never its answer."""
    print(*args, file=sys.stderr, flush=True)


def load_config(argv, default=None):
    """Reads the JSON configuration named by --config PATH.

    Relative paths inside it are resolved against the file's directory by
    the hooks that use them (see config_path)."""
    path = None
    i = 1
    while i < len(argv):
        a = argv[i]
        if a == "--config" and i + 1 < len(argv):
            path = argv[i + 1]
            i += 2
            continue
        if a.startswith("--config="):
            path = a.split("=", 1)[1]
        i += 1
    if path is None:
        if default is None:
            raise SystemExit("usage: %s --config FILE.json" % argv[0])
        return dict(default), os.getcwd()
    with open(path) as f:
        cfg = json.load(f)
    return cfg, os.path.dirname(os.path.abspath(path))


def config_path(base, p):
    """Resolves p against the configuration's directory."""
    if not p:
        return p
    p = os.path.expanduser(p)
    return p if os.path.isabs(p) else os.path.join(base, p)


def read_input():
    return json.load(sys.stdin)


def answer(doc, code=0):
    """Prints the one JSON answer and exits."""
    sys.stdout.write(json.dumps(doc) + "\n")
    sys.stdout.flush()
    sys.exit(code)


def now():
    """The time, or SHUNTER_HOOK_NOW (seconds since the epoch) in tests."""
    v = os.environ.get("SHUNTER_HOOK_NOW")
    return float(v) if v else time.time()


_UNITS = {"ms": 0.001, "s": 1, "m": 60, "h": 3600}


def parse_duration(v, default=None):
    """'90s', '10m', '1h30m' or a number of seconds."""
    if v is None or v == "":
        return default
    if isinstance(v, (int, float)):
        return float(v)
    total, pos = 0.0, 0
    for m in re.finditer(r"(\d+(?:\.\d+)?)(ms|s|m|h)", v):
        if m.start() != pos:
            raise ValueError("duration %r" % v)
        total += float(m.group(1)) * _UNITS[m.group(2)]
        pos = m.end()
    if pos != len(v) or pos == 0:
        raise ValueError("duration %r" % v)
    return total


class GitError(Exception):
    pass


def git(cwd, *args, check=True, env=None):
    """Runs git and returns its stdout, stripped."""
    e = dict(os.environ)
    e.update({"GIT_TERMINAL_PROMPT": "0", "LC_ALL": "C"})
    if env:
        e.update(env)
    p = subprocess.run(["git"] + list(args), cwd=cwd, env=e,
                       stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
    if check and p.returncode != 0:
        raise GitError("git %s: exit %d: %s" % (" ".join(args), p.returncode, p.stderr.strip()[:500]))
    return p.stdout.strip()


def line_id(line):
    """A line's id, in either spelling Shunter writes (Go's field name for
    the pre-flight's lines, "id" for blame's)."""
    return str(line.get("id", line.get("ID", "")))


def line_changes(line):
    out = []
    for c in line.get("changes", line.get("Changes", [])) or []:
        out.append({"id": str(c.get("ID", c.get("id", ""))), "head": c.get("Head", c.get("head", ""))})
    return out


# The default way to find a repository path in failure text: at least one
# slash and an extension, not part of a URL or an absolute path, with an
# optional :line. "./" is dropped.
DEFAULT_FILE = r"(?<![A-Za-z0-9_./:-])(?:\./)?((?:[A-Za-z0-9_.-]+/)+[A-Za-z0-9_.-]+\.[A-Za-z0-9]+)(?::\d+)?"
# A bare file name with a line number ("foo_test.go:88"), as test runners
# print the file of a failed assertion. Kept only when it is a test file.
DEFAULT_TEST_REF = r"(?<![A-Za-z0-9_./-])([A-Za-z0-9_.-]+\.[A-Za-z0-9]+):\d+"
DEFAULT_TEST_FILES = [r"_test\.go$", r"\.(spec|test)\.[cm]?[jt]sx?$", r"(^|/)test_[^/]+\.py$", r"_test\.py$"]


class BlameRules:
    """The old queue's blame by line, generic.

    The failure text names things: files (a path with a slash and an
    extension), directories (from configured patterns, for example a test
    runner's "FAIL <package>" line; with "subdirs", everything under one),
    path prefixes (a "prefix" pattern, often with a fixed "path", for a
    failure that names no file), test files by name, and things a change's
    description says (a "description" pattern). Each line of the train
    touches files, and may carry its changes' descriptions. Then, in order:

    0. The most specific rule (PROTOCOL.md §5.6): a line is named when its
       description matches what a "description" pattern finds in the
       failure. If that names some lines, not every line of several, they
       are the blame, and the rules below aren't asked.
    1. A line is named when it touches a file the failure names, or a file
       under a prefix it names.
    2. For each directory the failure names: if the failure also names
       test files by name, the lines touching one of those test files in
       the directory; else the lines touching the directory's other files
       (the code the test exercises); else the lines touching any file in it.
       If the failure names no test file there, the lines touching any file
       in it.
    3. If that names every line of a train of several, the narrower rule:
       a file the failure names that exactly one line touched names that
       line alone.

    A result that names no line, or every line of several, is no blame:
    the failure goes to J1, whose default splits the train in halves by
    line (bisection by line). A stack is one line, so it is never split.
    """

    KINDS = ("file", "dir", "test", "prefix", "description")

    def __init__(self, cfg):
        cfg = cfg or {}
        self.patterns = []
        self.subdirs = set()
        self.path_of = {}
        self.describes = []
        for p in cfg.get("patterns") or []:
            kind = p.get("kind", "file")
            if kind not in self.KINDS:
                raise ValueError("blame pattern %r: kind %r: one of %s" % (p.get("regex"), kind, ", ".join(self.KINDS)))
            rx = re.compile(p["regex"], re.M)
            if kind == "description":
                # The failure names something ({} in "describes", from the
                # regex's group); a line whose description matches is named.
                self.describes.append((rx, int(p.get("group", 1)), p.get("describes", "{}")))
                continue
            # A pattern with a fixed "path" names that path whenever it
            # matches, for failures that name no file (a count below its
            # floor names the directory the count is of).
            group = int(p.get("group", 1 if rx.groups else 0))
            self.patterns.append((rx, kind, group, p.get("strip", ""), p.get("suffix", "")))
            if p.get("path"):
                self.path_of[rx.pattern] = p["path"]
            if kind == "dir" and p.get("subdirs"):
                self.subdirs.add(rx.pattern)
        if cfg.get("default_patterns", True):
            self.patterns.append((re.compile(DEFAULT_FILE, re.M), "file", 1, "", ""))
            self.patterns.append((re.compile(DEFAULT_TEST_REF, re.M), "test", 1, "", ""))
        self.test_files = [re.compile(x) for x in (cfg.get("test_files") or DEFAULT_TEST_FILES)]
        self.flaky = [re.compile(x, re.M) for x in cfg.get("flaky") or []]
        self.deterministic = [re.compile(x, re.M) for x in cfg.get("deterministic") or []]
        self.timing = [re.compile(x, re.M) for x in cfg.get("timing") or []]

    def is_test(self, path):
        return any(r.search(path) for r in self.test_files)

    def refs(self, text):
        """The files, directories (each with whether its subdirectories
        count), path prefixes and test-file names a failure names."""
        files, dirs, tests, prefixes = set(), set(), set(), set()
        for rx, kind, group, strip, suffix in self.patterns:
            for m in rx.finditer(text):
                v = self.path_of.get(rx.pattern) or m.group(group)
                if not v:
                    continue
                if strip and v.startswith(strip):
                    v = v[len(strip):]
                v = v.strip("/") + suffix
                if kind == "prefix":
                    prefixes.add(v)
                elif kind == "dir":
                    dirs.add((v, rx.pattern in self.subdirs))
                elif kind == "test":
                    if self.is_test(v):
                        tests.add(os.path.basename(v))
                else:
                    files.add(v)
                    if self.is_test(v):
                        tests.add(os.path.basename(v))
        return files, dirs, tests, prefixes

    def described(self, text, order, texts_of):
        """The lines whose description matches what the failure names, by
        the "description" patterns: the most specific rule (§5.6), a rule a
        change's description breaks. Returns (named, why)."""
        named, why = [], []
        for rx, group, tmpl in self.describes:
            for m in rx.finditer(text):
                v = m.group(group)
                if not v:
                    continue
                want = re.compile(tmpl.replace("{}", re.escape(v)), re.I | re.M)
                for lid in order:
                    if want.search(texts_of.get(lid) or "") and lid not in named:
                        named.append(lid)
                        why.append("line %s: its description matches %r, which the failure names" % (lid, v))
        return named, why

    def kind(self, text):
        """flaky or deterministic, as the old queue judged a failure: a
        timing pattern makes it flaky; else a deterministic pattern makes it
        deterministic; else a flaky pattern makes it flaky."""
        if any(r.search(text) for r in self.timing):
            return "flaky"
        if any(r.search(text) for r in self.deterministic):
            return "deterministic"
        if any(r.search(text) for r in self.flaky):
            return "flaky"
        return "deterministic"

    def blame(self, text, order, files_of, texts_of=None):
        """order is the train's line ids in queue order; files_of maps each
        to the set of files it touches, and texts_of, optionally, to its
        changes' descriptions. Returns (blamed, why)."""
        if texts_of:
            hit, why = self.described(text, order, texts_of)
            if hit and not (len(order) > 1 and len(hit) == len(order)):
                return [lid for lid in order if lid in hit], why
        files, dirs, tests, prefixes = self.refs(text)
        named, why = [], []

        def add(lid, reason):
            if lid not in named:
                named.append(lid)
            why.append("line %s: %s" % (lid, reason))

        for lid in order:
            for f in sorted(files_of.get(lid, ())):
                if f in files:
                    add(lid, "touches %s, which the failure names" % f)
                    break
        for p in sorted(prefixes):
            for lid in order:
                hit = sorted(f for f in files_of.get(lid, ()) if f == p or f.startswith(p + "/"))
                if hit:
                    add(lid, "touches %s, under %s, which the failure names" % (hit[0], p))
        for d, sub in sorted(dirs):
            def within(f):
                return os.path.dirname(f) == d or (sub and f.startswith(d + "/"))

            def touching(pred):
                return [lid for lid in order if any(within(f) and pred(f) for f in files_of.get(lid, ()))]
            if tests:
                t1 = touching(lambda f: os.path.basename(f) in tests and self.is_test(f))
                t2 = touching(lambda f: not self.is_test(f))
                t3 = touching(lambda f: True)
                hit, rule = (t1, "touches the failing test's file in %s" % d) if t1 else \
                    (t2, "touches %s's code, not its failing test's file" % d) if t2 else \
                    (t3, "touches a file of %s" % d)
            else:
                hit, rule = touching(lambda f: True), "touches a file of %s" % d
            for lid in hit:
                add(lid, rule)
        if len(order) > 1 and len(named) == len(order):
            only = []
            for f in sorted(files):
                who = [lid for lid in order if f in files_of.get(lid, ())]
                if len(who) == 1 and who[0] not in only:
                    only.append(who[0])
                    why.append("line %s alone touches %s, which the failure names" % (who[0], f))
            if only and len(only) < len(order):
                return [lid for lid in order if lid in only], why
        if not named:
            return [], ["the failure names no file, directory or description of a line of the train"]
        if len(order) > 1 and len(named) == len(order):
            return [], why + ["blame names every line: no blame (J1 splits the train by line)"]
        return [lid for lid in order if lid in named], why


def files_between(cwd, a, b):
    """The files that differ between two commits."""
    out = git(cwd, "diff", "--name-only", "--no-renames", a, b)
    return set(x for x in out.splitlines() if x)


def files_of_tips(cwd, base, order, tips):
    """Each line's files in the train: the difference from the tip before."""
    out, prev = {}, base
    for lid in order:
        tip = tips.get(lid)
        if not tip:
            raise GitError("no tip for line %s" % lid)
        out[lid] = files_between(cwd, prev, tip)
        prev = tip
    return out
