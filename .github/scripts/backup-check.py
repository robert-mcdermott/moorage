#!/usr/bin/env python3
"""The restore drill: a real server backs itself up, loses everything but its master key, and has it all
back, from a directory and from a bucket on a real S3-compatible server.

    python3 .github/scripts/backup-check.py MOOR [S3_ENDPOINT]

S3_ENDPOINT is an S3-compatible service whose keys are in AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY.
"""
import base64
import datetime
import hashlib
import hmac
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
import urllib.request

moor = sys.argv[1]
s3 = sys.argv[2] if len(sys.argv) > 2 else ""
port = int(os.environ.get("MOORAGE_BACKUP_PORT", "8095"))
address = f"http://127.0.0.1:{port}"

work = pathlib.Path(tempfile.mkdtemp(prefix="moorage-backup-"))
data = work / "data"
env = {**os.environ, "MOORAGE_CONFIG": str(work / "server.toml"), "MOOR_CONFIG": str(work / "client.toml")}
log_file = open(work / "server.log", "ab")
server = None
failures = []


def check(what, ok, detail=""):
    print(f"{'ok   ' if ok else 'FAIL '} {what}{'' if ok or not detail else f' ({detail})'}", flush=True)
    if not ok:
        failures.append(what)


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


def run_moor(*args, given=None, on_server=True, check_it=True):
    extra = ["--server", address, "--token", token()] if on_server else []
    command, rest = list(args[:1]), list(args[1:])
    tail = []
    if "--" in rest:
        at = rest.index("--")
        rest, tail = rest[:at], rest[at:]
    done = subprocess.run([moor, *command, *rest, *extra, *tail], env=env, capture_output=True, text=True, input=given)
    if check_it and done.returncode != 0:
        raise RuntimeError(f"moor {' '.join(args)}: {done.stdout}\n{done.stderr}")
    return done


def wait_for(what, cond, seconds=60):
    deadline = time.monotonic() + seconds
    while time.monotonic() < deadline:
        try:
            if cond():
                return True
        except (OSError, RuntimeError):
            pass
        time.sleep(0.3)
    check(what, False, "timed out")
    return False


def make_bucket():
    """A service starts with no bucket, and asking for one takes a signed request."""
    url = urllib.parse.urlparse(s3)
    access, secret = os.environ["AWS_ACCESS_KEY_ID"], os.environ["AWS_SECRET_ACCESS_KEY"]
    now = datetime.datetime.now(datetime.timezone.utc)
    stamp, day = now.strftime("%Y%m%dT%H%M%SZ"), now.strftime("%Y%m%d")
    empty = hashlib.sha256(b"").hexdigest()
    headers = f"host:{url.netloc}\nx-amz-content-sha256:{empty}\nx-amz-date:{stamp}\n"
    signed = "host;x-amz-content-sha256;x-amz-date"
    request = f"PUT\n/drill\n\n{headers}\n{signed}\n{empty}"
    scope = f"{day}/us-east-1/s3/aws4_request"
    to_sign = f"AWS4-HMAC-SHA256\n{stamp}\n{scope}\n{hashlib.sha256(request.encode()).hexdigest()}"
    key = ("AWS4" + secret).encode()
    for part in (day, "us-east-1", "s3", "aws4_request"):
        key = hmac.new(key, part.encode(), hashlib.sha256).digest()
    signature = hmac.new(key, to_sign.encode(), hashlib.sha256).hexdigest()
    req = urllib.request.Request(s3 + "/drill", method="PUT", headers={
        "X-Amz-Date": stamp, "X-Amz-Content-Sha256": empty,
        "Authorization": f"AWS4-HMAC-SHA256 Credential={access}/{scope}, SignedHeaders={signed}, Signature={signature}"})
    urllib.request.urlopen(req)


