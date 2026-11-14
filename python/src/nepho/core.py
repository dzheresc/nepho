"""Core types: spec, text streams, process, machine, provider base.

No third-party imports here; each provider module imports its own SDK.
"""
from __future__ import annotations

import asyncio
import codecs
import math
import posixpath
import secrets
import shlex
import time
import urllib.request
from contextlib import suppress
from dataclasses import dataclass, field, replace
from datetime import datetime, timedelta, timezone
from typing import Awaitable, Callable, Mapping, Protocol, Sequence

# Labels (tags) put on every resource, identical in all three implementations.
L_MANAGED = "nepho-managed"
L_ID = "nepho-id"
L_EXPIRES = "nepho-expires"  # unix seconds
L_USER = "nepho-user"        # SSH login user, where it depends on the OS
L_KIND = "nepho-kind"        # services only: postgres | valkey
L_VERSION = "nepho-version"  # services only
L_SECRET = "nepho-secret"    # services on Docker: the generated password

PENDING, RUNNING, STOPPED, TERMINATED = "pending", "running", "stopped", "terminated"


class MachineError(Exception):
    pass


class ProvisionError(MachineError):
    """The backend could not create, find or destroy a machine."""


class TransportError(MachineError):
    """The machine could not be reached or the command could not be started."""


class SetupError(MachineError):
    """A prerequisite step failed. `output` holds what the step printed."""

    def __init__(self, step: str, code: int, output: str):
        super().__init__(f"setup step '{step}' failed with exit code {code}:\n{output}")
        self.step, self.code, self.output = step, code, output


class CommandError(MachineError):
    def __init__(self, result: "Result"):
        super().__init__(f"command failed with exit code {result.code}:\n{result.stderr}")
        self.result = result


@dataclass(frozen=True)
class Setup:
    """Prerequisites, applied in this order: packages, files, script. All run as root."""

    packages: Sequence[str] = ()
    files: Mapping[str, str] = field(default_factory=dict)  # absolute path -> text
    script: str | None = None
    env: Mapping[str, str] = field(default_factory=dict)    # environment of `script`


@dataclass(frozen=True)
class MachineSpec:
    os: str = "ubuntu-24.04"  # ubuntu-24.04 | ubuntu-22.04 | debian-12
    cpus: int = 1
    memory_gb: int = 1
    disk_gb: int = 20
    setup: Setup = field(default_factory=Setup)
    labels: Mapping[str, str] = field(default_factory=dict)
    ttl: timedelta | None = timedelta(hours=4)


@dataclass
class Result:
    code: int
    stdout: str
    stderr: str


# ---------------------------------------------------------------- streams

class TextReader:
    """Text view of a byte stream. Iterate it for lines, or call read()."""

    def __init__(self, read_chunk: Callable[[], Awaitable[bytes]]):
        self._read_chunk = read_chunk
        self._dec = codecs.getincrementaldecoder("utf-8")(errors="replace")
        self._buf = ""
        self._eof = False

    async def _fill(self) -> None:
        data = await self._read_chunk()
        if data:
            self._buf += self._dec.decode(data)
        else:
            self._buf += self._dec.decode(b"", final=True)
            self._eof = True

    async def read(self, n: int = -1) -> str:
        """Up to n characters; everything until EOF when n < 0; '' at EOF."""
        if n < 0:
            while not self._eof:
                await self._fill()
            n = len(self._buf)
        while not self._buf and not self._eof:
            await self._fill()
        out, self._buf = self._buf[:n], self._buf[n:]
        return out

    async def readline(self) -> str:
        while "\n" not in self._buf and not self._eof:
            await self._fill()
        i = self._buf.find("\n")
        return await self.read(i + 1 if i >= 0 else len(self._buf))

    def __aiter__(self) -> "TextReader":
        return self

    async def __anext__(self) -> str:
        line = await self.readline()
        if not line:
            raise StopAsyncIteration
        return line


class TextWriter:
    def __init__(self, write: Callable[[bytes], None],
                 drain: Callable[[], Awaitable[None]], eof: Callable[[], None]):
        self._write, self._drain, self._eof = write, drain, eof
        self._closed = False

    def write(self, text: str) -> None:
        self._write(text.encode("utf-8"))

    async def drain(self) -> None:
        await self._drain()

    def close(self) -> None:
        """Send end-of-file to the command. Output streams stay open."""
        if not self._closed:
            self._closed = True
            self._eof()


