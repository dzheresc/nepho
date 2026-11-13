"""GCP backend: one Compute Engine instance plus one firewall rule per machine."""
from __future__ import annotations

import google.auth
from google.api_core.exceptions import NotFound
from google.cloud import compute_v1

from .core import (L_ID, L_MANAGED, PENDING, RUNNING, STOPPED, Info, Machine, MachineSpec,
                   Provider, ProvisionError, my_cidr, pick_size)
from .ssh import SshTransport, ensure_key, ssh_ready

USER = "nepho"
IMAGES = {
    "ubuntu-24.04": "projects/ubuntu-os-cloud/global/images/family/ubuntu-2404-lts-amd64",
    "ubuntu-22.04": "projects/ubuntu-os-cloud/global/images/family/ubuntu-2204-lts",
    "debian-12": "projects/debian-cloud/global/images/family/debian-12",
}
SIZES = [(2, 1, "e2-micro"), (2, 2, "e2-small"), (2, 4, "e2-medium"), (2, 8, "e2-standard-2"),
         (4, 16, "e2-standard-4"), (8, 32, "e2-standard-8"), (16, 64, "e2-standard-16"),
         (32, 128, "e2-standard-32")]
STATES = {"PROVISIONING": PENDING, "STAGING": PENDING, "RUNNING": RUNNING}


class GcpProvider(Provider):
    """Uses the `default` network and Application Default Credentials.

    Nothing stops the instance by itself when the TTL passes; call reap().
    Label keys and values must be lowercase letters, digits, '-' or '_'.
    """

    name = "gcp"

    def __init__(self, *, project: str | None = None, zone: str = "us-central1-a",
                 machine_type: str | None = None, ssh_cidr: str | None = None,
                 key_path: str | None = None):
        self._project = project or google.auth.default()[1]
        self._zone, self._machine_type, self._ssh_cidr = zone, machine_type, ssh_cidr
        self._key_path, self._pub = ensure_key(key_path)
        self._vms = compute_v1.InstancesClient()
        self._fw = compute_v1.FirewallsClient()

    def _launch(self, id: str, spec: MachineSpec, labels: dict[str, str]) -> Info:
        if spec.os not in IMAGES:
            raise ProvisionError(f"gcp: unsupported os {spec.os!r}")
        mtype = self._machine_type or pick_size(SIZES, spec.cpus, spec.memory_gb)
        # The network tag `id` ties the firewall rule to this one instance.
        self._fw.insert(project=self._project, firewall_resource=compute_v1.Firewall(
            name=id, description="nepho-managed", network="global/networks/default",
            direction="INGRESS", source_ranges=[self._ssh_cidr or my_cidr()], target_tags=[id],
            allowed=[compute_v1.Allowed(I_p_protocol="tcp", ports=["22"])])).result(timeout=300)
        metadata = {"ssh-keys": f"{USER}:{self._pub} {USER}", "enable-oslogin": "FALSE"}
        self._vms.insert(project=self._project, zone=self._zone, instance_resource=compute_v1.Instance(
            name=id, labels=labels, tags=compute_v1.Tags(items=[id]),
            machine_type=f"zones/{self._zone}/machineTypes/{mtype}",
            disks=[compute_v1.AttachedDisk(
                boot=True, auto_delete=True,
                initialize_params=compute_v1.AttachedDiskInitializeParams(
                    source_image=IMAGES[spec.os], disk_size_gb=spec.disk_gb))],
            network_interfaces=[compute_v1.NetworkInterface(
                network="global/networks/default",
                access_configs=[compute_v1.AccessConfig(name="External NAT", type_="ONE_TO_ONE_NAT")])],
            metadata=compute_v1.Metadata(
                items=[compute_v1.Items(key=k, value=v) for k, v in metadata.items()]),
        )).result(timeout=600)
        return self._info(self._vms.get(project=self._project, zone=self._zone, instance=id))

    @staticmethod
    def _info(vm: "compute_v1.Instance") -> Info:
        address = None
        for nic in vm.network_interfaces:
            for ac in nic.access_configs:
                address = address or ac.nat_i_p or None
        labels = dict(vm.labels)
        return Info(labels.get(L_ID, vm.name), STATES.get(vm.status, STOPPED), labels, address, USER)

    def _list(self) -> list[Info]:
        return [self._info(vm) for vm in self._vms.list(project=self._project, zone=self._zone)
                if vm.labels.get(L_MANAGED) == "true"]

    def _destroy(self, id: str) -> None:
        for delete in (lambda: self._vms.delete(project=self._project, zone=self._zone, instance=id),
                       lambda: self._fw.delete(project=self._project, firewall=id)):
            try:
                delete().result(timeout=600)
            except NotFound:
                pass

    def _transport(self, info: Info) -> SshTransport:
        return SshTransport(info.address, USER, self._key_path)

    async def _ready(self, m: Machine, spec: MachineSpec) -> None:
        await ssh_ready(m)
