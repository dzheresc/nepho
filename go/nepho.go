// Package nepho is one API for machines on Docker, AWS, GCP and Azure.
//
// The backends live in sub-packages (docker, aws, gcp, azure) so that only the
// SDK you use is linked. Each returns a *Provider:
//
//	p, _ := docker.New(docker.Options{})
//	m, _ := p.Create(ctx, nepho.Spec{OS: "ubuntu-24.04"})
//	proc, _ := m.Execute(ctx, []string{"cat"}, nil)
package nepho

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Labels (tags) put on every resource, identical in all three implementations.
const (
	LManaged = "nepho-managed"
	LID      = "nepho-id"
	LExpires = "nepho-expires" // unix seconds
	LUser    = "nepho-user"    // SSH login user, where it depends on the OS
)

type State string

const (
	Pending    State = "pending"
	Running    State = "running"
	Stopped    State = "stopped"
	Terminated State = "terminated"
)

var (
	// ErrProvision: the backend could not create, find or destroy a machine.
	ErrProvision = errors.New("provision error")
	// ErrTransport: the machine could not be reached or the command could not be started.
	ErrTransport = errors.New("transport error")
)

// SetupError reports a failed prerequisite step. Output holds what the step printed.
type SetupError struct {
	Step   string
	Code   int
	Output string
}

func (e *SetupError) Error() string {
	return fmt.Sprintf("setup step '%s' failed with exit code %d:\n%s", e.Step, e.Code, e.Output)
}

// Setup lists prerequisites, applied in this order: packages, files, script. All run as root.
type Setup struct {
	Packages []string
	Files    map[string]string // absolute path -> text
	Script   string
	Env      map[string]string // environment of Script
}

// Spec describes a machine. Zero values mean the defaults.
type Spec struct {
	OS       string // ubuntu-24.04 (default) | ubuntu-22.04 | debian-12
	CPUs     int    // default 1
	MemoryGB int    // default 1
	DiskGB   int    // default 20
	Setup    Setup
	Labels   map[string]string
	TTL      time.Duration // default 4 hours; negative = never expires
}

func (s Spec) withDefaults() Spec {
	if s.OS == "" {
		s.OS = "ubuntu-24.04"
	}
	if s.CPUs == 0 {
		s.CPUs = 1
	}
	if s.MemoryGB == 0 {
		s.MemoryGB = 1
	}
	if s.DiskGB == 0 {
		s.DiskGB = 20
	}
	if s.TTL == 0 {
		s.TTL = 4 * time.Hour
	}
	return s
}

// TTLMinutes is the TTL rounded up to minutes, or 0 when the machine never expires.
func (s Spec) TTLMinutes() int {
	if s.TTL < 0 {
		return 0
	}
	return max(1, int((s.TTL+time.Minute-1)/time.Minute))
}

type ExecOptions struct {
	Env  map[string]string
	Dir  string
	User string
	PTY  bool
}

type Result struct {
	Code   int
	Stdout string
	Stderr string
}

// RawProcess is what a transport hands back.
type RawProcess struct {
	Stdin  io.WriteCloser
	Stdout io.Reader
	Stderr io.Reader
	Wait   func() (int, error)
}

type Transport interface {
	Exec(ctx context.Context, script, user string, pty bool) (*RawProcess, error)
	Close() error
}

// Process is a command running on a machine.
type Process struct {
	PID    int            // process id inside the machine
	Stdin  io.WriteCloser // Close sends end-of-file; the output streams stay open
	Stdout io.Reader
	Stderr io.Reader

	m    *Machine
	wait func() (int, error)
	once sync.Once
	code int
	err  error
}

// Wait returns the exit code. It is negative when the command was ended by a
// signal or the link dropped.
func (p *Process) Wait() (int, error) {
	p.once.Do(func() { p.code, p.err = p.wait() })
	return p.code, p.err
}

// Kill sends a signal ("TERM" when empty) to the command.
func (p *Process) Kill(ctx context.Context, signal string) error {
	if signal == "" {
		signal = "TERM"
	}
	_, err := p.m.Run(ctx, []string{"kill", "-s", signal, strconv.Itoa(p.PID)}, "", &ExecOptions{User: "root"})
	return err
}

// ---------------------------------------------------------------- helpers

// Sh turns a shell command line into an argv for Execute and Run.
func Sh(command string) []string { return []string{"sh", "-c", command} }

// Quote quotes one word for a POSIX shell.
func Quote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func quoteAll(argv []string) string {
	q := make([]string, len(argv))
	for i, a := range argv {
		q[i] = Quote(a)
	}
	return strings.Join(q, " ")
}

func newID() string {
	b := make([]byte, 5)
	_, _ = rand.Read(b)
	return "m-" + hex.EncodeToString(b)
}

func makeLabels(id string, spec Spec) map[string]string {
	labels := map[string]string{}
	for k, v := range spec.Labels {
		labels[k] = v
	}
	labels[LManaged], labels[LID] = "true", id
	if spec.TTL > 0 {
		labels[LExpires] = strconv.FormatInt(time.Now().Add(spec.TTL).Unix(), 10)
	}
	return labels
}

