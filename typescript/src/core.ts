// Core types: spec, process, machine, provider base. No third-party imports here;
// each provider module imports its own SDK.
import { randomBytes } from "node:crypto";
import { posix } from "node:path";
import type { Readable, Writable } from "node:stream";

// Labels (tags) put on every resource, identical in all three implementations.
export const L_MANAGED = "nepho-managed";
export const L_ID = "nepho-id";
export const L_EXPIRES = "nepho-expires"; // unix seconds
export const L_USER = "nepho-user"; // SSH login user, where it depends on the OS

export type State = "pending" | "running" | "stopped" | "terminated";

export class MachineError extends Error {
  constructor(message: string, options?: ErrorOptions) {
    super(message, options);
    this.name = new.target.name;
  }
}
/** The backend could not create, find or destroy a machine. */
export class ProvisionError extends MachineError {}
/** The machine could not be reached or the command could not be started. */
export class TransportError extends MachineError {}
/** A prerequisite step failed. `output` holds what the step printed. */
export class SetupError extends MachineError {
  constructor(readonly step: string, readonly code: number, readonly output: string) {
    super(`setup step '${step}' failed with exit code ${code}:\n${output}`);
  }
}
export class CommandError extends MachineError {
  constructor(readonly result: Result) {
    super(`command failed with exit code ${result.code}:\n${result.stderr}`);
  }
}

/** Prerequisites, applied in this order: packages, files, script. All run as root. */
export interface Setup {
  packages?: string[];
  files?: Record<string, string>; // absolute path -> text
  script?: string;
  env?: Record<string, string>; // environment of `script`
}

export interface MachineSpec {
  os?: string; // ubuntu-24.04 (default) | ubuntu-22.04 | debian-12
  cpus?: number; // default 2
  memoryGb?: number; // default 4
  diskGb?: number; // default 20
  setup?: Setup;
  labels?: Record<string, string>;
  ttlSeconds?: number | null; // default 4 hours; null = never expires
}

export type FullSpec = Required<Omit<MachineSpec, "ttlSeconds">> & { ttlSeconds: number | null };

export function fullSpec(s: MachineSpec): FullSpec {
  return {
    os: s.os ?? "ubuntu-24.04", cpus: s.cpus ?? 2, memoryGb: s.memoryGb ?? 4,
    diskGb: s.diskGb ?? 20, setup: s.setup ?? {}, labels: s.labels ?? {},
    ttlSeconds: s.ttlSeconds === undefined ? 4 * 3600 : s.ttlSeconds,
  };
}

export interface ExecOptions {
  env?: Record<string, string>;
  cwd?: string;
  user?: string;
  pty?: boolean;
}

export interface Result {
  code: number;
  stdout: string;
  stderr: string;
}

/** What a transport hands back: byte streams and a way to learn the exit code. */
export interface RawProcess {
  stdin: Writable;
  stdout: Readable;
  stderr: Readable;
  wait(): Promise<number>;
}

/** A command running on a machine. stdout and stderr are utf-8 text streams. */
export interface Process extends RawProcess {
  /** Process id inside the machine. */
  pid: number;
  /** Exit code. Negative when the command was ended by a signal or the link dropped. */
  wait(): Promise<number>;
  kill(signal?: string): Promise<void>;
}

export interface Transport {
  exec(script: string, user: string | undefined, pty: boolean): Promise<RawProcess>;
  close(): Promise<void>;
}

// ---------------------------------------------------------------- helpers

