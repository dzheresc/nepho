// GCP backend: one Compute Engine instance plus one firewall rule per machine.
import { FirewallsClient, GlobalOperationsClient, InstancesClient, ZoneOperationsClient, type protos } from "@google-cloud/compute";
import {
  type FullSpec, type Info, L_ID, L_MANAGED, type Machine, Provider, ProvisionError, type State,
  type Transport, myCidr, pickSize,
} from "./core.js";
import { SshTransport, ensureKey, sshReady } from "./ssh.js";

type Operation = protos.google.cloud.compute.v1.IOperation;
type Instance = protos.google.cloud.compute.v1.IInstance;

const USER = "nepho";
const IMAGES: Record<string, string> = {
  "ubuntu-24.04": "projects/ubuntu-os-cloud/global/images/family/ubuntu-2404-lts-amd64",
  "ubuntu-22.04": "projects/ubuntu-os-cloud/global/images/family/ubuntu-2204-lts",
  "debian-12": "projects/debian-cloud/global/images/family/debian-12",
};
const SIZES: [number, number, string][] = [
  [2, 1, "e2-micro"], [2, 2, "e2-small"], [2, 4, "e2-medium"], [2, 8, "e2-standard-2"],
  [4, 16, "e2-standard-4"], [8, 32, "e2-standard-8"], [16, 64, "e2-standard-16"],
  [32, 128, "e2-standard-32"],
];
const STATES: Record<string, State> = { PROVISIONING: "pending", STAGING: "pending", RUNNING: "running" };
const notFound = (e: unknown) => (e as { code?: number }).code === 404 || (e as { code?: number }).code === 5;

export interface GcpOptions {
  project?: string;
  zone?: string; // default us-central1-a
  machineType?: string;
  sshCidr?: string;
  keyPath?: string;
}

/**
 * Uses the `default` network and Application Default Credentials.
 *
 * Nothing stops the instance by itself when the TTL passes; call reap().
 * Label keys and values must be lowercase letters, digits, '-' or '_'.
 */
export class GcpProvider extends Provider {
  readonly name = "gcp";
  private readonly vms = new InstancesClient();
  private readonly fw = new FirewallsClient();
  private readonly zoneOps = new ZoneOperationsClient();
  private readonly globalOps = new GlobalOperationsClient();
  private readonly zone: string;
  private readonly key: { keyPath: string; publicKey: string };
  private projectId?: string;

  constructor(private readonly opts: GcpOptions = {}) {
    super();
    this.zone = opts.zone ?? "us-central1-a";
    this.projectId = opts.project;
    this.key = ensureKey(opts.keyPath);
  }

  private async project(): Promise<string> {
    return (this.projectId ??= await this.vms.getProjectId());
  }

  /** Wait for a zonal (or, without `zone`, global) operation and raise its error. */
  private async done(op: Operation, zone?: string): Promise<void> {
    const project = await this.project();
    while (op.status !== "DONE") {
      [op] = zone
        ? await this.zoneOps.wait({ operation: op.name!, project, zone })
        : await this.globalOps.wait({ operation: op.name!, project });
    }
    const err = op.error?.errors?.[0];
    if (err) throw new ProvisionError(`gcp: ${err.code}: ${err.message}`);
  }

  protected async launch(id: string, spec: FullSpec, labels: Record<string, string>): Promise<Info> {
    const image = IMAGES[spec.os];
    if (!image) throw new ProvisionError(`gcp: unsupported os '${spec.os}'`);
    const mtype = this.opts.machineType ?? pickSize(SIZES, spec.cpus, spec.memoryGb);
    const project = await this.project();
    const zone = this.zone;
    // The network tag `id` ties the firewall rule to this one instance.
    const [fw] = await this.fw.insert({
      project,
      firewallResource: {
        name: id, description: "nepho-managed", network: "global/networks/default",
        direction: "INGRESS", sourceRanges: [this.opts.sshCidr ?? (await myCidr())], targetTags: [id],
        allowed: [{ IPProtocol: "tcp", ports: ["22"] }],
      },
    });
    await this.done(fw.latestResponse as Operation);
    const [vm] = await this.vms.insert({
      project, zone,
      instanceResource: {
        name: id, labels, tags: { items: [id] },
        machineType: `zones/${zone}/machineTypes/${mtype}`,
        disks: [{
          boot: true, autoDelete: true,
          initializeParams: { sourceImage: image, diskSizeGb: spec.diskGb },
        }],
        networkInterfaces: [{
          network: "global/networks/default",
          accessConfigs: [{ name: "External NAT", type: "ONE_TO_ONE_NAT" }],
        }],
        metadata: {
          items: [
            { key: "ssh-keys", value: `${USER}:${this.key.publicKey} ${USER}` },
            { key: "enable-oslogin", value: "FALSE" },
          ],
        },
      },
    });
    await this.done(vm.latestResponse as Operation, zone);
    const [created] = await this.vms.get({ project, zone, instance: id });
    return this.info(created);
  }

  private info(vm: Instance): Info {
    let address: string | undefined;
    for (const nic of vm.networkInterfaces ?? []) {
      for (const ac of nic.accessConfigs ?? []) address ??= ac.natIP ?? undefined;
    }
    const labels = { ...(vm.labels ?? {}) };
    return {
      id: labels[L_ID] ?? vm.name!, state: STATES[String(vm.status)] ?? "stopped", labels, address, user: USER,
    };
  }

  protected async infos(): Promise<Info[]> {
    const [vms] = await this.vms.list({ project: await this.project(), zone: this.zone });
    return vms.filter((vm) => vm.labels?.[L_MANAGED] === "true").map((vm) => this.info(vm));
  }

  protected async remove(id: string): Promise<void> {
    const project = await this.project();
    try {
      const [op] = await this.vms.delete({ project, zone: this.zone, instance: id });
      await this.done(op.latestResponse as Operation, this.zone);
    } catch (e) {
      if (!notFound(e)) throw e;
    }
    try {
      const [op] = await this.fw.delete({ project, firewall: id });
      await this.done(op.latestResponse as Operation);
    } catch (e) {
      if (!notFound(e)) throw e;
    }
  }

  protected transport(info: Info): Transport {
    return new SshTransport(info.address, USER, this.key.keyPath);
  }

  protected override async ready(m: Machine): Promise<void> {
    await sshReady(m);
  }
}
