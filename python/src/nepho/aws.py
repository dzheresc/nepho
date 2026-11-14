"""AWS backend: one EC2 instance plus one security group per machine.

Services are RDS (postgres) and ElastiCache (valkey), each with its own security
group and its password in the Parameter Store under /nepho/<id>.
"""
from __future__ import annotations

import secrets
import time
from datetime import datetime, timezone

import boto3
from botocore.exceptions import ClientError

from .core import (L_ID, L_KIND, L_MANAGED, L_USER, L_VERSION, PENDING, RUNNING, SERVICES, STOPPED,
                   TERMINATED, Info, Machine, MachineSpec, Provider, ProvisionError, Service,
                   ServiceSpec, expires_of, my_cidr, pick_size, ttl_minutes)
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
DB_SIZES = [(2, 1, "db.t4g.micro"), (2, 2, "db.t4g.small"), (2, 4, "db.t4g.medium"),
            (2, 8, "db.t4g.large"), (4, 16, "db.t4g.xlarge"), (8, 32, "db.t4g.2xlarge")]
# Cache nodes have less usable memory than the row says: 0.5, 1.4, 3.1, 6.4 and 12.9 GB.
CACHE_SIZES = [(2, 1, "cache.t4g.micro"), (2, 2, "cache.t4g.small"), (2, 4, "cache.t4g.medium"),
               (2, 8, "cache.m7g.large"), (4, 16, "cache.m7g.xlarge")]
# status of a database or cache -> state; anything else is "pending"
SERVICE_STATES = {"available": RUNNING, "stopping": STOPPED, "stopped": STOPPED,
                  "deleting": TERMINATED}
SECRETS = "/nepho/"  # Parameter Store path of the service passwords
GONE = {"DBInstanceNotFound", "ReplicationGroupNotFoundFault"}
BUSY = {"InvalidDBInstanceState", "InvalidReplicationGroupState"}


