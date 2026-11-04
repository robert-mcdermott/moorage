#!/usr/bin/env python3
"""Tries how apps are kept apart, on a real server running as root. It changes this machine's cgroups
and runs as root, so that it is for throwaway machines such as CI's runners, and not for yours.

    sudo python3 .github/scripts/isolation-check.py /usr/local/bin/moor-check
"""
import http.client
import json
import os
import pathlib
import shutil
import signal
import subprocess
import sys
import tempfile
import time
import urllib.parse

moor = sys.argv[1]
port = int(os.environ.get("MOORAGE_ISOLATION_PORT", "8094"))
address = f"http://127.0.0.1:{port}"
CGROUPS = pathlib.Path("/sys/fs/cgroup/moorage/apps")

failures = []


def check(what, ok, detail=""):
    print(f"{'ok   ' if ok else 'FAIL '} {what}{'' if ok or not detail else f' ({detail})'}", flush=True)
    if not ok:
        failures.append(what)


if os.geteuid() != 0:
    sys.exit("this has to run as root, on a machine you can throw away")

# Under /var/tmp, since a temporary directory of the user's own would be closed to the apps' users.
work = pathlib.Path(tempfile.mkdtemp(prefix="moorage-isolation-", dir="/var/tmp"))
work.chmod(0o755)
data = work / "data"
env = {**os.environ, "MOORAGE_CONFIG": str(work / "server.toml"), "MOOR_CONFIG": str(work / "client.toml")}
log_file = open(work / "server.log", "ab")
server = None
subprocess.run([moor, "server", "init", "--preset", "local", "--config", str(work / "server.toml"), "--data-dir", str(data)], env=env, check=True, capture_output=True)
config = (work / "server.toml").read_text().replace("127.0.0.1:8080", f"127.0.0.1:{port}")
config = "\n".join('ssh = ""' if line.startswith("ssh ") else line for line in config.splitlines()) + "\n"


def set_level(level):
    text = config.replace('# level = "auto"', f'level = "{level}"')
    assert f'level = "{level}"' in text
    (work / "server.toml").write_text(text)


def start_server():
    global server
    server = subprocess.Popen([moor, "server", "run"], env=env, stdout=log_file, stderr=log_file)
    for _ in range(150):
        try:
            if get("127.0.0.1", "/.moorage/health")[0] == 200:
                return
        except OSError:
            pass
        time.sleep(0.2)
    raise SystemExit("the server did not start:\n" + (work / "server.log").read_text()[-3000:])


def stop_server():
    server.send_signal(signal.SIGTERM)
    server.wait(timeout=30)


def get(host, path, timeout=30, headers=None):
    conn = http.client.HTTPConnection("127.0.0.1", port, timeout=timeout)
    try:
        conn.request("GET", path, headers={"Host": host, **(headers or {})})
        res = conn.getresponse()
        return res.status, res.read()
    finally:
        conn.close()


def token():
    return (data / "keys" / "admin-token").read_text().strip()


def api(path):
    status, body = get("moorage.localhost", "/api/v1" + path, headers={"Authorization": "Bearer " + token()})
    if status != 200:
        raise RuntimeError(f"{path}: {status} {body[:300]!r}")
    return json.loads(body)


def run_moor(*args, check_it=True, given=None):
    # The options go first, for a command that takes the rest as what to run.
    command, rest = args[0], args[1:]
    if "--" in rest:
        at = rest.index("--")
        options, rest = rest[:at], rest[at:]
    else:
        options, rest = rest, ()
    done = subprocess.run([moor, command, *options, "--server", address, "--token", token(), *rest], env=env, capture_output=True, text=True, input=given)
    if check_it and done.returncode != 0:
        raise RuntimeError(f"moor {' '.join(args)}: {done.stdout}\n{done.stderr}")
    return done