def lose_everything():
    """As if the disk had gone, with the master key kept apart."""
    subprocess.run([moor, "server", "stop", "--all"], env=env, capture_output=True)
    if server.poll() is None:
        server.wait(timeout=30)
    kept = work / "master.key"
    shutil.copy(data / "keys" / "master.key", kept)
    shutil.rmtree(data)
    (data / "keys").mkdir(parents=True)
    shutil.copy(kept, data / "keys" / "master.key")
    (data / "keys" / "master.key").chmod(0o600)


def all_there(from_where):
    print(f"--- restored from {from_where}", flush=True)
    wait_for("the app serves again", lambda: get("shop.localhost", "/")[0] == 200)
    check("the static site serves", b"the site" in get("docs.localhost", "/")[1])
    check("the app's data is there", (data / "data" / "shop" / "production" / "notes.txt").read_text() == "written by the app")
    out = run_moor("run", "--app", "shop", "--", 'echo "length ${#THE_SECRET}"').stdout
    check("the secret opens", "length 7" in out, out)
    status, body = get("moorage.localhost", "/api/v1/apps/shop/envs/production/releases", headers={"Authorization": "Bearer " + token()})
    check("the releases are there", status == 200 and len(json.loads(body)["items"]) >= 1, str(body)[:200])
    status, body = get("moorage.localhost", "/api/v1/users", headers={"Authorization": "Bearer " + token()})
    check("the accounts are there", status == 200 and b"owner@example.com" in body, str(body)[:200])
    status, body = get("moorage.localhost", "/api/v1/server/backups", headers={"Authorization": "Bearer " + token()})
    items = json.loads(body)["items"] if status == 200 else []
    check("no backup says that it was interrupted", not any("stopped" in i.get("error", "") for i in items), str(items)[:300])


