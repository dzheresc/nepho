// Package azure is the Azure backend: one resource group per machine, holding the VM and its network.
package azure

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v6"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v6"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/resources/armresources"

	"example.com/nepho"
)

const (
	user      = "azureuser"
	minDiskGB = 30 // size of the marketplace images; the OS disk cannot be smaller
)

var images = map[string]struct{ publisher, offer, sku string }{
	"ubuntu-24.04": {"Canonical", "ubuntu-24_04-lts", "server"},
	"ubuntu-22.04": {"Canonical", "0001-com-ubuntu-server-jammy", "22_04-lts-gen2"},
	"debian-12":    {"Debian", "debian-12", "12-gen2"},
}

var sz = nepho.NewSize

var sizes = []nepho.Size{
	sz(2, 1, "Standard_B2ts_v2"), sz(2, 4, "Standard_B2ls_v2"), sz(2, 8, "Standard_B2s_v2"),
	sz(4, 16, "Standard_B4s_v2"), sz(8, 32, "Standard_B8s_v2"), sz(16, 64, "Standard_D16s_v5"), sz(32, 128, "Standard_D32s_v5"),
}

type Options struct {
	SubscriptionID string // default: $AZURE_SUBSCRIPTION_ID
	Location       string // default eastus
	VMSize         string
	SSHCIDR        string // source range allowed to reach port 22; default: this host's public IP
	KeyPath        string
}

type driver struct {
	opts    Options
	groups  *armresources.ResourceGroupsClient
	nsgs    *armnetwork.SecurityGroupsClient
	vnets   *armnetwork.VirtualNetworksClient
	ips     *armnetwork.PublicIPAddressesClient
	nics    *armnetwork.InterfacesClient
	vms     *armcompute.VirtualMachinesClient
	keyPath string
	pub     string
}

// New returns the Azure provider. The resource group is named after the machine
// id and carries the labels as tags; deleting it removes everything. It uses
// DefaultAzureCredential.
//
// Nothing stops the VM by itself when the TTL passes; call Reap.
func New(opts Options) (*nepho.Provider, error) {
	fail := func(err error) (*nepho.Provider, error) {
		return nil, fmt.Errorf("%w: azure: %w", nepho.ErrProvision, err)
	}
	if opts.SubscriptionID == "" {
		opts.SubscriptionID = os.Getenv("AZURE_SUBSCRIPTION_ID")
	}
	if opts.SubscriptionID == "" {
		return fail(errors.New("set Options.SubscriptionID or AZURE_SUBSCRIPTION_ID"))
	}
	if opts.Location == "" {
		opts.Location = "eastus"
	}
	cred, err := azidentity.NewDefaultAzureCredential(nil)
	if err != nil {
		return fail(err)
	}
	d := &driver{opts: opts}
	if d.keyPath, d.pub, err = nepho.EnsureKey(opts.KeyPath); err != nil {
		return fail(err)
	}
	sub := opts.SubscriptionID
	if d.groups, err = armresources.NewResourceGroupsClient(sub, cred, nil); err != nil {
		return fail(err)
	}
	if d.nsgs, err = armnetwork.NewSecurityGroupsClient(sub, cred, nil); err != nil {
		return fail(err)
	}
	if d.vnets, err = armnetwork.NewVirtualNetworksClient(sub, cred, nil); err != nil {
		return fail(err)
	}
	if d.ips, err = armnetwork.NewPublicIPAddressesClient(sub, cred, nil); err != nil {
		return fail(err)
	}
	if d.nics, err = armnetwork.NewInterfacesClient(sub, cred, nil); err != nil {
		return fail(err)
	}
	if d.vms, err = armcompute.NewVirtualMachinesClient(sub, cred, nil); err != nil {
		return fail(err)
	}
	return nepho.NewProvider(d), nil
}

func (d *driver) Name() string { return "azure" }

