#!/usr/bin/env python3
"""Tries the images that services would run, under one engine, with real clients, and says what it found.

It is a test of what spec/13-services.md claims, more than a test of Moorage: nothing here runs Moorage. A claim that turns out wrong
is a finding, so that most of what it tries is reported rather than failing the run. What would stop the design altogether does fail it.

services-check.py ENGINE RESULTS.json
"""

import json
import os
import re
import secrets
import shutil
import socket
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.request

ENGINE = sys.argv[1]
RESULTS = sys.argv[2]

POSTGRES_IMAGE = "docker.io/library/postgres:18-trixie"
VERSITY_IMAGE = "docker.io/versity/versitygw:v1.10.0"
SEAWEED_IMAGE = "docker.io/chrislusf/seaweedfs:4.52"
SEAWEED_FALLBACK = "docker.io/chrislusf/seaweedfs:latest"

results = {"engine": ENGINE, "postgres": {}, "versity": {}, "seaweedfs": {}}
blocking = []
to_remove = {"containers": [], "volumes": [], "networks": []}


def engine(*args, check=True, timeout=300, capture=True):
    done = subprocess.run([ENGINE, *args], capture_output=capture, text=True, timeout=timeout)
    if check and done.returncode != 0:
        raise RuntimeError(f"{ENGINE} {' '.join(args[:3])}: {done.stderr.strip()[-400:]}")
    return done


def free_port():
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


def container(name, *args):
    to_remove["containers"].append(name)
    engine("run", "-d", "--name", name, "--restart=no", "--label", "io.moorage.first-test=true", *args)


def volume(name):
    to_remove["volumes"].append(name)
    engine("volume", "create", name)
    return name


def remove(name):
    engine("rm", "-f", name, check=False)


def wait_until(what, ready, limit=120):
    began = time.monotonic()
    while time.monotonic() - began < limit:
        try:
            if ready():
                return round(time.monotonic() - began, 2)
        except Exception:
            pass
        time.sleep(0.25)
    raise TimeoutError(f"{what} was not ready in {limit}s")


def memory_at_rest(name, settle=20):
    time.sleep(settle)
    return engine("stats", "--no-stream", "--format", "{{.MemUsage}}", name).stdout.strip()


def image_size(image):
    out = engine("image", "inspect", "--format", "{{.Size}}", image).stdout.strip()
    return f"{int(out) / 1e6:.0f} MB"


def image_digest(image):
    done = engine("image", "inspect", "--format", "{{index .RepoDigests 0}}", image, check=False)
    return done.stdout.strip()


def section(title):
    print(f"\n=== {title}", flush=True)


def note(group, name, value):
    results[group][name] = value
    print(f"  {name}: {value}", flush=True)


def tried(group, name, try_it):
    """Reports how a claim turned out, whichever way it did."""
    try:
        value = try_it()
        note(group, name, "yes" if value is None else value)
    except Exception as e:  # pylint: disable=broad-except
        text = re.sub(r"\s+", " ", str(e))[:300]
        note(group, name, f"no: {text}")


