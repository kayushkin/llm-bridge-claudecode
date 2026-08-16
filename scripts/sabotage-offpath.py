"""Sabotage-score discover.go's numbers OFF the truncation path.

`sabotage-truncation.py` scores the cut and only the cut: its `-run` filter names
five tests, all about `truncateAtRuneBoundary`. Everything else in this file --
the subagent-path classifier, the "latest rollout" index, the scanner ceiling,
three emptiness guards -- carries numbers no mutation in that scorer moves. This
harness scores those.

Same shape as `sabotage-offpath.py` in logstack (the 188th pass) and found the
same way: after closing the scored target, read what the `-run` filter leaves out.

Two things this harness is careful about, inherited from the truncation scorer
and re-earned here:

  * CAUGHT is split into assertion-fired and guard-fired. Most tests in
    discover_test.go build a temp CC home and a state.db first, and every one of
    those setup steps is a t.Fatalf. A mutation that makes the fixture fall over
    is not a mutation the suite detected.

  * Known-NEGATIVE controls are first-class rows. Five of the numbers in this
    file are dominated -- something downstream already rejects the input the
    mutation lets through -- so moving them is behaviour-preserving. A scorer
    that reports CAUGHT for those is reporting CAUGHT for everything, and a
    scorer that files them as holes reports gaps nothing can ever close.

Every CONTROL row below was probed directly before it was written, not reasoned
about (the 185th/186th/188th all found a cheap direct measurement changing what
an inference claimed):

    bufio.Scanner grows its buffer from the initial size up to the max, so the
    256*1024 initial cap is dominated by the 1024*1024 max: 256*1024 -> 1024,
    -> 0, even -> 2MB all scan identically at every line length. Only the max
    moves anything, and it rejects a line of exactly max bytes (measured:
    max=1048576 accepts 1048575, rejects 1048576).

    json.Unmarshal into BOTH struct targets here fails on "" and on any 1-byte
    input ("1" and "0" parse as JSON but not into a struct), so widening the
    len()==0 guards to ==1 only ever skips a line that the very next statement
    would have discarded anyway.

    len(msgObj.Content) > 0 -> > 1 is dominated for the same reason from the
    other side: a 1-byte Content is a bare number, which fails both the string
    and the block-array unmarshal, and the function returns "" either way.

Run from anywhere:  python3 scripts/sabotage-offpath.py
"""

import os
import pathlib
import re
import signal
import subprocess
import sys

REPO = pathlib.Path(__file__).resolve().parent.parent

SHALLOW = "if subIdx < 2 {"
RETURN = "return source, ccProjectToPath(parts[subIdx-2]), parts[subIdx-1], true"
LOOKAHEAD = 'if subIdx+1 < len(parts) && parts[subIdx+1] == "workflows" {'
BUFFER = "scanner.Buffer(make([]byte, 0, 256*1024), 1024*1024)"

CASES = [
    # (label, file, old, new, expect_caught)

    # ---- classifySubagentPath ---------------------------------------------
    # Four numbers decide which directory becomes the project and which becomes
    # the parent session id. The doc comment records that this exact expression
    # once read the wrong offset and dropped every subagent's lineage, so this
    # is the historically-real defect this family exists to catch.
    ("the shallowness guard rejects one level deeper than it should",
     "discover.go", SHALLOW, "if subIdx < 3 {", True),
    ("the shallowness guard admits a path with no room for the project, and the "
     "negative index panics",
     "discover.go", SHALLOW, "if subIdx < 1 {", True),
    ("the project is read one segment too high",
     "discover.go", RETURN,
     "return source, ccProjectToPath(parts[subIdx-3]), parts[subIdx-1], true", True),
    ("the parent is read from the project's segment -- the historical defect, "
     "which dropped every subagent's lineage",
     "discover.go", RETURN,
     "return source, ccProjectToPath(parts[subIdx-2]), parts[subIdx-2], true", True),
    ("the workflows lookahead reads one segment too far, so the deeper layout "
     "is classified as a plain subagent",
     "discover.go", LOOKAHEAD,
     'if subIdx+1 < len(parts) && parts[subIdx+2] == "workflows" {', True),
    ("the lookahead's bounds check stops protecting the index it guards",
     "discover.go", LOOKAHEAD,
     'if subIdx+1 <= len(parts) && parts[subIdx+1] == "workflows" {', True),

    # ---- buildStoredSession ----------------------------------------------
    # "latest" is an index, and an index is a boundary.
    ("the latest rollout becomes the earliest one",
     "discover.go", "latest := rollouts[len(rollouts)-1]", "latest := rollouts[0]", True),
    ("the empty-rollouts guard swallows a session that has exactly one rollout",
     "discover.go", "if len(rollouts) == 0 {", "if len(rollouts) <= 1 {", True),

    # ---- parseSessionHead's scanner ceiling ------------------------------
    # The line cap that ships. A JSONL line over it ends the scan; the
    # unchecked scanner.Err() half of that is recorded on 603e3ded, not here.
    # This row asks only whether anything pins the number.
    ("the scanner's line ceiling shrinks by one byte",
     "discover.go", BUFFER,
     "scanner.Buffer(make([]byte, 0, 256*1024), 1024*1024-1)", True),

    # ---- known-NEGATIVE controls -----------------------------------------
    # Each of these is dominated: something downstream already discards the
    # input the mutation lets through. Moving them is behaviour-preserving, so
    # UNNOTICED is the correct verdict and CAUGHT would mean this harness is
    # firing on noise. Probed directly -- see the module docstring.
    ("CONTROL (dominated): the not-found sentinel changes from -1 to 0, which "
     "the shallowness guard rejects either way",
     "discover.go", "subIdx := -1", "subIdx := 0", False),
    ("CONTROL (dominated): the scanner's INITIAL buffer shrinks; bufio grows it "
     "to the max regardless",
     "discover.go", BUFFER,
     "scanner.Buffer(make([]byte, 0, 1024), 1024*1024)", False),
    ("CONTROL (dominated): the blank-line skip widens to 1 byte, which no "
     "struct unmarshal would have accepted",
     "discover.go", "if len(line) == 0 {", "if len(line) == 1 {", False),
    ("CONTROL (dominated): the empty-message guard widens to 1 byte, and the "
     "unmarshal below rejects it anyway",
     "discover.go", "if len(raw) == 0 {", "if len(raw) == 1 {", False),
    ("CONTROL (dominated): the content-present check widens to 1 byte, and a "
     "1-byte content parses as neither a string nor blocks",
     "discover.go", "len(msgObj.Content) > 0", "len(msgObj.Content) > 1", False),
]

