"""Behaviour every backend must share.

Runs against Docker (exec transport) and against an sshd inside a Docker
container (SSH transport, the one the cloud providers use). No cloud is contacted.
"""
import asyncio
from datetime import timedelta

import pytest
import pytest_asyncio

from nepho import (RUNNING, STOPPED, Machine, MachineSpec, ProvisionError, ServiceSpec, Setup,
                   SetupError)
from nepho.core import Info
from nepho.docker import DockerProvider
from nepho.ssh import SshTransport, ensure_key

SSHD_SETUP = """
useradd -m -s /bin/bash tester
echo 'tester ALL=(ALL) NOPASSWD:ALL' > /etc/sudoers.d/tester
install -d -m 700 -o tester -g tester /home/tester/.ssh
install -m 600 -o tester -g tester /tmp/key.pub /home/tester/.ssh/authorized_keys
mkdir -p /run/sshd && /usr/sbin/sshd
"""


@pytest_asyncio.fixture(scope="session")
async def provider():
    return DockerProvider()


@pytest_asyncio.fixture(scope="session")
async def docker_machine(provider):
    m = await provider.create(MachineSpec(os="ubuntu-24.04", setup=Setup(
        files={"/etc/demo/hello.txt": "hello file\n"}, script="echo $GREETING > /made-by-script",
        env={"GREETING": "hi there"})))
    yield m
    await m.destroy()


@pytest_asyncio.fixture(scope="session")
async def ssh_machine(provider, tmp_path_factory):
    key_path, pub = ensure_key(str(tmp_path_factory.mktemp("key") / "id_rsa"))
    host = await provider.create(MachineSpec(os="debian-12", setup=Setup(
        packages=["openssh-server", "sudo"], files={"/tmp/key.pub": pub + "\n"}, script=SSHD_SETUP)))
    ip = (await host.run("hostname -i", check=True)).stdout.strip()
    m = Machine(provider, Info(host.id, RUNNING, host.labels, ip, "tester"),
                SshTransport(ip, "tester", key_path, timeout=30))
    yield m
    await m.destroy()


@pytest.fixture(params=["docker", "ssh"])
def m(request):
    return request.getfixturevalue(request.param + "_machine")


async def test_run(m):
    r = await m.run(["echo", "hi"])
    assert (r.code, r.stdout, r.stderr) == (0, "hi\n", "")


async def test_live_streams(m):
    p = await m.execute(["cat"])
    p.stdin.write("one\n")
    assert await p.stdout.readline() == "one\n"     # answered before stdin is closed
    p.stdin.write("two\nthree\n")
    p.stdin.close()
    assert [line async for line in p.stdout] == ["two\n", "three\n"]
    assert await p.wait() == 0


async def test_stderr_and_exit_code(m):
    r = await m.run("echo out; echo err >&2; exit 3")
    assert (r.code, r.stdout, r.stderr) == (3, "out\n", "err\n")


async def test_env_cwd_quoting(m):
    r = await m.run("echo \"$FOO\"; pwd", env={"FOO": "a b'c"}, cwd="/tmp")
    assert r.stdout == "a b'c\n/tmp\n"
    r = await m.run(["printf", "%s|", "a b", "c'd", "$x", "é"])
    assert r.stdout == "a b|c'd|$x|é|"


async def test_bad_cwd(m):
    r = await m.run(["true"], cwd="/nonexistent")
    assert r.code != 0 and "nonexistent" in r.stderr


async def test_user(m):
    assert (await m.run(["id", "-un"], user="root")).stdout == "root\n"


async def test_pty(m):
    r = await m.run("test -t 1 && echo is-a-tty", pty=True)
    assert "is-a-tty" in r.stdout and r.code == 0
    assert (await m.run("test -t 1 || echo no-tty")).stdout == "no-tty\n"


async def test_kill(m):
    p = await m.execute(["sleep", "1000"])
    assert p.pid > 0
    await p.kill()
    assert await asyncio.wait_for(p.wait(), 10) != 0


async def test_big_output_and_input(m):
    text = "x" * 79 + "\n"
    r = await m.run(["cat"], input=text * 20000)
    assert r.stdout == text * 20000


async def test_concurrent(m):
    rs = await asyncio.gather(*(m.run(f"sleep 0.3; echo {i}") for i in range(8)))
    assert [r.stdout for r in rs] == [f"{i}\n" for i in range(8)]


async def test_setup_applied(docker_machine):
    r = await docker_machine.run("cat /etc/demo/hello.txt /made-by-script")
    assert r.stdout == "hello file\nhi there\n"


async def test_setup_failure_destroys(provider):
    before = {x.id for x in await provider.list()}
    with pytest.raises(SetupError) as e:
        await provider.create(MachineSpec(setup=Setup(script="echo boom; exit 7")))
    assert e.value.code == 7 and "boom" in e.value.output
    assert {x.id for x in await provider.list()} == before


async def test_attach_detach(provider, docker_machine):
    p = await docker_machine.execute("sleep 2; echo done > /tmp/survivor")
    await docker_machine.detach()

    other = await DockerProvider().attach(docker_machine.id)   # as a second client would
    assert other.state == RUNNING and other.expires == docker_machine.expires
    await asyncio.sleep(3)
    assert (await other.run(["cat", "/tmp/survivor"])).stdout == "done\n"
    await other.detach()
    assert (await docker_machine.run(["echo", "back"])).stdout == "back\n"  # handle still usable
    with pytest.raises(ProvisionError):
        await provider.attach("m-doesnotexist")


