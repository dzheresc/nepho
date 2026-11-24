// Package docker is the Docker backend: a long-lived container stands in for a VM;
// services are the official images of the open-source servers.
package docker

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"strconv"
	"sync"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/docker/go-connections/nat"

	nepho "github.com/dzheresc/nepho/go"
)

var images = map[string]string{
	"ubuntu-24.04": "ubuntu:24.04", "ubuntu-22.04": "ubuntu:22.04", "debian-12": "debian:12",
}

var states = map[string]nepho.State{
	"running": nepho.Running, "created": nepho.Pending, "restarting": nepho.Pending,
}

type Options struct {
	CpusetCpus string
	CpusetMems string
	Client     *client.Client // default: from the environment
}

type driver struct {
	opts Options
	cli  *client.Client
}

// New returns the Docker provider.
//
// Spec.DiskGB is ignored. When the TTL passes a machine's container exits by
// itself (state "stopped"); Reap removes it.
//
// A service is published on a random port of 127.0.0.1 (Host/Port) and is
// reachable from other containers at its bridge address (InternalHost). Its
// data lives in the container and goes away with it. Nothing stops a service
// at the TTL; Reap removes it.
func New(opts Options) (*nepho.Provider, error) {
	cli := opts.Client
	if cli == nil {
		var err error
		cli, err = client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
		if err != nil {
			return nil, fmt.Errorf("%w: docker: %w", nepho.ErrProvision, err)
		}
	}
	return nepho.NewProvider(&driver{opts, cli}), nil
}

func (d *driver) Name() string { return "docker" }

func (d *driver) Launch(ctx context.Context, id string, spec nepho.Spec, labels map[string]string) (nepho.Info, error) {
	ref, ok := images[spec.OS]
	if !ok {
		return nepho.Info{}, fmt.Errorf("%w: docker: unsupported os %q", nepho.ErrProvision, spec.OS)
	}
	life := "infinity"
	if spec.TTL > 0 {
		life = strconv.Itoa(max(1, int(spec.TTL/time.Second)))
	}
	err := d.run(ctx, id, &container.Config{Image: ref, Cmd: []string{"sleep", life}, Labels: labels},
		&container.HostConfig{}, spec.CPUs, spec.MemoryGB)
	if err != nil {
		return nepho.Info{}, err
	}
	return nepho.Info{ID: id, State: nepho.Running, Labels: labels}, nil
}

// run creates and starts a container named id, pulling the image when needed.
func (d *driver) run(ctx context.Context, id string, cfg *container.Config, host *container.HostConfig, cpus, memoryGB int) error {
	init := true
	host.Init = &init
	host.Resources = container.Resources{NanoCPUs: int64(cpus) * 1e9, Memory: int64(memoryGB) << 30,
		CpusetCpus: d.opts.CpusetCpus, CpusetMems: d.opts.CpusetMems}
	_, err := d.cli.ContainerCreate(ctx, cfg, host, nil, nil, id)
	if client.IsErrNotFound(err) { // image not present locally
		rc, perr := d.cli.ImagePull(ctx, cfg.Image, image.PullOptions{})
		if perr != nil {
			return perr
		}
		_, _ = io.Copy(io.Discard, rc)
		rc.Close()
		_, err = d.cli.ContainerCreate(ctx, cfg, host, nil, nil, id)
	}
	if err != nil {
		return err
	}
	return d.cli.ContainerStart(ctx, id, container.StartOptions{})
}

var serviceImages = map[string]string{"postgres": "postgres:", "valkey": "valkey/valkey:"}