class BytePipe:
    """Unbounded in-memory byte pipe (used to split Docker's multiplexed stream)."""

    def __init__(self) -> None:
        self._q: asyncio.Queue[bytes] = asyncio.Queue()
        self._eof = False

    def feed(self, data: bytes) -> None:
        if data:
            self._q.put_nowait(data)

    def close(self) -> None:
        self._q.put_nowait(b"")

    async def read(self) -> bytes:
        if self._eof:
            return b""
        data = await self._q.get()
        self._eof = not data
        return data


class Process:
    """A command running on a machine."""

    pid: int = 0  # process id inside the machine

    def __init__(self, stdin: TextWriter, stdout: TextReader, stderr: TextReader,
                 wait: Callable[[], Awaitable[int]]):
        self.stdin, self.stdout, self.stderr = stdin, stdout, stderr
        self._wait = wait
        self._task: asyncio.Future[int] | None = None
        self._kill: Callable[[str], Awaitable[None]] | None = None

    async def wait(self) -> int:
        """Exit code. Negative when the command was ended by a signal or the link dropped."""
        if self._task is None:
            self._task = asyncio.ensure_future(self._wait())
        return await asyncio.shield(self._task)

    async def kill(self, signal: str = "TERM") -> None:
        assert self._kill is not None
        await self._kill(signal)


class Transport(Protocol):
    async def exec(self, script: str, user: str | None, pty: bool) -> Process: ...
    async def close(self) -> None: ...


# ---------------------------------------------------------------- helpers

def new_id(prefix: str = "m") -> str:
    return f"{prefix}-" + secrets.token_hex(5)


def make_labels(id: str, spec: "MachineSpec | ServiceSpec") -> dict[str, str]:
    labels = {**spec.labels, L_MANAGED: "true", L_ID: id}
    if spec.ttl is not None:
        labels[L_EXPIRES] = str(int(time.time() + spec.ttl.total_seconds()))
    return labels


def expires_of(labels: Mapping[str, str]) -> datetime | None:
    try:
        return datetime.fromtimestamp(int(labels[L_EXPIRES]), timezone.utc)
    except (KeyError, ValueError):
        return None


def wrap(argv: Sequence[str] | str, env: Mapping[str, str] | None, cwd: str | None) -> str:
    """Shell script that prints its PID on the first line, then becomes the command."""
    cmd = "sh -c " + shlex.quote(argv) if isinstance(argv, str) else shlex.join(argv)
    if env:
        cmd = "env " + " ".join(shlex.quote(f"{k}={v}") for k, v in env.items()) + " " + cmd
    cd = f"cd {shlex.quote(cwd)} && " if cwd else ""
    return f"echo $$; {cd}exec {cmd}"


def pick_size(table: Sequence[tuple[int, int, str]], cpus: int, memory_gb: int) -> str:
    """First (cpus, memory_gb, name) row that is large enough."""
    for c, m, name in table:
        if c >= cpus and m >= memory_gb:
            return name
    raise ProvisionError(f"no instance size for {cpus} CPUs / {memory_gb} GB")


def my_cidr() -> str:
    """Public address of this host as a /32, used to restrict inbound SSH."""
    try:
        with urllib.request.urlopen("https://checkip.amazonaws.com", timeout=10) as r:
            return r.read().decode().strip() + "/32"
    except OSError as e:
        raise ProvisionError(f"cannot detect the public IP; pass ssh_cidr explicitly ({e})") from e


def ttl_minutes(spec: MachineSpec) -> int | None:
    return None if spec.ttl is None else max(1, math.ceil(spec.ttl.total_seconds() / 60))


# ---------------------------------------------------------------- machine

@dataclass
class Info:
    """What a provider knows about one machine."""

    id: str
    state: str
    labels: dict[str, str]
    address: str | None = None
    user: str | None = None


