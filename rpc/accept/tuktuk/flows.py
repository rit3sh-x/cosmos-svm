"""The flows of the tuktuk run. Helium's published memo and cron examples queue
tasks on a chain, and the published crank turner runs them. tuktuk.sh starts
the crank turner beside this script. Each line on fd 3 is one result for
gate.py: {"test", "ok", "error"}. The script reports no values, because block
times and units differ between the chains by design.

    flows.py RPC_URL EXAMPLES_DIR PAYER_JSON TURNER_PUBKEY TUKTUK_CLI

EXAMPLES_DIR is tuktuk's typescript-examples with its node_modules installed.

CRON_WAIT (360) is how long the cron flow waits for the crank turner to run two
of the job's memos. It waits on the condition, not on the clock. The job runs
each minute and skips its first run, so its second memo is due at most 180 s
after the job is made. On Agave the first attempt of the crank turner at a task
often fails. The crank turner then tries again 30 to 90 s later
(tuktuk-crank-turner 0.2.31: a delay of 30 s, doubled per retry, plus up to
60 s of jitter), so 360 s holds the latest case with 90 s to spare.
"""
import json
import os
import subprocess
import sys
import time
import urllib.request

TUKTUK = "tuktukUrfhXT6ZT77QTU8RQtvgL967uRuVagWF57zVA"
MEMO = "MemoSq4gqABAXKb96qnH8TysNcWxMyWCqXgDLGmfcHr"
QUEUE = "accept"
CRON = "accept-cron"

rpc_url, examples, payer, turner, cli = sys.argv[1:6]
out = os.fdopen(3, "w")


def emit(test, ok, error=""):
    out.write(json.dumps({"test": f"flows: {test}", "ok": ok, "error": error}) + "\n")
    out.flush()
    print(f"{'ok  ' if ok else 'FAIL'} {test}{': ' + error if error else ''}", flush=True)


def rpc(method, *params):
    body = json.dumps({"jsonrpc": "2.0", "id": 1, "method": method, "params": list(params)}).encode()
    req = urllib.request.Request(rpc_url, body, {"content-type": "application/json"})
    with urllib.request.urlopen(req, timeout=60) as r:
        d = json.load(r)
    if "error" in d:
        raise RuntimeError(f"{method}: {d['error']}")
    return d["result"]


def balance(pubkey):
    return rpc("getBalance", pubkey, {"commitment": "confirmed"})["value"]


def memo_runs(since_slot):
    """Counts the successful RunTaskV0 transactions after since_slot that ran a memo."""
    runs = 0
    for s in rpc("getSignaturesForAddress", TUKTUK, {"limit": 1000, "commitment": "confirmed"}):
        if s["slot"] <= since_slot or s.get("err"):
            continue
        tx = rpc("getTransaction", s["signature"], {"encoding": "json", "maxSupportedTransactionVersion": 0, "commitment": "confirmed"})
        logs = (tx or {}).get("meta", {}).get("logMessages") or []
        if any("Instruction: RunTaskV0" in line for line in logs) and any(MEMO in line for line in logs):
            runs += 1
    return runs


def example(script, *args, timeout=300):
    """Runs one of tuktuk's examples through yarn, as its README does."""
    cmd = ["corepack", "yarn", script, "--", "--queueName", QUEUE, "--walletPath", payer, "--rpcUrl", rpc_url, *args]
    try:
        p = subprocess.run(cmd, cwd=examples, capture_output=True, text=True, timeout=timeout,
                           env={**os.environ, "COREPACK_ENABLE_DOWNLOAD_PROMPT": "0"})
    except subprocess.TimeoutExpired as e:
        return 124, (e.stdout or "") + (e.stderr or "")
    return p.returncode, p.stdout + p.stderr


def wait_until(seconds, what):
    """Polls what() every 2 s until it is true or seconds have passed."""
    deadline = time.monotonic() + seconds
    while True:
        if what():
            return True
        if time.monotonic() > deadline:
            return False
        time.sleep(2)


start_slot = rpc("getSlot", {"commitment": "confirmed"})
before = balance(turner)

code, text = example("memo", "--message", "tuktuk acceptance")
emit("the memo example queues a task and sees it run", code == 0 and "Task completed!" in text,
     "" if code == 0 and "Task completed!" in text else f"exit {code}: {text[-400:]}")
print(text[-1200:], flush=True)

ran = wait_until(60, lambda: memo_runs(start_slot) >= 1)
emit("a RunTaskV0 ran the memo", ran, "" if ran else "no successful RunTaskV0 with a memo within 60 s")

paid = wait_until(30, lambda: balance(turner) > before)
emit("the crank turner was paid", paid, "" if paid else f"balance {balance(turner)} is not above {before}")

p = subprocess.run([cli, "-u", rpc_url, "-w", payer, "task-queue", "get", "--task-queue-name", QUEUE],
                   capture_output=True, text=True, timeout=120)
emit("the CLI reads the queue", p.returncode == 0 and '"name": "accept"' in p.stdout,
     "" if p.returncode == 0 else f"exit {p.returncode}: {(p.stdout + p.stderr)[-400:]}")

cron_slot = rpc("getSlot", {"commitment": "confirmed"})
code, text = example("cron-memo", "--cronName", CRON, "--message", "cron acceptance", "--fundingAmount", "100000000")
emit("the cron example makes a cron job", code == 0 and "Cron job created!" in text,
     "" if code == 0 and "Cron job created!" in text else f"exit {code}: {text[-400:]}")
print(text[-800:], flush=True)

window = int(os.environ.get("CRON_WAIT", "360"))
fired = wait_until(window, lambda: memo_runs(cron_slot) >= 2)
emit(f"the cron job queued and ran two memos within {window} s", fired,
     "" if fired else f"{memo_runs(cron_slot)} memo runs within {window} s")
