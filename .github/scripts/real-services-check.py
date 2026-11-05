#!/usr/bin/env python3
"""Tries a real server against the real services it talks to, here where running them costs nothing: a forge in a
container, which reaches the server with real webhooks, and an OpenTelemetry collector and a syslog server, which read
what it sends with parsers of their own.

    python3 .github/scripts/real-services-check.py MOOR gitea telemetry
    python3 .github/scripts/real-services-check.py MOOR gitlab

A pull request from a fork is a real fork, by a second user, and its code is really fetched once it is approved.
"""
import base64
import http.client
import json
import os
import pathlib
import re
import shutil
import signal
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.parse
import urllib.request

moor = sys.argv[1]
wanted = sys.argv[2:]
port = 8097
work = pathlib.Path(tempfile.mkdtemp(prefix="moorage-real-"))
out_dir = work / "out"
out_dir.mkdir()
out_dir.chmod(0o777)
data = work / "data"
env = {**os.environ, "MOORAGE_CONFIG": str(work / "server.toml"), "MOOR_CONFIG": str(work / "client.toml"), "SSL_CERT_FILE": str(work / "ca.pem")}
log_file = open(work / "server.log", "ab")
server = None
failures = []
PASSWORD = "a long and unusual passphrase for the drill"
APP = "shop"


def check(what, ok, detail=""):
    print(f"{'ok   ' if ok else 'FAIL '} {what}{'' if ok or not detail else f' ({detail})'}", flush=True)
    if not ok:
        failures.append(what)
    return ok


def wait_for(what, cond, seconds=90):
    """Waits for cond, and says so if it never comes. Returns what it gave."""
    deadline = time.monotonic() + seconds
    last = None
    while time.monotonic() < deadline:
        try:
            last = cond()
            if last:
                check(what, True)
                return last
        except (OSError, RuntimeError, KeyError, ValueError, urllib.error.URLError) as e:
            last = e
        time.sleep(1)
    check(what, False, f"timed out, last: {last!r}"[:600])
    return None


def sh(*args, given=None, check_it=True):
    done = subprocess.run(args, capture_output=True, text=True, input=given)
    if check_it and done.returncode != 0:
        raise RuntimeError(f"{' '.join(args)}: {done.stdout}\n{done.stderr}")
    return done


def write_config():
    # The console's hostname is what a container reaches this machine by, so that webhooks really cross a network.
    (work / "server.toml").write_text(f"""
data_dir = "{data}"
domain = "localhost"
console_host = "host.docker.internal"
routing = "host"

[access]
production = "public"
named = "public"
preview = "public"

[notifications]
allow = ["127.0.0.1", "::1"]

[listen]
http = ":{port}"
https = ""
ssh = ""

[tls]
mode = "off"

[proxy]
trusted = []
external_scheme = "http"
""")


def start_server():
    global server
    server = subprocess.Popen([moor, "server", "run"], env=env, stdout=log_file, stderr=log_file)
    for _ in range(150):
        try:
            if get("host.docker.internal", "/.moorage/health")[0] == 200:
                return
        except OSError:
            pass
        time.sleep(0.2)
    raise SystemExit("the server did not start:\n" + (work / "server.log").read_text()[-3000:])


def get(host, path):
    conn = http.client.HTTPConnection("127.0.0.1", port, timeout=30)
    try:
        conn.request("GET", path, headers={"Host": host})
        res = conn.getresponse()
        return res.status, res.read().decode(errors="replace")
    finally:
        conn.close()


def serves(host, text):
    status, body = get(host, "/")
    return status == 200 and text in body


def run_moor(*args, given=None, check_it=True):
    token = (data / "keys" / "admin-token").read_text().strip()
    command, rest = list(args[:1]), list(args[1:])
    tail = []
    if "--" in rest:
        at = rest.index("--")
        rest, tail = rest[:at], rest[at:]
    done = subprocess.run([moor, *command, *rest, "--server", f"http://127.0.0.1:{port}", "--token", token, *tail], env=env, capture_output=True, text=True, input=given)
    if check_it and done.returncode != 0:
        raise RuntimeError(f"moor {' '.join(args)}: {done.stdout}\n{done.stderr}")
    return done