def try_postgres():
    section("PostgreSQL")
    group = "postgres"
    password = secrets.token_urlsafe(24)
    began = time.monotonic()
    engine("pull", POSTGRES_IMAGE, timeout=600)
    note(group, "pull_seconds", round(time.monotonic() - began, 1))
    note(group, "image_size", image_size(POSTGRES_IMAGE))
    note(group, "digest", image_digest(POSTGRES_IMAGE))
    port = free_port()
    vol = volume("moorage-first-test-pg")
    run = ("-e", f"POSTGRES_PASSWORD={password}", "-p", f"127.0.0.1:{port}:5432", "-v", f"{vol}:/var/lib/postgresql", "--stop-signal=SIGINT", "--stop-timeout=120")
    name = "moorage-svc-first-test-pg"
    container(name, *run, POSTGRES_IMAGE)

    def healthy():
        return engine("exec", name, "pg_isready", "-U", "postgres", "-h", "127.0.0.1", check=False).returncode == 0

    def sql(query, target=name):
        return engine("exec", target, "psql", "-U", "postgres", "-At", "-c", query).stdout.strip()

    try:
        note(group, "first_start_seconds", wait_until("postgres", healthy))
    except TimeoutError as e:
        print(engine("logs", name, check=False).stderr[-3000:])
        blocking.append(f"PostgreSQL: {e}")
        return
    note(group, "memory_at_rest", memory_at_rest(name))
    note(group, "version", sql("show server_version"))
    note(group, "data_directory", sql("show data_directory"))
    note(group, "runs_as", engine("exec", name, "id", "-un").stdout.strip())

    sql("create table kept (value text)")
    sql("insert into kept values ('still here')")
    # What a native app does, on the address it would be given, as rootless Podman publishes differently.
    if shutil.which("psql"):
        env = {**os.environ, "PGPASSWORD": password}

        def over_loopback():
            done = subprocess.run(["psql", "-h", "127.0.0.1", "-p", str(port), "-U", "postgres", "-At", "-c", "select value from kept"], capture_output=True, text=True, env=env, timeout=30)
            if done.stdout.strip() != "still here":
                raise RuntimeError(done.stderr.strip())

        tried(group, "reachable_on_loopback_port", over_loopback)
    tried(group, "dump_with_the_image_own_tools", lambda: f"{len(engine('exec', name, 'pg_dump', '-U', 'postgres').stdout)} bytes")

    began = time.monotonic()
    engine("stop", "-t", "120", name, timeout=150)
    note(group, "stop_seconds", round(time.monotonic() - began, 2))
    logs = engine("logs", name, check=False)
    text = logs.stdout + logs.stderr
    note(group, "stopped_cleanly", "yes" if "database system is shut down" in text else "no")
    note(group, "stopped_by_fast_shutdown", "yes" if "fast shutdown" in text else "no")
    remove(name)

    # The data outlives the container.
    began = time.monotonic()
    container(name, *run, POSTGRES_IMAGE)
    try:
        wait_until("postgres again", healthy)
        note(group, "second_start_seconds", round(time.monotonic() - began, 2))
        note(group, "data_survived_a_new_container", "yes" if sql("select value from kept") == "still here" else "no")
    except Exception as e:  # pylint: disable=broad-except
        blocking.append(f"PostgreSQL did not come back on its volume: {e}")

    # A container app reaches it by name on a network of its environment, with no port published.
    def by_name():
        network = "moorage-first-test-net"
        to_remove["networks"].append(network)
        engine("network", "create", network)
        vol2 = volume("moorage-first-test-pg-net")
        db = "moorage-svc-first-test-pg-net"
        container(db, "--network", network, "-e", f"POSTGRES_PASSWORD={password}", "-v", f"{vol2}:/var/lib/postgresql", POSTGRES_IMAGE)
        wait_until("postgres on a network", lambda: engine("exec", db, "pg_isready", "-U", "postgres", "-h", "127.0.0.1", check=False).returncode == 0)
        done = engine("run", "--rm", "--network", network, "-e", f"PGPASSWORD={password}", POSTGRES_IMAGE, "psql", "-h", db, "-U", "postgres", "-At", "-c", "select 1", check=False)
        remove(db)
        if done.stdout.strip() != "1":
            raise RuntimeError(done.stderr.strip())

    tried(group, "reachable_by_name_on_a_network", by_name)

    # A dump made by this version loads into a fresh volume, which is how a restore and a preview's seed work.
    def restores():
        dump = engine("exec", name, "pg_dump", "-U", "postgres").stdout
        vol3 = volume("moorage-first-test-pg-restore")
        target = "moorage-svc-first-test-pg-restore"
        container(target, "-e", f"POSTGRES_PASSWORD={password}", "-v", f"{vol3}:/var/lib/postgresql", POSTGRES_IMAGE)
        wait_until("a fresh postgres", lambda: engine("exec", target, "pg_isready", "-U", "postgres", "-h", "127.0.0.1", check=False).returncode == 0)
        subprocess.run([ENGINE, "exec", "-i", target, "psql", "-U", "postgres", "-q"], input=dump, text=True, check=True, capture_output=True, timeout=120)
        value = sql("select value from kept", target)
        remove(target)
        if value != "still here":
            raise RuntimeError(f"it read {value!r}")

    tried(group, "dump_restores_into_a_fresh_volume", restores)

    # The major version on a volume is what a refused deployment is for: what the older image makes of the newer's data.
    def older_image():
        older = "docker.io/library/postgres:17-trixie"
        engine("pull", older, timeout=600)
        engine("stop", "-t", "120", name, timeout=150)
        target = "moorage-svc-first-test-pg-older"
        # 17 looks in another directory of the volume, so that it finds no data, which is worse than refusing.
        vol_of_18 = vol
        container(target, "-e", f"POSTGRES_PASSWORD={password}", "-v", f"{vol_of_18}:/var/lib/postgresql/data", older)
        time.sleep(8)
        logs = engine("logs", target, check=False)
        running = engine("inspect", "--format", "{{.State.Running}}", target, check=False).stdout.strip()
        tail = (logs.stdout + logs.stderr).strip()[-300:]
        remove(target)
        return f"running={running}; it said: {tail}"

    tried(group, "an_older_major_on_the_same_volume", older_image)
    remove(name)