async def test_dead_machines_listed_then_reaped(provider):
    m = await provider.create(MachineSpec(ttl=timedelta(seconds=2)))
    await asyncio.sleep(4)
    listed = {x.id: x for x in await provider.list()}
    assert listed[m.id].state == STOPPED          # dead, but still enumerated
    assert await provider.reap() >= 1
    assert m.id not in {x.id for x in await provider.list()}


# ---------------------------------------------------------------- services

async def _valkey(host, port, *commands):
    """Send commands in the Redis protocol and return one reply line for each."""
    reader, writer = await asyncio.open_connection(host, port)
    replies = []
    for words in commands:
        writer.write(b"".join([b"*%d\r\n" % len(words)]
                              + [b"$%d\r\n%s\r\n" % (len(w), w.encode()) for w in words]))
        line = (await reader.readline()).decode().strip()
        if line.startswith("$"):  # bulk string: the value is on the next line
            line = (await reader.readline()).decode().strip()
        replies.append(line)
    writer.close()
    return replies


async def _postgres_auth_request(host, port):
    """Open a session in the PostgreSQL protocol; return the authentication method asked for."""
    reader, writer = await asyncio.open_connection(host, port)
    body = (196608).to_bytes(4, "big") + b"user\0nepho\0database\0nepho\0\0"
    writer.write((len(body) + 4).to_bytes(4, "big") + body)
    head = await reader.readexactly(9)
    writer.close()
    return chr(head[0]), int.from_bytes(head[5:9], "big")


def _exec(provider, id, argv):
    r = provider._c.containers.get(id).exec_run(argv)
    return r.exit_code, r.output.decode()


async def test_valkey(provider):
    s = await provider.create_service(ServiceSpec(kind="valkey"))
    try:
        assert s.state == RUNNING and s.version == "8" and s.host == "127.0.0.1"
        assert s.url == f"redis://default:{s.password}@127.0.0.1:{s.port}/0"
        assert s.internal_url == f"redis://default:{s.password}@{s.internal_host}:6379/0"
        assert await _valkey(s.host, s.port, ["PING"]) == ["-NOAUTH Authentication required."]
        assert await _valkey(s.host, s.port, ["AUTH", s.password], ["SET", "k", "v"],
                             ["GET", "k"]) == ["+OK", "+OK", "v"]
        code, out = _exec(provider, s.id, ["valkey-cli", "-u", s.internal_url, "--no-auth-warning", "GET", "k"])
        assert (code, out.strip()) == (0, "v")
    finally:
        await s.destroy()
    assert s.state == "terminated"


async def test_postgres(provider):
    s = await provider.create_service(ServiceSpec(kind="postgres", version="16"))
    try:
        assert s.state == RUNNING and s.version == "16"
        assert s.url == f"postgresql://nepho:{s.password}@127.0.0.1:{s.port}/nepho"
        # 'R' + 10: the server on the published port asks for SCRAM password authentication
        assert await _postgres_auth_request(s.host, s.port) == ("R", 10)
        code, out = _exec(provider, s.id, ["psql", s.internal_url, "-Atc", "select current_user, version()"])
        assert code == 0 and out.startswith("nepho|PostgreSQL 16."), out
        bad = s.internal_url.replace(s.password, "wrong")
        assert _exec(provider, s.id, ["psql", bad, "-Atc", "select 1"])[0] != 0
    finally:
        await s.destroy()


async def test_services_list_attach_reap(provider, docker_machine):
    s = await provider.create_service(ServiceSpec(kind="valkey", labels={"team": "x"},
                                                  ttl=timedelta(seconds=1)))
    assert s.id.startswith("s-") and s.labels["team"] == "x"
    assert s.id in {x.id for x in await provider.list_services()}
    machines = {x.id for x in await provider.list()}
    assert s.id not in machines and docker_machine.id in machines   # the two lists are separate
    assert docker_machine.id not in {x.id for x in await provider.list_services()}

    other = await DockerProvider().attach_service(s.id)             # as a second client would
    assert other.url == s.url and other.expires == s.expires
    with pytest.raises(ProvisionError):
        await provider.attach_service("s-doesnotexist")
    with pytest.raises(ProvisionError):
        await provider.attach_service(docker_machine.id)

    await asyncio.sleep(2)
    assert (await provider.attach_service(s.id)).state == RUNNING   # nothing stops it at the TTL
    assert await provider.reap() >= 1
    assert s.id not in {x.id for x in await provider.list_services()}
    assert docker_machine.id in {x.id for x in await provider.list()}


async def test_service_bad_requests(provider):
    before = {x.id for x in await provider.list_services()}
    with pytest.raises(ProvisionError):
        await provider.create_service(ServiceSpec(kind="mysql"))
    with pytest.raises(ProvisionError):
        await provider.create_service(ServiceSpec(kind="valkey", version="0.0.0-nope"))
    assert {x.id for x in await provider.list_services()} == before
