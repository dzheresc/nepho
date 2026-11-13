// Docker backend: a long-lived container stands in for a VM.
import { PassThrough } from "node:stream";
import Docker from "dockerode";
import {
  type FullSpec, type Info, L_ID, L_MANAGED, Provider, ProvisionError, type RawProcess, type State,
  type Transport, TransportError, sleep,
} from "./core.js";

const IMAGES: Record<string, string> = {
  "ubuntu-24.04": "ubuntu:24.04", "ubuntu-22.04": "ubuntu:22.04", "debian-12": "debian:12",
};
const STATES: Record<string, State> = { running: "running", created: "pending", restarting: "pending" };

export interface DockerOptions {
  cpusetCpus?: string;
  cpusetMems?: string;
  client?: Docker;
}

/**
 * `diskGb` is ignored. When the TTL passes the container exits by itself
 * (state "stopped"); reap() removes it.
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
    const have = await this.docker.getImage(image).inspect().then(() => true, () => false);
    if (!have) {
      const progress = await this.docker.pull(image);
      await new Promise<void>((resolve, reject) =>
        this.docker.modem.followProgress(progress, (err) => (err ? reject(err) : resolve())));
    }
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

  protected async infos(): Promise<Info[]> {
    const found = await this.docker.listContainers({ all: true, filters: { label: [`${L_MANAGED}=true`] } });
    return found.map((c) => ({ id: c.Labels[L_ID]!, state: STATES[c.State] ?? "stopped", labels: c.Labels }));
  }

  protected async remove(id: string): Promise<void> {
    await this.docker.getContainer(id).remove({ force: true }).catch((e) => {
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