func (d *driver) LaunchService(ctx context.Context, id string, spec nepho.ServiceSpec, labels map[string]string) (nepho.ServiceInfo, error) {
	none := nepho.ServiceInfo{}
	kind := nepho.Services[spec.Kind]
	secret := make([]byte, 12)
	_, _ = rand.Read(secret)
	password := hex.EncodeToString(secret)
	labels[nepho.LSecret] = password
	port := nat.Port(fmt.Sprintf("%d/tcp", kind.Port))
	cfg := &container.Config{Image: serviceImages[spec.Kind] + spec.Version, Labels: labels,
		ExposedPorts: nat.PortSet{port: {}}}
	var probe []string
	if spec.Kind == "postgres" {
		cfg.Env = []string{"POSTGRES_USER=" + kind.User, "POSTGRES_PASSWORD=" + password, "POSTGRES_DB=" + kind.Database}
		// Over TCP: during first-time setup the server listens on its socket only.
		probe = []string{"pg_isready", "-h", "127.0.0.1", "-U", kind.User, "-d", kind.Database}
	} else {
		cfg.Cmd = []string{"valkey-server", "--requirepass", password}
		probe = []string{"valkey-cli", "-a", password, "--no-auth-warning", "ping"}
	}
	host := &container.HostConfig{PortBindings: nat.PortMap{port: {{HostIP: "127.0.0.1"}}}}
	if err := d.run(ctx, id, cfg, host, spec.CPUs, spec.MemoryGB); err != nil {
		return none, err
	}
	for deadline := time.Now().Add(2 * time.Minute); ; time.Sleep(300 * time.Millisecond) {
		code, err := d.exec(ctx, id, probe)
		if err != nil {
			return none, err
		}
		if code == 0 {
			break
		}
		state, err := d.cli.ContainerInspect(ctx, id)
		if err != nil {
			return none, err
		}
		if !state.State.Running || time.Now().After(deadline) {
			var tail bytes.Buffer
			if rc, err := d.cli.ContainerLogs(ctx, id, container.LogsOptions{ShowStdout: true, ShowStderr: true, Tail: "20"}); err == nil {
				_, _ = stdcopy.StdCopy(&tail, &tail, rc)
				rc.Close()
			}
			return none, fmt.Errorf("%w: docker: %s did not start:\n%s", nepho.ErrProvision, spec.Kind, tail.String())
		}
	}
	services, err := d.ListServices(ctx)
	if err != nil {
		return none, err
	}
	for _, s := range services {
		if s.ID == id {
			return s, nil
		}
	}
	return none, fmt.Errorf("%w: docker: service %s vanished", nepho.ErrProvision, id)
}