// ExpiresOf reads the expiry time from labels; the zero time means "never".
func ExpiresOf(labels map[string]string) time.Time {
	n, err := strconv.ParseInt(labels[LExpires], 10, 64)
	if err != nil {
		return time.Time{}
	}
	return time.Unix(n, 0)
}

// wrap builds a shell script that prints its PID on the first line, then becomes the command.
func wrap(argv []string, env map[string]string, dir string) string {
	var b strings.Builder
	b.WriteString("echo $$; ")
	if dir != "" {
		b.WriteString("cd " + Quote(dir) + " && ")
	}
	b.WriteString("exec ")
	if len(env) > 0 {
		keys := make([]string, 0, len(env))
		for k := range env {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		b.WriteString("env ")
		for _, k := range keys {
			b.WriteString(Quote(k+"="+env[k]) + " ")
		}
	}
	b.WriteString(quoteAll(argv))
	return b.String()
}

// Size is one row of a provider's instance size table.
type Size struct {
	CPUs, MemoryGB int
	Name           string
}

func NewSize(cpus, memoryGB int, name string) Size { return Size{cpus, memoryGB, name} }

// PickSize returns the first row that is large enough.
func PickSize(table []Size, cpus, memoryGB int) (string, error) {
	for _, s := range table {
		if s.CPUs >= cpus && s.MemoryGB >= memoryGB {
			return s.Name, nil
		}
	}
	return "", fmt.Errorf("%w: no instance size for %d CPUs / %d GB", ErrProvision, cpus, memoryGB)
}

// MyCIDR returns the public address of this host as a /32, used to restrict inbound SSH.
func MyCIDR(ctx context.Context) (string, error) {
	fail := func(err error) (string, error) {
		return "", fmt.Errorf("%w: cannot detect the public IP; set SSHCIDR explicitly (%v)", ErrProvision, err)
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://checkip.amazonaws.com", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fail(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64))
	if err != nil {
		return fail(err)
	}
	return strings.TrimSpace(string(body)) + "/32", nil
}

// ---------------------------------------------------------------- machine

// Info is what a driver knows about one machine.
type Info struct {
	ID      string
	State   State
	Labels  map[string]string
	Address string
	User    string
}

// Machine is a handle to a machine. Creating or dropping a handle never changes the machine.
type Machine struct {
	ID       string
	Provider string
	State    State // as of Create/List/Attach
	Labels   map[string]string
	Address  string
	Expires  time.Time // zero = never

	owner     *Provider
	transport Transport
}

// ctrlD makes Close on a terminal's input send Ctrl-D: a terminal has no end-of-file.
type ctrlD struct{ io.WriteCloser }

func (c ctrlD) Close() error {
	_, err := c.Write([]byte{4})
	return err
}

// Execute starts a command. Use Sh("...") for a shell command line. opts may be nil.
func (m *Machine) Execute(ctx context.Context, argv []string, opts *ExecOptions) (*Process, error) {
	if opts == nil {
		opts = &ExecOptions{}
	}
	raw, err := m.transport.Exec(ctx, wrap(argv, opts.Env, opts.Dir), opts.User, opts.PTY)
	if err != nil {
		return nil, err
	}
	out := bufio.NewReader(raw.Stdout)
	line, _ := out.ReadString('\n')
	pid, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil {
		stderr, _ := io.ReadAll(raw.Stderr)
		code, _ := raw.Wait()
		return nil, fmt.Errorf("%w: command did not start (exit %d): %s%s", ErrTransport, code, line, stderr)
	}
	p := &Process{PID: pid, Stdin: raw.Stdin, Stdout: out, Stderr: raw.Stderr, m: m, wait: raw.Wait}
	if opts.PTY {
		p.Stdin = ctrlD{raw.Stdin}
	}
	return p, nil
}

// Run runs a command to completion and collects the output. opts may be nil.
func (m *Machine) Run(ctx context.Context, argv []string, input string, opts *ExecOptions) (Result, error) {
	p, err := m.Execute(ctx, argv, opts)
	if err != nil {
		return Result{}, err
	}
	var stdout, stderr []byte
	var wg sync.WaitGroup
	wg.Add(3)
	go func() { // the command may exit before reading its input, so errors are ignored
		defer wg.Done()
		_, _ = io.WriteString(p.Stdin, input)
		_ = p.Stdin.Close()
	}()
	go func() { defer wg.Done(); stdout, _ = io.ReadAll(p.Stdout) }()
	go func() { defer wg.Done(); stderr, _ = io.ReadAll(p.Stderr) }()
	code, err := p.Wait()
	wg.Wait()
	return Result{code, string(stdout), string(stderr)}, err
}

// Detach drops the connection. The machine and its processes keep running.
func (m *Machine) Detach() error { return m.transport.Close() }

func (m *Machine) Destroy(ctx context.Context) error {
	_ = m.Detach()
	if err := m.owner.Destroy(ctx, m.ID); err != nil {
		return err
	}
	m.State = Terminated
	return nil
}

func applySetup(ctx context.Context, m *Machine, s Setup) error {
	step := func(name string, argv []string, input string, env map[string]string) error {
		r, err := m.Run(ctx, argv, input, &ExecOptions{User: "root", Env: env})
		if err != nil {
			return err
		}
		if r.Code != 0 {
			return &SetupError{name, r.Code, r.Stdout + r.Stderr}
		}
		return nil
	}
	if len(s.Packages) > 0 {
		cmd := "apt-get update -q && apt-get install -y -q --no-install-recommends " +
			"-o DPkg::Lock::Timeout=300 " + quoteAll(s.Packages)
		if err := step("packages", Sh(cmd), "", map[string]string{"DEBIAN_FRONTEND": "noninteractive"}); err != nil {
			return err
		}
	}
	paths := make([]string, 0, len(s.Files))
	for p := range s.Files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		cmd := "mkdir -p " + Quote(path.Dir(p)) + " && cat > " + Quote(p)
		if err := step("file "+p, Sh(cmd), s.Files[p], nil); err != nil {
			return err
		}
	}
	if s.Script != "" {
		return step("script", []string{"sh", "-s"}, s.Script, s.Env)
	}
	return nil
}

// ---------------------------------------------------------------- provider

// Driver is what a backend implements.
type Driver interface {
	Name() string
	Launch(ctx context.Context, id string, spec Spec, labels map[string]string) (Info, error)
	List(ctx context.Context) ([]Info, error)
	// Destroy must be idempotent and must also clean up a half-created machine.
	Destroy(ctx context.Context, id string) error
	Transport(info Info) Transport
}

// Optional driver hooks.
type (
	readier interface {
		Ready(ctx context.Context, m *Machine, spec Spec) error
	}
	reaper interface {
		Reapable(ctx context.Context) ([]string, error)
	}
)

// Provider is the public API, the same for every backend.
type Provider struct{ d Driver }

func NewProvider(d Driver) *Provider { return &Provider{d} }

func (p *Provider) Name() string { return p.d.Name() }

func (p *Provider) wrap(err error) error {
	var se *SetupError
	if err == nil || errors.Is(err, ErrProvision) || errors.Is(err, ErrTransport) || errors.As(err, &se) {
		return err
	}
	return fmt.Errorf("%w: %s: %w", ErrProvision, p.Name(), err)
}

func (p *Provider) machine(i Info) *Machine { return NewMachine(p, i, p.d.Transport(i)) }

// NewMachine builds a handle with an explicit transport, for example to reach a
// machine through a different route than the driver would choose.
func NewMachine(p *Provider, i Info, t Transport) *Machine {
	return &Machine{ID: i.ID, Provider: p.Name(), State: i.State, Labels: i.Labels, Address: i.Address,
		Expires: ExpiresOf(i.Labels), owner: p, transport: t}
}

// Create makes a machine and applies spec.Setup. It returns when the machine is ready for Execute.
func (p *Provider) Create(ctx context.Context, spec Spec) (*Machine, error) {
	spec = spec.withDefaults()
	id := newID()
	var m *Machine
	err := func() error {
		info, err := p.d.Launch(ctx, id, spec, makeLabels(id, spec))
		if err != nil {
			return err
		}
		m = p.machine(info)
		if r, ok := p.d.(readier); ok {
			if err := r.Ready(ctx, m, spec); err != nil {
				return err
			}
		}
		return applySetup(ctx, m, spec.Setup)
	}()
	if err != nil {
		if m != nil {
			_ = m.Detach()
		}
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Minute)
		defer cancel()
		_ = p.d.Destroy(cleanup, id)
		return nil, p.wrap(err)
	}
	return m, nil
}

