"""One real round trip for services on a cloud: PostgreSQL and Valkey.

Usage: smoke_services.py aws

Creates both services and one machine, connects from this host (psql in a
Docker container) and from the machine, checks list and attach, then destroys
everything. Takes 15 to 30 minutes, mostly waiting for the cloud. Nothing
stops a service by itself if this is interrupted: check list_services().
"""
import asyncio
import subprocess
import sys
import time
from datetime import timedelta

from nepho import MachineSpec, ServiceSpec, Setup


def hide(text, *services):
    for s in services:
        text = text.replace(s.password, "<password>")
    return text


async def main():
    if sys.argv[1] == "aws":
        from nepho.aws import AwsProvider as Cloud
    else:
        raise SystemExit("usage: smoke_services.py aws")
    p = Cloud()
    ttl = timedelta(hours=1)
    t = time.time()
    made = await asyncio.gather(
        p.create_service(ServiceSpec(kind="postgres", ttl=ttl)),
        p.create_service(ServiceSpec(kind="valkey", ttl=ttl)),
        p.create(MachineSpec(ttl=ttl, setup=Setup(packages=["postgresql-client", "redis-tools"]))),
        return_exceptions=True)
    print("created", [getattr(x, "id", x) for x in made], f"{time.time() - t:.0f}s")
    try:
        for x in made:
            if isinstance(x, BaseException):
                raise x
        db, cache, m = made
        print("postgres:", db.state, hide(f"{db.url} | {db.internal_url}", db))
        print("valkey:  ", cache.state, hide(f"{cache.url} | {cache.internal_url}", cache))

        r = subprocess.run(["docker", "run", "--rm", "postgres:17", "psql", db.url, "-Atc",
                            "select current_user, split_part(version(), ' ', 2), ssl from pg_stat_ssl "
                            "where pid = pg_backend_pid()"], capture_output=True, text=True)
        print("postgres from this host:", r.returncode, r.stdout.strip(), hide(r.stderr.strip(), db))
        r = await m.run(["psql", db.internal_url, "-Atc", "create table t as select 42 as n; select n from t"])
        print("postgres from the machine:", r.code, r.stdout.split(), hide(r.stderr.strip(), db))
        for words in (["ping"], ["set", "k", "v"], ["get", "k"], ["info", "server"]):
            r = await m.run(["redis-cli", "-u", cache.internal_url, "--no-auth-warning", *words])
            out = [x for x in r.stdout.split() if "version" in x][:2] if words[0] == "info" else r.stdout.strip()
            print("valkey from the machine:", words, r.code, out, hide(r.stderr.strip(), cache))

        listed = {s.id: s for s in await Cloud().list_services()}  # as a second client would
        print("listed:", sorted((s.id, s.kind, s.version, s.state) for s in listed.values()))
        print("attach gives the same urls:",
              listed[db.id].url == db.url and listed[cache.id].internal_url == cache.internal_url)
        print("machines:", [(x.id, x.state) for x in await p.list()])
    finally:
        t = time.time()
        gone = await asyncio.gather(*(x.destroy() for x in made if not isinstance(x, BaseException)),
                                    return_exceptions=True)
        print("destroyed", gone, f"{time.time() - t:.0f}s")
        print("services left:", [(s.id, s.state) for s in await p.list_services()])
        print("machines left:", [(x.id, x.state) for x in await p.list() if x.state != "terminated"])


asyncio.run(main())
