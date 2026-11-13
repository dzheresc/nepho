"""Docker backend: a long-lived container stands in for a VM."""
from __future__ import annotations

import asyncio
import socket
import struct
import time

import docker
import docker.errors

from .core import (L_ID, L_MANAGED, PENDING, RUNNING, STOPPED, BytePipe, Info, MachineSpec,
                   Process, Provider, ProvisionError, TextReader, TextWriter, TransportError)

IMAGES = {"ubuntu-24.04": "ubuntu:24.04", "ubuntu-22.04": "ubuntu:22.04", "debian-12": "debian:12"}
STATES = {"running": RUNNING, "created": PENDING, "restarting": PENDING}


class DockerProvider(Provider):
    """`spec.disk_gb` is ignored. When the TTL passes the container exits by itself
    (state "stopped"); reap() removes it."""

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
        try:
            self._c.images.get(image)
        except docker.errors.ImageNotFound:
            self._c.images.pull(image)
        life = "infinity" if spec.ttl is None else str(max(1, int(spec.ttl.total_seconds())))
        self._c.containers.run(
            image, ["sleep", life], name=id, labels=labels, detach=True, init=True,
            nano_cpus=spec.cpus * 10**9, mem_limit=f"{spec.memory_gb}g", **self._cpuset)
        return Info(id, RUNNING, labels)

    def _list(self) -> list[Info]:
        found = self._c.containers.list(all=True, filters={"label": f"{L_MANAGED}=true"})
        return [Info(c.labels[L_ID], STATES.get(c.status, STOPPED), dict(c.labels)) for c in found]

    def _destroy(self, id: str) -> None:
        try:
            self._c.containers.get(id).remove(force=True)
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
