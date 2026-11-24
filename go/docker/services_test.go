package docker_test

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"

	nepho "github.com/dzheresc/nepho/go"
	"github.com/dzheresc/nepho/go/docker"
)

// valkey sends commands in the Redis protocol and returns one reply line for each.
func valkey(t *testing.T, s *nepho.Service, commands ...[]string) []string {
	t.Helper()
	conn, err := net.Dial("tcp", net.JoinHostPort(s.Host, strconv.Itoa(s.Port)))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	lines := bufio.NewReader(conn)
	next := func() string {
		line, err := lines.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(line)
	}
	var replies []string
	for _, words := range commands {
		fmt.Fprintf(conn, "*%d\r\n", len(words))
		for _, w := range words {
			fmt.Fprintf(conn, "$%d\r\n%s\r\n", len(w), w)
		}
		line := next()
		if strings.HasPrefix(line, "$") { // bulk string: the value is on the next line
			line = next()
		}
		replies = append(replies, line)
	}
	return replies
}

// postgresAuthRequest opens a session in the PostgreSQL protocol and returns
// the authentication method the server asks for.
func postgresAuthRequest(t *testing.T, s *nepho.Service) (byte, uint32) {
	t.Helper()
	conn, err := net.Dial("tcp", net.JoinHostPort(s.Host, strconv.Itoa(s.Port)))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	body := append([]byte{0, 3, 0, 0}, "user\x00nepho\x00database\x00nepho\x00\x00"...)
	_ = binary.Write(conn, binary.BigEndian, uint32(len(body)+4))
	_, _ = conn.Write(body)
	head := make([]byte, 9)
	if _, err := io.ReadFull(conn, head); err != nil {
		t.Fatal(err)
	}
	return head[0], binary.BigEndian.Uint32(head[5:])
}

// inside runs a command in a service container.
func inside(t *testing.T, id string, argv ...string) (int, string) {
	t.Helper()
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	created, err := cli.ContainerExecCreate(ctx, id, container.ExecOptions{Cmd: argv, AttachStdout: true, AttachStderr: true})
	if err != nil {
		t.Fatal(err)
	}
	hj, err := cli.ContainerExecAttach(ctx, created.ID, container.ExecAttachOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer hj.Close()
	var out bytes.Buffer
	_, _ = stdcopy.StdCopy(&out, &out, hj.Reader)
	state, err := cli.ContainerExecInspect(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	return state.ExitCode, out.String()
}

func serviceIDs(t *testing.T) map[string]nepho.State {
	t.Helper()
	all, err := provider.ListServices(ctx)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]nepho.State{}
	for _, s := range all {
		out[s.ID] = s.State
	}
	return out
}

func TestValkey(t *testing.T) {
	s, err := provider.CreateService(ctx, nepho.ServiceSpec{Kind: "valkey"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Destroy(ctx)
	eq(t, s.State, nepho.Running)
	eq(t, s.Version, "8")
	eq(t, s.URL(), fmt.Sprintf("redis://default:%s@127.0.0.1:%d/0", s.Password, s.Port))
	eq(t, s.InternalURL(), fmt.Sprintf("redis://default:%s@%s:6379/0", s.Password, s.InternalHost))
	eq(t, valkey(t, s, []string{"PING"})[0], "-NOAUTH Authentication required.")
	eq(t, strings.Join(valkey(t, s, []string{"AUTH", s.Password}, []string{"SET", "k", "v"}, []string{"GET", "k"}), " "), "+OK +OK v")
	code, out := inside(t, s.ID, "valkey-cli", "-u", s.InternalURL(), "--no-auth-warning", "GET", "k")
	eq(t, code, 0)
	eq(t, strings.TrimSpace(out), "v")
	if err := s.Destroy(ctx); err != nil {
		t.Fatal(err)
	}
	eq(t, s.State, nepho.Terminated)
}

func TestPostgres(t *testing.T) {
	s, err := provider.CreateService(ctx, nepho.ServiceSpec{Kind: "postgres", Version: "16"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Destroy(ctx)
	eq(t, s.Version, "16")
	eq(t, s.URL(), fmt.Sprintf("postgresql://nepho:%s@127.0.0.1:%d/nepho", s.Password, s.Port))
	// 'R' + 10: the server on the published port asks for SCRAM password authentication
	kind, method := postgresAuthRequest(t, s)
	eq(t, kind, byte('R'))
	eq(t, method, uint32(10))
	code, out := inside(t, s.ID, "psql", s.InternalURL(), "-Atc", "select current_user, version()")
	if code != 0 || !strings.HasPrefix(out, "nepho|PostgreSQL 16.") {
		t.Fatal(code, out)
	}
	bad := strings.Replace(s.InternalURL(), s.Password, "wrong", 1)
	if code, _ := inside(t, s.ID, "psql", bad, "-Atc", "select 1"); code == 0 {
		t.Fatal("a wrong password was accepted")
	}
}

func TestServicesListAttachReap(t *testing.T) {
	dm := machines["docker"]
	s, err := provider.CreateService(ctx, nepho.ServiceSpec{Kind: "valkey", Labels: map[string]string{"team": "x"}, TTL: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Destroy(ctx)
	if !strings.HasPrefix(s.ID, "s-") || s.Labels["team"] != "x" {
		t.Fatal(s.ID, s.Labels)
	}
	_, isService := serviceIDs(t)[s.ID]
	_, isMachine := ids(t)[s.ID]
	_, machineListed := ids(t)[dm.ID]
	_, machineIsService := serviceIDs(t)[dm.ID]
	eq(t, [4]bool{isService, isMachine, machineListed, machineIsService}, [4]bool{true, false, true, false}) // the two lists are separate

	second, _ := docker.New(docker.Options{}) // as a second client would
	other, err := second.AttachService(ctx, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	eq(t, other.URL(), s.URL())
	eq(t, other.Expires, s.Expires)
	for _, id := range []string{"s-doesnotexist", dm.ID} {
		if _, err := provider.AttachService(ctx, id); !errors.Is(err, nepho.ErrProvision) {
			t.Fatalf("unexpected error: %v", err)
		}
	}

	time.Sleep(2 * time.Second)
	eq(t, serviceIDs(t)[s.ID], nepho.Running) // nothing stops it at the TTL
	if n, err := provider.Reap(ctx); err != nil || n < 1 {
		t.Fatal(n, err)
	}
	if _, ok := serviceIDs(t)[s.ID]; ok {
		t.Fatal("still listed after reap")
	}
	if _, ok := ids(t)[dm.ID]; !ok {
		t.Fatal("reap removed a live machine")
	}
}

func TestServiceBadRequests(t *testing.T) {
	before := len(serviceIDs(t))
	for _, spec := range []nepho.ServiceSpec{{Kind: "mysql"}, {Kind: "valkey", Version: "0.0.0-nope"}} {
		if _, err := provider.CreateService(ctx, spec); !errors.Is(err, nepho.ErrProvision) {
			t.Fatalf("unexpected error: %v", err)
		}
	}
	eq(t, len(serviceIDs(t)), before)
}
