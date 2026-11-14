// Behaviour every backend must share.
//
// Runs against Docker (exec transport) and against an sshd inside a Docker
// container (SSH transport, the one the cloud providers use). No cloud is contacted.
import assert from "node:assert/strict";
import { once } from "node:events";
import { mkdtempSync } from "node:fs";
import { connect } from "node:net";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { createInterface } from "node:readline";
import { PassThrough } from "node:stream";
import { after, before, describe, test } from "node:test";
import { Machine, ProvisionError, SetupError } from "../src/index.js";
import Docker from "dockerode";
import { DockerProvider } from "../src/docker.js";
import { SshTransport, ensureKey } from "../src/ssh.js";

const SSHD_SETUP = `
useradd -m -s /bin/bash tester
echo 'tester ALL=(ALL) NOPASSWD:ALL' > /etc/sudoers.d/tester
install -d -m 700 -o tester -g tester /home/tester/.ssh
install -m 600 -o tester -g tester /tmp/key.pub /home/tester/.ssh/authorized_keys
mkdir -p /run/sshd && /usr/sbin/sshd
`;
const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));
const provider = new DockerProvider();
const ids = async () => (await provider.list()).map((x) => x.id).sort();

let dockerMachine: Machine;
let sshMachine: Machine;

before(async () => {
  const { keyPath, publicKey } = ensureKey(join(mkdtempSync(join(tmpdir(), "nepho-key-")), "id_rsa"));
  let host: Machine;
  [dockerMachine, host] = await Promise.all([
    provider.create({
      setup: {
        files: { "/etc/demo/hello.txt": "hello file\n" },
        script: "echo $GREETING > /made-by-script", env: { GREETING: "hi there" },
      },
    }),
    provider.create({
      os: "debian-12",
      setup: { packages: ["openssh-server", "sudo"], files: { "/tmp/key.pub": publicKey + "\n" }, script: SSHD_SETUP },
    }),
  ]);
  const ip = (await host.run("hostname -i", { check: true })).stdout.trim();
  sshMachine = new Machine(provider, { id: host.id, state: "running", labels: host.labels, address: ip },
    new SshTransport(ip, "tester", keyPath, 30_000));
});

after(async () => {
  await dockerMachine?.destroy();
  await sshMachine?.destroy();
});

for (const kind of ["docker", "ssh"] as const) {
  describe(kind, () => {
    let m: Machine;
    before(() => { m = kind === "docker" ? dockerMachine : sshMachine; });

    test("run", async () => {
      assert.deepEqual(await m.run(["echo", "hi"]), { code: 0, stdout: "hi\n", stderr: "" });
    });

    test("live streams", async () => {
      const p = await m.execute(["cat"]);
      const lines = createInterface({ input: p.stdout })[Symbol.asyncIterator]();
      p.stdin.write("one\n");
      assert.equal((await lines.next()).value, "one"); // answered before stdin is closed
      p.stdin.write("two\nthree\n");
      p.stdin.end();
      assert.equal((await lines.next()).value, "two");
      assert.equal((await lines.next()).value, "three");
      assert.equal((await lines.next()).done, true);
      assert.equal(await p.wait(), 0);
    });

    test("stderr and exit code", async () => {
      assert.deepEqual(await m.run("echo out; echo err >&2; exit 3"), { code: 3, stdout: "out\n", stderr: "err\n" });
    });

    test("env, cwd, quoting", async () => {
      const r = await m.run('echo "$FOO"; pwd', { env: { FOO: "a b'c" }, cwd: "/tmp" });
      assert.equal(r.stdout, "a b'c\n/tmp\n");
      assert.equal((await m.run(["printf", "%s|", "a b", "c'd", "$x", "é"])).stdout, "a b|c'd|$x|é|");
    });

    test("bad cwd", async () => {
      const r = await m.run(["true"], { cwd: "/nonexistent" });
      assert.ok(r.code !== 0 && r.stderr.includes("nonexistent"));
    });

    test("user", async () => {
      assert.equal((await m.run(["id", "-un"], { user: "root" })).stdout, "root\n");
    });

    test("pty", async () => {
      const r = await m.run("test -t 1 && echo is-a-tty", { pty: true });
      assert.ok(r.stdout.includes("is-a-tty") && r.code === 0);
      assert.equal((await m.run("test -t 1 || echo no-tty")).stdout, "no-tty\n");
    });

    test("kill", async () => {
      const p = await m.execute(["sleep", "1000"]);
      assert.ok(p.pid > 0);
      p.stdout.resume();
      await p.kill();
      assert.notEqual(await p.wait(), 0);
    });

    test("big output and input", async () => {
      const text = ("x".repeat(79) + "\n").repeat(20000);
      assert.equal((await m.run(["cat"], { input: text })).stdout, text);
    });

    test("concurrent", async () => {
      const rs = await Promise.all([...Array(8).keys()].map((i) => m.run(`sleep 0.3; echo ${i}`)));
      assert.deepEqual(rs.map((r) => r.stdout), [...Array(8).keys()].map((i) => `${i}\n`));
    });
  });
}