def pulls():
    return json.loads(run_moor("forge", "pulls", "--app", APP, "--json").stdout or "[]")


def pull(number):
    for p in pulls():
        if p.get("number") == number:
            return p
    return None


def request(base, method, path, body=None, headers=None):
    req = urllib.request.Request(base + path, method=method, data=None if body is None else json.dumps(body).encode(), headers={"Content-Type": "application/json", **(headers or {})})
    try:
        with urllib.request.urlopen(req, timeout=60) as res:
            text = res.read().decode()
            return json.loads(text) if text else None
    except urllib.error.HTTPError as e:
        raise RuntimeError(f"{method} {path}: {e.code} {e.read().decode(errors='replace')[:400]}") from e


MANIFEST = f"""[app]
name = "{APP}"
runtime = "static"

[previews]
access = "public"
"""


def page(text):
    return f"<!doctype html><title>{text}</title><p>{text}</p>\n"


class Gitea:
    name = "Gitea"
    kind = "gitea"
    base = "http://127.0.0.1:3000"
    owner, stranger = "owner", "stranger"

    def start(self):
        sh("docker", "run", "-d", "--name", "gitea", "-p", "3000:3000", "--add-host=host.docker.internal:host-gateway",
           "-e", "GITEA__security__INSTALL_LOCK=true", "-e", "GITEA__webhook__ALLOWED_HOST_LIST=*", "-e", f"GITEA__server__ROOT_URL={self.base}/", "gitea/gitea:1.24.2")
        wait_for("Gitea starts", lambda: request(self.base, "GET", "/api/healthz") is not None, 120)
        for user in (self.owner, self.stranger):
            sh("docker", "exec", "-u", "git", "gitea", "gitea", "admin", "user", "create", "--username", user, "--password", PASSWORD, "--email", f"{user}@example.test", "--must-change-password=false", *(["--admin"] if user == self.owner else []))
        print(sh("docker", "exec", "-u", "git", "gitea", "gitea", "--version").stdout.strip())
        self.setup = self.token_of(self.owner, "setup", ["all"])
        self.their = self.token_of(self.stranger, "theirs", ["all"])
        # What docs/forges.md says a token needs, and no more.
        self.token = self.token_of(self.owner, "moorage", ["read:repository", "write:repository", "write:issue"])
        request(self.base, "POST", "/api/v1/user/repos", {"name": APP, "default_branch": "main", "auto_init": True}, self.auth())
        self.write("moorage.toml", MANIFEST, "main", "How to serve it", new=True)
        self.write("index.html", page("first on main"), "main", "A page", new=True)

    def token_of(self, user, name, scopes):
        basic = base64.b64encode(f"{user}:{PASSWORD}".encode()).decode()
        return request(self.base, "POST", f"/api/v1/users/{user}/tokens", {"name": name, "scopes": scopes}, {"Authorization": "Basic " + basic})["sha1"]

    def auth(self, token=None):
        return {"Authorization": "token " + (token or self.setup)}

    @property
    def repo_url(self):
        return f"{self.base}/{self.owner}/{APP}.git"

    def write(self, path, content, branch, message, new=False, new_branch=None, repo=None, token=None):
        repo = repo or f"{self.owner}/{APP}"
        body = {"message": message, "content": base64.b64encode(content.encode()).decode(), "branch": branch}
        if new_branch:
            body["new_branch"] = new_branch
        if new:
            return request(self.base, "POST", f"/api/v1/repos/{repo}/contents/{path}", body, self.auth(token))["commit"]["sha"]
        sha = request(self.base, "GET", f"/api/v1/repos/{repo}/contents/{path}?ref={branch}", None, self.auth(token))["sha"]
        return request(self.base, "PUT", f"/api/v1/repos/{repo}/contents/{path}", {**body, "sha": sha}, self.auth(token))["commit"]["sha"]

    def add_hook(self, address, secret):
        # The events docs/forges.md lists.
        request(self.base, "POST", f"/api/v1/repos/{self.owner}/{APP}/hooks", {"type": "gitea", "active": True, "events": ["push", "pull_request", "pull_request_sync"], "config": {"url": address, "content_type": "json", "secret": secret}}, self.auth())

    def open_pr(self, branch, title):
        return request(self.base, "POST", f"/api/v1/repos/{self.owner}/{APP}/pulls", {"head": branch, "base": "main", "title": title}, self.auth())["number"]

    def fork_and_open(self, title, text):
        fork = request(self.base, "POST", f"/api/v1/repos/{self.owner}/{APP}/forks", {}, self.auth(self.their))
        full = fork["full_name"]
        sha = self.write("index.html", page(text), "main", title, repo=full, token=self.their, new_branch="change")
        number = request(self.base, "POST", f"/api/v1/repos/{self.owner}/{APP}/pulls", {"head": f"{self.stranger}:change", "base": "main", "title": title}, self.auth(self.their))["number"]
        return number, sha

    def status_of(self, sha):
        rows = request(self.base, "GET", f"/api/v1/repos/{self.owner}/{APP}/commits/{sha}/statuses", None, self.auth())
        return next((r["status"] for r in rows if r.get("context") == "moorage"), None)

    def comments(self, number):
        return [c["body"] for c in request(self.base, "GET", f"/api/v1/repos/{self.owner}/{APP}/issues/{number}/comments", None, self.auth())]

    def close(self, number):
        request(self.base, "PATCH", f"/api/v1/repos/{self.owner}/{APP}/pulls/{number}", {"state": "closed"}, self.auth())

    def merge(self, number):
        request(self.base, "POST", f"/api/v1/repos/{self.owner}/{APP}/pulls/{number}/merge", {"Do": "merge"}, self.auth())


