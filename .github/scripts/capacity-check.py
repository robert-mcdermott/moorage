"""What capacity-check.sh runs: sixty environments on one server, with sleep on."""
import concurrent.futures
import http.client
import json
import os
import pathlib
import statistics
import subprocess
import sys
import threading
import time

moor, work, port, server = sys.argv[1], pathlib.Path(sys.argv[2]), int(sys.argv[3]), sys.argv[4]
apps = int(os.environ.get("CAPACITY_APPS", "10"))
envs = int(os.environ.get("CAPACITY_ENVS", "6"))
ballast = int(os.environ.get("CAPACITY_BALLAST_MB", "64"))
marker = "moorage-capacity-check"
token = (work / "data" / "keys" / "admin-token").read_text().strip()
address = f"http://127.0.0.1:{port}"

# The limits it must stay within, for 4 CPUs and 8 GB.
LIMITS = {
    "server_rss_mb": 1024,   # the server itself, however many environments it has
    "idle_cpu_percent": 25,  # of one core, with everything asleep
    "wake_one_s": 15,        # the slowest of the environments woken one at a time
    "wake_all_s": 120,       # the slowest of all of them woken together
    "p95_ms": 250,           # requests to those awake, with the others asleep
    "page_ms": 1500,         # the console's API, listing them all
}

results = []
failures = []


def report(what, value, unit="", limit=None):
    over = limit is not None and value > limit
    if over:
        failures.append(f"{what}: {value:g} {unit}, over {limit:g}")
    results.append((what, f"{value:g} {unit}".strip(), "" if limit is None else f"{limit:g}", "over" if over else "ok"))
    print(f"{'FAIL' if over else 'ok  '}  {what}: {value:g} {unit}" + ("" if limit is None else f" (limit {limit:g})"), flush=True)


def targets():
    for a in range(apps):
        for e in range(envs):
            yield f"cap-{a:02d}", ("production" if e == 0 else f"e{e}")


def host_of(app, env):
    return f"{app}.localhost" if env == "production" else f"{app}--{env}.localhost"


def get(host, path="/", timeout=300, headers=None):
    conn = http.client.HTTPConnection("127.0.0.1", port, timeout=timeout)
    try:
        conn.request("GET", path, headers={"Host": host, **(headers or {})})
        res = conn.getresponse()
        return res.status, res.read()
    finally:
        conn.close()


def api(path):
    status, body = get(f"moorage.localhost", "/api/v1" + path, headers={"Authorization": "Bearer " + token})
    if status != 200:
        raise RuntimeError(f"{path}: {status} {body[:200]!r}")
    return json.loads(body)


def memory():
    """What the server, its shims and the apps hold, in MB, and what the server alone does."""
    out = subprocess.run(["ps", "-A", "-o", "pid=,rss=,command="], capture_output=True, text=True, check=True).stdout
    total = own = 0
    for line in out.splitlines():
        pid, rss, command = line.split(None, 2)
        if pid == server:
            own = int(rss)
        if pid == server or marker in command or str(work) in command:
            total += int(rss)
    return total // 1024, own // 1024


def cgroup():
    try:
        path = pathlib.Path("/proc/self/cgroup").read_text().strip().split("::")[-1]
        base = pathlib.Path("/sys/fs/cgroup" + path)
        peak = int((base / "memory.peak").read_text()) // (1 << 20)
        limit = (base / "memory.max").read_text().strip()
        events = dict(line.split() for line in (base / "memory.events").read_text().splitlines())
        return peak, limit, int(events.get("oom_kill", 0))
    except (OSError, ValueError):
        return None


def cpu_percent(seconds=30):
    """What the server uses of one core, over a while."""
    def used():
        out = subprocess.run(["ps", "-o", "time=", "-p", server], capture_output=True, text=True, check=True).stdout.strip()
        days, _, clock = out.rpartition("-")
        parts = [float(p) for p in clock.split(":")]
        while len(parts) < 3:
            parts.insert(0, 0.0)
        return (int(days or 0) * 86400) + parts[0] * 3600 + parts[1] * 60 + parts[2]
    before = used()
    time.sleep(seconds)
    return (used() - before) / seconds * 100


def write_app():
    app = work / "app"
    app.mkdir()
    (app / "app.py").write_text(f"""
import http.server, os
held = bytearray({ballast} * 1024 * 1024)
for i in range(0, len(held), 4096):
    held[i] = 1
class Handler(http.server.BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    disable_nagle_algorithm = True
    def do_GET(self):
        self.send_response(200)
        self.send_header("Content-Length", "5")
        self.end_headers()
        self.wfile.write(b"hello")
    def log_message(self, *args):
        pass
http.server.ThreadingHTTPServer(("127.0.0.1", int(os.environ["PORT"])), Handler).serve_forever()
""")
    environments = "".join(f'\n[environments.{"production" if e == 0 else f"e{e}"}]\nsleep_after = "20s"\n' for e in range(envs))
    (app / "moorage.toml").write_text(f"""[app]
runtime = "command"

[processes.web]
command = "python3 app.py {marker}"
health = {{ path = "/", interval = "200ms", startup = "120s" }}

[deploy]
drain = "0s"
{environments}""")
    return app


def deploy_all(app):
    start = time.monotonic()
    took = []

    def deploy(target):
        name, env = target
        began = time.monotonic()
        done = subprocess.run([moor, "deploy", "--server", address, "--token", token, "--app", name, "--env", env, str(app)], capture_output=True, text=True)
        if done.returncode != 0:
            raise RuntimeError(f"deploying {name} {env}: {done.stdout}\n{done.stderr}")
        took.append(time.monotonic() - began)

    with concurrent.futures.ThreadPoolExecutor(4) as pool:
        list(pool.map(deploy, targets()))
    report("environments deployed", len(took))
    report("all of them, four at a time", round(time.monotonic() - start), "s")
    report("the slowest deployment", round(max(took), 1), "s")


