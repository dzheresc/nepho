// Package docker is the Docker backend: a long-lived container stands in for a VM.
package docker

import (
	"bytes"
	"context"
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

	"example.com/nepho"
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
// Spec.DiskGB is ignored. When the TTL passes the container exits by itself
// (state "stopped"); Reap removes it.
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
	init := true
	create := func() error {
		_, err := d.cli.ContainerCreate(ctx,
			&container.Config{Image: ref, Cmd: []string{"sleep", life}, Labels: labels},
			&container.HostConfig{Init: &init, Resources: container.Resources{
				NanoCPUs: int64(spec.CPUs) * 1e9, Memory: int64(spec.MemoryGB) << 30,
				CpusetCpus: d.opts.CpusetCpus, CpusetMems: d.opts.CpusetMems}},
			nil, nil, id)
		return err
	}
	err := create()
	if client.IsErrNotFound(err) { // image not present locally
		rc, perr := d.cli.ImagePull(ctx, ref, image.PullOptions{})
		if perr != nil {
			return nepho.Info{}, perr
		}
		_, _ = io.Copy(io.Discard, rc)
		rc.Close()
		err = create()
	}
	if err != nil {
		return nepho.Info{}, err
	}
	if err := d.cli.ContainerStart(ctx, id, container.StartOptions{}); err != nil {
		return nepho.Info{}, err
	}
	return nepho.Info{ID: id, State: nepho.Running, Labels: labels}, nil
}

func (d *driver) List(ctx context.Context) ([]nepho.Info, error) {
	found, err := d.cli.ContainerList(ctx, container.ListOptions{All: true,
		Filters: filters.NewArgs(filters.Arg("label", nepho.LManaged+"=true"))})
	if err != nil {
		return nil, err
	}
	out := make([]nepho.Info, 0, len(found))
	for _, c := range found {
		state, ok := states[string(c.State)]
		if !ok {
			state = nepho.Stopped
		}
		out = append(out, nepho.Info{ID: c.Labels[nepho.LID], State: state, Labels: c.Labels})
	}
	return out, nil
}

func (d *driver) Destroy(ctx context.Context, id string) error {
	err := d.cli.ContainerRemove(ctx, id, container.RemoveOptions{Force: true})
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