test("setup applied", async () => {
  const r = await dockerMachine.run("cat /etc/demo/hello.txt /made-by-script");
  assert.equal(r.stdout, "hello file\nhi there\n");
});

test("setup failure destroys the machine", async () => {
  const before = await ids();
  await assert.rejects(provider.create({ setup: { script: "echo boom; exit 7" } }), (e: unknown) =>
    e instanceof SetupError && e.code === 7 && e.output.includes("boom"));
  assert.deepEqual(await ids(), before);
});

test("attach and detach", async () => {
  await dockerMachine.execute("sleep 2; echo done > /tmp/survivor");
  await dockerMachine.detach();

  const other = await new DockerProvider().attach(dockerMachine.id); // as a second client would
  assert.equal(other.state, "running");
  assert.deepEqual(other.expires, dockerMachine.expires);
  await sleep(3000);
  assert.equal((await other.run(["cat", "/tmp/survivor"])).stdout, "done\n");
  await other.detach();
  assert.equal((await dockerMachine.run(["echo", "back"])).stdout, "back\n"); // handle still usable
  await assert.rejects(provider.attach("m-doesnotexist"), ProvisionError);
});

test("dead machines are listed, then reaped", async () => {
  const m = await provider.create({ ttlSeconds: 2 });
  await sleep(4000);
  const listed = (await provider.list()).find((x) => x.id === m.id);
  assert.equal(listed?.state, "stopped"); // dead, but still enumerated
  assert.ok((await provider.reap()) >= 1);
  assert.ok(!(await ids()).includes(m.id));
});

// ---------------------------------------------------------------- services

/** Send commands in the Redis protocol and return one reply line for each. */
async function valkey(host: string, port: number, ...commands: string[][]): Promise<string[]> {
  const socket = connect(port, host);
  const lines = createInterface({ input: socket })[Symbol.asyncIterator]();
  const replies: string[] = [];
  for (const words of commands) {
    socket.write(`*${words.length}\r\n` + words.map((w) => `$${Buffer.byteLength(w)}\r\n${w}\r\n`).join(""));
    let line = (await lines.next()).value as string;
    if (line.startsWith("$")) line = (await lines.next()).value as string; // bulk string: value on the next line
    replies.push(line);
  }
  socket.destroy();
  return replies;
}

/** Open a session in the PostgreSQL protocol; return the authentication method asked for. */
async function postgresAuthRequest(host: string, port: number): Promise<[string, number]> {
  const socket = connect(port, host);
  const body = Buffer.concat([Buffer.from([0, 3, 0, 0]), Buffer.from("user\0nepho\0database\0nepho\0\0")]);
  const size = Buffer.alloc(4);
  size.writeUInt32BE(body.length + 4);
  socket.write(Buffer.concat([size, body]));
  const [head] = (await once(socket, "data")) as [Buffer];
  socket.destroy();
  return [String.fromCharCode(head[0]!), head.readUInt32BE(5)];
}