func (d *driver) Launch(ctx context.Context, id string, spec nepho.Spec, labels map[string]string) (nepho.Info, error) {
	none := nepho.Info{}
	image, ok := images[spec.OS]
	if !ok {
		return none, fmt.Errorf("%w: azure: unsupported os %q", nepho.ErrProvision, spec.OS)
	}
	size := d.opts.VMSize
	if size == "" {
		var err error
		if size, err = nepho.PickSize(sizes, spec.CPUs, spec.MemoryGB); err != nil {
			return none, err
		}
	}
	cidr := d.opts.SSHCIDR
	if cidr == "" {
		var err error
		if cidr, err = nepho.MyCIDR(ctx); err != nil {
			return none, err
		}
	}
	loc := to.Ptr(d.opts.Location)
	tags := map[string]*string{}
	for k, v := range labels {
		tags[k] = to.Ptr(v)
	}
	if _, err := d.groups.CreateOrUpdate(ctx, id, armresources.ResourceGroup{Location: loc, Tags: tags}, nil); err != nil {
		return none, err
	}

	nsgPoll, err := d.nsgs.BeginCreateOrUpdate(ctx, id, "nsg", armnetwork.SecurityGroup{Location: loc,
		Properties: &armnetwork.SecurityGroupPropertiesFormat{SecurityRules: []*armnetwork.SecurityRule{{
			Name: to.Ptr("ssh"),
			Properties: &armnetwork.SecurityRulePropertiesFormat{
				Protocol:  to.Ptr(armnetwork.SecurityRuleProtocolTCP),
				Direction: to.Ptr(armnetwork.SecurityRuleDirectionInbound),
				Access:    to.Ptr(armnetwork.SecurityRuleAccessAllow), Priority: to.Ptr[int32](100),
				SourceAddressPrefix: to.Ptr(cidr), SourcePortRange: to.Ptr("*"),
				DestinationAddressPrefix: to.Ptr("*"), DestinationPortRange: to.Ptr("22")}}}}}, nil)
	if err != nil {
		return none, err
	}
	nsg, err := nsgPoll.PollUntilDone(ctx, nil)
	if err != nil {
		return none, err
	}

	vnetPoll, err := d.vnets.BeginCreateOrUpdate(ctx, id, "vnet", armnetwork.VirtualNetwork{Location: loc,
		Properties: &armnetwork.VirtualNetworkPropertiesFormat{
			AddressSpace: &armnetwork.AddressSpace{AddressPrefixes: []*string{to.Ptr("10.0.0.0/16")}},
			Subnets: []*armnetwork.Subnet{{Name: to.Ptr("default"),
				Properties: &armnetwork.SubnetPropertiesFormat{AddressPrefix: to.Ptr("10.0.0.0/24")}}}}}, nil)
	if err != nil {
		return none, err
	}
	vnet, err := vnetPoll.PollUntilDone(ctx, nil)
	if err != nil {
		return none, err
	}
	if vnet.Properties == nil || len(vnet.Properties.Subnets) == 0 {
		return none, errors.New("virtual network came back without a subnet")
	}

	ipPoll, err := d.ips.BeginCreateOrUpdate(ctx, id, "ip", armnetwork.PublicIPAddress{Location: loc,
		SKU: &armnetwork.PublicIPAddressSKU{Name: to.Ptr(armnetwork.PublicIPAddressSKUNameStandard)},
		Properties: &armnetwork.PublicIPAddressPropertiesFormat{
			PublicIPAllocationMethod: to.Ptr(armnetwork.IPAllocationMethodStatic)}}, nil)
	if err != nil {
		return none, err
	}
	ip, err := ipPoll.PollUntilDone(ctx, nil)
	if err != nil {
		return none, err
	}

	nicPoll, err := d.nics.BeginCreateOrUpdate(ctx, id, "nic", armnetwork.Interface{Location: loc,
		Properties: &armnetwork.InterfacePropertiesFormat{
			NetworkSecurityGroup: &armnetwork.SecurityGroup{ID: nsg.ID},
			IPConfigurations: []*armnetwork.InterfaceIPConfiguration{{Name: to.Ptr("ipconfig"),
				Properties: &armnetwork.InterfaceIPConfigurationPropertiesFormat{
					Subnet:          &armnetwork.Subnet{ID: vnet.Properties.Subnets[0].ID},
					PublicIPAddress: &armnetwork.PublicIPAddress{ID: ip.ID}}}}}}, nil)
	if err != nil {
		return none, err
	}
	nic, err := nicPoll.PollUntilDone(ctx, nil)
	if err != nil {
		return none, err
	}

	vmPoll, err := d.vms.BeginCreateOrUpdate(ctx, id, "vm", armcompute.VirtualMachine{Location: loc,
		Properties: &armcompute.VirtualMachineProperties{
			HardwareProfile: &armcompute.HardwareProfile{VMSize: to.Ptr(armcompute.VirtualMachineSizeTypes(size))},
			StorageProfile: &armcompute.StorageProfile{
				ImageReference: &armcompute.ImageReference{Publisher: to.Ptr(image.publisher),
					Offer: to.Ptr(image.offer), SKU: to.Ptr(image.sku), Version: to.Ptr("latest")},
				OSDisk: &armcompute.OSDisk{CreateOption: to.Ptr(armcompute.DiskCreateOptionTypesFromImage),
					DiskSizeGB: to.Ptr(int32(max(spec.DiskGB, minDiskGB)))}},
			OSProfile: &armcompute.OSProfile{ComputerName: to.Ptr(id), AdminUsername: to.Ptr(user),
				LinuxConfiguration: &armcompute.LinuxConfiguration{
					DisablePasswordAuthentication: to.Ptr(true),
					SSH: &armcompute.SSHConfiguration{PublicKeys: []*armcompute.SSHPublicKey{{
						Path:    to.Ptr("/home/" + user + "/.ssh/authorized_keys"),
						KeyData: to.Ptr(d.pub)}}}}},
			NetworkProfile: &armcompute.NetworkProfile{
				NetworkInterfaces: []*armcompute.NetworkInterfaceReference{{ID: nic.ID}}}}}, nil)
	if err != nil {
		return none, err
	}
	if _, err := vmPoll.PollUntilDone(ctx, nil); err != nil {
		return none, err
	}
	address := ""
	if ip.Properties != nil && ip.Properties.IPAddress != nil {
		address = *ip.Properties.IPAddress
	}
	return nepho.Info{ID: id, State: nepho.Running, Labels: labels, Address: address, User: user}, nil
}