class Machine:
    """Handle to a machine. Creating or dropping a handle never changes the machine."""

    def __init__(self, provider: "Provider", info: Info, transport: Transport):
        self.id = info.id
        self.provider = provider.name
        self.state = info.state            # as of create()/list()/attach()
        self.labels = info.labels
        self.address = info.address
        self.expires = expires_of(info.labels)
        self._provider, self._transport = provider, transport

    def __repr__(self) -> str:
        return f"<Machine {self.provider}:{self.id} {self.state}>"

    async def execute(self, argv: Sequence[str] | str, *, env: Mapping[str, str] | None = None,
                      cwd: str | None = None, user: str | None = None,
                      pty: bool = False) -> Process:
        """Start a command. A list is run as is; a string is run by `sh -c`."""
        p = await self._transport.exec(wrap(argv, env, cwd), user, pty)
        line = await p.stdout.readline()
        try:
            p.pid = int(line.strip())
        except ValueError:
            code, err = await asyncio.gather(p.wait(), p.stderr.read())
            raise TransportError(f"command did not start (exit {code}): {line}{err}") from None
        p._kill = lambda sig: self._signal(p.pid, sig)
        if pty:  # a terminal has no end-of-file; Ctrl-D is the closest thing
            p.stdin._eof = lambda: p.stdin._write(b"\x04")
        return p

    async def _signal(self, pid: int, sig: str) -> None:
        await self.run(["kill", "-s", sig, str(pid)], user="root")

    async def run(self, argv: Sequence[str] | str, *, input: str | None = None,
                  check: bool = False, **kw) -> Result:
        """Run to completion and collect the output."""
        p = await self.execute(argv, **kw)

        async def feed() -> None:
            with suppress(Exception):  # the command may exit before reading its input
                if input:
                    p.stdin.write(input)
                    await p.stdin.drain()
                p.stdin.close()

        _, out, err, code = await asyncio.gather(feed(), p.stdout.read(), p.stderr.read(), p.wait())
        result = Result(code, out, err)
        if check and code != 0:
            raise CommandError(result)
        return result

    async def detach(self) -> None:
        """Drop the connection. The machine and its processes keep running."""
        await self._transport.close()

    async def destroy(self) -> None:
        await self.detach()
        await self._provider.destroy(self.id)
        self.state = TERMINATED

    async def __aenter__(self) -> "Machine":
        return self

    async def __aexit__(self, *exc) -> None:
        await self.detach()


async def apply_setup(m: Machine, setup: Setup) -> None:
    async def step(name: str, argv: Sequence[str] | str, **kw) -> None:
        r = await m.run(argv, user="root", **kw)
        if r.code != 0:
            raise SetupError(name, r.code, r.stdout + r.stderr)

    if setup.packages:
        await step("packages",
                   "apt-get update -q && apt-get install -y -q --no-install-recommends "
                   "-o DPkg::Lock::Timeout=300 " + shlex.join(setup.packages),
                   env={"DEBIAN_FRONTEND": "noninteractive"})
    for path, text in setup.files.items():
        q = shlex.quote(path)
        await step(f"file {path}", f"mkdir -p {shlex.quote(posixpath.dirname(path) or '/')} && cat > {q}",
                   input=text)
    if setup.script:
        await step("script", ["sh", "-s"], input=setup.script, env=setup.env)


# ---------------------------------------------------------------- services

# kind -> (port, default version, URL scheme, user, database)
SERVICES = {"postgres": (5432, "17", "postgresql", "nepho", "nepho"),
            "valkey": (6379, "8", "redis", "default", "0")}


@dataclass(frozen=True)
class ServiceSpec:
    kind: str                   # postgres | valkey
    version: str | None = None  # major version; default: see SERVICES
    cpus: int = 1
    memory_gb: int = 1
    disk_gb: int = 20
    labels: Mapping[str, str] = field(default_factory=dict)
    ttl: timedelta | None = timedelta(hours=4)


@dataclass
class Service:
    """A running database or cache. Clients connect with their normal driver.

    `host`/`port` is the endpoint as seen from the caller; `internal_host`/
    `internal_port` is the one to use from machines of the same provider.
    Either can be missing when the service is not reachable that way.
    """
    id: str
    kind: str
    version: str
    state: str
    labels: dict[str, str]
    user: str
    password: str = field(repr=False)
    database: str
    host: str | None = None
    port: int | None = None
    internal_host: str | None = None
    internal_port: int | None = None
    provider: str = ""
    _owner: "Provider | None" = field(default=None, repr=False, compare=False)

    def _url(self, host: str | None, port: int | None) -> str | None:
        if host is None:
            return None
        scheme = SERVICES[self.kind][2]
        return f"{scheme}://{self.user}:{self.password}@{host}:{port}/{self.database}"

    @property
    def url(self) -> str | None:
        return self._url(self.host, self.port)

    @property
    def internal_url(self) -> str | None:
        return self._url(self.internal_host, self.internal_port)

    @property
    def expires(self) -> datetime | None:
        return expires_of(self.labels)

    async def destroy(self) -> None:
        assert self._owner is not None
        await self._owner.destroy(self.id)
        self.state = TERMINATED


