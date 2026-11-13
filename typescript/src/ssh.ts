// SSH transport, shared by the three cloud providers.
import { generateKeyPairSync } from "node:crypto";
import { existsSync, mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { homedir } from "node:os";
import { dirname, join } from "node:path";
import ssh2 from "ssh2";
import { type Machine, type RawProcess, type Transport, TransportError, shq, sleep } from "./core.js";

const DEFAULT_KEY = join(homedir(), ".config", "nepho", "id_rsa");

/**
 * Return the private key path and the OpenSSH public key, creating the key on first use.
 * The key is RSA in PKCS#1 PEM so that the Python, TypeScript and Go implementations
 * can share one file, and every cloud accepts it.
 */
export function ensureKey(path?: string): { keyPath: string; publicKey: string } {
  const keyPath = path ?? DEFAULT_KEY;
  if (!existsSync(keyPath)) {
    mkdirSync(dirname(keyPath), { recursive: true, mode: 0o700 });
    const { privateKey } = generateKeyPairSync("rsa", {
      modulusLength: 3072,
      privateKeyEncoding: { type: "pkcs1", format: "pem" },
      publicKeyEncoding: { type: "spki", format: "pem" },
    });
    try {
      writeFileSync(keyPath, privateKey, { mode: 0o600, flag: "wx" });
    } catch (e) {
      if ((e as NodeJS.ErrnoException).code !== "EEXIST") throw e; // else another process won the race
    }
  }
  const parsed = ssh2.utils.parseKey(readFileSync(keyPath));
  if (parsed instanceof Error) throw parsed;
  const key = Array.isArray(parsed) ? parsed[0] : parsed;
  return { keyPath, publicKey: `${key.type} ${key.getPublicSSH().toString("base64")}` };
}

/** One lazily opened connection per machine; each command is a channel on it. */
export class SshTransport implements Transport {
  private conn?: Promise<ssh2.Client>;

  constructor(private readonly host: string | undefined, private readonly user: string,
              private readonly keyPath: string, private readonly timeoutMs = 600_000) {}

  private connectOnce(): Promise<ssh2.Client> {
    return new Promise((resolve, reject) => {
      const c = new ssh2.Client();
      c.once("ready", () => {
        c.removeAllListeners("error");
        c.on("error", () => {}); // later failures surface through the commands
        c.once("close", () => (this.conn = undefined));
        resolve(c);
      });
      c.once("error", reject);
      // Host keys of freshly created VMs are unknown, so they are not checked.
      c.connect({
        host: this.host, username: this.user, privateKey: readFileSync(this.keyPath),
        readyTimeout: 10_000, keepaliveInterval: 30_000,
      });
    });
  }

  private async connect(): Promise<ssh2.Client> {
    if (!this.host) throw new TransportError("machine has no address (is it running?)");
    const deadline = Date.now() + this.timeoutMs;
    for (;;) { // a new VM refuses connections until sshd and the key are in place
      try {
        return await this.connectOnce();
      } catch (e) {
        if (Date.now() > deadline) {
          throw new TransportError(`ssh ${this.user}@${this.host}: ${(e as Error).message}`, { cause: e });
        }
        await sleep(3000);
      }
    }
  }

  async exec(script: string, user: string | undefined, pty: boolean): Promise<RawProcess> {
    const pending = (this.conn ??= this.connect());
    let conn: ssh2.Client;
    try {
      conn = await pending;
    } catch (e) {
      if (this.conn === pending) this.conn = undefined;
      throw e;
    }
    if (user && user !== this.user) script = `sudo -n -u ${shq(user)} -- sh -c ${shq(script)}`;
    const ch = await new Promise<ssh2.ClientChannel>((resolve, reject) =>
      conn.exec(script, { pty: pty ? { term: "xterm" } : false }, (err, stream) =>
        err ? reject(new TransportError(`ssh ${this.host}: ${err.message}`, { cause: err })) : resolve(stream)));
    const exited = new Promise<number>((resolve) => {
      ch.once("exit", (code: number | null) => resolve(code ?? -1));
      ch.once("close", () => resolve(-1)); // closed without an exit status
    });
    return { stdin: ch, stdout: ch, stderr: ch.stderr, wait: () => exited };
  }

  async close(): Promise<void> {
    const pending = this.conn;
    this.conn = undefined;
    if (!pending) return;
    const conn = await pending.catch(() => undefined);
    if (conn) await new Promise<void>((r) => conn.once("close", () => r()).end());
  }
}

/** Wait until first-boot provisioning has finished, so apt is free to use. */
export async function sshReady(m: Machine): Promise<void> {
  await m.run("cloud-init status --wait >/dev/null 2>&1 || true");
}
