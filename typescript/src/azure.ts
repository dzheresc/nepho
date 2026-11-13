// Azure backend: one resource group per machine, holding the VM and its network.
import { ComputeManagementClient } from "@azure/arm-compute";
import { NetworkManagementClient } from "@azure/arm-network";
import { type ResourceGroup, ResourceManagementClient } from "@azure/arm-resources";
import { DefaultAzureCredential } from "@azure/identity";
import {
  type FullSpec, type Info, L_ID, L_MANAGED, type Machine, Provider, ProvisionError, type State,
  type Transport, myCidr, pickSize,
} from "./core.js";
import { SshTransport, ensureKey, sshReady } from "./ssh.js";

const USER = "azureuser";
// os -> [publisher, offer, sku]
const IMAGES: Record<string, [string, string, string]> = {
  "ubuntu-24.04": ["Canonical", "ubuntu-24_04-lts", "server"],
  "ubuntu-22.04": ["Canonical", "0001-com-ubuntu-server-jammy", "22_04-lts-gen2"],
  "debian-12": ["Debian", "debian-12", "12-gen2"],
};
const SIZES: [number, number, string][] = [
  [2, 1, "Standard_B2ts_v2"], [2, 4, "Standard_B2ls_v2"], [2, 8, "Standard_B2s_v2"],
  [4, 16, "Standard_B4s_v2"], [8, 32, "Standard_B8s_v2"], [16, 64, "Standard_D16s_v5"], [32, 128, "Standard_D32s_v5"],
];
const MIN_DISK_GB = 30; // size of the marketplace images; the OS disk cannot be smaller
const notFound = (e: unknown) => (e as { statusCode?: number }).statusCode === 404;

export interface AzureOptions {
  subscriptionId?: string; // default: $AZURE_SUBSCRIPTION_ID
  location?: string; // default eastus
  vmSize?: string;
  sshCidr?: string;
  keyPath?: string;
}

/**
 * The resource group is named after the machine id and carries the labels as tags;
 * deleting it removes everything. Uses DefaultAzureCredential.
 *
 * Nothing stops the VM by itself when the TTL passes; call reap().
 */
export class AzureProvider extends Provider {
  readonly name = "azure";
  private readonly res: ResourceManagementClient;
  private readonly net: NetworkManagementClient;
  private readonly vms: ComputeManagementClient;
  private readonly location: string;
  private readonly key: { keyPath: string; publicKey: string };

  constructor(private readonly opts: AzureOptions = {}) {
    super();
    const sub = opts.subscriptionId ?? process.env.AZURE_SUBSCRIPTION_ID;
    if (!sub) throw new ProvisionError("azure: pass subscriptionId or set AZURE_SUBSCRIPTION_ID");
    const cred = new DefaultAzureCredential();
    this.res = new ResourceManagementClient(cred, sub);
    this.net = new NetworkManagementClient(cred, sub);
    this.vms = new ComputeManagementClient(cred, sub);
    this.location = opts.location ?? "eastus";
    this.key = ensureKey(opts.keyPath);
  }

  protected async launch(id: string, spec: FullSpec, labels: Record<string, string>): Promise<Info> {
    const image = IMAGES[spec.os];
    if (!image) throw new ProvisionError(`azure: unsupported os '${spec.os}'`);
    const [publisher, offer, sku] = image;
    const vmSize = this.opts.vmSize ?? pickSize(SIZES, spec.cpus, spec.memoryGb);
    const location = this.location;
    const net = this.net;
    await this.res.resourceGroups.createOrUpdate(id, { location, tags: labels });
    const nsg = await net.networkSecurityGroups.beginCreateOrUpdateAndWait(id, "nsg", {
      location,
      securityRules: [{
        name: "ssh", protocol: "Tcp", direction: "Inbound", access: "Allow", priority: 100,
        sourceAddressPrefix: this.opts.sshCidr ?? (await myCidr()), sourcePortRange: "*",
        destinationAddressPrefix: "*", destinationPortRange: "22",
      }],
    });
    const vnet = await net.virtualNetworks.beginCreateOrUpdateAndWait(id, "vnet", {
      location, addressSpace: { addressPrefixes: ["10.0.0.0/16"] },
      subnets: [{ name: "default", addressPrefix: "10.0.0.0/24" }],
    });
    const ip = await net.publicIPAddresses.beginCreateOrUpdateAndWait(id, "ip", {
      location, sku: { name: "Standard" }, publicIPAllocationMethod: "Static",
    });
    const nic = await net.networkInterfaces.beginCreateOrUpdateAndWait(id, "nic", {
      location, networkSecurityGroup: { id: nsg.id },
      ipConfigurations: [{ name: "ipconfig", subnet: { id: vnet.subnets?.[0]?.id }, publicIPAddress: { id: ip.id } }],
    });
    await this.vms.virtualMachines.beginCreateOrUpdateAndWait(id, "vm", {
      location,
      hardwareProfile: { vmSize },
      storageProfile: {
        imageReference: { publisher, offer, sku, version: "latest" },
        osDisk: { createOption: "FromImage", diskSizeGB: Math.max(spec.diskGb, MIN_DISK_GB) },
      },
      osProfile: {
        computerName: id, adminUsername: USER,
        linuxConfiguration: {
          disablePasswordAuthentication: true,
          ssh: { publicKeys: [{ path: `/home/${USER}/.ssh/authorized_keys`, keyData: this.key.publicKey }] },
        },
      },
      networkProfile: { networkInterfaces: [{ id: nic.id }] },
    });
    return { id, state: "running", labels, address: ip.ipAddress, user: USER };
  }

  private async info(group: ResourceGroup): Promise<Info> {
    const labels = { ...(group.tags ?? {}) };
    let address: string | undefined;
    let state: State = "pending"; // pending: the group exists but the VM does not (yet)
    try {
      address = (await this.net.publicIPAddresses.get(group.name!, "ip")).ipAddress;
      const view = await this.vms.virtualMachines.instanceView(group.name!, "vm");
      const power = view.statuses?.find((s) => s.code?.startsWith("PowerState/"))?.code;
      state = power === "PowerState/running" ? "running" : power === "PowerState/starting" ? "pending" : "stopped";
    } catch (e) {
      if (!notFound(e)) throw e;
    }
    return { id: labels[L_ID] ?? group.name!, state, labels, address, user: USER };
  }

  protected async infos(): Promise<Info[]> {
    const out: Info[] = [];
    const groups = this.res.resourceGroups.list({ filter: `tagName eq '${L_MANAGED}' and tagValue eq 'true'` });
    for await (const g of groups) out.push(await this.info(g));
    return out;
  }

  protected async remove(id: string): Promise<void> {
    await this.res.resourceGroups.beginDeleteAndWait(id).catch((e) => {
      if (!notFound(e)) throw e;
    });
  }

  protected transport(info: Info): Transport {
    return new SshTransport(info.address, USER, this.key.keyPath);
  }

  protected override async ready(m: Machine): Promise<void> {
    await sshReady(m);
  }
}
