// Package gcp is the GCP backend: one Compute Engine instance plus one firewall rule per machine.
package gcp

import (
	"context"
	"errors"
	"fmt"

	compute "cloud.google.com/go/compute/apiv1"
	"cloud.google.com/go/compute/apiv1/computepb"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/iterator"
	"google.golang.org/protobuf/proto"

	"example.com/nepho"
)

const user = "nepho"

var images = map[string]string{
	"ubuntu-24.04": "projects/ubuntu-os-cloud/global/images/family/ubuntu-2404-lts-amd64",
	"ubuntu-22.04": "projects/ubuntu-os-cloud/global/images/family/ubuntu-2204-lts",
	"debian-12":    "projects/debian-cloud/global/images/family/debian-12",
}

var sz = nepho.NewSize

var sizes = []nepho.Size{
	sz(2, 1, "e2-micro"), sz(2, 2, "e2-small"), sz(2, 4, "e2-medium"), sz(2, 8, "e2-standard-2"),
	sz(4, 16, "e2-standard-4"), sz(8, 32, "e2-standard-8"), sz(16, 64, "e2-standard-16"),
	sz(32, 128, "e2-standard-32"),
}

var states = map[string]nepho.State{
	"PROVISIONING": nepho.Pending, "STAGING": nepho.Pending, "RUNNING": nepho.Running,
}

type Options struct {
	Project     string // default: the project of the Application Default Credentials
	Zone        string // default us-central1-a
	MachineType string
	SSHCIDR     string // source range allowed to reach port 22; default: this host's public IP
	KeyPath     string
}

type driver struct {
	opts    Options
	vms     *compute.InstancesClient
	fw      *compute.FirewallsClient
	keyPath string
	pub     string
}

// New returns the GCP provider. It uses the `default` network and Application
// Default Credentials.
//
// Nothing stops the instance by itself when the TTL passes; call Reap. Label
// keys and values must be lowercase letters, digits, '-' or '_'.
func New(ctx context.Context, opts Options) (*nepho.Provider, error) {
	fail := func(err error) (*nepho.Provider, error) {
		return nil, fmt.Errorf("%w: gcp: %w", nepho.ErrProvision, err)
	}
	if opts.Zone == "" {
		opts.Zone = "us-central1-a"
	}
	if opts.Project == "" {
		creds, err := google.FindDefaultCredentials(ctx, compute.DefaultAuthScopes()...)
		if err != nil {
			return fail(err)
		}
		if opts.Project = creds.ProjectID; opts.Project == "" {
			return fail(errors.New("set Options.Project: the credentials name no project"))
		}
	}
	keyPath, pub, err := nepho.EnsureKey(opts.KeyPath)
	if err != nil {
		return fail(err)
	}
	vms, err := compute.NewInstancesRESTClient(ctx)
	if err != nil {
		return fail(err)
	}
	fw, err := compute.NewFirewallsRESTClient(ctx)
	if err != nil {
		return fail(err)
	}
	return nepho.NewProvider(&driver{opts, vms, fw, keyPath, pub}), nil
}

func (d *driver) Name() string { return "gcp" }

