"""Docker backend: a long-lived container stands in for a VM; services are the
official images of the open-source servers."""
from __future__ import annotations

import asyncio
import secrets
import socket
import struct
import time

import docker
import docker.errors

from .core import (L_ID, L_KIND, L_MANAGED, L_SECRET, L_VERSION, PENDING, RUNNING, SERVICES,
                   STOPPED, BytePipe, Info, MachineSpec, Process, Provider, ProvisionError,
                   Service, ServiceSpec, TextReader, TextWriter, TransportError)

IMAGES = {"ubuntu-24.04": "ubuntu:24.04", "ubuntu-22.04": "ubuntu:22.04", "debian-12": "debian:12"}
STATES = {"running": RUNNING, "created": PENDING, "restarting": PENDING}
SERVICE_IMAGES = {"postgres": "postgres:{}", "valkey": "valkey/valkey:{}"}


class DockerProvider(Provider):
    """`spec.disk_gb` is ignored. When the TTL passes a machine's container exits by
    itself (state "stopped"); reap() removes it.

    A service is published on a random port of 127.0.0.1 (`host`/`port`) and is
    reachable from other containers at its bridge address (`internal_host`). Its
    data lives in the container and goes away with it. Nothing stops a service
    at the TTL; reap() removes it.
    """

    name = "docker"

    def __init__(self, *, cpuset_cpus: str | None = None, cpuset_mems: str | None = None,
                 client: "docker.DockerClient | None" = None):
        self._c = client or docker.from_env()
        self._cpuset = {k: v for k, v in
                        (("cpuset_cpus", cpuset_cpus), ("cpuset_mems", cpuset_mems)) if v}

    def _launch(self, id: str, spec: MachineSpec, labels: dict[str, str]) -> Info:
        image = IMAGES.get(spec.os)
        if image is None:
            raise ProvisionError(f"docker: unsupported os {spec.os!r}")
        self._pull(image)
        life = "infinity" if spec.ttl is None else str(max(1, int(spec.ttl.total_seconds())))
        self._c.containers.run(
            image, ["sleep", life], name=id, labels=labels, detach=True, init=True,
            nano_cpus=spec.cpus * 10**9, mem_limit=f"{spec.memory_gb}g", **self._cpuset)
        return Info(id, RUNNING, labels)

    def _pull(self, image: str) -> None:
        try:
            self._c.images.get(image)
        except docker.errors.ImageNotFound:
            self._c.images.pull(image)

    def _list(self) -> list[Info]:
        found = self._c.containers.list(all=True, filters={"label": f"{L_MANAGED}=true"})
        return [Info(c.labels[L_ID], STATES.get(c.status, STOPPED), dict(c.labels))
                for c in found if L_KIND not in c.labels]

    def _launch_service(self, id: str, spec: ServiceSpec, labels: dict[str, str]) -> Service:
        port, _, _, user, database = SERVICES[spec.kind]
        password = secrets.token_hex(12)
        image = SERVICE_IMAGES[spec.kind].format(spec.version)
        if spec.kind == "postgres":
            env = {"POSTGRES_USER": user, "POSTGRES_PASSWORD": password, "POSTGRES_DB": database}
            # Over TCP: during first-time setup the server listens on its socket only.
            cmd, probe = None, ["pg_isready", "-h", "127.0.0.1", "-U", user, "-d", database]
        else:
            env, cmd = {}, ["valkey-server", "--requirepass", password]
            probe = ["valkey-cli", "-a", password, "--no-auth-warning", "ping"]
        self._pull(image)
        c = self._c.containers.run(
            image, cmd, name=id, labels={**labels, L_SECRET: password}, environment=env,
            detach=True, init=True, ports={f"{port}/tcp": ("127.0.0.1", None)},
            nano_cpus=spec.cpus * 10**9, mem_limit=f"{spec.memory_gb}g", **self._cpuset)
        deadline = time.monotonic() + 120
        while c.exec_run(probe).exit_code != 0:
            c.reload()
            if c.status != "running" or time.monotonic() > deadline:
                tail = c.logs(tail=20).decode(errors="replace")
                raise ProvisionError(f"docker: {spec.kind} did not start:\n{tail}")
            time.sleep(0.3)
        c.reload()
        return self._service(c)

    @staticmethod
    def _service(c) -> Service:
        labels = dict(c.labels)
        kind = labels[L_KIND]
        port, _, _, user, database = SERVICES[kind]
        net = c.attrs["NetworkSettings"]
        bound = ((net.get("Ports") or {}).get(f"{port}/tcp") or [None])[0]  # None unless running
        inner = next((n["IPAddress"] for n in net.get("Networks", {}).values()
                      if n.get("IPAddress")), None)
        return Service(
            labels[L_ID], kind, labels.get(L_VERSION, ""), STATES.get(c.status, STOPPED), labels,
            user, labels.get(L_SECRET, ""), database,
            host=bound and "127.0.0.1", port=bound and int(bound["HostPort"]),
            internal_host=inner, internal_port=inner and port)

    def _list_services(self) -> list[Service]:
        found = self._c.containers.list(all=True, filters={"label": [f"{L_MANAGED}=true", L_KIND]})
        return [self._service(c) for c in found]

    def _destroy(self, id: str) -> None:
        try:
            self._c.containers.get(id).remove(force=True, v=True)  # v: its anonymous volumes
        except docker.errors.NotFound:
            pass

    def _transport(self, info: Info) -> "DockerTransport":
        return DockerTransport(self._c.api, info.id)


class DockerTransport:
    def __init__(self, api: "docker.APIClient", container: str):
        self._api, self._container = api, container

    async def exec(self, script: str, user: str | None, pty: bool) -> Process:
        api = self._api

        def start():
            e = api.exec_create(self._container, ["sh", "-c", script], stdin=True, tty=pty,
                                user=user or "")
            return e["Id"], api.exec_start(e["Id"], socket=True, tty=pty)

        try:
            exec_id, holder = await asyncio.to_thread(start)
        except docker.errors.DockerException as e:
            raise TransportError(f"docker exec in {self._container}: {e}") from e

        # `holder` owns the hijacked HTTP connection; hand its socket to asyncio.
        raw = getattr(holder, "_sock", holder)
        opener = asyncio.open_unix_connection if raw.family == socket.AF_UNIX else asyncio.open_connection
        reader, writer = await opener(sock=raw)
        out, err = BytePipe(), BytePipe()

        async def pump() -> None:
            try:
                if pty:  # raw stream
                    while data := await reader.read(65536):
                        out.feed(data)
                else:    # frames: 1 byte stream id, 3 zero bytes, 4 bytes length, payload
                    while True:
                        kind, size = struct.unpack(">BxxxI", await reader.readexactly(8))
                        (err if kind == 2 else out).feed(await reader.readexactly(size))
            except (asyncio.IncompleteReadError, OSError):
                pass
            finally:
                out.close()
                err.close()

        pumping = asyncio.ensure_future(pump())

        async def wait() -> int:
            await pumping
            writer.close()
            holder.close()
            deadline = time.monotonic() + 10
            while True:
                state = await asyncio.to_thread(api.exec_inspect, exec_id)
                if not state["Running"] or time.monotonic() > deadline:
                    code = state["ExitCode"]
                    return -1 if code is None else code
                await asyncio.sleep(0.05)

        def eof() -> None:
            if writer.can_write_eof():
                writer.write_eof()

        return Process(TextWriter(writer.write, writer.drain, eof),
                       TextReader(out.read), TextReader(err.read), wait)

    async def close(self) -> None:
        pass
