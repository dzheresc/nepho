"""SSH transport, shared by the three cloud providers."""
from __future__ import annotations

import asyncio
import os
import shlex
from pathlib import Path

import asyncssh

from .core import Machine, Process, TextReader, TextWriter, TransportError

DEFAULT_KEY = "~/.config/nepho/id_rsa"


def ensure_key(path: str | None = None) -> tuple[str, str]:
    """Return (private key path, OpenSSH public key), creating the key on first use.

    The key is RSA in PKCS#1 PEM so that the Python, TypeScript and Go
    implementations can share one file, and every cloud accepts it.
    """
    p = Path(path or DEFAULT_KEY).expanduser()
    if not p.exists():
        p.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
        pem = asyncssh.generate_private_key("ssh-rsa", key_size=3072).export_private_key("pkcs1-pem")
        try:
            fd = os.open(p, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
            with os.fdopen(fd, "wb") as f:
                f.write(pem)
        except FileExistsError:  # another process won the race
            pass
    pub = asyncssh.read_private_key(str(p)).export_public_key("openssh").decode().split()
    return str(p), " ".join(pub[:2])


class SshTransport:
    """One lazily opened connection per machine; each command is a channel on it."""

    def __init__(self, host: str | None, user: str, key_path: str, timeout: float = 600):
        self.host, self.user, self.key_path, self.timeout = host, user, key_path, timeout
        self._conn: asyncssh.SSHClientConnection | None = None
        self._lock = asyncio.Lock()

    async def _connect(self) -> asyncssh.SSHClientConnection:
        async with self._lock:
            if self._conn is not None and not self._conn.is_closed():
                return self._conn
            if not self.host:
                raise TransportError("machine has no address (is it running?)")
            loop = asyncio.get_running_loop()
            deadline = loop.time() + self.timeout
            while True:  # a new VM refuses connections until sshd and the key are in place
                try:
                    # Host keys of freshly created VMs are unknown, so they are not checked.
                    self._conn = await asyncssh.connect(
                        self.host, username=self.user, client_keys=[self.key_path],
                        known_hosts=None, keepalive_interval=30, connect_timeout=10)
                    return self._conn
                except (OSError, asyncssh.Error, asyncio.TimeoutError) as e:
                    if loop.time() > deadline:
                        raise TransportError(f"ssh {self.user}@{self.host}: {e}") from e
                    await asyncio.sleep(3)

    async def exec(self, script: str, user: str | None, pty: bool) -> Process:
        conn = await self._connect()
        if user and user != self.user:
            script = f"sudo -n -u {shlex.quote(user)} -- sh -c {shlex.quote(script)}"
        try:
            p = await conn.create_process(script, encoding=None,
                                          term_type="xterm" if pty else None)
        except (OSError, asyncssh.Error) as e:
            raise TransportError(f"ssh {self.host}: {e}") from e

        async def wait() -> int:
            await p.wait()
            return -1 if p.returncode is None else p.returncode

        return Process(
            TextWriter(p.stdin.write, p.stdin.drain, p.stdin.write_eof),
            TextReader(lambda: p.stdout.read(65536)),
            TextReader(lambda: p.stderr.read(65536)),
            wait)

    async def close(self) -> None:
        conn, self._conn = self._conn, None
        if conn is not None:
            conn.close()
            await conn.wait_closed()


async def ssh_ready(m: Machine) -> None:
    """Wait until first-boot provisioning has finished, so apt is free to use."""
    await m.run("cloud-init status --wait >/dev/null 2>&1 || true")