// exec runs a command in a container without streams and returns its exit code.
func (d *driver) exec(ctx context.Context, id string, argv []string) (int, error) {
	created, err := d.cli.ContainerExecCreate(ctx, id, container.ExecOptions{Cmd: argv})
	if err != nil {
		return -1, err
	}
	if err := d.cli.ContainerExecStart(ctx, created.ID, container.ExecStartOptions{Detach: true}); err != nil {
		return -1, err
	}
	for {
		state, err := d.cli.ContainerExecInspect(ctx, created.ID)
		if err != nil {
			return -1, err
		}
		if !state.Running {
			return state.ExitCode, nil
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func (d *driver) ListServices(ctx context.Context) ([]nepho.ServiceInfo, error) {
	found, err := d.cli.ContainerList(ctx, container.ListOptions{All: true, Filters: filters.NewArgs(
		filters.Arg("label", nepho.LManaged+"=true"), filters.Arg("label", nepho.LKind))})
	if err != nil {
		return nil, err
	}
	out := make([]nepho.ServiceInfo, 0, len(found))
	for _, c := range found {
		kind := nepho.Services[c.Labels[nepho.LKind]]
		state, ok := states[string(c.State)]
		if !ok {
			state = nepho.Stopped
		}
		s := nepho.ServiceInfo{ID: c.Labels[nepho.LID], Kind: c.Labels[nepho.LKind], Version: c.Labels[nepho.LVersion],
			State: state, Labels: c.Labels, User: kind.User, Password: c.Labels[nepho.LSecret], Database: kind.Database}
		for _, p := range c.Ports { // empty unless running
			if int(p.PrivatePort) == kind.Port && p.PublicPort != 0 {
				s.Host, s.Port = "127.0.0.1", int(p.PublicPort)
			}
		}
		if c.NetworkSettings != nil {
			for _, n := range c.NetworkSettings.Networks {
				if n.IPAddress != "" {
					s.InternalHost, s.InternalPort = n.IPAddress, kind.Port
				}
			}
		}
		out = append(out, s)
	}
	return out, nil
}

func (d *driver) List(ctx context.Context) ([]nepho.Info, error) {
	found, err := d.cli.ContainerList(ctx, container.ListOptions{All: true,
		Filters: filters.NewArgs(filters.Arg("label", nepho.LManaged+"=true"))})
	if err != nil {
		return nil, err
	}
	out := make([]nepho.Info, 0, len(found))
	for _, c := range found {
		if _, service := c.Labels[nepho.LKind]; service {
			continue
		}
		state, ok := states[string(c.State)]
		if !ok {
			state = nepho.Stopped
		}
		out = append(out, nepho.Info{ID: c.Labels[nepho.LID], State: state, Labels: c.Labels})
	}
	return out, nil
}

func (d *driver) Destroy(ctx context.Context, id string) error {
	err := d.cli.ContainerRemove(ctx, id, container.RemoveOptions{Force: true, RemoveVolumes: true})
	if client.IsErrNotFound(err) {
		return nil
	}
	return err
}

func (d *driver) Transport(info nepho.Info) nepho.Transport {
	return &transport{d.cli, info.ID}
}

type transport struct {
	cli       *client.Client
	container string
}

func (t *transport) Exec(ctx context.Context, script, user string, pty bool) (*nepho.RawProcess, error) {
	fail := func(err error) (*nepho.RawProcess, error) {
		return nil, fmt.Errorf("%w: docker exec in %s: %w", nepho.ErrTransport, t.container, err)
	}
	created, err := t.cli.ContainerExecCreate(ctx, t.container, container.ExecOptions{
		Cmd: []string{"sh", "-c", script}, User: user, Tty: pty,
		AttachStdin: true, AttachStdout: true, AttachStderr: true})
	if err != nil {
		return fail(err)
	}
	// The attachment must outlive ctx: it carries the command's streams.
	hj, err := t.cli.ContainerExecAttach(context.WithoutCancel(ctx), created.ID, container.ExecAttachOptions{Tty: pty})
	if err != nil {
		return fail(err)
	}
	stdout, stderr := newPipe(), newPipe()
	done := make(chan struct{})
	go func() {
		if pty { // raw stream
			_, _ = io.Copy(stdout, hj.Reader)
		} else { // stdout/stderr frames
			_, _ = stdcopy.StdCopy(stdout, stderr, hj.Reader)
		}
		stdout.Close()
		stderr.Close()
		close(done)
	}()
	wait := func() (int, error) {
		<-done
		hj.Close()
		for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(50 * time.Millisecond) {
			state, err := t.cli.ContainerExecInspect(context.Background(), created.ID)
			if err != nil {
				return -1, fmt.Errorf("%w: %w", nepho.ErrTransport, err)
			}
			if !state.Running || time.Now().After(deadline) {
				return state.ExitCode, nil
			}
		}
	}
	return &nepho.RawProcess{Stdin: stdin{hj}, Stdout: stdout, Stderr: stderr, Wait: wait}, nil
}

func (t *transport) Close() error { return nil }

// stdin half-closes the hijacked connection on Close: the command sees
// end-of-file while its output keeps flowing.
type stdin struct{ hj types.HijackedResponse }

func (s stdin) Write(b []byte) (int, error) { return s.hj.Conn.Write(b) }
func (s stdin) Close() error                { return s.hj.CloseWrite() }

// pipe is an unbounded in-memory pipe, so that Wait never blocks on unread output.
type pipe struct {
	mu     sync.Mutex
	cond   *sync.Cond
	buf    bytes.Buffer
	closed bool
}

func newPipe() *pipe {
	p := &pipe{}
	p.cond = sync.NewCond(&p.mu)
	return p
}

func (p *pipe) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cond.Broadcast()
	return p.buf.Write(b)
}

func (p *pipe) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true
	p.cond.Broadcast()
	return nil
}

func (p *pipe) Read(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for p.buf.Len() == 0 && !p.closed {
		p.cond.Wait()
	}
	if p.buf.Len() == 0 {
		return 0, io.EOF
	}
	return p.buf.Read(b)
}