/** Run a command inside a service container. No terminal: psql would page or prompt on one. */
async function inside(id: string, argv: string[]): Promise<{ code: number; out: string }> {
  const docker = new Docker();
  const exec = await docker.getContainer(id).exec({ Cmd: argv, AttachStdout: true, AttachStderr: true });
  const stream = await exec.start({});
  const all = new PassThrough();
  let out = "";
  all.on("data", (d: Buffer) => { out += d; });
  docker.modem.demuxStream(stream, all, all);
  await once(stream, "end");
  return { code: (await exec.inspect()).ExitCode ?? -1, out };
}

const serviceIds = async () => (await provider.listServices()).map((x) => x.id).sort();

test("valkey", async () => {
  const s = await provider.createService({ kind: "valkey" });
  try {
    assert.deepEqual([s.state, s.version, s.host], ["running", "8", "127.0.0.1"]);
    assert.equal(s.url, `redis://default:${s.password}@127.0.0.1:${s.port}/0`);
    assert.equal(s.internalUrl, `redis://default:${s.password}@${s.internalHost}:6379/0`);
    assert.deepEqual(await valkey(s.host!, s.port!, ["PING"]), ["-NOAUTH Authentication required."]);
    assert.deepEqual(await valkey(s.host!, s.port!, ["AUTH", s.password], ["SET", "k", "v"], ["GET", "k"]),
      ["+OK", "+OK", "v"]);
    const r = await inside(s.id, ["valkey-cli", "-u", s.internalUrl!, "--no-auth-warning", "GET", "k"]);
    assert.deepEqual([r.code, r.out.trim()], [0, "v"]);
  } finally {
    await s.destroy();
  }
  assert.equal(s.state, "terminated");
});

test("postgres", async () => {
  const s = await provider.createService({ kind: "postgres", version: "16" });
  try {
    assert.deepEqual([s.state, s.version], ["running", "16"]);
    assert.equal(s.url, `postgresql://nepho:${s.password}@127.0.0.1:${s.port}/nepho`);
    // 'R' + 10: the server on the published port asks for SCRAM password authentication
    assert.deepEqual(await postgresAuthRequest(s.host!, s.port!), ["R", 10]);
    const r = await inside(s.id, ["psql", s.internalUrl!, "-Atc", "select current_user, version()"]);
    assert.ok(r.code === 0 && r.out.startsWith("nepho|PostgreSQL 16."), r.out);
    const bad = s.internalUrl!.replace(s.password, "wrong");
    assert.notEqual((await inside(s.id, ["psql", bad, "-Atc", "select 1"])).code, 0);
  } finally {
    await s.destroy();
  }
});

test("services: list, attach, reap", async () => {
  const s = await provider.createService({ kind: "valkey", labels: { team: "x" }, ttlSeconds: 1 });
  assert.ok(s.id.startsWith("s-") && s.labels.team === "x");
  assert.ok((await serviceIds()).includes(s.id));
  const machines = await ids();
  assert.ok(!machines.includes(s.id) && machines.includes(dockerMachine.id)); // the two lists are separate
  assert.ok(!(await serviceIds()).includes(dockerMachine.id));

  const other = await new DockerProvider().attachService(s.id); // as a second client would
  assert.equal(other.url, s.url);
  assert.deepEqual(other.expires, s.expires);
  await assert.rejects(provider.attachService("s-doesnotexist"), ProvisionError);
  await assert.rejects(provider.attachService(dockerMachine.id), ProvisionError);

  await sleep(2000);
  assert.equal((await provider.attachService(s.id)).state, "running"); // nothing stops it at the TTL
  assert.ok((await provider.reap()) >= 1);
  assert.ok(!(await serviceIds()).includes(s.id));
  assert.ok((await ids()).includes(dockerMachine.id));
});

test("services: bad requests", async () => {
  const before = await serviceIds();
  await assert.rejects(provider.createService({ kind: "mysql" }), ProvisionError);
  await assert.rejects(provider.createService({ kind: "valkey", version: "0.0.0-nope" }), ProvisionError);
  assert.deepEqual(await serviceIds(), before);
});
