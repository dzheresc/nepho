// AWS backend: one EC2 instance plus one security group per machine.
import {
  AuthorizeSecurityGroupIngressCommand, CreateSecurityGroupCommand, DeleteSecurityGroupCommand,
  DescribeImagesCommand, DescribeSecurityGroupsCommand, EC2Client, type Instance, RunInstancesCommand,
  type SecurityGroup, type Tag, TerminateInstancesCommand, paginateDescribeInstances,
  waitUntilInstanceRunning, waitUntilInstanceTerminated,
} from "@aws-sdk/client-ec2";
import { GetParameterCommand, SSMClient } from "@aws-sdk/client-ssm";
import {
  type FullSpec, type Info, L_ID, L_MANAGED, L_USER, type Machine, Provider, ProvisionError,
  type State, type Transport, expiresOf, myCidr, pickSize, sleep, ttlMinutes,
} from "./core.js";
import { SshTransport, ensureKey, sshReady } from "./ssh.js";

const ubuntu = (v: string, gp: number) =>
  `/aws/service/canonical/ubuntu/server/${v}/stable/current/amd64/hvm/ebs-gp${gp}/ami-id`;
// os -> [public SSM parameter holding the current AMI id, login user]
const IMAGES: Record<string, [string, string]> = {
  "ubuntu-24.04": [ubuntu("24.04", 3), "ubuntu"],
  "ubuntu-22.04": [ubuntu("22.04", 2), "ubuntu"],
  "debian-12": ["/aws/service/debian/release/12/latest/amd64", "admin"],
};
const SIZES: [number, number, string][] = [
  [2, 1, "t3.micro"], [2, 2, "t3.small"], [2, 4, "t3.medium"], [2, 8, "t3.large"],
  [4, 16, "t3.xlarge"], [8, 32, "t3.2xlarge"], [16, 64, "m6i.4xlarge"],
  [32, 128, "m6i.8xlarge"], [64, 256, "m6i.16xlarge"],
];
const STATES: Record<string, State> = {
  pending: "pending", running: "running", stopping: "stopped", stopped: "stopped",
  "shutting-down": "terminated", terminated: "terminated",
};
const fromTags = (tags?: Tag[]): Record<string, string> =>
  Object.fromEntries((tags ?? []).map((t) => [t.Key ?? "", t.Value ?? ""]));

export interface AwsOptions {
  region?: string;
  instanceType?: string;
  sshCidr?: string;
  keyPath?: string;
}

/**
 * Uses the default VPC of the region and the SDK's default credential chain.
 *
 * When the TTL passes the instance shuts itself down, which terminates it;
 * reap() then removes the leftover security group. EC2 keeps listing a
 * terminated instance for about an hour.
 */
export class AwsProvider extends Provider {
  readonly name = "aws";
  private readonly ec2: EC2Client;
  private readonly ssm: SSMClient;
  private readonly key: { keyPath: string; publicKey: string };

  constructor(private readonly opts: AwsOptions = {}) {
    super();
    this.ec2 = new EC2Client({ region: opts.region });
    this.ssm = new SSMClient({ region: opts.region });
    this.key = ensureKey(opts.keyPath);
  }