# ---------------------------------------------------------------- provider

class Provider:
    """Base class. Subclasses implement the four blocking `_` hooks."""

    name = "?"

    # hooks (run in a worker thread)
    def _launch(self, id: str, spec: MachineSpec, labels: dict[str, str]) -> Info:
        raise NotImplementedError

    def _list(self) -> list[Info]:
        raise NotImplementedError

    def _destroy(self, id: str) -> None:
        """Must be idempotent and must also clean up a half-created machine."""
        raise NotImplementedError

    def _transport(self, info: Info) -> Transport:
        raise NotImplementedError

    def _launch_service(self, id: str, spec: ServiceSpec, labels: dict[str, str]) -> Service:
        """Returns when the service accepts connections."""
        raise ProvisionError(f"{self.name}: services are not supported yet")

    def _list_services(self) -> list[Service]:
        return []

    def _reapable(self) -> set[str]:
        now = datetime.now(timezone.utc)
        return {i.id for i in [*self._list(), *self._list_services()]
                if (e := expires_of(i.labels)) and e < now}

    async def _ready(self, m: Machine, spec: MachineSpec) -> None:
        pass

    # public API
    async def create(self, spec: MachineSpec) -> Machine:
        """Create a machine and apply `spec.setup`. Returns when it is ready for execute()."""
        id = new_id()
        m = None
        try:
            info = await self._call(self._launch, id, spec, make_labels(id, spec))
            m = self._machine(info)
            await self._ready(m, spec)
            await apply_setup(m, spec.setup)
            return m
        except BaseException:
            with suppress(Exception):
                if m:
                    await m.detach()
                await asyncio.shield(self._call(self._destroy, id))
            raise

    async def list(self) -> list[Machine]:
        """Every machine the backend still knows, in any state."""
        return [self._machine(i) for i in await self._call(self._list)]

    async def attach(self, id: str) -> Machine:
        for m in await self.list():
            if m.id == id:
                return m
        raise ProvisionError(f"{self.name}: no machine {id}")

    async def destroy(self, id: str) -> None:
        """Destroy a machine or a service."""
        await self._call(self._destroy, id)

    async def create_service(self, spec: ServiceSpec) -> Service:
        """Create a database or cache. Returns when it accepts connections."""
        if spec.kind not in SERVICES:
            raise ProvisionError(f"{self.name}: unknown service kind {spec.kind!r}")
        spec = replace(spec, version=spec.version or SERVICES[spec.kind][1])
        id = new_id("s")
        labels = {**make_labels(id, spec), L_KIND: spec.kind, L_VERSION: spec.version}
        try:
            return self._own(await self._call(self._launch_service, id, spec, labels))
        except BaseException:
            with suppress(Exception):
                await asyncio.shield(self._call(self._destroy, id))
            raise

    async def list_services(self) -> list[Service]:
        """Every service the backend still knows, in any state."""
        return [self._own(s) for s in await self._call(self._list_services)]

    async def attach_service(self, id: str) -> Service:
        for s in await self.list_services():
            if s.id == id:
                return s
        raise ProvisionError(f"{self.name}: no service {id}")

    def _own(self, s: Service) -> Service:
        s.provider, s._owner = self.name, self
        return s

    async def reap(self) -> int:
        """Destroy machines and services whose TTL has passed. Returns how many."""
        ids = await self._call(self._reapable)
        for id in ids:
            await self.destroy(id)
        return len(ids)

    def _machine(self, info: Info) -> Machine:
        return Machine(self, info, self._transport(info))

    async def _call(self, fn, *args):
        try:
            return await asyncio.to_thread(fn, *args)
        except MachineError:
            raise
        except Exception as e:
            raise ProvisionError(f"{self.name}: {e}") from e