def wait_asleep():
    start = time.monotonic()
    left = list(targets())
    while left:
        if time.monotonic() - start > 300:
            failures.append(f"{len(left)} environments did not go to sleep")
            print(f"FAIL  {len(left)} environments did not go to sleep: {left[:3]}", flush=True)
            return
        left = [t for t in left if not api(f"/apps/{t[0]}/envs/{t[1]}").get("asleep")]
        if left:
            time.sleep(2)
    report("all asleep, after", round(time.monotonic() - start), "s")


def wake_one_at_a_time():
    took = []
    for name, env in targets():
        began = time.monotonic()
        status, body = get(host_of(name, env))
        if status != 200 or body != b"hello":
            failures.append(f"waking {name} {env}: {status} {body[:100]!r}")
        took.append(time.monotonic() - began)
    report("waking one at a time: the middle one", round(statistics.median(took), 2), "s")
    report("waking one at a time: the slowest", round(max(took), 2), "s", LIMITS["wake_one_s"])


def wake_all_together():
    def wake(target):
        began = time.monotonic()
        status, body = get(host_of(*target))
        return time.monotonic() - began, status, body, target

    with concurrent.futures.ThreadPoolExecutor(len(list(targets()))) as pool:
        got = list(pool.map(wake, targets()))
    for _, status, body, target in got:
        if status != 200 or body != b"hello":
            failures.append(f"waking {target} with the rest: {status} {body[:100]!r}")
    took = [g[0] for g in got]
    report("waking all together: the middle one", round(statistics.median(took), 2), "s")
    report("waking all together: the slowest", round(max(took), 2), "s", LIMITS["wake_all_s"])


def serve_while_others_sleep(seconds=20, threads=20):
    awake = list(targets())[:5]
    for t in awake:
        get(host_of(*t))
    stop = time.monotonic() + seconds
    took, errors = [], []
    lock = threading.Lock()

    def hammer(i):
        host = host_of(*awake[i % len(awake)])
        conn = http.client.HTTPConnection("127.0.0.1", port, timeout=30)
        while time.monotonic() < stop:
            began = time.monotonic()
            try:
                conn.request("GET", "/", headers={"Host": host})
                res = conn.getresponse()
                body = res.read()
                ok = res.status == 200 and body == b"hello"
            except OSError as e:
                ok, body = False, str(e)
                conn = http.client.HTTPConnection("127.0.0.1", port, timeout=30)
            with lock:
                took.append(time.monotonic() - began)
                if not ok:
                    errors.append(body)

    with concurrent.futures.ThreadPoolExecutor(threads) as pool:
        list(pool.map(hammer, range(threads)))
    took.sort()
    report("requests a second to five awake", round(len(took) / seconds), "/s")
    report("the middle request", round(took[len(took) // 2] * 1000, 1), "ms")
    report("95% of requests within", round(took[int(len(took) * 0.95)] * 1000, 1), "ms", LIMITS["p95_ms"])
    report("requests that failed", len(errors), "", 0)


def console_pages():
    def timed(path):
        began = time.monotonic()
        api(path)
        return (time.monotonic() - began) * 1000
    worst = max(timed("/apps?limit=200"), timed("/previews"), timed("/events?limit=100"), timed("/apps/cap-00/envs"))
    report("the slowest list in the API", round(worst, 1), "ms", LIMITS["page_ms"])


def main():
    app = write_app()
    print(f"{apps * envs} environments of {apps} apps, each holding {ballast} MB\n", flush=True)
    deploy_all(app)
    total, own = memory()
    report("in use with all awake", total, "MB")
    report("of which the server", own, "MB", LIMITS["server_rss_mb"])

    wait_asleep()
    total, own = memory()
    report("in use with all asleep", total, "MB")
    report("of which the server", own, "MB", LIMITS["server_rss_mb"])
    report("the server's CPU with all asleep", round(cpu_percent(), 1), "% of a core", LIMITS["idle_cpu_percent"])
    console_pages()

    wake_one_at_a_time()
    wait_asleep()
    serve_while_others_sleep()
    wait_asleep()
    wake_all_together()
    total, own = memory()
    report("in use with all woken together", total, "MB")
    report("of which the server", own, "MB", LIMITS["server_rss_mb"])
    console_pages()

    free = subprocess.run(["du", "-sm", str(work / "data")], capture_output=True, text=True, check=True).stdout.split()[0]
    report("on disk", int(free), "MB")
    for entry in sorted((work / "data").iterdir()):
        size = subprocess.run(["du", "-sm", str(entry)], capture_output=True, text=True, check=True).stdout.split()[0]
        print(f"      {entry.name}: {size} MB", flush=True)
    limits = cgroup()
    if limits:
        peak, limit, oom = limits
        report("the most the cgroup ever held", peak, "MB")
        report("what ran out of memory and was killed", oom, "", 0)
        print(f"      its limit: {limit}", flush=True)

    summary = os.environ.get("GITHUB_STEP_SUMMARY")
    if summary:
        with open(summary, "a") as f:
            f.write("| What | How much | Limit | |\n|---|---|---|---|\n")
            for row in results:
                f.write("| " + " | ".join(row) + " |\n")
    if failures:
        print("\nOver a limit:", *failures, sep="\n  ")
        sys.exit(1)
    print("\nevery limit held")


main()