class AwsProvider(Provider):
    """Uses the default VPC of the region and boto3's default credential chain.

    When the TTL passes the instance shuts itself down, which terminates it;
    reap() then removes the leftover security group. EC2 keeps listing a
    terminated instance for about an hour.

    Services: postgres is an RDS instance with a public endpoint that accepts
    the caller's address (`ssh_cidr`) and the VPC. valkey is a one-node
    ElastiCache replication group, reachable only from inside the VPC, so it
    has an `internal_url` and no `url`. Both accept only TLS connections.
    Nothing stops a service at the TTL; call reap().
    """

    name = "aws"

    def __init__(self, *, region: str | None = None, instance_type: str | None = None,
                 ssh_cidr: str | None = None, key_path: str | None = None):
        session = boto3.session.Session(region_name=region)
        self._ec2, self._ssm = session.client("ec2"), session.client("ssm")
        self._rds, self._cache = session.client("rds"), session.client("elasticache")
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

    # ------------------------------------------------------------ services

    def _launch_service(self, id: str, spec: ServiceSpec, labels: dict[str, str]) -> Service:
        ec2, port = self._ec2, SERVICES[spec.kind][0]
        password = secrets.token_hex(16)
        tags = [{"Key": k, "Value": v} for k, v in {**labels, "Name": id}.items()]
        vpcs = ec2.describe_vpcs(Filters=[{"Name": "is-default", "Values": ["true"]}])["Vpcs"]
        if not vpcs:
            raise ProvisionError("aws: the region has no default VPC")
        sources = [{"CidrIp": vpcs[0]["CidrBlock"], "Description": "machines in the VPC"}]
        if spec.kind == "postgres":
            sources.append({"CidrIp": self._ssh_cidr or my_cidr(), "Description": "caller"})
        sg = ec2.create_security_group(
            GroupName=id, Description=f"nepho {id}",
            TagSpecifications=[{"ResourceType": "security-group", "Tags": tags}])["GroupId"]
        ec2.authorize_security_group_ingress(GroupId=sg, IpPermissions=[
            {"IpProtocol": "tcp", "FromPort": port, "ToPort": port, "IpRanges": sources}])
        self._ssm.put_parameter(Name=SECRETS + id, Value=password, Type="SecureString", Tags=tags)
        wait = {"Delay": 15, "MaxAttempts": 160}  # 40 minutes
        if spec.kind == "postgres":
            _, _, _, user, database = SERVICES["postgres"]
            self._rds.create_db_instance(
                DBInstanceIdentifier=id, Engine="postgres", EngineVersion=spec.version,
                DBInstanceClass=pick_size(DB_SIZES, spec.cpus, spec.memory_gb),
                AllocatedStorage=max(spec.disk_gb, 20), StorageType="gp3",
                MasterUsername=user, MasterUserPassword=password, DBName=database,
                VpcSecurityGroupIds=[sg], PubliclyAccessible=True, MultiAZ=False,
                BackupRetentionPeriod=0, DeletionProtection=False, Tags=tags)
            self._rds.get_waiter("db_instance_available").wait(
                DBInstanceIdentifier=id, WaiterConfig=wait)
        else:
            self._cache.create_replication_group(
                ReplicationGroupId=id, ReplicationGroupDescription=f"nepho {id}",
                Engine="valkey", EngineVersion=self._cache_version(spec.version),
                CacheNodeType=pick_size(CACHE_SIZES, spec.cpus, spec.memory_gb),
                NumCacheClusters=1, AutomaticFailoverEnabled=False, SecurityGroupIds=[sg],
                TransitEncryptionEnabled=True, AuthToken=password,  # a password requires TLS
                SnapshotRetentionLimit=0, Tags=tags)
            self._cache.get_waiter("replication_group_available").wait(
                ReplicationGroupId=id, WaiterConfig=wait)
        return self._service(labels, password)

    def _cache_version(self, wanted: str) -> str:
        """ElastiCache wants major.minor: pick the newest one that matches `wanted`."""
        pages = self._cache.get_paginator("describe_cache_engine_versions").paginate(Engine="valkey")
        found = [v["EngineVersion"] for page in pages for v in page["CacheEngineVersions"]]
        matching = [v for v in found if v == wanted or v.startswith(wanted + ".")]
        if not matching:
            raise ProvisionError(f"aws: no valkey version {wanted!r}; available: {sorted(found)}")
        return max(matching, key=lambda v: [int(p) if p.isdigit() else 0 for p in v.split(".")])

    def _service(self, labels: dict[str, str], password: str) -> Service:
        id, kind = labels[L_ID], labels[L_KIND]
        port, _, _, user, database = SERVICES[kind]
        status = endpoint = None
        try:
            if kind == "postgres":
                db = self._rds.describe_db_instances(DBInstanceIdentifier=id)["DBInstances"][0]
                status, endpoint = db["DBInstanceStatus"], db.get("Endpoint")
            else:
                group = self._cache.describe_replication_groups(
                    ReplicationGroupId=id)["ReplicationGroups"][0]
                status = group["Status"]
                endpoint = next((n.get("PrimaryEndpoint") for n in group.get("NodeGroups", [])), None)
        except ClientError as e:
            if e.response["Error"]["Code"] not in GONE:
                raise
        state = TERMINATED if status is None else SERVICE_STATES.get(status, PENDING)
        host, port = (endpoint["Address"], endpoint["Port"]) if endpoint else (None, None)
        return Service(
            id, kind, labels.get(L_VERSION, ""), state, labels, user, password, database,
            host=host if kind == "postgres" else None, port=port if kind == "postgres" else None,
            internal_host=host, internal_port=port, tls=True)

    def _list_services(self) -> list[Service]:
        # Every service owns a tagged security group until it is destroyed.
        groups = self._ec2.describe_security_groups(Filters=[
            {"Name": f"tag:{L_MANAGED}", "Values": ["true"]},
            {"Name": "tag-key", "Values": [L_KIND]}])["SecurityGroups"]
        if not groups:
            return []
        pages = self._ssm.get_paginator("get_parameters_by_path").paginate(
            Path=SECRETS, WithDecryption=True)
        passwords = {p["Name"]: p["Value"] for page in pages for p in page["Parameters"]}
        out = []
        for g in groups:
            labels = {t["Key"]: t["Value"] for t in g.get("Tags", [])}
            out.append(self._service(labels, passwords.get(SECRETS + labels[L_ID], "")))
        return out

    def _destroy_service(self, id: str) -> None:
        rds, cache = self._rds, self._cache
        for delete, status in (
            (lambda: rds.delete_db_instance(DBInstanceIdentifier=id, SkipFinalSnapshot=True,
                                            DeleteAutomatedBackups=True),
             lambda: rds.describe_db_instances(
                 DBInstanceIdentifier=id)["DBInstances"][0]["DBInstanceStatus"]),
            (lambda: cache.delete_replication_group(ReplicationGroupId=id),
             lambda: cache.describe_replication_groups(
                 ReplicationGroupId=id)["ReplicationGroups"][0]["Status"]),
        ):
            deadline = time.monotonic() + 3600
            while True:
                try:
                    if status() != "deleting":
                        try:
                            delete()
                        except ClientError as e:  # still being created: ask again later
                            if e.response["Error"]["Code"] not in BUSY:
                                raise
                except ClientError as e:
                    if e.response["Error"]["Code"] in GONE:
                        break
                    raise
                if time.monotonic() > deadline:
                    raise ProvisionError(f"aws: {id} is still not deleted")
                time.sleep(15)
        try:
            self._ssm.delete_parameter(Name=SECRETS + id)
        except ClientError as e:
            if e.response["Error"]["Code"] != "ParameterNotFound":
                raise

    def _destroy(self, id: str) -> None:
        ec2 = self._ec2
        if id.startswith("s-"):
            self._destroy_service(id)
        ids = [i["InstanceId"] for i in self._instances(L_ID, id)
               if i["State"]["Name"] != "terminated"]
        if ids:
            ec2.terminate_instances(InstanceIds=ids)
            ec2.get_waiter("instance_terminated").wait(InstanceIds=ids)
        for g in self._groups(L_ID, id):
            deadline = time.monotonic() + 300
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