# Anchored. Unanchored "TestDiscover" also matches TestDiscoveredPromptStaysValidUTF8,
# which belongs to the truncation scorer -- borrowing another scorer's tests would
# credit this one for coverage it did not measure.
TESTS = ("^(TestDiscover_|TestColdImport_|TestClassifySubagentPath|"
         "TestBuildStoredSession|TestParseSessionHead)")

# Messages from fixture guards rather than from an assertion about the value
# under test. These tests build a temp CC home and a state.db before they can
# assert anything, and every step of that is a t.Fatalf.
GUARD_MARKERS = (
    "mkdir:",
    "mkdir projects:",
    "write ",
    "discoverSessions:",
    "discoverSessions A:",
    "discoverSessions B:",
    "discoverSessions empty:",
    "discoverSessions on empty home:",
    "second discoverSessions:",
    "OpenState:",
    "UpsertSession:",
    "InsertRollout:",
    "coldImportRollouts:",
    "AllSessions:",
    "ListRollouts:",
    "GetSession:",
)

FAIL_LINE = re.compile(r"^\s*\w*_?test\.go:\d+: (.*)$", re.M)
# A stack frame naming a .go file. The full path is captured because the Go
# runtime's own frames (runtime/panic.go) would otherwise pass a bare-filename
# filter and be mistaken for our source.
FRAME = re.compile(r"^\s+(/\S+\.go):(\d+)", re.M)


def classify(output):
    """Return (verdict, detail) for one sabotage run's output."""
    if "[build failed]" in output or "declared and not used" in output:
        return "COMPILE ERROR", "mutation orphaned an identifier -- rewrite it"
    if "panic:" in output:
        # Read WHERE the panic is, not just that there is one. A mutation that
        # crashes production code the test drove it into IS detection -- the
        # program died instead of returning a wrong answer. A panic in the
        # fixture is the test falling over before it asserted anything.
        frames = [f for f, _ in FRAME.findall(output.split("panic:", 1)[1])
                  if f.startswith(str(REPO) + "/")]
        source = next((f for f in frames if not f.endswith("_test.go")), None)
        if source:
            return ("CAUGHT (panic in %s)" % pathlib.Path(source).name,
                    "test drove the mutation into a crash")
        return "CAUGHT (panic in fixture -- NOT coverage)", "the test fell over before asserting"
    if "--- FAIL" not in output:
        return "UNNOTICED", ""
    messages = FAIL_LINE.findall(output)
    if not messages:
        return "CAUGHT (no assertion text -- NOT coverage)", ""
    guard = [m for m in messages if any(g in m for g in GUARD_MARKERS)]
    real = [m for m in messages if m not in guard]
    if not real:
        return "CAUGHT (guard only -- NOT coverage)", guard[0][:90]
    return "CAUGHT", real[0][:90]