func (d *driver) Launch(ctx context.Context, id string, spec nepho.Spec, labels map[string]string) (nepho.Info, error) {
	none := nepho.Info{}
	image, ok := images[spec.OS]
	if !ok {
		return none, fmt.Errorf("%w: gcp: unsupported os %q", nepho.ErrProvision, spec.OS)
	}
	mtype := d.opts.MachineType
	if mtype == "" {
		var err error
		if mtype, err = nepho.PickSize(sizes, spec.CPUs, spec.MemoryGB); err != nil {
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
	project, zone := d.opts.Project, d.opts.Zone
	// The network tag `id` ties the firewall rule to this one instance.
	fwOp, err := d.fw.Insert(ctx, &computepb.InsertFirewallRequest{Project: project,
		FirewallResource: &computepb.Firewall{
			Name: proto.String(id), Description: proto.String("nepho-managed"),
			Network: proto.String("global/networks/default"), Direction: proto.String("INGRESS"),
			SourceRanges: []string{cidr}, TargetTags: []string{id},
			Allowed: []*computepb.Allowed{{IPProtocol: proto.String("tcp"), Ports: []string{"22"}}}}})
	if err != nil {
		return none, err
	}
	if err := fwOp.Wait(ctx); err != nil {
		return none, err
	}
	vmOp, err := d.vms.Insert(ctx, &computepb.InsertInstanceRequest{Project: project, Zone: zone,
		InstanceResource: &computepb.Instance{
			Name: proto.String(id), Labels: labels, Tags: &computepb.Tags{Items: []string{id}},
			MachineType: proto.String("zones/" + zone + "/machineTypes/" + mtype),
			Disks: []*computepb.AttachedDisk{{
				Boot: proto.Bool(true), AutoDelete: proto.Bool(true),
				InitializeParams: &computepb.AttachedDiskInitializeParams{
					SourceImage: proto.String(image), DiskSizeGb: proto.Int64(int64(spec.DiskGB))}}},
			NetworkInterfaces: []*computepb.NetworkInterface{{
				Network: proto.String("global/networks/default"),
				AccessConfigs: []*computepb.AccessConfig{{
					Name: proto.String("External NAT"), Type: proto.String("ONE_TO_ONE_NAT")}}}},
			Metadata: &computepb.Metadata{Items: []*computepb.Items{
				{Key: proto.String("ssh-keys"), Value: proto.String(user + ":" + d.pub + " " + user)},
				{Key: proto.String("enable-oslogin"), Value: proto.String("FALSE")}}}}})
	if err != nil {
		return none, err
	}
	if err := vmOp.Wait(ctx); err != nil {
		return none, err
	}
	vm, err := d.vms.Get(ctx, &computepb.GetInstanceRequest{Project: project, Zone: zone, Instance: id})
	if err != nil {
		return none, err
	}
	return info(vm), nil
}

func info(vm *computepb.Instance) nepho.Info {
	address := ""
	for _, nic := range vm.GetNetworkInterfaces() {
		for _, ac := range nic.GetAccessConfigs() {
			if address == "" {
				address = ac.GetNatIP()
			}
		}
	}
	labels := vm.GetLabels()
	id := labels[nepho.LID]
	if id == "" {
		id = vm.GetName()
	}
	state, ok := states[vm.GetStatus()]
	if !ok {
		state = nepho.Stopped
	}
	return nepho.Info{ID: id, State: state, Labels: labels, Address: address, User: user}
}

func (d *driver) List(ctx context.Context) ([]nepho.Info, error) {
	var out []nepho.Info
	it := d.vms.List(ctx, &computepb.ListInstancesRequest{Project: d.opts.Project, Zone: d.opts.Zone})
	for {
		vm, err := it.Next()
		if errors.Is(err, iterator.Done) {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		if vm.GetLabels()[nepho.LManaged] == "true" {
			out = append(out, info(vm))
		}
	}
}

func notFound(err error) bool {
	var api *googleapi.Error
	return errors.As(err, &api) && api.Code == 404
}

func (d *driver) Destroy(ctx context.Context, id string) error {
	project, zone := d.opts.Project, d.opts.Zone
	vmOp, err := d.vms.Delete(ctx, &computepb.DeleteInstanceRequest{Project: project, Zone: zone, Instance: id})
	if err == nil {
		err = vmOp.Wait(ctx)
	}
	if err != nil && !notFound(err) {
		return err
	}
	fwOp, err := d.fw.Delete(ctx, &computepb.DeleteFirewallRequest{Project: project, Firewall: id})
	if err == nil {
		err = fwOp.Wait(ctx)
	}
	if err != nil && !notFound(err) {
		return err
	}
	return nil
}

func (d *driver) Transport(info nepho.Info) nepho.Transport {
	return &nepho.SSHTransport{Host: info.Address, User: user, KeyPath: d.keyPath}
}

func (d *driver) Ready(ctx context.Context, m *nepho.Machine, _ nepho.Spec) error {
	return nepho.SSHReady(ctx, m)
}