def s3_client(endpoint, access, secret):
    import boto3  # pylint: disable=import-outside-toplevel
    from botocore.config import Config  # pylint: disable=import-outside-toplevel

    return boto3.client(
        "s3",
        endpoint_url=endpoint,
        aws_access_key_id=access,
        aws_secret_access_key=secret,
        region_name="us-east-1",
        config=Config(signature_version="s3v4", s3={"addressing_style": "path"}, retries={"max_attempts": 1}),
    )


def try_s3_claims(group, s3, endpoint, access, secret):
    bucket = "moorage-test"

    def make_bucket():
        try:
            s3.create_bucket(Bucket=bucket)
        except s3.exceptions.BucketAlreadyOwnedByYou:
            pass
        except s3.exceptions.BucketAlreadyExists:
            pass

    tried(group, "make_a_bucket", make_bucket)

    def put_get():
        s3.put_object(Bucket=bucket, Key="dir/hello.txt", Body=b"hello", ContentType="text/plain; charset=utf-8", Metadata={"owner": "me"}, CacheControl="no-store")
        got = s3.get_object(Bucket=bucket, Key="dir/hello.txt")
        if got["Body"].read() != b"hello":
            raise RuntimeError("it read something else")
        return f"yes; content type {got['ContentType']}, metadata {got['Metadata']}, cache control {got.get('CacheControl')}"

    tried(group, "put_get_and_what_goes_with_it", put_get)
    tried(group, "ranged_get", lambda: s3.get_object(Bucket=bucket, Key="dir/hello.txt", Range="bytes=1-3")["Body"].read().decode())
    tried(group, "list_with_prefix_and_delimiter", lambda: str([p["Prefix"] for p in s3.list_objects_v2(Bucket=bucket, Delimiter="/").get("CommonPrefixes", [])]))
    tried(group, "copy", lambda: s3.copy_object(Bucket=bucket, Key="copy.txt", CopySource={"Bucket": bucket, "Key": "dir/hello.txt"}) and None)
    tried(group, "tags", lambda: s3.put_object_tagging(Bucket=bucket, Key="copy.txt", Tagging={"TagSet": [{"Key": "a", "Value": "b"}]}) and str(s3.get_object_tagging(Bucket=bucket, Key="copy.txt")["TagSet"]))
    tried(group, "a_name_with_unicode", lambda: s3.put_object(Bucket=bucket, Key="réçu/日本語 file.txt", Body=b"x") and s3.get_object(Bucket=bucket, Key="réçu/日本語 file.txt")["Body"].read().decode())

    def multipart():
        started = s3.create_multipart_upload(Bucket=bucket, Key="big.bin")
        parts = []
        chunk = os.urandom(5 * 1024 * 1024)
        for number in (1, 2, 3):
            part = s3.upload_part(Bucket=bucket, Key="big.bin", UploadId=started["UploadId"], PartNumber=number, Body=chunk)
            parts.append({"ETag": part["ETag"], "PartNumber": number})
        s3.complete_multipart_upload(Bucket=bucket, Key="big.bin", UploadId=started["UploadId"], MultipartUpload={"Parts": parts})
        size = s3.head_object(Bucket=bucket, Key="big.bin")["ContentLength"]
        if size != 15 * 1024 * 1024:
            raise RuntimeError(f"it is {size} bytes")

    tried(group, "multipart", multipart)

    def presigned_get():
        url = s3.generate_presigned_url("get_object", Params={"Bucket": bucket, "Key": "dir/hello.txt"}, ExpiresIn=60)
        with urllib.request.urlopen(url, timeout=20) as res:
            if res.read() != b"hello":
                raise RuntimeError("it read something else")

    tried(group, "presigned_get", presigned_get)

    def presigned_put():
        url = s3.generate_presigned_url("put_object", Params={"Bucket": bucket, "Key": "signed.txt"}, ExpiresIn=60)
        req = urllib.request.Request(url, data=b"signed", method="PUT")
        with urllib.request.urlopen(req, timeout=20):
            pass
        if s3.get_object(Bucket=bucket, Key="signed.txt")["Body"].read() != b"signed":
            raise RuntimeError("it read something else")

    tried(group, "presigned_put", presigned_put)

    def presigned_other_host():
        # A presigned URL is signed for its host, which is what the spec says stops a loopback address from being any use to a browser.
        other = endpoint.replace("127.0.0.1", "localhost")
        url = s3.generate_presigned_url("get_object", Params={"Bucket": bucket, "Key": "dir/hello.txt"}, ExpiresIn=60).replace(endpoint, other)
        try:
            with urllib.request.urlopen(url, timeout=20) as res:
                return f"accepted, with {res.status}, which the spec says it should not be"
        except urllib.error.HTTPError as e:
            return f"refused with {e.code}, as the spec says"

    tried(group, "presigned_for_another_host", presigned_other_host)

    def cors():
        rules = {"CORSRules": [{"AllowedMethods": ["GET"], "AllowedOrigins": ["https://example.com"], "AllowedHeaders": ["*"]}]}
        s3.put_bucket_cors(Bucket=bucket, CORSConfiguration=rules)
        return str(s3.get_bucket_cors(Bucket=bucket)["CORSRules"][0]["AllowedOrigins"])

    tried(group, "cors", cors)

    def policy():
        document = json.dumps({"Version": "2012-10-17", "Statement": [{"Effect": "Allow", "Principal": "*", "Action": "s3:GetObject", "Resource": f"arn:aws:s3:::{bucket}/public/*"}]})
        s3.put_object(Bucket=bucket, Key="public/open.txt", Body=b"open")
        s3.put_object(Bucket=bucket, Key="private/closed.txt", Body=b"closed")
        s3.put_bucket_policy(Bucket=bucket, Policy=document)
        s3.get_bucket_policy(Bucket=bucket)
        with urllib.request.urlopen(f"{endpoint}/{bucket}/public/open.txt", timeout=20) as res:
            opened = res.status
        try:
            with urllib.request.urlopen(f"{endpoint}/{bucket}/private/closed.txt", timeout=20) as res:
                closed = res.status
        except urllib.error.HTTPError as e:
            closed = e.code
        return f"public read {opened}, private read {closed}"

    tried(group, "bucket_policy_is_enforced", policy)

    def versioning():
        s3.put_bucket_versioning(Bucket=bucket, VersioningConfiguration={"Status": "Enabled"})
        s3.put_object(Bucket=bucket, Key="versioned.txt", Body=b"one")
        s3.put_object(Bucket=bucket, Key="versioned.txt", Body=b"two")
        count = len(s3.list_object_versions(Bucket=bucket, Prefix="versioned.txt").get("Versions", []))
        s3.put_bucket_versioning(Bucket=bucket, VersioningConfiguration={"Status": "Suspended"})
        return f"{count} versions"

    tried(group, "versioning", versioning)

    def lifecycle():
        s3.put_bucket_lifecycle_configuration(Bucket=bucket, LifecycleConfiguration={"Rules": [{"ID": "old", "Status": "Enabled", "Filter": {"Prefix": "tmp/"}, "Expiration": {"Days": 1}}]})
        return str(len(s3.get_bucket_lifecycle_configuration(Bucket=bucket)["Rules"]))

    tried(group, "lifecycle_rules", lifecycle)

    def acl():
        s3.put_object_acl(Bucket=bucket, Key="copy.txt", ACL="public-read")
        return str(len(s3.get_object_acl(Bucket=bucket, Key="copy.txt")["Grants"]))

    tried(group, "object_acls", acl)

    def file_and_directory():
        s3.put_object(Bucket=bucket, Key="both", Body=b"a file")
        s3.put_object(Bucket=bucket, Key="both/inside", Body=b"below it")
        return "both exist"

    tried(group, "keys_a_and_a_slash_b", file_and_directory)

    def many_small():
        began = time.monotonic()
        for i in range(1000):
            s3.put_object(Bucket=bucket, Key=f"many/{i:04}.txt", Body=b"x")
        wrote = time.monotonic() - began
        began = time.monotonic()
        count = 0
        for page in s3.get_paginator("list_objects_v2").paginate(Bucket=bucket, Prefix="many/"):
            count += len(page.get("Contents", []))
        return f"1000 written in {wrote:.1f}s, {count} listed in {time.monotonic() - began:.2f}s"

    tried(group, "many_small_objects", many_small)

    def aws_cli():
        env = {**os.environ, "AWS_ACCESS_KEY_ID": access, "AWS_SECRET_ACCESS_KEY": secret, "AWS_DEFAULT_REGION": "us-east-1"}
        with tempfile.TemporaryDirectory() as d:
            for name, content in (("a.txt", "a"), ("b.txt", "b")):
                with open(os.path.join(d, name), "w", encoding="utf-8") as f:
                    f.write(content)
            for args in (["sync", d, f"s3://{bucket}/synced"], ["ls", f"s3://{bucket}/synced/"]):
                done = subprocess.run(["aws", "--endpoint-url", endpoint, "s3", *args], capture_output=True, text=True, env=env, timeout=120)
                if done.returncode != 0:
                    raise RuntimeError(done.stderr.strip())

    if shutil.which("aws"):
        tried(group, "aws_cli_sync_and_ls", aws_cli)
    return bucket


