// Package aws is the AWS backend: one EC2 instance plus one security group per machine.
package aws

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	"github.com/aws/smithy-go"

	nepho "github.com/dzheresc/nepho/go"
)

func ubuntu(version string, gp int) string {
	return fmt.Sprintf("/aws/service/canonical/ubuntu/server/%s/stable/current/amd64/hvm/ebs-gp%d/ami-id", version, gp)
}

// os -> public SSM parameter holding the current AMI id, and the login user
var images = map[string]struct{ param, user string }{
	"ubuntu-24.04": {ubuntu("24.04", 3), "ubuntu"},
	"ubuntu-22.04": {ubuntu("22.04", 2), "ubuntu"},
	"debian-12":    {"/aws/service/debian/release/12/latest/amd64", "admin"},
}

var sz = nepho.NewSize

var sizes = []nepho.Size{
	sz(2, 1, "t3.micro"), sz(2, 2, "t3.small"), sz(2, 4, "t3.medium"), sz(2, 8, "t3.large"),
	sz(4, 16, "t3.xlarge"), sz(8, 32, "t3.2xlarge"), sz(16, 64, "m6i.4xlarge"),
	sz(32, 128, "m6i.8xlarge"), sz(64, 256, "m6i.16xlarge"),
}

var states = map[types.InstanceStateName]nepho.State{
	"pending": nepho.Pending, "running": nepho.Running, "stopping": nepho.Stopped,
	"stopped": nepho.Stopped, "shutting-down": nepho.Terminated, "terminated": nepho.Terminated,
}

type Options struct {
	Region       string
	InstanceType string
	SSHCIDR      string // source range allowed to reach port 22; default: this host's public IP
	KeyPath      string
}

type driver struct {
	opts    Options
	ec2     *ec2.Client
	ssm     *ssm.Client
	keyPath string
	pub     string
}

// New returns the AWS provider. It uses the default VPC of the region and the
// SDK's default credential chain.
//
// When the TTL passes the instance shuts itself down, which terminates it; Reap
// then removes the leftover security group. EC2 keeps listing a terminated
// instance for about an hour.
func New(ctx context.Context, opts Options) (*nepho.Provider, error) {
	var load []func(*config.LoadOptions) error
	if opts.Region != "" {
		load = append(load, config.WithRegion(opts.Region))
	}
	cfg, err := config.LoadDefaultConfig(ctx, load...)
	if err != nil {
		return nil, fmt.Errorf("%w: aws: %w", nepho.ErrProvision, err)
	}
	keyPath, pub, err := nepho.EnsureKey(opts.KeyPath)
	if err != nil {
		return nil, fmt.Errorf("%w: aws: %w", nepho.ErrProvision, err)
	}
	return nepho.NewProvider(&driver{opts, ec2.NewFromConfig(cfg), ssm.NewFromConfig(cfg), keyPath, pub}), nil
}

func (d *driver) Name() string { return "aws" }

func fromTags(tags []types.Tag) map[string]string {
	out := map[string]string{}
	for _, t := range tags {
		out[awssdk.ToString(t.Key)] = awssdk.ToString(t.Value)
	}
	return out
}

func tagFilter(key, value string) []types.Filter {
	return []types.Filter{{Name: awssdk.String("tag:" + key), Values: []string{value}}}
}

