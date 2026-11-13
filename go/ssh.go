package nepho

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

// EnsureKey returns the private key path and the OpenSSH public key, creating the
// key on first use. An empty path means ~/.config/nepho/id_rsa.
//
// The key is RSA in PKCS#1 PEM so that the Python, TypeScript and Go
// implementations can share one file, and every cloud accepts it.
func EnsureKey(path string) (keyPath, publicKey string, err error) {
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", "", err
		}
		path = filepath.Join(home, ".config", "nepho", "id_rsa")
	}
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return "", "", err
		}
		key, err := rsa.GenerateKey(rand.Reader, 3072)
		if err != nil {
			return "", "", err
		}
		data := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			_, err = f.Write(data)
			if cerr := f.Close(); err == nil {
				err = cerr
			}
		}
		if err != nil && !errors.Is(err, os.ErrExist) { // ErrExist: another process won the race
			return "", "", err
		}
	}
	signer, err := loadSigner(path)
	if err != nil {
		return "", "", err
	}
	return path, strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey()))), nil
}

func loadSigner(path string) (ssh.Signer, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ssh.ParsePrivateKey(data)
}

// SSHTransport runs commands over SSH; the three cloud backends share it.
// One connection is opened lazily per machine and each command is a channel on it.
type SSHTransport struct {
	Host    string
	User    string
	KeyPath string
	Timeout time.Duration // how long to keep retrying the first connection; default 10 minutes

	mu     sync.Mutex
	client *ssh.Client
}

func (t *SSHTransport) connect(ctx context.Context) (*ssh.Client, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.client != nil {
		if _, _, err := t.client.SendRequest("keepalive@openssh.com", true, nil); err == nil {
			return t.client, nil
		}
		t.client.Close()
		t.client = nil
	}
	if t.Host == "" {
		return nil, fmt.Errorf("%w: machine has no address (is it running?)", ErrTransport)
	}
	signer, err := loadSigner(t.KeyPath)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrTransport, err)
	}
	// Host keys of freshly created VMs are unknown, so they are not checked.
	cfg := &ssh.ClientConfig{User: t.User, Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), Timeout: 10 * time.Second}
	timeout := t.Timeout
	if timeout == 0 {
		timeout = 10 * time.Minute
	}
	addr := net.JoinHostPort(t.Host, "22")
	deadline := time.Now().Add(timeout)
	for { // a new VM refuses connections until sshd and the key are in place
		client, err := dial(ctx, addr, cfg)
		if err == nil {
			t.client = client
			return client, nil
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return nil, fmt.Errorf("%w: ssh %s@%s: %w", ErrTransport, t.User, t.Host, err)
		}
		select {
		case <-ctx.Done():
		case <-time.After(3 * time.Second):
		}
	}
}

func dial(ctx context.Context, addr string, cfg *ssh.ClientConfig) (*ssh.Client, error) {
	conn, err := (&net.Dialer{Timeout: cfg.Timeout}).DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	_ = conn.SetDeadline(time.Now().Add(cfg.Timeout))
	c, chans, reqs, err := ssh.NewClientConn(conn, addr, cfg)
	if err != nil {
		conn.Close()
		return nil, err
	}
	_ = conn.SetDeadline(time.Time{})
	return ssh.NewClient(c, chans, reqs), nil
}

func (t *SSHTransport) Exec(ctx context.Context, script, user string, pty bool) (*RawProcess, error) {
	client, err := t.connect(ctx)
	if err != nil {
		return nil, err
	}
	if user != "" && user != t.User {
		script = "sudo -n -u " + Quote(user) + " -- sh -c " + Quote(script)
	}
	fail := func(err error) (*RawProcess, error) {
		return nil, fmt.Errorf("%w: ssh %s: %w", ErrTransport, t.Host, err)
	}
	s, err := client.NewSession()
	if err != nil {
		return fail(err)
	}
	if pty {
		if err := s.RequestPty("xterm", 24, 80, ssh.TerminalModes{}); err != nil {
			s.Close()
			return fail(err)
		}
	}
	stdin, _ := s.StdinPipe()
	stdout, _ := s.StdoutPipe()
	stderr, _ := s.StderrPipe()
	if err := s.Start(script); err != nil {
		s.Close()
		return fail(err)
	}
	wait := func() (int, error) {
		defer s.Close()
		err := s.Wait()
		var exit *ssh.ExitError
		var missing *ssh.ExitMissingError
		switch {
		case err == nil:
			return 0, nil
		case errors.As(err, &exit):
			if exit.Signal() != "" {
				return -1, nil
			}
			return exit.ExitStatus(), nil
		case errors.As(err, &missing):
			return -1, nil
		default:
			return -1, fmt.Errorf("%w: %w", ErrTransport, err)
		}
	}
	return &RawProcess{Stdin: stdin, Stdout: stdout, Stderr: stderr, Wait: wait}, nil
}

func (t *SSHTransport) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.client == nil {
		return nil
	}
	err := t.client.Close()
	t.client = nil
	return err
}

// SSHReady waits until first-boot provisioning has finished, so apt is free to use.
func SSHReady(ctx context.Context, m *Machine) error {
	_, err := m.Run(ctx, Sh("cloud-init status --wait >/dev/null 2>&1 || true"), "", nil)
	return err
}
