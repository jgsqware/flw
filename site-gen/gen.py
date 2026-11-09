#!/usr/bin/env python3
"""Builds site/assets/flw-data.js from real flw runs against the fake glab (fixtures.py).

Each screen is captured from tmux with its colors, turned into runs of [class, text],
deduplicated into a line pool, and stored as frames for the players on the page.
With --rex, it also runs flw in a background Rex tab to record the real Activity records
and desktop notifications it emits.
"""
import json
import os
import re
import subprocess
import sys
import tempfile
import time
import base64
import unicodedata

HERE = os.path.dirname(os.path.abspath(__file__))
REPO = os.path.dirname(HERE)
FLW = os.path.join(REPO, "flw")
OUT = os.path.join(REPO, "site", "assets", "flw-data.js")
W, H = 136, 24
SESSION = "flwsite"

# flw's Tokyo Night colors -> rack-room terminal classes (see the CSS in index.html)
FG = {(122, 162, 247): "t", (121, 162, 247): "t", (125, 207, 255): "t", (187, 154, 247): "v",
      (158, 206, 106): "g", (247, 118, 142): "r", (224, 175, 104): "y", (255, 158, 100): "a",
      (115, 122, 162): "d", (86, 95, 137): "d2", (192, 202, 245): "", (26, 27, 38): "k"}
BG = {(40, 52, 87): "Bs", (255, 158, 100): "Ba", (122, 162, 247): "Bt", (121, 162, 247): "Bt"}


def nearest(rgb, table):
    if rgb in table:
        return table[rgb]
    return min(table.items(), key=lambda kv: sum((a - b) ** 2 for a, b in zip(kv[0], rgb)))[1]


SGR = re.compile(r"\x1b\[([0-9;:]*)m")


def runs_of(line):
    """ANSI line -> [[classes, text], ...]"""
    fg, bg, bold, out, pos = "", "", False, [], 0

    def push(text):
        if not text:
            return
        cls = " ".join(c for c in (fg, bg, "b" if bold else "") if c)
        if out and out[-1][0] == cls:
            out[-1][1] += text
        else:
            out.append([cls, text])

    for m in SGR.finditer(line):
        push(line[pos:m.start()])
        pos = m.end()
        p = [int(x) if x else 0 for x in re.split(r"[;:]", m.group(1) or "0")]
        i = 0
        while i < len(p):
            c = p[i]
            if c == 0:
                fg, bg, bold = "", "", False
            elif c == 1:
                bold = True
            elif c == 22:
                bold = False
            elif c == 39:
                fg = ""
            elif c == 49:
                bg = ""
            elif c in (38, 48) and i + 4 < len(p) + 0 and p[i + 1] == 2:
                rgb = tuple(p[i + 2:i + 5])
                if c == 38:
                    fg = nearest(rgb, FG)
                else:
                    bg = nearest(rgb, BG)
                i += 4
            elif c in (38, 48) and i + 2 < len(p) and p[i + 1] == 5:
                i += 2
            i += 1
    push(line[pos:])
    out = split_wide(out)
    # drop trailing plain spaces
    while out and not out[-1][0] and not out[-1][1].strip():
        out.pop()
    if out and not out[-1][0]:
        out[-1][1] = out[-1][1].rstrip()
    return out


def is_wide(c):
    return unicodedata.east_asian_width(c) in ("W", "F")


def split_wide(runs):
    """Give each double-width character (⏳, 💤…) its own run with class w2, so the page can
    give it exactly two cells, as the terminal does."""
    out = []
    for cls, text in runs:
        buf = ""
        for ch in text:
            if ch == "\ufe0f":
                continue
            if is_wide(ch):
                if buf:
                    out.append([cls, buf])
                    buf = ""
                out.append([(cls + " w2").strip(), ch])
            else:
                buf += ch
        if buf:
            out.append([cls, buf])
    return out


class Recorder:
    def __init__(self):
        self.pool, self.index = [], {}

    def line_id(self, runs):
        k = json.dumps(runs, ensure_ascii=False)
        if k not in self.index:
            self.index[k] = len(self.pool)
            self.pool.append(runs)
        return self.index[k]