export const shq = (s: string): string => `'${s.replace(/'/g, `'\\''`)}'`;
export const shjoin = (argv: string[]): string => argv.map(shq).join(" ");
export const newId = (): string => "m-" + randomBytes(5).toString("hex");
export const sleep = (ms: number) => new Promise<void>((r) => setTimeout(r, ms));

export function makeLabels(id: string, spec: FullSpec): Record<string, string> {
  const labels: Record<string, string> = { ...spec.labels, [L_MANAGED]: "true", [L_ID]: id };
  if (spec.ttlSeconds !== null) {
    labels[L_EXPIRES] = String(Math.floor(Date.now() / 1000 + spec.ttlSeconds));
  }
  return labels;
}

export function expiresOf(labels: Record<string, string>): Date | undefined {
  const n = Number.parseInt(labels[L_EXPIRES] ?? "", 10);
  return Number.isNaN(n) ? undefined : new Date(n * 1000);
}

/** Shell script that prints its PID on the first line, then becomes the command. */
export function wrap(argv: string[] | string, env?: Record<string, string>, cwd?: string): string {
  let cmd = typeof argv === "string" ? "sh -c " + shq(argv) : shjoin(argv);
  const vars = Object.entries(env ?? {});
  if (vars.length) cmd = "env " + vars.map(([k, v]) => shq(`${k}=${v}`)).join(" ") + " " + cmd;
  return `echo $$; ${cwd ? `cd ${shq(cwd)} && ` : ""}exec ${cmd}`;
}

/** First [cpus, memoryGb, name] row that is large enough. */
export function pickSize(table: [number, number, string][], cpus: number, memoryGb: number): string {
  for (const [c, m, name] of table) if (c >= cpus && m >= memoryGb) return name;
  throw new ProvisionError(`no instance size for ${cpus} CPUs / ${memoryGb} GB`);
}

/** Public address of this host as a /32, used to restrict inbound SSH. */
export async function myCidr(): Promise<string> {
  try {
    const r = await fetch("https://checkip.amazonaws.com", { signal: AbortSignal.timeout(10_000) });
    return (await r.text()).trim() + "/32";
  } catch (e) {
    throw new ProvisionError(`cannot detect the public IP; pass sshCidr explicitly (${e})`, { cause: e });
  }
}

export const ttlMinutes = (spec: FullSpec): number | null =>
  spec.ttlSeconds === null ? null : Math.max(1, Math.ceil(spec.ttlSeconds / 60));

/** Read one line of bytes from a stream and leave the rest in it. */
function readLine(src: Readable): Promise<string> {
  return new Promise((resolve, reject) => {
    const chunks: Buffer[] = [];
    const done = (line: string) => {
      src.off("readable", onReadable).off("end", onEnd).off("error", onError);
      resolve(line);
    };
    const onReadable = () => {
      let chunk: Buffer | null;
      while ((chunk = src.read()) !== null) {
        const i = chunk.indexOf(10);
        if (i < 0) {
          chunks.push(chunk);
          continue;
        }
        chunks.push(chunk.subarray(0, i));
        if (i + 1 < chunk.length) src.unshift(chunk.subarray(i + 1));
        return done(Buffer.concat(chunks).toString());
      }
    };
    const onEnd = () => done(Buffer.concat(chunks).toString());
    const onError = (e: Error) => reject(e);
    src.on("readable", onReadable).on("end", onEnd).on("error", onError);
    onReadable();
  });
}

async function readAll(src: Readable): Promise<string> {
  let out = "";
  for await (const chunk of src) out += chunk;
  return out;
}

// ---------------------------------------------------------------- machine

/** What a provider knows about one machine. */
export interface Info {
  id: string;
  state: State;
  labels: Record<string, string>;
  address?: string;
  user?: string;
}

/** Handle to a machine. Creating or dropping a handle never changes the machine. */
export class Machine {
  readonly id: string;
  readonly provider: string;
  /** As of create()/list()/attach(). */
  state: State;
  readonly labels: Record<string, string>;
  readonly address?: string;
  readonly expires?: Date;

  constructor(private readonly owner: Provider, info: Info, private readonly transport: Transport) {
    this.id = info.id;
    this.provider = owner.name;
    this.state = info.state;
    this.labels = info.labels;
    this.address = info.address;
    this.expires = expiresOf(info.labels);
  }

  /** Start a command. An array is run as is; a string is run by `sh -c`. */
  async execute(argv: string[] | string, opts: ExecOptions = {}): Promise<Process> {
    const pty = opts.pty ?? false;
    const raw = await this.transport.exec(wrap(argv, opts.env, opts.cwd), opts.user, pty);
    raw.stdin.on("error", () => {}); // writing to a command that already exited is not fatal
    const line = await readLine(raw.stdout);
    raw.stdout.setEncoding("utf8");
    raw.stderr.setEncoding("utf8");
    const pid = Number.parseInt(line, 10);
    if (Number.isNaN(pid)) {
      const [code, err] = await Promise.all([raw.wait(), readAll(raw.stderr)]);
      throw new TransportError(`command did not start (exit ${code}): ${line}${err}`);
    }
    if (pty) {
      // A terminal has no end-of-file; Ctrl-D is the closest thing.
      const stdin = raw.stdin;
      stdin.end = ((...args: unknown[]) => {
        const cb = args.find((a) => typeof a === "function") as (() => void) | undefined;
        stdin.write("\x04", cb);
        return stdin;
      }) as Writable["end"];
    }
    let waiting: Promise<number> | undefined;
    return {
      pid, stdin: raw.stdin, stdout: raw.stdout, stderr: raw.stderr,
      wait: () => (waiting ??= raw.wait()),
      kill: async (signal = "TERM") => {
        await this.run(["kill", "-s", signal, String(pid)], { user: "root" });
      },
    };
  }

  /** Run to completion and collect the output. */
  async run(argv: string[] | string,
            opts: ExecOptions & { input?: string; check?: boolean } = {}): Promise<Result> {
    const p = await this.execute(argv, opts);
    p.stdin.end(opts.input ?? "");
    const [stdout, stderr, code] = await Promise.all([readAll(p.stdout), readAll(p.stderr), p.wait()]);
    const result = { code, stdout, stderr };
    if (opts.check && code !== 0) throw new CommandError(result);
    return result;
  }

  /** Drop the connection. The machine and its processes keep running. */
  async detach(): Promise<void> {
    await this.transport.close();
  }

  async destroy(): Promise<void> {
    await this.detach();
    await this.owner.destroy(this.id);
    this.state = "terminated";
  }
}

export async function applySetup(m: Machine, setup: Setup): Promise<void> {
  const step = async (name: string, argv: string[] | string, opts: ExecOptions & { input?: string } = {}) => {
    const r = await m.run(argv, { ...opts, user: "root" });
    if (r.code !== 0) throw new SetupError(name, r.code, r.stdout + r.stderr);
  };
  if (setup.packages?.length) {
    await step("packages",
      "apt-get update -q && apt-get install -y -q --no-install-recommends " +
      "-o DPkg::Lock::Timeout=300 " + shjoin(setup.packages),
      { env: { DEBIAN_FRONTEND: "noninteractive" } });
  }
  for (const [path, text] of Object.entries(setup.files ?? {})) {
    await step(`file ${path}`, `mkdir -p ${shq(posix.dirname(path))} && cat > ${shq(path)}`, { input: text });
  }
  if (setup.script) await step("script", ["sh", "-s"], { input: setup.script, env: setup.env });
}

// ---------------------------------------------------------------- provider

/** Base class. Subclasses implement the `launch`, `infos`, `remove` and `transport` hooks. */
export abstract class Provider {
  abstract readonly name: string;

  protected abstract launch(id: string, spec: FullSpec, labels: Record<string, string>): Promise<Info>;
  protected abstract infos(): Promise<Info[]>;
  /** Must be idempotent and must also clean up a half-created machine. */
  protected abstract remove(id: string): Promise<void>;
  protected abstract transport(info: Info): Transport;

  protected async reapable(): Promise<string[]> {
    const now = new Date();
    return (await this.infos()).filter((i) => (expiresOf(i.labels) ?? now) < now).map((i) => i.id);
  }

  protected async ready(_m: Machine, _spec: FullSpec): Promise<void> {}

  /** Create a machine and apply `spec.setup`. Resolves when it is ready for execute(). */
  async create(spec: MachineSpec = {}): Promise<Machine> {
    const full = fullSpec(spec);
    const id = newId();
    let m: Machine | undefined;
    try {
      const info = await this.guard(() => this.launch(id, full, makeLabels(id, full)));
      m = this.machine(info);
      await this.ready(m, full);
      await applySetup(m, full.setup);
      return m;
    } catch (e) {
      await m?.detach().catch(() => {});
      await this.remove(id).catch(() => {});
      throw e;
    }
  }

  /** Every machine the backend still knows, in any state. */
  async list(): Promise<Machine[]> {
    return (await this.guard(() => this.infos())).map((i) => this.machine(i));
  }

  async attach(id: string): Promise<Machine> {
    const m = (await this.list()).find((x) => x.id === id);
    if (!m) throw new ProvisionError(`${this.name}: no machine ${id}`);
    return m;
  }

  async destroy(id: string): Promise<void> {
    await this.guard(() => this.remove(id));
  }

  /** Destroy machines whose TTL has passed. Returns how many. */
  async reap(): Promise<number> {
    const ids = await this.guard(() => this.reapable());
    for (const id of ids) await this.destroy(id);
    return ids.length;
  }

  private machine(info: Info): Machine {
    return new Machine(this, info, this.transport(info));
  }

  private async guard<T>(fn: () => Promise<T>): Promise<T> {
    try {
      return await fn();
    } catch (e) {
      if (e instanceof MachineError) throw e;
      throw new ProvisionError(`${this.name}: ${e instanceof Error ? e.message : e}`, { cause: e });
    }
  }
}