// List returns every machine the backend still knows, in any state.
func (p *Provider) List(ctx context.Context) ([]*Machine, error) {
	infos, err := p.d.List(ctx)
	if err != nil {
		return nil, p.wrap(err)
	}
	out := make([]*Machine, len(infos))
	for i, info := range infos {
		out[i] = p.machine(info)
	}
	return out, nil
}

func (p *Provider) Attach(ctx context.Context, id string) (*Machine, error) {
	ms, err := p.List(ctx)
	if err != nil {
		return nil, err
	}
	for _, m := range ms {
		if m.ID == id {
			return m, nil
		}
	}
	return nil, fmt.Errorf("%w: %s: no machine %s", ErrProvision, p.Name(), id)
}

func (p *Provider) Destroy(ctx context.Context, id string) error {
	return p.wrap(p.d.Destroy(ctx, id))
}

// Reap destroys machines whose TTL has passed and returns how many.
func (p *Provider) Reap(ctx context.Context) (int, error) {
	var ids []string
	if r, ok := p.d.(reaper); ok {
		var err error
		if ids, err = r.Reapable(ctx); err != nil {
			return 0, p.wrap(err)
		}
	} else {
		infos, err := p.d.List(ctx)
		if err != nil {
			return 0, p.wrap(err)
		}
		for _, i := range infos {
			if e := ExpiresOf(i.Labels); !e.IsZero() && e.Before(time.Now()) {
				ids = append(ids, i.ID)
			}
		}
	}
	for n, id := range ids {
		if err := p.Destroy(ctx, id); err != nil {
			return n, err
		}
	}
	return len(ids), nil
}
