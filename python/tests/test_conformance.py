"""Behaviour every backend must share.

Runs against Docker (exec transport) and against an sshd inside a Docker
container (SSH transport, the one the cloud providers use). No cloud is contacted.
"""
import asyncio
from datetime import timedelta

import pytest
import pytest_asyncio

from nepho import RUNNING, STOPPED, Machine, MachineSpec, ProvisionError, Setup, SetupError
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