def tmux(*a):
    return subprocess.run(["tmux", *a], check=False, capture_output=True, text=True).stdout


def start():
    tmux("kill-session", "-t", SESSION)
    cmd = f"bash -c 'source {HERE}/demo-env.sh; cd /tmp; exec {FLW}'"
    tmux("new-session", "-d", "-s", SESSION, "-x", str(W), "-y", str(H), cmd)
    wait_for("on you")


def screen():
    return tmux("capture-pane", "-e", "-p", "-t", SESSION).rstrip("\n").split("\n")[:H]


def plain():
    return tmux("capture-pane", "-p", "-t", SESSION)


def wait_for(text, timeout=10):
    end = time.time() + timeout
    while time.time() < end:
        if text in plain():
            time.sleep(0.25)
            return
        time.sleep(0.1)
    raise SystemExit(f"timeout waiting for {text!r}:\n{plain()}")


def keys(*k, pause=0.35):
    for x in k:
        tmux("send-keys", "-t", SESSION, x)
        time.sleep(0.12)
    time.sleep(pause)


class Movie:
    """A sequence of frames sharing one line pool."""

    def __init__(self):
        self.rec = Recorder()
        self.frames = []

    def snap(self, hold=1):
        ids = [self.rec.line_id(runs_of(l)) for l in screen()]
        self.frames.extend([ids] * hold)

    def data(self):
        return {"pool": self.rec.pool, "frames": self.frames, "cols": W}


def goto(iid):
    """move the cursor to the row of !iid on the dashboard"""
    keys("g", pause=0.2)
    for _ in range(20):
        sel = [l for l in plain().split("\n") if "▌!" in l]
        if sel and f"▌!{iid} " in sel[0]:
            return
        keys("j", pause=0.15)
    raise SystemExit(f"row !{iid} not found")


def record():
    data = {}

    # hero: dashboard, cursor moves, focus "on me", open an MR, back
    start()
    m = Movie()
    m.snap(10)
    for _ in range(3):
        keys("j", pause=0.15)
        m.snap(3)
    keys("g", pause=0.2)
    m.snap(3)
    keys("t")
    m.snap(10)
    keys("t")
    m.snap(10)
    for _ in range(4):
        keys("t", pause=0.15)
    wait_for("showing: all")
    m.snap(4)
    keys("Enter")
    wait_for("YOUR TURN")
    m.snap(16)
    keys("Escape")
    wait_for("on you")
    m.snap(4)
    data["hero"] = m.data()

    # dashboard tab: the focus modes
    m = Movie()
    keys("g")
    m.snap(10)
    for _ in range(5):
        keys("t")
        m.snap(9)
    keys("t")
    m.snap(4)
    data["dash"] = m.data()

    # MR tab: detail of !412, scrolled
    m = Movie()
    goto("412")
    keys("Enter")
    wait_for("YOUR TURN")
    m.snap(14)
    for _ in range(14):
        keys("j", pause=0.08)
        m.snap(2)
    m.snap(10)
    keys("Escape")
    wait_for("on you")
    data["detail"] = m.data()

    # act tab: ping, reviewers, snooze
    m = Movie()
    goto("231")
    m.snap(5)
    keys("n")
    wait_for("gentle reminder")
    m.snap(16)
    keys("Escape")
    goto("57")
    m.snap(4)
    keys("v")
    wait_for("reviewers of !57")
    m.snap(12)
    keys("Escape")
    goto("409")
    m.snap(4)
    keys("z")
    wait_for("snooze !409")
    m.snap(12)
    keys("Escape")
    data["act"] = m.data()

    # pipelines tab: pipelines of !412, jobs, a log
    m = Movie()
    goto("412")
    keys("p")
    wait_for("Return Retry-After")
    m.snap(10)
    keys("Enter")
    wait_for("golangci-lint")
    m.snap(10)
    keys("j", "j", pause=0.2)
    m.snap(4)
    keys("l")
    wait_for("TestBucketPerKey")
    m.snap(16)
    keys("Escape", "Escape", "Escape")
    wait_for("on you")
    data["pipe"] = m.data()

    # diff tab
    m = Movie()
    goto("412")
    keys("d")
    wait_for("bucket.go")
    m.snap(10)
    keys("]")
    m.snap(10)
    keys("]")
    m.snap(10)
    keys("Escape")
    data["diff"] = m.data()

    tmux("kill-session", "-t", SESSION)
    return data