func notFound(err error) bool {
	var re *azcore.ResponseError
	return errors.As(err, &re) && re.StatusCode == 404
}

func (d *driver) info(ctx context.Context, group *armresources.ResourceGroup) (nepho.Info, error) {
	name := *group.Name
	labels := map[string]string{}
	for k, v := range group.Tags {
		if v != nil {
			labels[k] = *v
		}
	}
	id := labels[nepho.LID]
	if id == "" {
		id = name
	}
	// Pending: the group exists but the VM does not (yet).
	out := nepho.Info{ID: id, State: nepho.Pending, Labels: labels, User: user}
	ip, err := d.ips.Get(ctx, name, "ip", nil)
	if notFound(err) {
		return out, nil
	} else if err != nil {
		return out, err
	}
	if ip.Properties != nil && ip.Properties.IPAddress != nil {
		out.Address = *ip.Properties.IPAddress
	}
	view, err := d.vms.InstanceView(ctx, name, "vm", nil)
	if notFound(err) {
		return out, nil
	} else if err != nil {
		return out, err
	}
	out.State = nepho.Stopped
	for _, s := range view.Statuses {
		if s.Code == nil || !strings.HasPrefix(*s.Code, "PowerState/") {
			continue
		}
		switch *s.Code {
		case "PowerState/running":
			out.State = nepho.Running
		case "PowerState/starting":
			out.State = nepho.Pending
		}
	}
	return out, nil
}

func (d *driver) List(ctx context.Context) ([]nepho.Info, error) {
	var out []nepho.Info
	pager := d.groups.NewListPager(&armresources.ResourceGroupsClientListOptions{
		Filter: to.Ptr("tagName eq '" + nepho.LManaged + "' and tagValue eq 'true'")})
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, g := range page.Value {
			i, err := d.info(ctx, g)
			if err != nil {
				return nil, err
			}
			out = append(out, i)
		}
	}
	return out, nil
}

func (d *driver) Destroy(ctx context.Context, id string) error {
	poll, err := d.groups.BeginDelete(ctx, id, nil)
	if err == nil {
		_, err = poll.PollUntilDone(ctx, nil)
	}
	if notFound(err) {
		return nil
	}
	return err
}

func (d *driver) Transport(info nepho.Info) nepho.Transport {
	return &nepho.SSHTransport{Host: info.Address, User: user, KeyPath: d.keyPath}
}

func (d *driver) Ready(ctx context.Context, m *nepho.Machine, _ nepho.Spec) error {
	return nepho.SSHReady(ctx, m)
}
