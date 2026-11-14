// Docker backend: a long-lived container stands in for a VM; services are the
// official images of the open-source servers.
import { randomBytes } from "node:crypto";
import { PassThrough } from "node:stream";
import Docker from "dockerode";
import {
  type FullServiceSpec, type FullSpec, type Info, L_ID, L_KIND, L_MANAGED, L_SECRET, L_VERSION,
  Provider, ProvisionError, type RawProcess, SERVICES, type ServiceInfo, type State, type Transport,
  TransportError, sleep,
} from "./core.js";

const IMAGES: Record<string, string> = {
  "ubuntu-24.04": "ubuntu:24.04", "ubuntu-22.04": "ubuntu:22.04", "debian-12": "debian:12",
};
const STATES: Record<string, State> = { running: "running", created: "pending", restarting: "pending" };
const SERVICE_IMAGES: Record<string, string> = { postgres: "postgres:", valkey: "valkey/valkey:" };

export interface DockerOptions {
  cpusetCpus?: string;
  cpusetMems?: string;
  client?: Docker;
}

/**
 * `diskGb` is ignored. When the TTL passes a machine's container exits by
 * itself (state "stopped"); reap() removes it.
 *
 * A service is published on a random port of 127.0.0.1 (`host`/`port`) and is
 * reachable from other containers at its bridge address (`internalHost`). Its
 * data lives in the container and goes away with it. Nothing stops a service
 * at the TTL; reap() removes it.
 */
export class DockerProvider extends Provider {
  readonly name = "docker";
  private readonly docker: Docker;

  constructor(private readonly opts: DockerOptions = {}) {
    super();
    this.docker = opts.client ?? new Docker();
  }

  protected async launch(id: string, spec: FullSpec, labels: Record<string, string>): Promise<Info> {
    const image = IMAGES[spec.os];
    if (!image) throw new ProvisionError(`docker: unsupported os '${spec.os}'`);
    await this.pull(image);
    const life = spec.ttlSeconds === null ? "infinity" : String(Math.max(1, Math.floor(spec.ttlSeconds)));
    const container = await this.docker.createContainer({
      name: id, Image: image, Cmd: ["sleep", life], Labels: labels,
      HostConfig: {
        Init: true, NanoCpus: spec.cpus * 1e9, Memory: spec.memoryGb * 2 ** 30,
        CpusetCpus: this.opts.cpusetCpus, CpusetMems: this.opts.cpusetMems,
      },
    });
    await container.start();
    return { id, state: "running", labels };
  }

  private async pull(image: string): Promise<void> {
    if (await this.docker.getImage(image).inspect().then(() => true, () => false)) return;
    const progress = await this.docker.pull(image);
    await new Promise<void>((resolve, reject) =>
      this.docker.modem.followProgress(progress, (err) => (err ? reject(err) : resolve())));
  }

  protected async infos(): Promise<Info[]> {
    const found = await this.docker.listContainers({ all: true, filters: { label: [`${L_MANAGED}=true`] } });
    return found.filter((c) => !(L_KIND in c.Labels))
      .map((c) => ({ id: c.Labels[L_ID]!, state: STATES[c.State] ?? "stopped", labels: c.Labels }));
  }

