package git_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
	"github.com/ehilzinger/kwerft/internal/git"
	"github.com/ehilzinger/kwerft/internal/git/gittest"
)

// sshHost is a minimal Git SSH server: it accepts one client key and
// answers git-upload-pack with a ref advertisement.
type sshHost struct {
	addr     string
	hostKey  ssh.Signer
	commands chan string
}

func newSSHHost(t *testing.T, clientKey ssh.PublicKey, repos map[string]*gittest.Repo) *sshHost {
	t.Helper()
	_, hk, _ := ed25519.GenerateKey(rand.Reader)
	hostKey, err := ssh.NewSignerFromKey(hk)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &ssh.ServerConfig{PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
		if bytes.Equal(key.Marshal(), clientKey.Marshal()) {
			return nil, nil
		}
		return nil, errors.New("unknown key")
	}}
	cfg.AddHostKey(hostKey)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	h := &sshHost{addr: ln.Addr().String(), hostKey: hostKey, commands: make(chan string, 10)}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go h.serve(c, cfg, repos)
		}
	}()
	return h
}

func (h *sshHost) serve(c net.Conn, cfg *ssh.ServerConfig, repos map[string]*gittest.Repo) {
	defer c.Close()
	_, chans, reqs, err := ssh.NewServerConn(c, cfg)
	if err != nil {
		return
	}
	go ssh.DiscardRequests(reqs)
	for nc := range chans {
		ch, chReqs, err := nc.Accept()
		if err != nil {
			return
		}
		for req := range chReqs {
			if req.Type != "exec" || len(req.Payload) < 4 {
				_ = req.Reply(false, nil)
				continue
			}
			n := binary.BigEndian.Uint32(req.Payload)
			cmd := string(req.Payload[4 : 4+n])
			_ = req.Reply(true, nil)
			h.commands <- cmd
			path := strings.TrimSuffix(strings.Trim(strings.TrimPrefix(cmd, "git-upload-pack "), "'"), ".git")
			status := uint32(0)
			if repo, ok := repos[path]; ok {
				_, _ = io.WriteString(ch, gittest.Advertisement(repo, false))
				buf := make([]byte, 4)
				_, _ = io.ReadFull(ch, buf) // the client's flush
			} else {
				_, _ = io.WriteString(ch.Stderr(), "ERROR: Repository not found.\n")
				status = 128
			}
			_, _ = ch.SendRequest("exit-status", false, binary.BigEndian.AppendUint32(nil, status))
			_ = ch.Close()
			break
		}
	}
}

func TestLsRemoteOverSSH(t *testing.T) {
	_, ck, _ := ed25519.GenerateKey(rand.Reader)
	block, err := ssh.MarshalPrivateKey(ck, "")
	if err != nil {
		t.Fatal(err)
	}
	clientPEM := pem.EncodeToMemory(block)
	signer, _ := ssh.NewSignerFromKey(ck)
	repo := &gittest.Repo{DefaultBranch: "main", Branches: map[string]gittest.Commit{"main": {SHA: shaMain}, "dev": {SHA: shaFeature}}}
	host := newSSHHost(t, signer.PublicKey(), map[string]*gittest.Repo{"acme/api": repo})
	knownLine := knownhosts.Line([]string{knownhosts.Normalize(host.addr)}, host.hostKey.PublicKey())

	f := &git.Factory{}
	c := git.Connection{Provider: kwerftv1.GitHub, URL: "https://127.0.0.1", Auth: kwerftv1.GitAuthSSHKey,
		SSHPrivateKey: clientPEM, KnownHosts: []byte(knownLine + "\n")}
	p, err := f.For(c)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	account, err := p.Verify(ctx)
	if err != nil || !strings.HasPrefix(account, "deploy key SHA256:") {
		t.Errorf("Verify = %q, %v", account, err)
	}
	r, err := git.ParseRepository("ssh://git@" + host.addr + "/acme/api.git")
	if err != nil {
		t.Fatal(err)
	}
	if def, err := p.DefaultBranch(ctx, r); err != nil || def != "main" {
		t.Errorf("DefaultBranch = %q, %v", def, err)
	}
	if h, err := p.Head(ctx, r, "dev"); err != nil || h.SHA != shaFeature {
		t.Errorf("Head(dev) = %+v, %v", h, err)
	}
	if cmd := <-host.commands; cmd != "git-upload-pack 'acme/api.git'" {
		t.Errorf("command = %q", cmd)
	}
	missing, _ := git.ParseRepository("ssh://git@" + host.addr + "/acme/gone.git")
	if _, err := p.Head(ctx, missing, "main"); !errors.Is(err, git.ErrNotFound) {
		t.Errorf("missing repository: %v", err)
	}

	// Another host key under the same name: refused, never connected blindly.
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	otherSigner, _ := ssh.NewSignerFromKey(other)
	c.KnownHosts = []byte(knownhosts.Line([]string{knownhosts.Normalize(host.addr)}, otherSigner.PublicKey()) + "\n")
	p, _ = f.For(c)
	if _, err := p.Head(ctx, r, "main"); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Errorf("wrong host key: %v", err)
	}
	c.KnownHosts = nil
	p, _ = f.For(c)
	if _, err := p.Head(ctx, r, "main"); err == nil || !strings.Contains(err.Error(), "not in the connection's known_hosts") {
		t.Errorf("unknown host: %v", err)
	}
	// Hashed known_hosts entries work too.
	hashed := knownhosts.HashHostname(knownhosts.Normalize(host.addr)) + " " + string(bytes.TrimSpace(ssh.MarshalAuthorizedKey(host.hostKey.PublicKey())))
	c.KnownHosts = []byte(hashed + "\n")
	p, _ = f.For(c)
	if h, err := p.Head(ctx, r, "main"); err != nil || h.SHA != shaMain {
		t.Errorf("hashed known_hosts: %+v %v", h, err)
	}

	// Trust on first use: the scan returns the line a connection stores.
	line, fp, err := git.ScanHostKey(ctx, host.addr)
	if err != nil || line != knownLine || fp != ssh.FingerprintSHA256(host.hostKey.PublicKey()) {
		t.Errorf("ScanHostKey = %q %q %v", line, fp, err)
	}

	// A key nobody accepts.
	_, stranger, _ := ed25519.GenerateKey(rand.Reader)
	sb, _ := ssh.MarshalPrivateKey(stranger, "")
	c.SSHPrivateKey, c.KnownHosts = pem.EncodeToMemory(sb), []byte(knownLine+"\n")
	p, _ = f.For(c)
	if _, err := p.Head(ctx, r, "main"); !errors.Is(err, git.ErrUnauthorized) {
		t.Errorf("unknown client key: %v", err)
	}
}