APP = """
import http.server, json, os, socket, subprocess, urllib.parse

held = []

def attempt(what):
    try:
        return {"ok": True, "value": what()}
    except Exception as e:
        return {"ok": False, "error": str(e)}

class Handler(http.server.BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    disable_nagle_algorithm = True

    def do_GET(self):
        url = urllib.parse.urlparse(self.path)
        q = {k: v[0] for k, v in urllib.parse.parse_qs(url.query).items()}
        path = q.get("path", "")
        if url.path == "/health":
            answer = {"ok": True}
        elif url.path == "/whoami":
            status = open("/proc/self/status").read()
            answer = {"uid": os.getuid(), "gid": os.getgid(), "groups": os.getgroups(), "pid": os.getpid(), "dir": os.getcwd(),
                      "data": os.environ["MOORAGE_DATA_DIR"], "tmp": os.environ["TMPDIR"], "home": os.environ["HOME"],
                      "secret": os.environ.get("THE_SECRET", ""), "no_new_privs": "NoNewPrivs:\\t1" in status}
        elif url.path == "/read":
            answer = attempt(lambda: open(path, "rb").read(200).decode("utf-8", "replace"))
        elif url.path == "/list":
            answer = attempt(lambda: os.listdir(path))
        elif url.path == "/write":
            answer = attempt(lambda: open(path, "w").write("written"))
        elif url.path == "/bind":
            def bind():
                s = socket.socket()
                s.bind(("127.0.0.1", 0))
                s.close()
                return True
            answer = attempt(bind)
        elif url.path == "/shell":
            answer = attempt(lambda: subprocess.run(q["command"], shell=True, capture_output=True, text=True, check=True).stdout)
        elif url.path == "/spawn":
            def spawn():
                kids = [subprocess.Popen(["sleep", "30"]) for _ in range(int(q["count"]))]
                for kid in kids:
                    kid.kill()
                    kid.wait()
                return len(kids)
            answer = attempt(spawn)
        elif url.path == "/alloc":
            block = bytearray(int(q["mb"]) * 1024 * 1024)
            for i in range(0, len(block), 4096):
                block[i] = 1
            held.append(block)
            answer = {"ok": True}
        else:
            answer = {"ok": False}
        body = json.dumps(answer).encode()
        self.send_response(200)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *args):
        pass

http.server.ThreadingHTTPServer(("127.0.0.1", int(os.environ["PORT"])), Handler).serve_forever()
"""


def deploy(name, more=""):
    app = work / "src" / name
    app.mkdir(parents=True, exist_ok=True)
    (app / "app.py").write_text(APP)
    (app / "moorage.toml").write_text(f"""[app]
runtime = "command"

[build]
command = "id -u > built-by; id -g >> built-by; touch $MOORAGE_BIN/made-by-build"

[processes.web]
command = "python3 app.py"
health = {{ path = "/health", interval = "200ms", startup = "60s" }}

[release]
command = "id -u > $MOORAGE_DATA_DIR/released-by"

[deploy]
drain = "0s"
{more}""")
    run_moor("deploy", "--app", name, str(app))


def ask(name, path, **query):
    url = path + ("?" + urllib.parse.urlencode(query) if query else "")
    status, body = get(f"{name}.localhost", url)
    if status != 200:
        raise RuntimeError(f"{name}{url}: {status} {body[:200]!r}")
    return json.loads(body)


def wait_for(what, cond, seconds=60):
    deadline = time.monotonic() + seconds
    while time.monotonic() < deadline:
        try:
            if cond():
                return True
        except (OSError, RuntimeError, http.client.HTTPException):
            pass
        time.sleep(0.5)
    check(what, False, "timed out")
    return False


def own_build_of(name):
    return next(p for p in sorted((data / "builds" / name).iterdir()) if (p / "app").is_dir() and not p.name.endswith(".partial"))


