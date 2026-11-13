"""AWS backend: one EC2 instance plus one security group per machine."""
from __future__ import annotations

import time
from datetime import datetime, timezone

import boto3
from botocore.exceptions import ClientError

from .core import (L_ID, L_MANAGED, L_USER, PENDING, RUNNING, STOPPED, TERMINATED, Info, Machine,
                   MachineSpec, Provider, ProvisionError, expires_of, my_cidr, pick_size,
                   ttl_minutes)
from .ssh import SshTransport, ensure_key, ssh_ready

_UBUNTU = "/aws/service/canonical/ubuntu/server/{}/stable/current/amd64/hvm/ebs-gp{}/ami-id"
# os -> (public SSM parameter holding the current AMI id, login user)
IMAGES = {
    "ubuntu-24.04": (_UBUNTU.format("24.04", 3), "ubuntu"),
    "ubuntu-22.04": (_UBUNTU.format("22.04", 2), "ubuntu"),
    "debian-12": ("/aws/service/debian/release/12/latest/amd64", "admin"),
}
SIZES = [(2, 1, "t3.micro"), (2, 2, "t3.small"), (2, 4, "t3.medium"), (2, 8, "t3.large"),
         (4, 16, "t3.xlarge"), (8, 32, "t3.2xlarge"), (16, 64, "m6i.4xlarge"),
         (32, 128, "m6i.8xlarge"), (64, 256, "m6i.16xlarge")]
STATES = {"pending": PENDING, "running": RUNNING, "stopping": STOPPED, "stopped": STOPPED,
          "shutting-down": TERMINATED, "terminated": TERMINATED}


class AwsProvider(Provider):
    """Uses the default VPC of the region and boto3's default credential chain.

    When the TTL passes the instance shuts itself down, which terminates it;
    reap() then removes the leftover security group. EC2 keeps listing a
    terminated instance for about an hour.
    """

    name = "aws"

    def __init__(self, *, region: str | None = None, instance_type: str | None = None,
                 ssh_cidr: str | None = None, key_path: str | None = None):
        session = boto3.session.Session(region_name=region)
        self._ec2, self._ssm = session.client("ec2"), session.client("ssm")
        self._instance_type, self._ssh_cidr = instance_type, ssh_cidr
        self._key_path, self._pub = ensure_key(key_path)

    def _launch(self, id: str, spec: MachineSpec, labels: dict[str, str]) -> Info:
        if spec.os not in IMAGES:
            raise ProvisionError(f"aws: unsupported os {spec.os!r}")
        param, user = IMAGES[spec.os]
        itype = self._instance_type or pick_size(SIZES, spec.cpus, spec.memory_gb)
        ec2 = self._ec2
        ami = self._ssm.get_parameter(Name=param)["Parameter"]["Value"]
        root = ec2.describe_images(ImageIds=[ami])["Images"][0]["RootDeviceName"]
        labels = {**labels, L_USER: user}
        tags = [{"Key": k, "Value": v} for k, v in {**labels, "Name": id}.items()]

        sg = ec2.create_security_group(
            GroupName=id, Description=f"nepho {id}",
            TagSpecifications=[{"ResourceType": "security-group", "Tags": tags}])["GroupId"]
        ec2.authorize_security_group_ingress(
            GroupId=sg, IpProtocol="tcp", FromPort=22, ToPort=22,
            CidrIp=self._ssh_cidr or my_cidr())
        iid = ec2.run_instances(
            ImageId=ami, InstanceType=itype, MinCount=1, MaxCount=1, SecurityGroupIds=[sg],
            UserData=f"#cloud-config\nssh_authorized_keys:\n  - {self._pub}\n",
            InstanceInitiatedShutdownBehavior="terminate",
            BlockDeviceMappings=[{"DeviceName": root, "Ebs": {
                "VolumeSize": spec.disk_gb, "VolumeType": "gp3", "DeleteOnTermination": True}}],
            TagSpecifications=[{"ResourceType": "instance", "Tags": tags},
                               {"ResourceType": "volume", "Tags": tags}],
        )["Instances"][0]["InstanceId"]
        ec2.get_waiter("instance_running").wait(InstanceIds=[iid])
        return next(i for i in self._list() if i.id == id)

    def _instances(self, key: str, value: str) -> list[dict]:
        pages = self._ec2.get_paginator("describe_instances").paginate(
            Filters=[{"Name": f"tag:{key}", "Values": [value]}])
        return [i for page in pages for r in page["Reservations"] for i in r["Instances"]]

    def _list(self) -> list[Info]:
        out = []
        for i in self._instances(L_MANAGED, "true"):
            labels = {t["Key"]: t["Value"] for t in i.get("Tags", [])}
            out.append(Info(labels[L_ID], STATES.get(i["State"]["Name"], STOPPED), labels,
                            i.get("PublicIpAddress"), labels.get(L_USER)))
        return out

    def _groups(self, key: str, value: str) -> list[dict]:
        return self._ec2.describe_security_groups(
            Filters=[{"Name": f"tag:{key}", "Values": [value]}])["SecurityGroups"]

    def _destroy(self, id: str) -> None:
        ec2 = self._ec2
        ids = [i["InstanceId"] for i in self._instances(L_ID, id)
               if i["State"]["Name"] != "terminated"]
        if ids:
            ec2.terminate_instances(InstanceIds=ids)
            ec2.get_waiter("instance_terminated").wait(InstanceIds=ids)
        for g in self._groups(L_ID, id):
            deadline = time.monotonic() + 120
            while True:  # the group stays "in use" for a moment after termination
                try:
                    ec2.delete_security_group(GroupId=g["GroupId"])
                    break
                except ClientError as e:
                    if (e.response["Error"]["Code"] != "DependencyViolation"
                            or time.monotonic() > deadline):
                        raise
                    time.sleep(5)

    def _reapable(self) -> set[str]:
        # Every machine owns a security group until it is destroyed, so the groups
        # are the complete list, including instances that already terminated themselves.
        now = datetime.now(timezone.utc)
        out = set()
        for g in self._groups(L_MANAGED, "true"):
            labels = {t["Key"]: t["Value"] for t in g.get("Tags", [])}
            expires = expires_of(labels)
            if L_ID in labels and expires and expires < now:
                out.add(labels[L_ID])
        return out

    def _transport(self, info: Info) -> SshTransport:
        return SshTransport(info.address, info.user or "ubuntu", self._key_path)

    async def _ready(self, m: Machine, spec: MachineSpec) -> None:
        await ssh_ready(m)
        minutes = ttl_minutes(spec)
        if minutes is not None:
            await m.run(["shutdown", "-h", f"+{minutes}"], user="root", check=True)
