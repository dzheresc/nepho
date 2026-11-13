"""One real round trip on AWS: create, run, stream, list, reattach, destroy.

Costs a few cents: one small instance for a few minutes. The machine has a
30-minute TTL, so it terminates itself even if this script is interrupted.
"""
import asyncio
import time
from datetime import timedelta

from nepho import MachineSpec, Setup
from nepho.aws import AwsProvider


async def main():
    p = AwsProvider()
    t = time.time()
    m = await p.create(MachineSpec(os="ubuntu-24.04", ttl=timedelta(minutes=30), setup=Setup(
        packages=["jq"], files={"/etc/nepho-test": "hello\n"},
        script="echo done > /root/marker")))
    print("created", m.id, f"{time.time() - t:.0f}s")
    try:
        r = await m.run("uname -a; cat /etc/nepho-test; jq --version; sudo cat /root/marker; whoami")
        print(r.code, r.stdout, r.stderr)
        pr = await m.execute(["cat"])
        pr.stdin.write("stream ok\n")
        print("echo:", (await pr.stdout.readline()).strip())
        pr.stdin.close()
        print("cat exit", await pr.wait())
        print("as root:", (await m.run("id -un", user="root")).stdout.strip())
        await m.detach()
        for i in await p.list():
            print("list:", i.id, i.state, i.address)
        m = await p.attach(m.id)
        print("reattached:", (await m.run("uptime")).stdout.strip())
    finally:
        t = time.time()
        await m.destroy()
        print("destroyed", f"{time.time() - t:.0f}s")
        for i in await p.list():
            print("after:", i.id, i.state)
        print("groups left:", len(p._groups("nepho-managed", "true")))


asyncio.run(main())