def try_user_level():
    print("--- each app as its own user", flush=True)
    run_moor("env", "set", "--app", "alice", "--secret", "THE_SECRET", given="hunter2")
    run_moor("env", "set", "--app", "bob", "--secret", "THE_SECRET", given="swordfish")
    alice, bob = ask("alice", "/whoami"), ask("bob", "/whoami")
    check("no app runs as root", alice["uid"] != 0 and bob["uid"] != 0, f"{alice['uid']} {bob['uid']}")
    check("each app has a user of its own", alice["uid"] != bob["uid"], f"{alice['uid']} {bob['uid']}")
    check("and a group, and no others", alice["gid"] == alice["uid"] and alice["groups"] == [], str(alice))
    check("the users come from the reserved range", alice["uid"] >= 300000 and bob["uid"] >= 300000)
    check("an app has its secrets", alice["secret"] == "hunter2", alice["secret"])
    check("and no sandbox at this level", not alice["no_new_privs"])

    private = alice["data"] + "/private.txt"
    check("an app writes to its data", ask("alice", "/write", path=private)["ok"])
    check("an app reads its data", ask("alice", "/read", path=private).get("value") == "written")
    check("an app writes to its temporary directory", ask("alice", "/write", path=alice["tmp"] + "/scratch")["ok"])
    check("another app cannot read its data", not ask("bob", "/read", path=private)["ok"])
    check("another app cannot list its data", not ask("bob", "/list", path=str(pathlib.Path(alice["data"]).parent))["ok"])
    check("another app cannot read its temporary files", not ask("bob", "/read", path=alice["tmp"] + "/scratch")["ok"])
    check("another app cannot read its secrets from /proc", not ask("bob", "/read", path=f"/proc/{alice['pid']}/environ")["ok"])
    check("an app reads its own environment", ask("alice", "/read", path=f"/proc/{alice['pid']}/environ")["ok"])

    code = alice["dir"] + "/app.py"
    check("an app reads its own build", ask("alice", "/read", path=code)["ok"])
    check("an app cannot change its own build", not ask("alice", "/write", path=code)["ok"])
    check("an app cannot add to its own build", not ask("alice", "/write", path=alice["dir"] + "/new")["ok"])
    check("another app cannot read its build", not ask("bob", "/read", path=code)["ok"])
    check("another app cannot list its builds", not ask("bob", "/list", path=str(data / "builds" / "alice"))["ok"])

    for what, path in [("the database", "moorage.db"), ("the keys", "keys/admin-token"), ("the master key", "keys/master.key"), ("the logs", "logs"), ("the repositories", "repos"), ("the shims' files", "run")]:
        target = str(data / path)
        ok = ask("bob", "/read", path=target)["ok"] if "." in path or "/" in path else ask("bob", "/list", path=target)["ok"]
        check(f"an app cannot read {what}", not ok)

    built = own_build_of("alice") / "app" / "built-by"
    lines = built.read_text().split()
    check("a build runs as a user of its own", int(lines[0]) == alice["uid"] + 1, str(lines))
    check("in the app's group", int(lines[1]) == alice["gid"], str(lines))
    check("what a build made is sealed", built.stat().st_uid == 0 and built.stat().st_mode & 0o222 == 0, oct(built.stat().st_mode))
    cache = data / "cache" / "apps" / "alice"
    check("a build has a cache of its own", cache.is_dir() and cache.stat().st_uid == alice["uid"] + 1)

    released = ask("alice", "/read", path=alice["data"] + "/released-by").get("value", "").strip()
    check("a release command runs as the app", released == str(alice["uid"]), released)
    out = run_moor("run", "--app", "alice", "--", "id", "-u").stdout.strip()
    check("moor run runs as the app", out == str(alice["uid"]), out)
    out = run_moor("run", "--app", "alice", "--", "sh", "-c", "echo through a shell | cat").stdout.strip()
    check("a command through the shell works", "through a shell" in out, out)
    out = run_moor("run", "--app", "bob", "--", "cat", private, check_it=False)
    check("moor run cannot read another app's data", out.returncode != 0, out.stdout)
    wait_for("a cron job runs as the app", lambda: ask("alice", "/read", path=alice["data"] + "/cron-by").get("value", "").strip() == str(alice["uid"]))


def try_limits():
    print("--- limits", flush=True)
    deploy("carol", """
[resources]
memory = "64MiB"
cpu = 0.5
pids = 40
""")
    who = ask("carol", "/whoami")
    instances = api("/apps/carol/envs/production/instances")
    instance = instances[0]["id"]
    group = CGROUPS / instance
    check("an instance has a cgroup", group.is_dir(), str(group))
    if group.is_dir():
        check("in it", f"moorage/apps/{instance}" in pathlib.Path(f"/proc/{who['pid']}/cgroup").read_text())
        check("its memory is limited", (group / "memory.max").read_text().strip() == str(64 << 20), (group / "memory.max").read_text())
        check("its CPU is limited", (group / "cpu.max").read_text().strip() == "50000 100000", (group / "cpu.max").read_text())
        check("its processes are limited", (group / "pids.max").read_text().strip() == "40", (group / "pids.max").read_text())
    doc = api("/apps/carol/envs/production").get("isolation") or {}
    check("the API says what is enforced", doc.get("enforced") is True and doc.get("memory") == 64 << 20 and doc.get("level") in ("user", "sandbox"), str(doc))
    spawned = ask("carol", "/spawn", count=100)
    check("too many processes fail", not spawned["ok"], str(spawned))

    try:
        get("carol.localhost", "/alloc?mb=300", timeout=60)
    except (OSError, http.client.HTTPException):
        pass
    def told():
        for i in api("/apps/carol/envs/production/instances"):
            if (i.get("last_exit") or {}).get("signal") == "OOM":
                return True
        return False
    wait_for("the kernel's kill is seen as out of memory", told)
    check("and it says so", told())
    wait_for("the instance comes back", lambda: ask("carol", "/whoami")["pid"] != who["pid"])
    run = run_moor("run", "--app", "carol", "--", "python3", "-c", "x = bytearray(300 * 1024 * 1024); x[::4096] = b'1' * len(x[::4096])", check_it=False)
    check("a command over the limit fails", run.returncode != 0, run.stdout + run.stderr)


def try_adoption():
    print("--- through a restart of the server", flush=True)
    before = ask("alice", "/whoami")
    stop_server()
    start_server()
    after = ask("alice", "/whoami")
    check("the app keeps running", after["pid"] == before["pid"], f"{before['pid']} {after['pid']}")
    check("as its user", after["uid"] == before["uid"])
    instance = api("/apps/carol/envs/production/instances")[0]["id"]
    check("a cgroup stays through a restart", (CGROUPS / instance).is_dir())