def try_versity():
    section("Versity Gateway")
    group = "versity"
    access, secret = "moorage", secrets.token_urlsafe(24)
    began = time.monotonic()
    engine("pull", VERSITY_IMAGE, timeout=600)
    note(group, "pull_seconds", round(time.monotonic() - began, 1))
    note(group, "image_size", image_size(VERSITY_IMAGE))
    note(group, "digest", image_digest(VERSITY_IMAGE))
    port = free_port()
    data, meta = volume("moorage-first-test-s3"), volume("moorage-first-test-s3-meta")
    name = "moorage-svc-first-test-s3"
    # A secret goes in the environment, as what is on a command line can be read by anyone on the machine.
    def run(n, d, m):
        container(
            n, "-p", f"127.0.0.1:{port}:7070", "-e", f"ROOT_ACCESS_KEY={access}", "-e", f"ROOT_SECRET_KEY={secret}",
            "-e", "VGW_BACKEND=posix", "-e", "VGW_BACKEND_ARGS=/data/s3", "-e", "VGW_META_SIDECAR=/data/meta", "-e", "VGW_HEALTH=/health",
            "-v", f"{d}:/data/s3", "-v", f"{m}:/data/meta", VERSITY_IMAGE,
        )

    endpoint = f"http://127.0.0.1:{port}"

    def healthy():
        with urllib.request.urlopen(f"{endpoint}/health", timeout=2) as res:
            return res.status == 200

    run(name, data, meta)
    try:
        note(group, "first_start_seconds", wait_until("versity", healthy, 60))
    except TimeoutError as e:
        logs = engine("logs", name, check=False)
        print(logs.stdout[-2000:], logs.stderr[-2000:])
        print(engine("run", "--rm", "--entrypoint", "cat", VERSITY_IMAGE, "/usr/local/bin/docker-entrypoint.sh", check=False).stdout)
        blocking.append(f"Versity Gateway: {e}")
        return
    note(group, "memory_at_rest", memory_at_rest(name))
    note(group, "runs_as", engine("exec", name, "id", "-un", check=False).stdout.strip())
    note(group, "has_tar", engine("exec", name, "sh", "-c", "command -v tar", check=False).stdout.strip() or "no")
    s3 = s3_client(endpoint, access, secret)
    bucket = try_s3_claims(group, s3, endpoint, access, secret)
    note(group, "memory_after_use", engine("stats", "--no-stream", "--format", "{{.MemUsage}}", name).stdout.strip())
    note(group, "what_is_on_disk", engine("exec", name, "sh", "-c", f"ls -la /data/s3/{bucket}/dir; ls -lan /data/s3/{bucket} | head -5; find /data/meta | head -8").stdout)

    # The claim that decides this: a plain tar of its directories is a dump, and restores with its metadata.
    def tar_restores():
        with tempfile.TemporaryDirectory() as d:
            dump = os.path.join(d, "dump.tar")
            with open(dump, "wb") as f:
                subprocess.run([ENGINE, "exec", name, "tar", "-C", "/data", "-cf", "-", "s3", "meta"], stdout=f, check=True, timeout=300)
            size = os.path.getsize(dump)
            engine("stop", "-t", "10", name)
            remove(name)
            data2, meta2 = volume("moorage-first-test-s3-b"), volume("moorage-first-test-s3-meta-b")
            run(name, data2, meta2)
            wait_until("versity on fresh volumes", healthy, 60)
            with open(dump, "rb") as f:
                subprocess.run([ENGINE, "exec", "-i", name, "tar", "-C", "/data", "-xf", "-"], stdin=f, check=True, timeout=300)
            got = s3.get_object(Bucket=bucket, Key="dir/hello.txt")
            body = got["Body"].read()
            if body != b"hello" or got["Metadata"] != {"owner": "me"} or not got["ContentType"].startswith("text/plain"):
                raise RuntimeError(f"it read {body!r}, {got['ContentType']}, {got['Metadata']}")
            return f"yes, with its metadata, from a tar of {size / 1e6:.1f} MB"

    tried(group, "a_tar_restores_with_its_metadata", tar_restores)

    def survives_a_new_container():
        engine("stop", "-t", "10", name)
        remove(name)
        run(name, data, meta)
        wait_until("versity again", healthy, 60)
        got = s3.get_object(Bucket=bucket, Key="dir/hello.txt")
        if got["Metadata"] != {"owner": "me"}:
            raise RuntimeError(str(got["Metadata"]))

    tried(group, "data_survived_a_new_container", survives_a_new_container)
    began = time.monotonic()
    engine("stop", "-t", "30", name, timeout=60)
    note(group, "stop_seconds", round(time.monotonic() - began, 2))
    remove(name)