def self_test():
    """Exercise classify() directly.

    The forty-third pass's rule: when you automate a check, the check is the
    next unmeasured claim. A classify() that can never return "guard only"
    prints the clean score you were hoping for.
    """
    probes = [
        ("--- FAIL: X\n    discover_test.go:318: Task subagent project = \"/x\", want /tmp/proj\n",
         "CAUGHT"),
        ("--- FAIL: X\n    discover_test.go:299: discoverSessions: boom\n",
         "CAUGHT (guard only -- NOT coverage)"),
        ("ok  \tgithub.com/x\n", "UNNOTICED"),
        ("panic: index out of range\n"
         "\t/usr/lib/go/src/runtime/panic.go:860 +0x13a\n"
         "\t%s/discover.go:266\n" % REPO, "CAUGHT (panic in discover.go)"),
        ("panic: index out of range\n"
         "\t/usr/lib/go/src/runtime/panic.go:860 +0x13a\n"
         "\t%s/discover_test.go:149\n" % REPO,
         "CAUGHT (panic in fixture -- NOT coverage)"),
        ("# github.com/x [build failed]\n", "COMPILE ERROR"),
    ]
    ok = True
    for output, want in probes:
        got, _ = classify(output)
        if got != want:
            print(f"  classifier SELF-TEST FAIL: got {got!r}, want {want!r}")
            ok = False
    print(f"  classifier self-test: {'all 5 verdicts reachable' if ok else 'BROKEN'}")
    return ok


# Every file any row in CASES mutates, derived from the table so it cannot drift
# from it. The truncation scorer had this hardcoded to one filename while its
# table carried a per-row file, and the first row naming a second file would
# have left that file mutated after the run (fixed by the 187th).
MUTATED_FILES = sorted({fname for _, fname, _, _, _ in CASES})


# `restore()` below is `git checkout --`, so it puts these files back to HEAD. It
# cannot tell a mutation this scorer wrote from work somebody has not committed
# yet, and it runs at the TOP of every case, before anything is read. So scoring a
# fix that is written but not yet committed deletes the fix and scores HEAD.
#
# The symptom accuses the wrong file. Every case then prints
# `SETUP FAIL: pattern not found`, which reads as a stale case list — so the
# obvious next move is to edit the case list, against a source file the scorer has
# already reverted. Nothing in that output mentions the checkout. Measured in
# memory-store by the 239th nightly pass: eight rows read SETUP FAIL and the ninth
# read ok, and the loss was found by being bitten rather than by reading.
#
# The shared engine (tool-store scripts/sabotage.py) has refused this for a long
# time. This scorer does not import the engine, so it never inherited the refusal.
_dirty = subprocess.run(["git", "status", "--porcelain", "--"] + MUTATED_FILES,
                        cwd=REPO, capture_output=True, text=True,
                        check=True).stdout.strip()
if _dirty:
    sys.exit("REFUSING: these have uncommitted changes; this harness restores "
             "from git and would delete them:\n%s" % _dirty)


def restore():
    subprocess.run(["git", "checkout", "--"] + MUTATED_FILES, cwd=REPO, check=True)


print("Sabotaging discover.go's numbers OFF the truncation path\n")
if not self_test():
    sys.exit(2)
print()

# The file under test holds a deliberately broken version of itself from the
# write below until the next restore. A try/finally alone does NOT close that
# window: Python raises KeyboardInterrupt for SIGINT, so a finally is on the way
# out for that one and for nothing else. SIGTERM and SIGHUP -- what a wall-clock
# cap, systemd and a process-group kill actually send -- kill the process between
# the write and the restore and leave the mutated file behind, looking exactly
# like ordinary uncommitted work.
#
# The handler restores, reinstates the disposition it replaced and re-raises, so
# the process dies BY the signal (rc 128+signum). A handler that restores and
# exits 0 tells every caller a killed run succeeded.
#
# SIGKILL cannot be caught by the process that receives it. That is the one gap
# left here, and it is named rather than papered over.
_previous_handlers = {}


def _restore_and_reraise(signum, frame):
    restore()
    signal.signal(signum, _previous_handlers[signum])
    os.kill(os.getpid(), signum)


for _sig in (signal.SIGINT, signal.SIGTERM, signal.SIGHUP):
    _previous_handlers[_sig] = signal.signal(_sig, _restore_and_reraise)

score = 0
try:
    for label, fname, old, new, expect in CASES:
        restore()
        p = REPO / fname
        text = p.read_text()
        if text.count(old) != 1:
            print(f"  SETUP FAIL   {label}\n      pattern appears {text.count(old)}x in {fname}, want 1")
            continue
        p.write_text(text.replace(old, new, 1))

        r = subprocess.run(["go", "test", "-count=1", "-run", TESTS, "."],
                           cwd=REPO, capture_output=True, text=True)
        verdict, detail = classify(r.stdout + r.stderr)

        caught = verdict.startswith("CAUGHT") and "NOT coverage" not in verdict
        ok = caught == expect
        score += ok
        want = "CAUGHT" if expect else "UNNOTICED"
        print(f"  {'ok  ' if ok else 'BAD '} {verdict:<34} (want {want:<9}) {label}")
        if detail:
            print(f"         -> {detail}")
finally:
    restore()
    for _sig, _handler in _previous_handlers.items():
        signal.signal(_sig, _handler)

reals = sum(1 for _, _, _, _, e in CASES if e)
print(f"\nscore {score}/{len(CASES)}   ({reals} real rows, {len(CASES) - reals} known-negative controls)")
sys.exit(0 if score == len(CASES) else 1)
