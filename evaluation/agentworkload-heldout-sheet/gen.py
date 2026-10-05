#!/usr/bin/env python3
"""Generate the SQL of the held-out sheet-correction study (see README.md).

One headless Claude Code call per (repetition, arm, question): the fixed
prompt of prompt.txt with the arm's product sheet and the one question, no
tools, no settings, an empty working directory, a clean environment. The
answer is written unedited to arms/<arm>-r<k>/queries/qNN.sql. A call is
repeated only on a harness failure (non-zero exit, error result, empty
output), at most twice, never because of what the SQL says.
"""
import concurrent.futures, datetime, hashlib, json, os, pathlib, subprocess, sys, tempfile, threading

HERE = pathlib.Path(__file__).resolve().parent
HELD = HERE.parent / "agentworkload-heldout"
CLAUDE = os.environ.get("HELDOUT_SHEET_CLAUDE", "claude")
MODEL = "claude-opus-5"  # the model the original held-out run recorded
REPS = 3
ARMS = {"original": HERE / "products-original.md", "corrected": HERE / "products-corrected.md"}
WORKERS = 4
LOG = HERE / "generation.log"
lock = threading.Lock()


def log(line):
    with lock:
        with LOG.open("a") as f:
            f.write(line + "\n")
        print(line, flush=True)


def questions():
    out = []
    for line in (HELD / "questions.md").read_text().splitlines():
        if line[:1] == "h" and line[1:3].isdigit() and line[3] == ":":
            out.append((line[1:3], line[4:].strip()))
    assert len(out) == 40, len(out)
    return out


def call(prompt):
    env = {"HOME": os.environ["HOME"], "PATH": "/usr/bin:/bin", "TERM": "dumb"}
    with tempfile.TemporaryDirectory(prefix="heldout-sheet-") as cwd:
        p = subprocess.run([CLAUDE, "-p", "--model", MODEL, "--tools", "", "--strict-mcp-config", "--no-session-persistence",
                            "--setting-sources", "", "--output-format", "json"],
                           input=prompt, capture_output=True, text=True, cwd=cwd, env=env, timeout=300)
    if p.returncode != 0:
        return None, f"exit {p.returncode}"
    try:
        d = json.loads(p.stdout)
    except ValueError:
        return None, "undecodable output"
    if d.get("is_error") or not (d.get("result") or "").strip():
        return None, f"error result {d.get('subtype')}"
    return d, ""


def one(job):
    rep, arm, number, question = job
    target = HERE / "arms" / f"{arm}-r{rep}" / "queries" / f"q{number}.sql"
    if target.exists():
        return
    prompt = (HERE / "prompt.txt").read_text().format(products=ARMS[arm].read_text().rstrip("\n"), question=question)
    attempts, d, why = 0, None, ""
    while d is None and attempts < 3:
        attempts += 1
        d, why = call(prompt)
        if d is None:
            log(f"{arm}-r{rep} h{number} harness-failure attempt={attempts} {why}")
    if d is None:
        raise SystemExit(f"{arm}-r{rep} h{number}: three harness failures; the invocation is void")
    target.parent.mkdir(parents=True, exist_ok=True)
    text = d["result"]
    target.write_text(text if text.endswith("\n") else text + "\n")
    models = ",".join(sorted((d.get("modelUsage") or {}).keys()))
    log(f"{arm}-r{rep} h{number} model={models} bytes={len(text.encode())} attempts={attempts} "
        f"{datetime.datetime.now(datetime.timezone.utc).strftime('%H:%M:%S')}")


def main():
    version = subprocess.run([CLAUDE, "--version"], capture_output=True, text=True).stdout.strip()
    digests = {name: hashlib.sha256(path.read_bytes()).hexdigest() for name, path in ARMS.items()}
    log(f"start {datetime.datetime.now(datetime.timezone.utc).isoformat(timespec='seconds')} claude {version} model {MODEL} "
        f"prompt {hashlib.sha256((HERE / 'prompt.txt').read_bytes()).hexdigest()} sheets {json.dumps(digests, sort_keys=True)}")
    jobs = [(rep, arm, number, question) for rep in range(1, REPS + 1) for number, question in questions() for arm in ARMS]
    with concurrent.futures.ThreadPoolExecutor(WORKERS) as pool:
        for _ in pool.map(one, jobs):
            pass
    log(f"EXIT 0 {datetime.datetime.now(datetime.timezone.utc).isoformat(timespec='seconds')}")


if __name__ == "__main__":
    sys.exit(main())