def try_sandbox():
    print("--- in a sandbox", flush=True)
    who = ask("alice", "/whoami")
    check("no_new_privs is set", who["no_new_privs"])
    check("an app still reads its data", ask("alice", "/read", path=who["data"] + "/private.txt")["ok"])
    check("an app still writes to its data", ask("alice", "/write", path=who["data"] + "/more.txt")["ok"])
    check("an app still writes to its temporary directory", ask("alice", "/write", path=who["tmp"] + "/more")["ok"])
    check("an app still has what programs need", ask("alice", "/read", path="/etc/hostname")["ok"] or ask("alice", "/read", path="/etc/hosts")["ok"])
    check("an app still reads its build", ask("alice", "/read", path=who["dir"] + "/app.py")["ok"])
    check("an app cannot write to /tmp", not ask("alice", "/write", path="/tmp/moorage-escaped")["ok"])
    check("an app cannot read elsewhere", not ask("alice", "/list", path="/root")["ok"] and not ask("alice", "/list", path=str(work))["ok"])
    check("what it starts is held to the same", not ask("alice", "/shell", command="touch /tmp/moorage-escaped")["ok"])
    check("and it does start things", ask("alice", "/shell", command="echo hello").get("value", "").strip() == "hello")
    abi = subprocess.run(["uname", "-r"], capture_output=True, text=True).stdout
    bound = ask("alice", "/bind")
    version = tuple(int(x) for x in abi.split(".")[:2])
    if version >= (6, 7):
        check("an app cannot listen on a second port", not bound["ok"], str(bound))
    out = run_moor("run", "--app", "alice", "--", "sh", "-c", "touch /tmp/moorage-escaped", check_it=False)
    check("moor run is in the sandbox too", out.returncode != 0, out.stdout + out.stderr)
    deploy("dave")
    built = (own_build_of("dave") / "app" / "built-by").read_text().split()
    check("a build works in a sandbox", int(built[0]) >= 300000, str(built))
    check("and the app it made runs", ask("dave", "/whoami")["no_new_privs"])


def main():
    print(subprocess.run(["uname", "-sr"], capture_output=True, text=True).stdout.strip(), flush=True)
    # A server that has run as root without isolation, as one upgraded from 0.5.0 has.
    set_level("none")
    start_server()
    cron = """
[[cron]]
name = "who"
schedule = "@every 2s"
command = 'id -u > "$MOORAGE_DATA_DIR/cron-by"'
"""
    deploy("alice", cron)
    deploy("bob")
    first = ask("alice", "/whoami")
    check("with no isolation, an app runs as the server", first["uid"] == 0, str(first["uid"]))
    ask("alice", "/write", path=first["data"] + "/from-before.txt")
    stop_server()

    set_level("user")
    start_server()
    run_moor("restart", "--app", "alice")
    run_moor("restart", "--app", "bob")
    who = ask("alice", "/whoami")
    check("restarting after an upgrade, an app has its own user", who["uid"] != 0, str(who["uid"]))
    check("and what it wrote before", ask("alice", "/read", path=who["data"] + "/from-before.txt").get("value") == "written")
    deploy("alice", cron)
    deploy("bob")
    try_user_level()
    try_limits()
    try_adoption()
    stop_server()

    set_level("sandbox")
    start_server()
    run_moor("restart", "--app", "alice")
    if "landlock" in pathlib.Path("/sys/kernel/security/lsm").read_text():
        try_sandbox()
    else:
        print("note  this kernel has no Landlock: no sandbox to try", flush=True)

    # Stopping everything leaves nothing behind.
    instance = api("/apps/carol/envs/production/instances")[0]["id"]
    done = subprocess.run([moor, "server", "stop", "--all"], env=env, capture_output=True, text=True)
    check("stopping everything works", done.returncode == 0, done.stdout + done.stderr)
    server.wait(timeout=30)
    time.sleep(1)
    check("and leaves no cgroup", not (CGROUPS / instance).exists(), str(CGROUPS / instance))
    left = subprocess.run(["pgrep", "-f", "python3 app.py"], capture_output=True, text=True).stdout.strip()
    check("and no app", left == "", left)

    if failures:
        print(f"\n{len(failures)} failed:", *failures, sep="\n  ")
        print("--- the server's log")
        print((work / "server.log").read_text()[-4000:])
        sys.exit(1)
    print("\nevery check passed")


try:
    main()
except Exception as e:
    print("FAIL  it stopped:", e)
    print("--- the server's log")
    print((work / "server.log").read_text()[-4000:])
    if server and server.poll() is None:
        subprocess.run([moor, "server", "stop", "--all"], env=env, capture_output=True)
    sys.exit(1)