class GitLab:
    name = "GitLab"
    kind = "gitlab"
    base = "http://127.0.0.1:3080"
    owner, stranger = "root", "stranger"

    def start(self):
        sh("docker", "run", "-d", "--name", "gitlab", "-p", "3080:80", "--shm-size", "256m", "--add-host=host.docker.internal:host-gateway",
           "-e", f"GITLAB_ROOT_PASSWORD={PASSWORD}", "-e", "GITLAB_OMNIBUS_CONFIG=external_url 'http://127.0.0.1'; prometheus_monitoring['enable'] = false; puma['worker_processes'] = 2; sidekiq['concurrency'] = 5", "gitlab/gitlab-ce:17.11.7-ce.0")
        deadline = time.monotonic() + 720
        while time.monotonic() < deadline:
            try:
                with urllib.request.urlopen(self.base + "/users/sign_in", timeout=10) as res:
                    if res.status == 200:
                        break
            except (OSError, urllib.error.URLError):
                pass
            time.sleep(5)
        else:
            raise SystemExit("GitLab did not start:\n" + sh("docker", "logs", "--tail", "60", "gitlab", check_it=False).stdout[-3000:])
        check("GitLab starts", True)
        script = ("user = User.find_by_username('root'); t = user.personal_access_tokens.create!(scopes: ['api'], name: 'setup', expires_at: 2.days.from_now); t.set_token('drill-setup-token-0123456789'); t.save!; "
                  "s = User.new(username: 'stranger', name: 'Stranger', email: 'stranger@example.test', password: '" + PASSWORD + "', password_confirmation: '" + PASSWORD + "'); s.skip_confirmation!; s.assign_personal_namespace(Organizations::Organization.default_organization); s.save!; "
                  "o = s.personal_access_tokens.create!(scopes: ['api'], name: 'theirs', expires_at: 2.days.from_now); o.set_token('drill-stranger-token-0123456789'); o.save!; "
                  "ApplicationSetting.current.update!(allow_local_requests_from_web_hooks_and_services: true)")
        sh("docker", "exec", "gitlab", "gitlab-rails", "runner", script)
        self.setup, self.their = "drill-setup-token-0123456789", "drill-stranger-token-0123456789"
        self.project = request(self.base, "POST", "/api/v4/projects", {"name": APP, "default_branch": "main", "initialize_with_readme": True, "visibility": "public"}, self.auth())["id"]
        # What docs/forges.md says a token needs: a project access token with the api scope, as a Developer.
        made = request(self.base, "POST", f"/api/v4/projects/{self.project}/access_tokens", {"name": "moorage", "scopes": ["api"], "access_level": 30, "expires_at": time.strftime("%Y-%m-%d", time.gmtime(time.time() + 172800))}, self.auth())
        self.token = made["token"]
        self.write("moorage.toml", MANIFEST, "main", "How to serve it", new=True)
        self.write("index.html", page("first on main"), "main", "A page", new=True)

    def auth(self, token=None):
        return {"PRIVATE-TOKEN": token or self.setup}

    @property
    def repo_url(self):
        return f"{self.base}/{self.owner}/{APP}.git"

    def write(self, path, content, branch, message, new=False, new_branch=None, project=None, token=None):
        project = project or self.project
        body = {"branch": new_branch or branch, "commit_message": message, "content": content}
        if new_branch:
            body["start_branch"] = branch
        encoded = urllib.parse.quote(path, safe="")
        request(self.base, "POST" if new else "PUT", f"/api/v4/projects/{project}/repository/files/{encoded}", body, self.auth(token))
        return request(self.base, "GET", f"/api/v4/projects/{project}/repository/commits/{urllib.parse.quote(new_branch or branch, safe='')}", None, self.auth(token))["id"]

    def add_hook(self, address, secret):
        request(self.base, "POST", f"/api/v4/projects/{self.project}/hooks", {"url": address, "token": secret, "push_events": True, "merge_requests_events": True, "enable_ssl_verification": False}, self.auth())

    def open_pr(self, branch, title):
        return request(self.base, "POST", f"/api/v4/projects/{self.project}/merge_requests", {"source_branch": branch, "target_branch": "main", "title": title}, self.auth())["iid"]

    def fork_and_open(self, title, text):
        fork = request(self.base, "POST", f"/api/v4/projects/{self.project}/fork", {}, self.auth(self.their))["id"]
        for _ in range(60):
            if request(self.base, "GET", f"/api/v4/projects/{fork}", None, self.auth(self.their)).get("import_status") in ("finished", "none"):
                break
            time.sleep(2)
        sha = self.write("index.html", page(text), "main", title, project=fork, token=self.their, new_branch="change")
        number = request(self.base, "POST", f"/api/v4/projects/{fork}/merge_requests", {"source_branch": "change", "target_branch": "main", "target_project_id": self.project, "title": title}, self.auth(self.their))["iid"]
        return number, sha

    def status_of(self, sha):
        rows = request(self.base, "GET", f"/api/v4/projects/{self.project}/repository/commits/{sha}/statuses", None, self.auth())
        state = next((r["status"] for r in rows if r.get("name") == "moorage"), None)
        return {"failed": "failure", "running": "pending"}.get(state, state)

    def comments(self, number):
        notes = request(self.base, "GET", f"/api/v4/projects/{self.project}/merge_requests/{number}/notes", None, self.auth())
        return [n["body"] for n in notes if not n.get("system")]

    def close(self, number):
        request(self.base, "PUT", f"/api/v4/projects/{self.project}/merge_requests/{number}", {"state_event": "close"}, self.auth())

    def merge(self, number):
        for _ in range(30):
            try:
                request(self.base, "PUT", f"/api/v4/projects/{self.project}/merge_requests/{number}/merge", {}, self.auth())
                return
            except RuntimeError as e:
                if "405" not in str(e) and "406" not in str(e) and "422" not in str(e):
                    raise
                time.sleep(2)
        raise RuntimeError("the merge request never became mergeable")