  protected async launchService(id: string, spec: FullServiceSpec, labels: Record<string, string>): Promise<ServiceInfo> {
    const { port, user, database } = SERVICES[spec.kind]!;
    const password = randomBytes(12).toString("hex");
    const image = SERVICE_IMAGES[spec.kind]! + spec.version;
    const postgres = spec.kind === "postgres";
    // pg_isready over TCP: during first-time setup the server listens on its socket only.
    const probe = postgres
      ? ["pg_isready", "-h", "127.0.0.1", "-U", user, "-d", database]
      : ["valkey-cli", "-a", password, "--no-auth-warning", "ping"];
    await this.pull(image);
    const container = await this.docker.createContainer({
      name: id, Image: image, Labels: { ...labels, [L_SECRET]: password },
      Cmd: postgres ? undefined : ["valkey-server", "--requirepass", password],
      Env: postgres ? [`POSTGRES_USER=${user}`, `POSTGRES_PASSWORD=${password}`, `POSTGRES_DB=${database}`] : [],
      ExposedPorts: { [`${port}/tcp`]: {} },
      HostConfig: {
        Init: true, NanoCpus: spec.cpus * 1e9, Memory: spec.memoryGb * 2 ** 30,
        CpusetCpus: this.opts.cpusetCpus, CpusetMems: this.opts.cpusetMems,
        PortBindings: { [`${port}/tcp`]: [{ HostIp: "127.0.0.1", HostPort: "" }] },
      },
    });
    await container.start();
    for (const deadline = Date.now() + 120_000; ; await sleep(300)) {
      const exec = await container.exec({ Cmd: probe, AttachStdout: true, AttachStderr: true });
      const output = await exec.start({});
      output.resume(); // read to the end: that is when the probe has exited
      await new Promise((resolve) => output.once("end", resolve).once("close", resolve).once("error", resolve));
      if ((await exec.inspect()).ExitCode === 0) break;
      if (!(await container.inspect()).State.Running || Date.now() > deadline) {
        const tail = String(await container.logs({ stdout: true, stderr: true, tail: 20 }));
        throw new ProvisionError(`docker: ${spec.kind} did not start:\n${tail}`);
      }
    }
    return (await this.serviceInfos()).find((s) => s.id === id)!;
  }

  protected async serviceInfos(): Promise<ServiceInfo[]> {
    const found = await this.docker.listContainers({ all: true, filters: { label: [`${L_MANAGED}=true`, L_KIND] } });
    return found.map((c) => {
      const kind = c.Labels[L_KIND]!;
      const { port, user, database } = SERVICES[kind]!;
      const bound = c.Ports.find((p) => p.PrivatePort === port && p.PublicPort); // absent unless running
      const inner = Object.values(c.NetworkSettings.Networks).find((n) => n.IPAddress)?.IPAddress;
      return {
        id: c.Labels[L_ID]!, kind, version: c.Labels[L_VERSION] ?? "", state: STATES[c.State] ?? "stopped",
        labels: c.Labels, user, password: c.Labels[L_SECRET] ?? "", database,
        host: bound && "127.0.0.1", port: bound?.PublicPort,
        internalHost: inner, internalPort: inner ? port : undefined,
      };
    });
  }

  protected async remove(id: string): Promise<void> {
    await this.docker.getContainer(id).remove({ force: true, v: true }).catch((e) => { // v: its anonymous volumes
      if (e.statusCode !== 404) throw e;
    });
  }

  protected transport(info: Info): Transport {
    return new DockerTransport(this.docker, info.id);
  }
}

class DockerTransport implements Transport {
  constructor(private readonly docker: Docker, private readonly container: string) {}

  async exec(script: string, user: string | undefined, pty: boolean): Promise<RawProcess> {
    let exec: Docker.Exec;
    let stream: NodeJS.ReadWriteStream;
    try {
      exec = await this.docker.getContainer(this.container).exec({
        Cmd: ["sh", "-c", script], AttachStdin: true, AttachStdout: true, AttachStderr: true,
        Tty: pty, User: user,
      });
      stream = await exec.start({ hijack: true, stdin: true, Tty: pty });
    } catch (e) {
      throw new TransportError(`docker exec in ${this.container}: ${(e as Error).message}`, { cause: e });
    }
    const stdout = new PassThrough();
    const stderr = new PassThrough();
    if (pty) stream.on("data", (d: Buffer) => stdout.write(d)); // raw stream
    else this.docker.modem.demuxStream(stream, stdout, stderr); // stdout/stderr frames
    const ended = new Promise<void>((resolve) => {
      const finish = () => {
        stdout.end();
        stderr.end();
        resolve();
      };
      stream.once("end", finish).once("close", finish).once("error", finish);
    });
    const wait = async (): Promise<number> => {
      await ended;
      for (const deadline = Date.now() + 10_000; ;) {
        const state = await exec.inspect();
        if (!state.Running || Date.now() > deadline) return state.ExitCode ?? -1;
        await sleep(50);
      }
    };
    // end() on the hijacked socket half-closes it: the command sees EOF, output keeps flowing.
    return { stdin: stream as unknown as RawProcess["stdin"], stdout, stderr, wait };
  }

  async close(): Promise<void> {}
}
