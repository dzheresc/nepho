// Behaviour every backend must share.
//
// Runs against Docker (exec transport) and against an sshd inside a Docker
// container (SSH transport, the one the cloud backends use). No cloud is contacted.
package docker_test

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	nepho "github.com/dzheresc/nepho/go"
	"github.com/dzheresc/nepho/go/docker"
)

const sshdSetup = `
useradd -m -s /bin/bash tester
echo 'tester ALL=(ALL) NOPASSWD:ALL' > /etc/sudoers.d/tester
install -d -m 700 -o tester -g tester /home/tester/.ssh
install -m 600 -o tester -g tester /tmp/key.pub /home/tester/.ssh/authorized_keys
mkdir -p /run/sshd && /usr/sbin/sshd
`

var (
	ctx      = context.Background()
	provider *nepho.Provider
	machines = map[string]*nepho.Machine{}
)

func TestMain(m *testing.M) {
	code := func() int {
		var err error
		if provider, err = docker.New(docker.Options{}); err != nil {
			panic(err)
		}
		dir, _ := os.MkdirTemp("", "nepho-key-")
		defer os.RemoveAll(dir)
		keyPath, pub, err := nepho.EnsureKey(filepath.Join(dir, "id_rsa"))
		if err != nil {
			panic(err)
		}
		dm, err := provider.Create(ctx, nepho.Spec{Setup: nepho.Setup{
			Files:  map[string]string{"/etc/demo/hello.txt": "hello file\n"},
			Script: "echo $GREETING > /made-by-script", Env: map[string]string{"GREETING": "hi there"}}})
		if err != nil {
			panic(err)
		}
		defer dm.Destroy(ctx)
		host, err := provider.Create(ctx, nepho.Spec{OS: "debian-12", Setup: nepho.Setup{
			Packages: []string{"openssh-server", "sudo"},
			Files:    map[string]string{"/tmp/key.pub": pub + "\n"}, Script: sshdSetup}})
		if err != nil {
			panic(err)
		}
		defer host.Destroy(ctx)
		r, err := host.Run(ctx, nepho.Sh("hostname -i"), "", nil)
		if err != nil || r.Code != 0 {
			panic(fmt.Sprint(r, err))
		}
		ip := strings.TrimSpace(r.Stdout)
		machines["docker"] = dm
		machines["ssh"] = nepho.NewMachine(provider,
			nepho.Info{ID: host.ID, State: nepho.Running, Labels: host.Labels, Address: ip},
			&nepho.SSHTransport{Host: ip, User: "tester", KeyPath: keyPath, Timeout: 30 * time.Second})
		return m.Run()
	}()
	os.Exit(code)
}

