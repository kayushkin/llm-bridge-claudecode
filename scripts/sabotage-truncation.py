"""Sabotage-score the rune-boundary truncation tests.

A suite that passes tells you nothing on its own. This breaks the fix in six
ways and checks that the tests notice five of them and, just as importantly, do
NOT notice the sixth. A scorer with no known-negative control reports CAUGHT for
everything and looks perfect while measuring nothing.

Two things this harness is careful about, both learned the expensive way by
earlier passes of this sweep:

  * Mutations are written as drifted comparisons or substitutions that keep every
    identifier live. Deleting the walk-back orphans the `utf8` import, and
    `go test` runs vet, so the case would report a compile error instead of a
    score -- and a compile error hides whether any test would have caught the
    behaviour.

  * CAUGHT is split into assertion-fired and guard-fired. A test can go red
    because its own fixture blew up (t.Fatalf on a setup step) rather than
    because it detected the defect. That is not coverage, and counting it as
    coverage inflates the score.

Run from anywhere:  python3 scripts/sabotage-truncation.py
"""

import pathlib
import re
import subprocess
import sys

REPO = pathlib.Path(__file__).resolve().parent.parent

WALKBACK = "for cut > 0 && !utf8.RuneStart(s[cut]) {"
CALLSITE = "prompt = truncateAtRuneBoundary(prompt, 200)"

CASES = [
    # (label, file, old, new, expect_caught)
    ("the walk-back never runs, so the cut splits a rune again",
     "discover.go", WALKBACK, "for cut > len(s) && !utf8.RuneStart(s[cut]) {", True),
    ("the helper trims to nothing instead of to the boundary",
     "discover.go", "\treturn s[:cut]", "\treturn s[:cut*0]", True),
    ("the walk-back stops one byte short, leaving the rune split",
     "discover.go", "\treturn s[:cut]", "\treturn s[:cut+1]", True),
    ("the empty-budget guard stops rejecting a budget of zero",
     "discover.go", "if maxBytes <= 0 {", "if maxBytes < -1 {", True),
    # The call site, not the helper. The helper being correct does not prove the
    # caller uses it, and this is the mutation that says so.
    ("only the call site reverts to a plain byte cut",
     "discover.go", CALLSITE,
     "if len(prompt) > 200 {\n\t\t\t\t\t\tprompt = prompt[:200]\n\t\t\t\t\t}", True),
    # Known-NEGATIVE control. maxBytes==0 already returns "" via the s[:cut]
    # path with cut==0, so narrowing this guard is a behavioural no-op. A harness
    # that reports CAUGHT here is reporting CAUGHT for everything.
    ("CONTROL (no-op): the maxBytes guard narrows from <=0 to <0",
     "discover.go", "if maxBytes <= 0 {", "if maxBytes < 0 {", False),
]

TESTS = ("TestTruncateAtRuneBoundarySlidesTheCutAcrossEveryOffset|"
         "TestTruncateAtRuneBoundaryMixedWidths|"
         "TestTruncateAtRuneBoundaryEdgeCases|"
         "TestDiscoveredPromptStaysValidUTF8")

# Messages from fixture guards rather than from an assertion about truncation.
# A red run that shows only these is the test falling over, not detecting.
GUARD_MARKERS = (
    "discoverSessions:",
    "want 1 session",
    "marshal:",
    "the cut never landed inside a rune",
    "the known-negative control never ran",
)

FAIL_LINE = re.compile(r"^\s*truncate_test\.go:\d+: (.*)$", re.M)
# A stack frame naming a .go file, e.g. "\t/home/u/repo/discover.go:388". The
# full path is captured because the Go runtime's own frames (runtime/panic.go)
# would otherwise pass a bare-filename filter and be mistaken for our source.
FRAME = re.compile(r"^\s+(/\S+\.go):(\d+)", re.M)


def classify(output):
    """Return (verdict, detail) for one sabotage run's output."""
    if "[build failed]" in output or "declared and not used" in output:
        return "COMPILE ERROR", "mutation orphaned an identifier -- rewrite it"
    if "panic:" in output:
        # Read WHERE the panic is, not just that there is one. A mutation that
        # crashes production code the test drove it into IS detection -- the
        # program died instead of returning a wrong answer, and the test names
        # the input that did it. A panic in the fixture is the test falling
        # over before it asserted anything, which is not coverage.
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

    The forty-third pass's rule: when you automate a check, the check is the next
    unmeasured claim. A classify() that can never return "guard only" prints the
    clean score you were hoping for. Rather than trust that these branches are
    reachable, drive all five from synthetic output.
    """
    probes = [
        ("--- FAIL: X\n    truncate_test.go:40: result is not valid UTF-8\n", "CAUGHT"),
        ("--- FAIL: X\n    truncate_test.go:99: want 1 session, got 0\n",
         "CAUGHT (guard only -- NOT coverage)"),
        ("ok  \tgithub.com/x\n", "UNNOTICED"),
        # Both panic branches, distinguished only by which file the top repo
        # frame names. The runtime's own panic.go frame is present in each and
        # must not be mistaken for our source.
        ("panic: slice bounds out of range\n"
         "\t/usr/lib/go/src/runtime/panic.go:860 +0x13a\n"
         "\t%s/discover.go:388\n" % REPO, "CAUGHT (panic in discover.go)"),
        ("panic: slice bounds out of range\n"
         "\t/usr/lib/go/src/runtime/panic.go:860 +0x13a\n"
         "\t%s/truncate_test.go:149\n" % REPO,
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


def restore():
    subprocess.run(["git", "checkout", "--", "discover.go"], cwd=REPO, check=True)


print("Sabotaging the rune-boundary truncation fix in llm-bridge-claudecode\n")
if not self_test():
    sys.exit(2)
print()

score = 0
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

restore()
print(f"\nscore {score}/{len(CASES)}")
sys.exit(0 if score == len(CASES) else 1)