def try_seaweedfs():
    section("SeaweedFS")
    group = "seaweedfs"
    access, secret = "moorage", secrets.token_urlsafe(24)
    image = SEAWEED_IMAGE
    began = time.monotonic()
    if engine("pull", image, check=False, timeout=600).returncode != 0:
        image = SEAWEED_FALLBACK
        engine("pull", image, timeout=600)
    note(group, "image", image)
    note(group, "pull_seconds", round(time.monotonic() - began, 1))
    note(group, "image_size", image_size(image))
    note(group, "digest", image_digest(image))
    port = free_port()
    data = volume("moorage-first-test-weed")
    name = "moorage-svc-first-test-weed"
    container(name, "-p", f"127.0.0.1:{port}:8333", "-e", f"AWS_ACCESS_KEY_ID={access}", "-e", f"AWS_SECRET_ACCESS_KEY={secret}", "-v", f"{data}:/data", image)
    endpoint = f"http://127.0.0.1:{port}"
    s3 = s3_client(endpoint, access, secret)

    def healthy():
        s3.list_buckets()
        return True

    try:
        note(group, "first_start_seconds", wait_until("seaweedfs", healthy, 120))
    except TimeoutError as e:
        logs = engine("logs", name, check=False)
        print(logs.stdout[-2000:], logs.stderr[-2000:])
        note(group, "failed", str(e))
        return
    note(group, "memory_at_rest", memory_at_rest(name))
    note(group, "processes", engine("exec", name, "sh", "-c", "ps -o args 2>/dev/null | head -12", check=False).stdout)
    try_s3_claims(group, s3, endpoint, access, secret)
    note(group, "memory_after_use", engine("stats", "--no-stream", "--format", "{{.MemUsage}}", name).stdout.strip())
    note(group, "what_is_on_disk", engine("exec", name, "sh", "-c", "find /data -maxdepth 2 | head -14", check=False).stdout)
    began = time.monotonic()
    engine("stop", "-t", "30", name, timeout=60)
    note(group, "stop_seconds", round(time.monotonic() - began, 2))
    remove(name)