def main():
    subprocess.run([moor, "server", "init", "--preset", "local", "--config", str(work / "server.toml"), "--data-dir", str(data)], env=env, check=True, capture_output=True)
    config = (work / "server.toml").read_text().replace("127.0.0.1:8080", f"127.0.0.1:{port}")
    (work / "server.toml").write_text("\n".join('ssh = ""' if line.startswith("ssh ") else line for line in config.splitlines()) + "\n")
    start_server()

    claim = (data / "keys" / "claim-code").read_text().strip()
    body = urllib.parse.urlencode({"code": claim, "email": "owner@example.com", "password": "a long and unusual passphrase", "confirm": "a long and unusual passphrase"})
    conn = http.client.HTTPConnection("127.0.0.1", port)
    conn.request("POST", "/setup", body, {"Host": "moorage.localhost", "Content-Type": "application/x-www-form-urlencoded"})
    conn.getresponse().read()
    conn.close()

    app = work / "src" / "shop"
    app.mkdir(parents=True)
    (app / "moorage.toml").write_text('[app]\nruntime = "command"\n\n[processes.web]\ncommand = "python3 -m http.server $PORT"\n\n[processes.web.health]\npath = "/"\n')
    run_moor("deploy", "--app", "shop", str(app))
    run_moor("env", "set", "--app", "shop", "--secret", "THE_SECRET", given="hunter2")
    site = work / "src" / "docs"
    site.mkdir(parents=True)
    (site / "index.html").write_text("<p>the site</p>")
    run_moor("deploy", "--static", str(site), "--app", "docs")
    notes = data / "data" / "shop" / "production" / "notes.txt"
    notes.write_text("written by the app")

    print("--- backing up, to a directory", flush=True)
    backups = work / "backups"
    made = run_moor("server", "backup", "--to", str(backups), on_server=False)
    check("it says what to keep apart", "master.key" in made.stdout, made.stdout)
    archives = sorted(backups.glob("moorage-*.tar.gz"))
    check("there is a backup", len(archives) == 1, str(archives))
    archive = archives[0]
    check("with what it should hash to beside it", (backups / (archive.name + ".sha256")).is_file())
    check("the server went on running", get("shop.localhost", "/")[0] == 200)
    listed = run_moor("server", "backup", "list", "--to", str(backups), on_server=False)
    check("it can be listed", archive.name in listed.stdout, listed.stdout)
    verified = run_moor("server", "backup", "verify", str(archive), on_server=False)
    check("it can be verified", "complete" in verified.stdout, verified.stdout)
    refused = run_moor("server", "restore", str(archive), on_server=False, check_it=False)
    check("a restore refuses while the server runs", refused.returncode != 0 and "running" in refused.stderr, refused.stderr)

    damaged = work / "damaged.tar.gz"
    damaged.write_bytes(archive.read_bytes()[: archive.stat().st_size // 2])
    verified = run_moor("server", "backup", "verify", str(damaged), on_server=False, check_it=False)
    check("half a backup is seen for what it is", verified.returncode != 0, verified.stdout)

    uploaded = None
    if s3:
        print("--- backing up, to a bucket", flush=True)
        make_bucket()
        env["AWS_ENDPOINT_URL"] = s3
        env["AWS_REGION"] = "us-east-1"
        made = run_moor("server", "backup", "--to", "s3://drill/moorage", on_server=False)
        check("it goes to a bucket", "Backed up" in made.stdout, made.stdout)
        listed = run_moor("server", "backup", "list", "--to", "s3://drill/moorage", on_server=False)
        names = [line.split()[0] for line in listed.stdout.splitlines() if line.startswith("moorage-")]
        check("and can be listed there", len(names) == 1, listed.stdout)
        uploaded = "s3://drill/moorage/" + names[0]
        verified = run_moor("server", "backup", "verify", uploaded, on_server=False)
        check("and verified there", "complete" in verified.stdout, verified.stdout)
        refused = run_moor("server", "backup", "--to", "s3://drill/moorage", "--include-keys", on_server=False, check_it=False)
        check("keys do not go to a bucket", refused.returncode != 0, refused.stdout)

    # What happens after the backup goes away with the restore.
    notes.write_text("changed since")
    stop_server()
    lose_everything()

    print("--- with the wrong key", flush=True)
    good_key = (data / "keys" / "master.key").read_bytes()
    other_key = base64.b64encode(os.urandom(32)) + b"\n"
    (data / "keys" / "master.key").write_bytes(other_key)
    refused = run_moor("server", "restore", str(archive), on_server=False, check_it=False)
    check("a restore refuses", refused.returncode != 0 and "different master key" in refused.stderr, refused.stdout + refused.stderr)
    check("and changes nothing", not (data / "moorage.db").exists())
    (data / "keys" / "master.key").write_bytes(good_key)

    restored = run_moor("server", "restore", str(archive), on_server=False)
    check("it restores from a directory", "Restored" in restored.stdout, restored.stdout)
    start_server()
    all_there("a directory")

    if uploaded:
        stop_server()
        lose_everything()
        restored = run_moor("server", "restore", uploaded, on_server=False)
        check("it restores from a bucket", "Restored" in restored.stdout, restored.stdout)
        start_server()
        all_there("a bucket")

    # Over a server that is there, what was there is kept.
    stop_server()
    refused = run_moor("server", "restore", str(archive), on_server=False, check_it=False)
    check("it does not restore over a server by accident", refused.returncode != 0 and "--force" in refused.stderr, refused.stderr)
    restored = run_moor("server", "restore", str(archive), "--force", on_server=False)
    check("and with --force, keeps what was there", "before-restore" in restored.stdout and any(data.glob("before-restore-*/moorage.db")), restored.stdout)
    start_server()
    wait_for("and the app serves", lambda: get("shop.localhost", "/")[0] == 200)
    subprocess.run([moor, "server", "stop", "--all"], env=env, capture_output=True)
    if server.poll() is None:
        server.wait(timeout=30)

    if failures:
        print(f"\n{len(failures)} failed:", *failures, sep="\n  ")
        print("--- the server's log")
        print((work / "server.log").read_text()[-3000:])
        sys.exit(1)
    print("\nevery check passed")


try:
    main()
except Exception as e:
    print("FAIL  it stopped:", repr(e))
    print("--- the server's log")
    print((work / "server.log").read_text()[-3000:])
    if server and server.poll() is None:
        subprocess.run([moor, "server", "stop", "--all"], env=env, capture_output=True)
    sys.exit(1)
