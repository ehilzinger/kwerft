package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

// runKeys are generated per run and never written to disk: the client key
// (its public half is uploaded to Hetzner and deleted afterwards) and the
// server's host key, handed to cloud-init so the harness can pin it instead
// of trusting whatever answers first.
type runKeys struct {
	client     ssh.Signer
	clientPub  string // authorized_keys line
	host       ssh.Signer
	hostPEM    string // OpenSSH private key for cloud-init
	hostPublic string
}

func newRunKeys(comment string) (*runKeys, error) {
	_, cpriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	client, err := ssh.NewSignerFromKey(cpriv)
	if err != nil {
		return nil, err
	}
	_, hpriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	host, err := ssh.NewSignerFromKey(hpriv)
	if err != nil {
		return nil, err
	}
	block, err := ssh.MarshalPrivateKey(hpriv, comment)
	if err != nil {
		return nil, err
	}
	return &runKeys{
		client:     client,
		clientPub:  strings.TrimSpace(string(ssh.MarshalAuthorizedKey(client.PublicKey()))) + " " + comment,
		host:       host,
		hostPEM:    string(pem.EncodeToMemory(block)),
		hostPublic: strings.TrimSpace(string(ssh.MarshalAuthorizedKey(host.PublicKey()))),
	}, nil
}

// cloudInit installs the pinned host key and nothing else; the installer
// does the rest, exactly as on a server a user prepared.
func cloudInit(k *runKeys) string {
	var b strings.Builder
	b.WriteString("#cloud-config\n")
	b.WriteString("ssh_deletekeys: true\n")
	b.WriteString("ssh_genkeytypes: []\n")
	b.WriteString("ssh_keys:\n")
	b.WriteString("  ed25519_private: |\n")
	for _, line := range strings.Split(strings.TrimRight(k.hostPEM, "\n"), "\n") {
		b.WriteString("    " + line + "\n")
	}
	b.WriteString("  ed25519_public: " + k.hostPublic + "\n")
	return b.String()
}

// remote runs commands on the test server.
type remote interface {
	// run executes cmd with stdout and stderr connected to the writers and
	// returns its exit status; err is for anything other than a non-zero
	// exit (connection lost, cancelled).
	run(ctx context.Context, cmd string, stdout, stderr io.Writer) (int, error)
	close() error
}

// dialer opens a remote to addr ("ip:22").
type dialer func(ctx context.Context, addr string, k *runKeys) (remote, error)

type sshRemote struct {
	c    *ssh.Client
	done chan struct{}
}

func dialSSH(ctx context.Context, addr string, k *runKeys) (remote, error) {
	cfg := &ssh.ClientConfig{
		User:              "root",
		Auth:              []ssh.AuthMethod{ssh.PublicKeys(k.client)},
		HostKeyCallback:   ssh.FixedHostKey(k.host.PublicKey()),
		HostKeyAlgorithms: []string{ssh.KeyAlgoED25519},
		Timeout:           15 * time.Second,
	}
	d := net.Dialer{Timeout: 15 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	cc, chans, reqs, err := ssh.NewClientConn(conn, addr, cfg)
	if err != nil {
		conn.Close()
		return nil, err
	}
	r := &sshRemote{c: ssh.NewClient(cc, chans, reqs), done: make(chan struct{})}
	go r.keepalive()
	return r, nil
}

// keepalive stops idle NAT and firewall timeouts from dropping the
// connection during a long, quiet installer stage.
func (r *sshRemote) keepalive() {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-r.done:
			return
		case <-t.C:
			if _, _, err := r.c.SendRequest("keepalive@openssh.com", true, nil); err != nil {
				return
			}
		}
	}
}

func (r *sshRemote) run(ctx context.Context, cmd string, stdout, stderr io.Writer) (int, error) {
	s, err := r.c.NewSession()
	if err != nil {
		return -1, err
	}
	defer s.Close()
	s.Stdout, s.Stderr = stdout, stderr
	if err := s.Start(cmd); err != nil {
		return -1, err
	}
	waited := make(chan error, 1)
	go func() { waited <- s.Wait() }()
	select {
	case <-ctx.Done():
		_ = s.Signal(ssh.SIGKILL)
		s.Close()
		return -1, ctx.Err()
	case err := <-waited:
		var exit *ssh.ExitError
		switch {
		case err == nil:
			return 0, nil
		case errors.As(err, &exit):
			return exit.ExitStatus(), nil
		default:
			return -1, err
		}
	}
}

func (r *sshRemote) close() error {
	close(r.done)
	return r.c.Close()
}

// waitSSH dials until the server answers with the pinned host key.
func waitSSH(ctx context.Context, dial dialer, addr string, k *runKeys, every time.Duration) (remote, error) {
	var last error
	for {
		r, err := dial(ctx, addr, k)
		if err == nil {
			return r, nil
		}
		last = err
		if serr := sleepCtx(ctx, every); serr != nil {
			return nil, fmt.Errorf("SSH to %s did not come up: %w", addr, last)
		}
	}
}