def clean_up():
    for name in to_remove["containers"]:
        engine("rm", "-f", name, check=False)
    for name in to_remove["networks"]:
        engine("network", "rm", name, check=False)
    for name in to_remove["volumes"]:
        engine("volume", "rm", "-f", name, check=False)


def summary():
    keys = []
    for group in ("postgres", "versity", "seaweedfs"):
        for key in results[group]:
            if key not in keys:
                keys.append(key)
    print("\n=== Results as JSON\n" + json.dumps(results, indent=1, ensure_ascii=False), flush=True)
    path = os.environ.get("GITHUB_STEP_SUMMARY")
    if not path:
        return
    with open(path, "a", encoding="utf-8") as out:
        out.write(f"## Under {ENGINE}\n\n")
        for group in ("postgres",):
            out.write(f"### {group}\n\n| | |\n|---|---|\n")
            for key, value in results[group].items():
                out.write(f"| {key} | {str(value)[:300].replace(chr(10), ' ').replace('|', '/')} |\n")
        out.write("\n### S3\n\n| | Versity Gateway | SeaweedFS |\n|---|---|---|\n")
        for key in keys:
            if key in results["versity"] or key in results["seaweedfs"]:
                row = [str(results[g].get(key, ""))[:220].replace("\n", " ").replace("|", "/") for g in ("versity", "seaweedfs")]
                out.write(f"| {key} | {row[0]} | {row[1]} |\n")


def main():
    try:
        try_postgres()
        try_versity()
        try_seaweedfs()
    finally:
        summary()
        with open(RESULTS, "w", encoding="utf-8") as f:
            json.dump(results, f, indent=1, ensure_ascii=False)
        clean_up()
    if blocking:
        print("\nWhat would stop the design:", file=sys.stderr)
        for line in blocking:
            print(f"  {line}", file=sys.stderr)
        sys.exit(1)


if __name__ == "__main__":
    main()