def try_forge(forge):
    print(f"--- {forge.name}", flush=True)
    forge.start()
    run_moor("apps", "create", APP)
    # A token the forge does not know is refused, on what the forge really says.
    bad = run_moor("forge", "connect", forge.repo_url, "--kind", forge.kind, "--token-stdin", "--app", APP, given="not-a-token-at-all", check_it=False)
    check(f"{forge.name}: a bad token is refused", bad.returncode != 0 and "does not accept" in bad.stderr + bad.stdout, bad.stderr + bad.stdout)
    done = run_moor("forge", "connect", forge.repo_url, "--kind", forge.kind, "--token-stdin", "--app", APP, given=forge.token)
    secret = re.search(r"Secret\s+(\S+)", done.stdout).group(1)
    address = re.search(r"Address\s+(\S+)", done.stdout).group(1)
    check(f"{forge.name}: connecting finds the default branch", "default branch is main" in done.stdout, done.stdout)
    forge.add_hook(address, secret)

    # A push to the default branch, told by a real webhook.
    forge.write("index.html", page("second on main"), "main", "A second page")
    wait_for(f"{forge.name}: a push deploys to production", lambda: serves(f"{APP}.localhost", "second on main"))

    # A pull request from a branch of the repository.
    first = forge.write("index.html", page("in a pull request"), "main", "A change", new_branch="change-one")
    number = forge.open_pr("change-one", "A change")
    wait_for(f"{forge.name}: a pull request gets a preview", lambda: serves(f"{APP}--pr-{number}.localhost", "in a pull request"))
    wait_for(f"{forge.name}: its check says it is live", lambda: forge.status_of(first) == "success")
    wait_for(f"{forge.name}: a comment says where it is", lambda: any(f"pr-{number}" in c for c in forge.comments(number)))

    # A new commit takes its place, and the same comment says so.
    second = forge.write("index.html", page("a second commit"), "change-one", "Another change")
    wait_for(f"{forge.name}: a new commit updates the preview", lambda: serves(f"{APP}--pr-{number}.localhost", "a second commit"))
    wait_for(f"{forge.name}: the new commit's check", lambda: forge.status_of(second) == "success")
    wait_for(f"{forge.name}: the same comment says so", lambda: any(second[:8] in c for c in forge.comments(number)))
    check(f"{forge.name}: one comment, and not two", len(forge.comments(number)) == 1, str(forge.comments(number)))

    # A commit that fails says so.
    third = forge.write("moorage.toml", MANIFEST + "\n[not a section\n", "change-one", "A mistake")
    wait_for(f"{forge.name}: a commit that fails says so", lambda: forge.status_of(third) == "failure")
    fourth = forge.write("moorage.toml", MANIFEST, "change-one", "Put right")
    wait_for(f"{forge.name}: putting it right says so", lambda: forge.status_of(fourth) == "success")

    # Closing takes the preview away, and merging another takes its code to production.
    forge.close(number)
    wait_for(f"{forge.name}: closing takes the preview away", lambda: get(f"{APP}--pr-{number}.localhost", "/")[0] == 404)

    other = forge.write("index.html", page("merged"), "main", "To merge", new_branch="change-two")
    merged = forge.open_pr("change-two", "To merge")
    wait_for(f"{forge.name}: the second pull request's preview", lambda: serves(f"{APP}--pr-{merged}.localhost", "merged"))
    forge.merge(merged)
    wait_for(f"{forge.name}: merging deploys to production", lambda: serves(f"{APP}.localhost", "merged"))
    wait_for(f"{forge.name}: merging takes its preview away", lambda: get(f"{APP}--pr-{merged}.localhost", "/")[0] == 404)

    # A real fork, by someone else.
    fork_number, fork_sha = forge.fork_and_open("From a stranger", "from a real fork")
    waiting = wait_for(f"{forge.name}: a fork's pull request waits", lambda: (pull(fork_number) or {}).get("state") == "waiting" and pull(fork_number))
    if waiting:
        check(f"{forge.name}: it is not built", get(f"{APP}--pr-{fork_number}.localhost", "/")[0] == 404)
        wait_for(f"{forge.name}: its check says it waits", lambda: forge.status_of(fork_sha) == "pending")
        wait_for(f"{forge.name}: a comment says it waits", lambda: any("approve" in c for c in forge.comments(fork_number)))
        run_moor("forge", "approve", str(fork_number), fork_sha[:8], "--app", APP)
        wait_for(f"{forge.name}: approving builds the fork's code", lambda: serves(f"{APP}--pr-{fork_number}.localhost", "from a real fork"))
        wait_for(f"{forge.name}: its check says it is live", lambda: forge.status_of(fork_sha) == "success")
    state = run_moor("forge", "--app", APP, "--json")
    check(f"{forge.name}: no problem is left standing", not json.loads(state.stdout).get("last_error"), state.stdout[:400])