func (d *driver) Launch(ctx context.Context, id string, spec nepho.Spec, base map[string]string) (nepho.Info, error) {
	none := nepho.Info{}
	image, ok := images[spec.OS]
	if !ok {
		return none, fmt.Errorf("%w: aws: unsupported os %q", nepho.ErrProvision, spec.OS)
	}
	itype := d.opts.InstanceType
	if itype == "" {
		var err error
		if itype, err = nepho.PickSize(sizes, spec.CPUs, spec.MemoryGB); err != nil {
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
	param, err := d.ssm.GetParameter(ctx, &ssm.GetParameterInput{Name: awssdk.String(image.param)})
	if err != nil {
		return none, err
	}
	ami := param.Parameter.Value
	described, err := d.ec2.DescribeImages(ctx, &ec2.DescribeImagesInput{ImageIds: []string{*ami}})
	if err != nil {
		return none, err
	}
	if len(described.Images) == 0 {
		return none, fmt.Errorf("image %s not found", *ami)
	}
	tags := []types.Tag{{Key: awssdk.String("Name"), Value: awssdk.String(id)},
		{Key: awssdk.String(nepho.LUser), Value: awssdk.String(image.user)}}
	for k, v := range base {
		tags = append(tags, types.Tag{Key: awssdk.String(k), Value: awssdk.String(v)})
	}
	tagged := func(kinds ...types.ResourceType) (out []types.TagSpecification) {
		for _, k := range kinds {
			out = append(out, types.TagSpecification{ResourceType: k, Tags: tags})
		}
		return out
	}

	sg, err := d.ec2.CreateSecurityGroup(ctx, &ec2.CreateSecurityGroupInput{
		GroupName: awssdk.String(id), Description: awssdk.String("nepho " + id),
		TagSpecifications: tagged(types.ResourceTypeSecurityGroup)})
	if err != nil {
		return none, err
	}
	_, err = d.ec2.AuthorizeSecurityGroupIngress(ctx, &ec2.AuthorizeSecurityGroupIngressInput{
		GroupId: sg.GroupId, IpProtocol: awssdk.String("tcp"), FromPort: awssdk.Int32(22),
		ToPort: awssdk.Int32(22), CidrIp: awssdk.String(cidr)})
	if err != nil {
		return none, err
	}
	userData := "#cloud-config\nssh_authorized_keys:\n  - " + d.pub + "\n"
	run, err := d.ec2.RunInstances(ctx, &ec2.RunInstancesInput{
		ImageId: ami, InstanceType: types.InstanceType(itype),
		MinCount: awssdk.Int32(1), MaxCount: awssdk.Int32(1),
		SecurityGroupIds:                  []string{*sg.GroupId},
		UserData:                          awssdk.String(base64.StdEncoding.EncodeToString([]byte(userData))),
		InstanceInitiatedShutdownBehavior: types.ShutdownBehaviorTerminate,
		BlockDeviceMappings: []types.BlockDeviceMapping{{
			DeviceName: described.Images[0].RootDeviceName,
			Ebs: &types.EbsBlockDevice{VolumeSize: awssdk.Int32(int32(spec.DiskGB)),
				VolumeType: types.VolumeTypeGp3, DeleteOnTermination: awssdk.Bool(true)}}},
		TagSpecifications: tagged(types.ResourceTypeInstance, types.ResourceTypeVolume)})
	if err != nil {
		return none, err
	}
	iid := *run.Instances[0].InstanceId
	err = ec2.NewInstanceRunningWaiter(d.ec2).Wait(ctx,
		&ec2.DescribeInstancesInput{InstanceIds: []string{iid}}, 10*time.Minute)
	if err != nil {
		return none, err
	}
	infos, err := d.List(ctx)
	if err != nil {
		return none, err
	}
	for _, i := range infos {
		if i.ID == id {
			return i, nil
		}
	}
	return none, fmt.Errorf("instance %s disappeared", iid)
}

func (d *driver) instances(ctx context.Context, key, value string) ([]types.Instance, error) {
	var out []types.Instance
	pages := ec2.NewDescribeInstancesPaginator(d.ec2, &ec2.DescribeInstancesInput{Filters: tagFilter(key, value)})
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, r := range page.Reservations {
			out = append(out, r.Instances...)
		}
	}
	return out, nil
}

func (d *driver) List(ctx context.Context) ([]nepho.Info, error) {
	found, err := d.instances(ctx, nepho.LManaged, "true")
	if err != nil {
		return nil, err
	}
	out := make([]nepho.Info, 0, len(found))
	for _, i := range found {
		labels := fromTags(i.Tags)
		state := nepho.Stopped
		if i.State != nil {
			if s, ok := states[i.State.Name]; ok {
				state = s
			}
		}
		out = append(out, nepho.Info{ID: labels[nepho.LID], State: state, Labels: labels,
			Address: awssdk.ToString(i.PublicIpAddress), User: labels[nepho.LUser]})
	}
	return out, nil
}

func (d *driver) groups(ctx context.Context, key, value string) ([]types.SecurityGroup, error) {
	r, err := d.ec2.DescribeSecurityGroups(ctx, &ec2.DescribeSecurityGroupsInput{Filters: tagFilter(key, value)})
	if err != nil {
		return nil, err
	}
	return r.SecurityGroups, nil
}

func (d *driver) Destroy(ctx context.Context, id string) error {
	if strings.HasPrefix(id, "s-") {
		return fmt.Errorf("%w: aws: %s is a service; only the Python version can destroy it", nepho.ErrProvision, id)
	}
	found, err := d.instances(ctx, nepho.LID, id)
	if err != nil {
		return err
	}
	var ids []string
	for _, i := range found {
		if i.State == nil || i.State.Name != types.InstanceStateNameTerminated {
			ids = append(ids, *i.InstanceId)
		}
	}
	if len(ids) > 0 {
		if _, err := d.ec2.TerminateInstances(ctx, &ec2.TerminateInstancesInput{InstanceIds: ids}); err != nil {
			return err
		}
		err := ec2.NewInstanceTerminatedWaiter(d.ec2).Wait(ctx,
			&ec2.DescribeInstancesInput{InstanceIds: ids}, 10*time.Minute)
		if err != nil {
			return err
		}
	}
	groups, err := d.groups(ctx, nepho.LID, id)
	if err != nil {
		return err
	}
	for _, g := range groups {
		deadline := time.Now().Add(2 * time.Minute)
		for { // the group stays "in use" for a moment after termination
			_, err := d.ec2.DeleteSecurityGroup(ctx, &ec2.DeleteSecurityGroupInput{GroupId: g.GroupId})
			if err == nil {
				break
			}
			var api smithy.APIError
			if !errors.As(err, &api) || api.ErrorCode() != "DependencyViolation" || time.Now().After(deadline) {
				return err
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(5 * time.Second):
			}
		}
	}
	return nil
}

// Reapable lists expired machines by their security groups: every machine owns one
// until it is destroyed, including instances that already terminated themselves.
// Service groups (created by the Python version) are skipped: this driver cannot
// delete the database behind them.
func (d *driver) Reapable(ctx context.Context) ([]string, error) {
	groups, err := d.groups(ctx, nepho.LManaged, "true")
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []string
	for _, g := range groups {
		labels := fromTags(g.Tags)
		if _, service := labels[nepho.LKind]; service {
			continue
		}
		id, expires := labels[nepho.LID], nepho.ExpiresOf(labels)
		if id != "" && !seen[id] && !expires.IsZero() && expires.Before(time.Now()) {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out, nil
}

func (d *driver) Transport(info nepho.Info) nepho.Transport {
	user := info.User
	if user == "" {
		user = "ubuntu"
	}
	return &nepho.SSHTransport{Host: info.Address, User: user, KeyPath: d.keyPath}
}

func (d *driver) Ready(ctx context.Context, m *nepho.Machine, spec nepho.Spec) error {
	if err := nepho.SSHReady(ctx, m); err != nil {
		return err
	}
	minutes := spec.TTLMinutes()
	if minutes == 0 {
		return nil
	}
	r, err := m.Run(ctx, []string{"shutdown", "-h", fmt.Sprintf("+%d", minutes)}, "", &nepho.ExecOptions{User: "root"})
	if err == nil && r.Code != 0 {
		err = fmt.Errorf("%w: aws: cannot schedule the TTL shutdown: %s", nepho.ErrProvision, r.Stderr)
	}
	return err
}