  protected async launch(id: string, spec: FullSpec, base: Record<string, string>): Promise<Info> {
    const image = IMAGES[spec.os];
    if (!image) throw new ProvisionError(`aws: unsupported os '${spec.os}'`);
    const [param, user] = image;
    const itype = this.opts.instanceType ?? pickSize(SIZES, spec.cpus, spec.memoryGb);
    const ec2 = this.ec2;
    const ami = (await this.ssm.send(new GetParameterCommand({ Name: param }))).Parameter?.Value;
    const root = (await ec2.send(new DescribeImagesCommand({ ImageIds: [ami!] }))).Images?.[0]?.RootDeviceName;
    const labels = { ...base, [L_USER]: user };
    const tags = Object.entries({ ...labels, Name: id }).map(([Key, Value]) => ({ Key, Value }));

    const sg = (await ec2.send(new CreateSecurityGroupCommand({
      GroupName: id, Description: `nepho ${id}`,
      TagSpecifications: [{ ResourceType: "security-group", Tags: tags }],
    }))).GroupId;
    await ec2.send(new AuthorizeSecurityGroupIngressCommand({
      GroupId: sg, IpProtocol: "tcp", FromPort: 22, ToPort: 22,
      CidrIp: this.opts.sshCidr ?? (await myCidr()),
    }));
    const userData = `#cloud-config\nssh_authorized_keys:\n  - ${this.key.publicKey}\n`;
    const run = await ec2.send(new RunInstancesCommand({
      ImageId: ami, InstanceType: itype as never, MinCount: 1, MaxCount: 1, SecurityGroupIds: [sg!],
      UserData: Buffer.from(userData).toString("base64"),
      InstanceInitiatedShutdownBehavior: "terminate",
      BlockDeviceMappings: [{
        DeviceName: root, Ebs: { VolumeSize: spec.diskGb, VolumeType: "gp3", DeleteOnTermination: true },
      }],
      TagSpecifications: [{ ResourceType: "instance", Tags: tags }, { ResourceType: "volume", Tags: tags }],
    }));
    const iid = run.Instances?.[0]?.InstanceId;
    await waitUntilInstanceRunning({ client: ec2, maxWaitTime: 600 }, { InstanceIds: [iid!] });
    const info = (await this.infos()).find((i) => i.id === id);
    if (!info) throw new ProvisionError(`aws: instance ${iid} disappeared`);
    return info;
  }

  private async instances(key: string, value: string): Promise<Instance[]> {
    const out: Instance[] = [];
    const pages = paginateDescribeInstances({ client: this.ec2 },
      { Filters: [{ Name: `tag:${key}`, Values: [value] }] });
    for await (const page of pages) {
      for (const r of page.Reservations ?? []) out.push(...(r.Instances ?? []));
    }
    return out;
  }

  protected async infos(): Promise<Info[]> {
    return (await this.instances(L_MANAGED, "true")).map((i) => {
      const labels = fromTags(i.Tags);
      return {
        id: labels[L_ID] ?? i.InstanceId!, state: STATES[i.State?.Name ?? ""] ?? "stopped", labels,
        address: i.PublicIpAddress, user: labels[L_USER],
      };
    });
  }

  private async groups(key: string, value: string): Promise<SecurityGroup[]> {
    const r = await this.ec2.send(new DescribeSecurityGroupsCommand(
      { Filters: [{ Name: `tag:${key}`, Values: [value] }] }));
    return r.SecurityGroups ?? [];
  }

  protected async remove(id: string): Promise<void> {
    const ec2 = this.ec2;
    const ids = (await this.instances(L_ID, id))
      .filter((i) => i.State?.Name !== "terminated").map((i) => i.InstanceId!);
    if (ids.length) {
      await ec2.send(new TerminateInstancesCommand({ InstanceIds: ids }));
      await waitUntilInstanceTerminated({ client: ec2, maxWaitTime: 600 }, { InstanceIds: ids });
    }
    for (const g of await this.groups(L_ID, id)) {
      for (const deadline = Date.now() + 120_000; ;) { // stays "in use" for a moment after termination
        try {
          await ec2.send(new DeleteSecurityGroupCommand({ GroupId: g.GroupId }));
          break;
        } catch (e) {
          if ((e as Error).name !== "DependencyViolation" || Date.now() > deadline) throw e;
          await sleep(5000);
        }
      }
    }
  }

  protected override async reapable(): Promise<string[]> {
    // Every machine owns a security group until it is destroyed, so the groups are the
    // complete list, including instances that already terminated themselves.
    const now = new Date();
    const out = new Set<string>();
    for (const g of await this.groups(L_MANAGED, "true")) {
      const labels = fromTags(g.Tags);
      const expires = expiresOf(labels);
      if (labels[L_ID] && expires && expires < now) out.add(labels[L_ID]);
    }
    return [...out];
  }

  protected transport(info: Info): Transport {
    return new SshTransport(info.address, info.user ?? "ubuntu", this.key.keyPath);
  }

  protected override async ready(m: Machine, spec: FullSpec): Promise<void> {
    await sshReady(m);
    const minutes = ttlMinutes(spec);
    if (minutes !== null) await m.run(["shutdown", "-h", `+${minutes}`], { user: "root", check: true });
  }
}