def try_telemetry():
    print("--- telemetry", flush=True)
    (work / "otel.yaml").write_text("""
receivers:
  otlp:
    protocols:
      http:
        endpoint: 0.0.0.0:4318
exporters:
  file:
    path: /out/otel.json
    flush_interval: 1s
service:
  pipelines:
    logs: {receivers: [otlp], exporters: [file]}
    metrics: {receivers: [otlp], exporters: [file]}
""")
    sh("docker", "run", "-d", "--name", "otel", "-p", "4318:4318", "-v", f"{out_dir}:/out", "-v", f"{work}/otel.yaml:/etc/otel.yaml:ro", "otel/opentelemetry-collector-contrib:0.120.0", "--config=/etc/otel.yaml")
    sh("openssl", "req", "-x509", "-newkey", "rsa:2048", "-nodes", "-keyout", str(work / "key.pem"), "-out", str(work / "ca.pem"), "-days", "2", "-subj", "/CN=localhost", "-addext", "subjectAltName=DNS:localhost,IP:127.0.0.1")
    (work / "key.pem").chmod(0o644)
    (work / "syslog-ng.conf").write_text("""@version: 4.8
@include "scl.conf"
source s_udp { syslog(transport("udp") port(5514)); };
source s_tcp { syslog(transport("tcp") port(5601)); };
source s_tls { syslog(transport("tls") port(6514) tls(key-file("/certs/key.pem") cert-file("/certs/ca.pem") peer-verify(optional-untrusted))); };
template t_fields { template("${HOST}|${PROGRAM}|${PID}|${MSGID}|${FACILITY}|${LEVEL}|${MSG}\\n"); };
log { source(s_udp); destination { file("/out/udp.log" template(t_fields) perm(0666)); }; };
log { source(s_tcp); destination { file("/out/tcp.log" template(t_fields) perm(0666)); }; };
log { source(s_tls); destination { file("/out/tls.log" template(t_fields) perm(0666)); }; };
""")
    certs = work / "certs"
    certs.mkdir()
    shutil.copy(work / "key.pem", certs / "key.pem")
    shutil.copy(work / "ca.pem", certs / "ca.pem")
    sh("docker", "run", "-d", "--name", "syslog", "-p", "5514:5514/udp", "-p", "5601:5601", "-p", "6514:6514", "-v", f"{out_dir}:/out", "-v", f"{certs}:/certs:ro", "-v", f"{work}/syslog-ng.conf:/etc/syslog-ng/syslog-ng.conf:ro", "balabit/syslog-ng:4.8.0")
    time.sleep(5)
    print(sh("docker", "logs", "--tail", "5", "syslog", check_it=False).stderr[-500:], flush=True)

    # Restart the server, so that it trusts the test CA for TLS.
    global server
    server.send_signal(signal.SIGTERM)
    server.wait(timeout=30)
    start_server()

    # Native, so that it can say something on demand, and no toolchain to fetch.
    name = "talker"
    run_moor("apps", "create", name)
    site = work / "site"
    site.mkdir()
    (site / "moorage.toml").write_text(f'[app]\nname = "{name}"\nruntime = "binary"\n\n[processes.web]\ncommand = "sh -c \'echo started-from-stdout; while true; do sleep 60; done\'"\nexpose = false\nhealth = {{ min_uptime = "1s" }}\n')
    run_moor("server", "telemetry", "add", "collector", "--kind", "otlp", "--address", "http://127.0.0.1:4318", "--logs", "--metrics", "--app", name)
    run_moor("server", "telemetry", "add", "udp", "--kind", "syslog", "--address", "udp://127.0.0.1:5514", "--app", name)
    run_moor("server", "telemetry", "add", "tcp", "--kind", "syslog", "--address", "tcp://127.0.0.1:5601", "--app", name)
    run_moor("server", "telemetry", "add", "tls", "--kind", "syslog", "--address", "tls://localhost:6514", "--app", name)
    done = subprocess.run([moor, "deploy", "--artifact", str(site), "--app", name, "--server", f"http://127.0.0.1:{port}", "--token", (data / "keys" / "admin-token").read_text().strip()], env=env, cwd=site, capture_output=True, text=True)
    check("telemetry: the app deploys", done.returncode == 0, done.stdout[-500:] + done.stderr[-500:])
    run_moor("run", "--app", name, "--", "sh", "-c", "echo telling-stdout; echo telling-stderr >&2")

    def seen(path, *texts):
        file = out_dir / path
        return file.exists() and all(t in file.read_text(errors="replace") for t in texts)

    def otel_logs():
        records = []
        file = out_dir / "otel.json"
        if not file.exists():
            return records
        for line in file.read_text(errors="replace").splitlines():
            for rl in json.loads(line).get("resourceLogs", []):
                attrs = {a["key"]: next(iter(a["value"].values())) for a in rl["resource"]["attributes"]}
                for sl in rl.get("scopeLogs", []):
                    for r in sl.get("logRecords", []):
                        records.append((attrs, {a["key"]: next(iter(a["value"].values())) for a in r.get("attributes", [])}, r.get("body", {}).get("stringValue", "")))
        return records

    def otel_metrics():
        names = set()
        file = out_dir / "otel.json"
        if file.exists():
            for line in file.read_text(errors="replace").splitlines():
                for rm in json.loads(line).get("resourceMetrics", []):
                    for sm in rm.get("scopeMetrics", []):
                        names |= {m["name"] for m in sm.get("metrics", [])}
        return names

    wait_for("telemetry: the collector takes logs", lambda: any(body == "telling-stdout" for _, _, body in otel_logs()))
    records = otel_logs()
    record = next((r for r in records if r[2] == "telling-stderr"), None)
    check("telemetry: a log record says which stream", record is not None and record[1].get("log.iostream") == "stderr", str(record))
    check("telemetry: a resource says which app", record is not None and record[0].get("service.name") == name and record[0].get("deployment.environment.name") == "production", str(record))
    names = wait_for("telemetry: the collector takes metrics", otel_metrics, 150)
    print("metrics seen:", sorted(names or [])[:12], flush=True)
    check("telemetry: a metric named as documented", bool(names) and any(n.startswith("moorage.") for n in names), str(names))

    for path, what in (("udp.log", "UDP"), ("tcp.log", "TCP"), ("tls.log", "TLS")):
        if wait_for(f"telemetry: syslog takes it over {what}", lambda: seen(path, "telling-stdout", "telling-stderr")):
            lines = (out_dir / path).read_text(errors="replace").splitlines()
            out = next(line for line in lines if line.endswith("telling-stdout"))
            err = next(line for line in lines if line.endswith("telling-stderr"))
            fields = out.split("|")
            check(f"telemetry: syslog over {what} reads its fields", fields[1] == name and fields[3] == "production" and fields[4] == "local0" and fields[5] == "info", out)
            check(f"telemetry: syslog over {what} reads standard error's severity", err.split("|")[5] == "err", err)
    listed = run_moor("server", "telemetry", "--json").stdout
    check("telemetry: no sink reports a problem", '"last_error":"' not in listed.replace(" ", "") or all(not s.get("last_error") for s in json.loads(listed)), listed[:600])