func run(t *testing.T, m *nepho.Machine, argv []string, input string, opts *nepho.ExecOptions) nepho.Result {
	t.Helper()
	r, err := m.Run(ctx, argv, input, opts)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func eq[T comparable](t *testing.T, got, want T) {
	t.Helper()
	if got != want {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}

func ids(t *testing.T) map[string]nepho.State {
	t.Helper()
	ms, err := provider.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]nepho.State{}
	for _, m := range ms {
		out[m.ID] = m.State
	}
	return out
}

func TestConformance(t *testing.T) {
	for _, kind := range []string{"docker", "ssh"} {
		m := machines[kind]
		t.Run(kind, func(t *testing.T) {
			t.Run("run", func(t *testing.T) {
				eq(t, run(t, m, []string{"echo", "hi"}, "", nil), nepho.Result{Code: 0, Stdout: "hi\n"})
			})
			t.Run("live streams", func(t *testing.T) {
				p, err := m.Execute(ctx, []string{"cat"}, nil)
				if err != nil {
					t.Fatal(err)
				}
				lines := bufio.NewReader(p.Stdout)
				fmt.Fprint(p.Stdin, "one\n")
				line, _ := lines.ReadString('\n')
				eq(t, line, "one\n") // answered before stdin is closed
				fmt.Fprint(p.Stdin, "two\nthree\n")
				p.Stdin.Close()
				line, _ = lines.ReadString('\n')
				eq(t, line, "two\n")
				line, _ = lines.ReadString('\n')
				eq(t, line, "three\n")
				_, err = lines.ReadString('\n')
				eq(t, err.Error(), "EOF")
				code, err := p.Wait()
				eq(t, code, 0)
				eq(t, err, nil)
			})
			t.Run("stderr and exit code", func(t *testing.T) {
				eq(t, run(t, m, nepho.Sh("echo out; echo err >&2; exit 3"), "", nil),
					nepho.Result{Code: 3, Stdout: "out\n", Stderr: "err\n"})
			})
			t.Run("env, dir, quoting", func(t *testing.T) {
				r := run(t, m, nepho.Sh(`echo "$FOO"; pwd`), "",
					&nepho.ExecOptions{Env: map[string]string{"FOO": "a b'c"}, Dir: "/tmp"})
				eq(t, r.Stdout, "a b'c\n/tmp\n")
				eq(t, run(t, m, []string{"printf", "%s|", "a b", "c'd", "$x", "é"}, "", nil).Stdout, "a b|c'd|$x|é|")
			})
			t.Run("bad dir", func(t *testing.T) {
				r := run(t, m, []string{"true"}, "", &nepho.ExecOptions{Dir: "/nonexistent"})
				if r.Code == 0 || !strings.Contains(r.Stderr, "nonexistent") {
					t.Fatalf("%+v", r)
				}
			})
			t.Run("user", func(t *testing.T) {
				eq(t, run(t, m, []string{"id", "-un"}, "", &nepho.ExecOptions{User: "root"}).Stdout, "root\n")
			})
			t.Run("pty", func(t *testing.T) {
				r := run(t, m, nepho.Sh("test -t 1 && echo is-a-tty"), "", &nepho.ExecOptions{PTY: true})
				if r.Code != 0 || !strings.Contains(r.Stdout, "is-a-tty") {
					t.Fatalf("%+v", r)
				}
				eq(t, run(t, m, nepho.Sh("test -t 1 || echo no-tty"), "", nil).Stdout, "no-tty\n")
			})
			t.Run("kill", func(t *testing.T) {
				p, err := m.Execute(ctx, []string{"sleep", "1000"}, nil)
				if err != nil || p.PID <= 0 {
					t.Fatal(p, err)
				}
				if err := p.Kill(ctx, ""); err != nil {
					t.Fatal(err)
				}
				done := make(chan int, 1)
				go func() { code, _ := p.Wait(); done <- code }()
				select {
				case code := <-done:
					if code == 0 {
						t.Fatal("exit code 0 after kill")
					}
				case <-time.After(10 * time.Second):
					t.Fatal("still running after kill")
				}
			})
			t.Run("big output and input", func(t *testing.T) {
				text := strings.Repeat(strings.Repeat("x", 79)+"\n", 20000)
				if run(t, m, []string{"cat"}, text, nil).Stdout != text {
					t.Fatal("output differs from input")
				}
			})
			t.Run("concurrent", func(t *testing.T) {
				var wg sync.WaitGroup
				out := make([]string, 8)
				for i := range out {
					wg.Add(1)
					go func() {
						defer wg.Done()
						r, _ := m.Run(ctx, nepho.Sh(fmt.Sprintf("sleep 0.3; echo %d", i)), "", nil)
						out[i] = r.Stdout
					}()
				}
				wg.Wait()
				eq(t, strings.Join(out, ""), "0\n1\n2\n3\n4\n5\n6\n7\n")
			})
		})
	}
}

func TestSetupApplied(t *testing.T) {
	r := run(t, machines["docker"], nepho.Sh("cat /etc/demo/hello.txt /made-by-script"), "", nil)
	eq(t, r.Stdout, "hello file\nhi there\n")
}

func TestSetupFailureDestroys(t *testing.T) {
	before := len(ids(t))
	_, err := provider.Create(ctx, nepho.Spec{Setup: nepho.Setup{Script: "echo boom; exit 7"}})
	var se *nepho.SetupError
	if !errors.As(err, &se) || se.Code != 7 || !strings.Contains(se.Output, "boom") {
		t.Fatalf("unexpected error: %v", err)
	}
	eq(t, len(ids(t)), before)
}

func TestAttachDetach(t *testing.T) {
	dm := machines["docker"]
	if _, err := dm.Execute(ctx, nepho.Sh("sleep 2; echo done > /tmp/survivor"), nil); err != nil {
		t.Fatal(err)
	}
	dm.Detach()

	second, _ := docker.New(docker.Options{}) // as a second client would
	other, err := second.Attach(ctx, dm.ID)
	if err != nil {
		t.Fatal(err)
	}
	eq(t, other.State, nepho.Running)
	eq(t, other.Expires, dm.Expires)
	time.Sleep(3 * time.Second)
	eq(t, run(t, other, []string{"cat", "/tmp/survivor"}, "", nil).Stdout, "done\n")
	other.Detach()
	eq(t, run(t, dm, []string{"echo", "back"}, "", nil).Stdout, "back\n") // handle still usable
	if _, err := provider.Attach(ctx, "m-doesnotexist"); !errors.Is(err, nepho.ErrProvision) {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestDeadMachinesListedThenReaped(t *testing.T) {
	m, err := provider.Create(ctx, nepho.Spec{TTL: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(4 * time.Second)
	eq(t, ids(t)[m.ID], nepho.Stopped) // dead, but still enumerated
	if n, err := provider.Reap(ctx); err != nil || n < 1 {
		t.Fatal(n, err)
	}
	if _, ok := ids(t)[m.ID]; ok {
		t.Fatal("still listed after reap")
	}
}