def rex_capture():
    """Run flw in a background Rex tab; read the Activity records and notifications it sends."""
    sess = os.environ.get("REX_SESSION")
    if not sess:
        print("not in Rex: skipping the Activity capture", file=sys.stderr)
        return None
    phase = tempfile.NamedTemporaryFile("w", delete=False, suffix=".phase")
    phase.write("1")
    phase.close()
    ev = subprocess.Popen(["rex", "events", "--session", sess, "--json"], stdout=subprocess.PIPE, text=True)
    cmd = (f"source {HERE}/demo-env.sh; export FLW_DEMO_PHASE_FILE={phase.name}; cd /tmp; exec {FLW}")
    out = json.loads(subprocess.run(["rex", "window", "new", "-s", sess, "flw-site-demo", "--focus=false", "--json",
                                     "--", "bash", "-c", cmd], env={**os.environ, "FLW_DEMO_KEEP_REX": "1"},
                                    capture_output=True, text=True, check=True).stdout)
    block, win = out["block_ids"][0], out["window_id"]
    time.sleep(6)
    records_1 = json.loads(subprocess.run(["rex", "block", "call", "-s", sess, "-b", block, "program_status"],
                                          capture_output=True, text=True).stdout)["records"]
    with open(phase.name, "w") as f:
        f.write("2")
    time.sleep(20)  # the next poll (notify.every = 15s) sees the changes
    records_2 = json.loads(subprocess.run(["rex", "block", "call", "-s", sess, "-b", block, "program_status"],
                                          capture_output=True, text=True).stdout)["records"]
    subprocess.run(["rex", "send-key", "-s", sess, "-b", block, "q"])
    time.sleep(1.5)
    subprocess.run(["rex", "window", "close", "-s", sess, win], capture_output=True)
    ev.terminate()
    notes = []
    for line in ev.stdout.read().splitlines():
        try:
            e = json.loads(line)["payload"]["event"]
        except (ValueError, KeyError, TypeError):
            continue
        p = e.get("payload", {})
        if e.get("type") == "block_event" and p.get("block_id") == block and p.get("name") == "desktop_notification":
            notes.append(p["payload"])
    os.unlink(phase.name)

    def clean(recs):
        keep = []
        for r in recs:
            if r.get("app") != "flw":
                continue
            keep.append({k: r.get(k) for k in ("id", "state", "kind", "title", "msg") if r.get(k) is not None})
        return sorted(keep, key=lambda r: (r.get("id") is not None, r.get("id") or ""))

    return {"before": clean(records_1), "after": clean(records_2), "notifications": notes}


def build_page():
    with open(os.path.join(HERE, "page.html")) as f:
        page = f.read()
    with open(os.path.join(HERE, "base.css")) as f:
        page = page.replace("{{BASECSS}}", f.read())
    with open(os.path.join(REPO, "site", "index.html"), "w") as f:
        f.write(page)
    print("wrote site/index.html")


def main():
    if "--page-only" in sys.argv:
        build_page()
        return
    subprocess.run(["go", "build", "-o", FLW, "."], cwd=REPO, check=True)
    data = record()
    if "--rex" in sys.argv:
        data["rex"] = rex_capture()
    with open(os.path.join(HERE, "demo-home", "config", "config.yaml")) as f:
        data["config"] = f.read()
    os.makedirs(os.path.dirname(OUT), exist_ok=True)
    js = "window.FLW_DATA = " + json.dumps(data, ensure_ascii=False, separators=(",", ":")) + ";\n"
    with open(OUT, "w") as f:
        f.write(js)
    print(f"wrote {OUT} ({len(js) // 1024} KB)")
    build_page()
    for k, v in data.items():
        if isinstance(v, dict) and "frames" in v:
            print(f"  {k}: {len(v['frames'])} frames, {len(v['pool'])} lines")
    if data.get("rex"):
        print("  rex:", len(data["rex"]["after"]), "records,", len(data["rex"]["notifications"]), "notifications")


if __name__ == "__main__":
    main()