try:
    write_config()
    subprocess.run(["sudo", "sh", "-c", "grep -q host.docker.internal /etc/hosts || echo '127.0.0.1 host.docker.internal' >> /etc/hosts"], check=True)
    sh("openssl", "version")
    (work / "ca.pem").write_text("")
    start_server()
    for what in wanted:
        {"gitea": lambda: try_forge(Gitea()), "gitlab": lambda: try_forge(GitLab()), "telemetry": try_telemetry}[what]()
except Exception as e:
    check("the drill ran to the end", False, repr(e))
    raise
finally:
    if server is not None and server.poll() is None:
        server.send_signal(signal.SIGTERM)
        try:
            server.wait(timeout=30)
        except subprocess.TimeoutExpired:
            server.kill()
    if failures:
        print("\n=== the server's log\n" + (work / "server.log").read_text(errors="replace")[-6000:])
        for c in ("gitea", "gitlab", "otel", "syslog"):
            logs = subprocess.run(["docker", "logs", "--tail", "40", c], capture_output=True, text=True)
            if logs.returncode == 0:
                print(f"\n=== {c}\n{logs.stdout[-2500:]}{logs.stderr[-2500:]}")
if failures:
    sys.exit(f"{len(failures)} failed: {', '.join(failures)}")
print("All of it works against the real thing.")
