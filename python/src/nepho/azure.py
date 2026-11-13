"""Azure backend: one resource group per machine, holding the VM and its network."""
from __future__ import annotations

import os

from azure.core.exceptions import ResourceNotFoundError
from azure.identity import DefaultAzureCredential
from azure.mgmt.compute import ComputeManagementClient
from azure.mgmt.network import NetworkManagementClient
from azure.mgmt.resource.resources import ResourceManagementClient

from .core import (L_ID, L_MANAGED, PENDING, RUNNING, STOPPED, Info, Machine, MachineSpec,
                   Provider, ProvisionError, my_cidr, pick_size)
from .ssh import SshTransport, ensure_key, ssh_ready

USER = "azureuser"
# os -> (publisher, offer, sku)
IMAGES = {
    "ubuntu-24.04": ("Canonical", "ubuntu-24_04-lts", "server"),
    "ubuntu-22.04": ("Canonical", "0001-com-ubuntu-server-jammy", "22_04-lts-gen2"),
    "debian-12": ("Debian", "debian-12", "12-gen2"),
}
# The B-series is not offered to many new subscriptions (capacity restrictions, zero quota).
SIZES = [(2, 4, "Standard_D2als_v6"), (2, 8, "Standard_D2as_v6"), (4, 16, "Standard_D4as_v6"),
         (8, 32, "Standard_D8as_v6"), (16, 64, "Standard_D16as_v6"), (32, 128, "Standard_D32as_v6")]
MIN_DISK_GB = 30  # size of the marketplace images; the OS disk cannot be smaller


class AzureProvider(Provider):
    """The resource group is named after the machine id and carries the labels as tags;
    deleting it removes everything. Uses DefaultAzureCredential.

    Nothing stops the VM by itself when the TTL passes; call reap().
    """

    name = "azure"

    def __init__(self, *, subscription_id: str | None = None, location: str = "eastus",
                 vm_size: str | None = None, ssh_cidr: str | None = None,
                 key_path: str | None = None):
        sub = subscription_id or os.environ.get("AZURE_SUBSCRIPTION_ID")
        if not sub:
            raise ProvisionError("azure: pass subscription_id or set AZURE_SUBSCRIPTION_ID")
        cred = DefaultAzureCredential()
        self._res = ResourceManagementClient(cred, sub)
        self._net = NetworkManagementClient(cred, sub)
        self._vms = ComputeManagementClient(cred, sub)
        self._location, self._vm_size, self._ssh_cidr = location, vm_size, ssh_cidr
        self._key_path, self._pub = ensure_key(key_path)

    def _launch(self, id: str, spec: MachineSpec, labels: dict[str, str]) -> Info:
        if spec.os not in IMAGES:
            raise ProvisionError(f"azure: unsupported os {spec.os!r}")
        publisher, offer, sku = IMAGES[spec.os]
        size = self._vm_size or pick_size(SIZES, spec.cpus, spec.memory_gb)
        loc, net = self._location, self._net
        self._res.resource_groups.create_or_update(id, {"location": loc, "tags": labels})
        # Request bodies are plain dicts in the REST wire format: the SDK sends them unchanged.
        nsg = net.network_security_groups.begin_create_or_update(id, "nsg", {
            "location": loc, "properties": {"securityRules": [{"name": "ssh", "properties": {
                "protocol": "Tcp", "direction": "Inbound", "access": "Allow", "priority": 100,
                "sourceAddressPrefix": self._ssh_cidr or my_cidr(), "sourcePortRange": "*",
                "destinationAddressPrefix": "*", "destinationPortRange": "22"}}]}}).result()
        vnet = net.virtual_networks.begin_create_or_update(id, "vnet", {
            "location": loc, "properties": {
                "addressSpace": {"addressPrefixes": ["10.0.0.0/16"]},
                "subnets": [{"name": "default",
                             "properties": {"addressPrefix": "10.0.0.0/24"}}]}}).result()
        ip = net.public_ip_addresses.begin_create_or_update(id, "ip", {
            "location": loc, "sku": {"name": "Standard"},
            "properties": {"publicIPAllocationMethod": "Static"}}).result()
        nic = net.network_interfaces.begin_create_or_update(id, "nic", {
            "location": loc, "properties": {
                "networkSecurityGroup": {"id": nsg.id},
                "ipConfigurations": [{"name": "ipconfig", "properties": {
                    "subnet": {"id": vnet.subnets[0].id},
                    "publicIPAddress": {"id": ip.id}}}]}}).result()
        self._vms.virtual_machines.begin_create_or_update(id, "vm", {
            "location": loc, "properties": {
                "hardwareProfile": {"vmSize": size},
                "storageProfile": {
                    "imageReference": {"publisher": publisher, "offer": offer, "sku": sku,
                                       "version": "latest"},
                    "osDisk": {"createOption": "FromImage",
                               "diskSizeGB": max(spec.disk_gb, MIN_DISK_GB)}},
                "osProfile": {
                    "computerName": id, "adminUsername": USER,
                    "linuxConfiguration": {
                        "disablePasswordAuthentication": True,
                        "ssh": {"publicKeys": [{"path": f"/home/{USER}/.ssh/authorized_keys",
                                                "keyData": self._pub}]}}},
                "networkProfile": {"networkInterfaces": [{"id": nic.id}]}}}).result()
        return Info(id, RUNNING, labels, ip.ip_address, USER)

    def _info(self, group) -> Info:
        labels = dict(group.tags or {})
        address, state = None, PENDING  # pending: the group exists but the VM does not (yet)
        try:
            address = self._net.public_ip_addresses.get(group.name, "ip").ip_address
            view = self._vms.virtual_machines.instance_view(group.name, "vm")
            power = [s.code for s in view.statuses or [] if (s.code or "").startswith("PowerState/")]
            state = {"PowerState/running": RUNNING, "PowerState/starting": PENDING}.get(
                power[0] if power else "", STOPPED)
        except ResourceNotFoundError:
            pass
        return Info(labels.get(L_ID, group.name), state, labels, address, USER)

    def _list(self) -> list[Info]:
        groups = self._res.resource_groups.list(
            filter=f"tagName eq '{L_MANAGED}' and tagValue eq 'true'")
        return [self._info(g) for g in groups]

    def _destroy(self, id: str) -> None:
        try:
            self._res.resource_groups.begin_delete(id).result()
        except ResourceNotFoundError:
            pass

    def _transport(self, info: Info) -> SshTransport:
        return SshTransport(info.address, USER, self._key_path)

    async def _ready(self, m: Machine, spec: MachineSpec) -> None:
        await ssh_ready(m)
