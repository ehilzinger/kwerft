package git

import (
	"bufio"
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha1" //nolint:gosec // known_hosts hashes host names with HMAC-SHA1; not a security choice of ours
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
)

// protocolProvider speaks the Git protocol itself: generic hosts, and every
// connection with an SSH deploy key. It lists a repository's refs
// (ls-remote) over smart HTTP or SSH, which yields branch heads and the
// default branch but no commit messages, files, hooks or statuses.
type protocolProvider struct {
	f    *Factory
	conn Connection
}

func (p *protocolProvider) Verify(context.Context) (string, error) {
	if p.conn.Auth != kwerftv1.GitAuthSSHKey {
		return "", nil
	}
	signer, err := ssh.ParsePrivateKey(p.conn.SSHPrivateKey)
	if err != nil {
		return "", fmt.Errorf("%w: SSH private key: %v", ErrUnauthorized, sshKeyError(err))
	}
	if len(p.conn.KnownHosts) > 0 {
		if _, err := parseKnownHosts(p.conn.KnownHosts); err != nil {
			return "", err
		}
	}
	return "deploy key " + ssh.FingerprintSHA256(signer.PublicKey()), nil
}

func sshKeyError(err error) error {
	var missing *ssh.PassphraseMissingError
	if errors.As(err, &missing) {
		return errors.New("the key has a passphrase; deploy keys must have none")
	}
	return err
}

func (p *protocolProvider) DefaultBranch(ctx context.Context, repo Repo) (string, error) {
	refs, err := p.lsRemote(ctx, repo)
	if err != nil {
		return "", err
	}
	if refs.head == "" {
		return "", fmt.Errorf("%s does not say which branch is its default", repo)
	}
	return strings.TrimPrefix(refs.head, "refs/heads/"), nil
}

func (p *protocolProvider) Head(ctx context.Context, repo Repo, branch string) (Commit, error) {
	refs, err := p.lsRemote(ctx, repo)
	if err != nil {
		return Commit{}, err
	}
	sha, ok := refs.refs["refs/heads/"+branch]
	if !ok {
		return Commit{}, fmt.Errorf("branch %s: %w", branch, ErrNotFound)
	}
	return Commit{SHA: sha}, nil
}

// Commit cannot look a commit up without cloning; the SHA is all there is.
func (p *protocolProvider) Commit(_ context.Context, _ Repo, sha string) (Commit, error) {
	return Commit{SHA: sha}, nil
}

func (p *protocolProvider) FileExists(context.Context, Repo, string, string) (bool, error) {
	return false, ErrUnsupported
}

func (p *protocolProvider) ConnectionWebhook(context.Context, Hook) (bool, error) { return false, nil }

func (p *protocolProvider) EnsureWebhook(context.Context, Repo, Hook, bool) error {
	return ErrUnsupported
}

func (p *protocolProvider) ReportStatus(context.Context, Repo, Status) (string, error) {
	return "", ErrUnsupported
}

type advertisement struct {
	refs map[string]string // full ref name → SHA
	head string            // what HEAD points to, e.g. refs/heads/main
}

func (p *protocolProvider) lsRemote(ctx context.Context, repo Repo) (*advertisement, error) {
	if p.conn.Auth == kwerftv1.GitAuthSSHKey {
		return p.lsRemoteSSH(ctx, repo)
	}
	return p.lsRemoteHTTP(ctx, repo)
}

// lsRemoteHTTP is the first request of a smart-HTTP fetch
// (git-scm.com/docs/http-protocol): GET <repo>/info/refs?service=git-upload-pack.
func (p *protocolProvider) lsRemoteHTTP(ctx context.Context, repo Repo) (*advertisement, error) {
	u := repo.HTTPS() + "/info/refs?service=git-upload-pack"
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "git/2.47.0 (kwerft)")
	if p.conn.Auth == kwerftv1.GitAuthToken && p.conn.Token != "" {
		// Hosts take the token as the password; the user name varies and is
		// mostly ignored.
		req.SetBasicAuth(tokenUser(p.conn.Provider), p.conn.Token)
	}
	res, err := p.f.httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("ls-remote %s: %w", repo, unwrapURLError(err))
	}
	defer res.Body.Close()
	switch res.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized:
		return nil, fmt.Errorf("ls-remote %s: %w", repo, ErrUnauthorized)
	case http.StatusForbidden:
		return nil, fmt.Errorf("ls-remote %s: %w", repo, ErrForbidden)
	case http.StatusNotFound:
		return nil, fmt.Errorf("ls-remote %s: %w", repo, ErrNotFound)
	default:
		return nil, fmt.Errorf("ls-remote %s: HTTP %d", repo, res.StatusCode)
	}
	if ct := res.Header.Get("Content-Type"); ct != "application/x-git-upload-pack-advertisement" {
		return nil, fmt.Errorf("ls-remote %s: the server does not speak the smart HTTP protocol (%s)", repo, ct)
	}
	return readAdvertisement(io.LimitReader(res.Body, 64<<20), true)
}

func tokenUser(p kwerftv1.GitProvider) string {
	switch p {
	case kwerftv1.GitHub:
		return "x-access-token"
	case kwerftv1.GitLab:
		return "oauth2"
	}
	return "kwerft"
}

// lsRemoteSSH runs git-upload-pack over SSH, reads the ref advertisement
// and hangs up with a flush packet.
func (p *protocolProvider) lsRemoteSSH(ctx context.Context, repo Repo) (*advertisement, error) {
	if !repo.SSH {
		// An https URL with a deploy key: the same repository over SSH.
		repo.SSH, repo.SSHUser = true, "git"
	}
	signer, err := ssh.ParsePrivateKey(p.conn.SSHPrivateKey)
	if err != nil {
		return nil, fmt.Errorf("%w: SSH private key: %v", ErrUnauthorized, sshKeyError(err))
	}
	hostKeys, err := HostKeyCallback(p.conn.KnownHosts)
	if err != nil {
		return nil, err
	}
	timeout := p.f.SSHTimeout
	if timeout == 0 {
		timeout = 20 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	addr := repo.SSHAddress()
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("ls-remote %s: %w", addr, err)
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	cc, chans, reqs, err := ssh.NewClientConn(conn, addr, &ssh.ClientConfig{
		User: repo.SSHUser, Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)}, HostKeyCallback: hostKeys, Timeout: timeout,
	})
	if err != nil {
		if strings.Contains(err.Error(), "unable to authenticate") {
			return nil, fmt.Errorf("ls-remote %s: %w", addr, ErrUnauthorized)
		}
		return nil, fmt.Errorf("ls-remote %s: %w", addr, err)
	}
	client := ssh.NewClient(cc, chans, reqs)
	defer client.Close()
	sess, err := client.NewSession()
	if err != nil {
		return nil, err
	}
	defer sess.Close()
	stdin, err := sess.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := sess.StdoutPipe()
	if err != nil {
		return nil, err
	}
	var stderr bytes.Buffer
	sess.Stderr = &stderr
	path := repo.Path + ".git"
	if strings.Contains(path, "'") {
		return nil, ErrInvalidRepository
	}
	if err := sess.Start("git-upload-pack '" + path + "'"); err != nil {
		return nil, fmt.Errorf("ls-remote %s: %w", addr, err)
	}
	adv, err := readAdvertisement(io.LimitReader(stdout, 64<<20), false)
	_, _ = stdin.Write([]byte("0000"))
	_ = stdin.Close()
	if err != nil {
		// The server's reason arrives on stderr, possibly after stdout
		// closed; the connection's deadline bounds the wait.
		_ = sess.Wait()
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			// e.g. "ERROR: Repository not found."
			return nil, fmt.Errorf("ls-remote %s: %s: %w", repo, shortLine(msg, 200), ErrNotFound)
		}
		return nil, fmt.Errorf("ls-remote %s: %w", repo, err)
	}
	return adv, nil
}

// readAdvertisement parses pkt-lines (git-scm.com/docs/gitprotocol-pack):
// over HTTP a "# service=" line and a flush come first; then
// "<sha> <ref>\0<capabilities>" and "<sha> <ref>" lines up to a flush.
func readAdvertisement(r io.Reader, http bool) (*advertisement, error) {
	br := bufio.NewReader(r)
	adv := &advertisement{refs: map[string]string{}}
	first := true
	if http {
		line, flush, err := readPkt(br)
		if err != nil {
			return nil, err
		}
		if flush || !strings.HasPrefix(line, "# service=") {
			return nil, errors.New("unexpected smart HTTP answer")
		}
		if _, flush, err = readPkt(br); err != nil || !flush {
			return nil, errors.New("unexpected smart HTTP answer")
		}
	}
	for {
		line, flush, err := readPkt(br)
		if err != nil {
			return nil, err
		}
		if flush {
			return adv, nil
		}
		line = strings.TrimSuffix(line, "\n")
		if strings.HasPrefix(line, "ERR ") {
			return nil, fmt.Errorf("%s: %w", strings.TrimPrefix(line, "ERR "), ErrNotFound)
		}
		if first {
			first = false
			if strings.HasPrefix(line, "version ") {
				return nil, errors.New("the server answered with protocol v2, which ls-remote here does not speak")
			}
			var caps string
			line, caps, _ = strings.Cut(line, "\x00")
			for _, c := range strings.Fields(caps) {
				if target, ok := strings.CutPrefix(c, "symref=HEAD:"); ok {
					adv.head = target
				}
			}
		}
		sha, ref, ok := strings.Cut(line, " ")
		if !ok || len(sha) != 40 {
			return nil, fmt.Errorf("unexpected ref line %q", shortLine(line, 80))
		}
		if ref == "capabilities^{}" {
			continue // an empty repository
		}
		adv.refs[ref] = sha
	}
}

// readPkt reads one pkt-line; flush is the 0000 packet.
func readPkt(br *bufio.Reader) (string, bool, error) {
	var head [4]byte
	if _, err := io.ReadFull(br, head[:]); err != nil {
		return "", false, fmt.Errorf("reading refs: %w", err)
	}
	n, err := strconv.ParseUint(string(head[:]), 16, 16)
	if err != nil {
		return "", false, fmt.Errorf("reading refs: bad packet length %q", head)
	}
	if n == 0 {
		return "", true, nil
	}
	if n < 4 {
		return "", false, fmt.Errorf("reading refs: bad packet length %d", n)
	}
	buf := make([]byte, n-4)
	if _, err := io.ReadFull(br, buf); err != nil {
		return "", false, fmt.Errorf("reading refs: %w", err)
	}
	return string(buf), false, nil
}

// ---- SSH host keys -------------------------------------------------------------

type knownHost struct {
	patterns []string
	key      ssh.PublicKey
}

func parseKnownHosts(data []byte) ([]knownHost, error) {
	var out []knownHost
	rest := data
	for len(bytes.TrimSpace(rest)) > 0 {
		marker, hosts, key, _, next, err := ssh.ParseKnownHosts(rest)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("known_hosts: %w", err)
		}
		rest = next
		if marker == "revoked" {
			continue
		}
		out = append(out, knownHost{patterns: hosts, key: key})
	}
	return out, nil
}

// HostKeyCallback checks SSH host keys against known_hosts content (plain or
// hashed host names; "[host]:port" for other ports). Without known_hosts
// nothing is trusted: Kwerft never connects blindly.
func HostKeyCallback(knownHostsData []byte) (ssh.HostKeyCallback, error) {
	entries, err := parseKnownHosts(knownHostsData)
	if err != nil {
		return nil, err
	}
	return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		names := []string{knownhosts.Normalize(hostname)}
		if remote != nil {
			names = append(names, knownhosts.Normalize(remote.String()))
		}
		hostKnown := false
		for _, e := range entries {
			if !matchesAny(e.patterns, names) {
				continue
			}
			if e.key.Type() == key.Type() {
				hostKnown = true
				if bytes.Equal(e.key.Marshal(), key.Marshal()) {
					return nil
				}
			}
		}
		if hostKnown {
			return fmt.Errorf("the SSH host key of %s does not match known_hosts (%s): refusing to connect", hostname, ssh.FingerprintSHA256(key))
		}
		return fmt.Errorf("%s is not in the connection's known_hosts (its key is %s)", hostname, ssh.FingerprintSHA256(key))
	}, nil
}

func matchesAny(patterns, names []string) bool {
	for _, p := range patterns {
		for _, n := range names {
			if p == n || hashedMatch(p, n) {
				return true
			}
		}
	}
	return false
}

// hashedMatch checks a "|1|salt|hash" known_hosts entry.
func hashedMatch(pattern, host string) bool {
	parts := strings.Split(pattern, "|")
	if len(parts) != 4 || parts[0] != "" || parts[1] != "1" {
		return false
	}
	salt, err := base64.StdEncoding.DecodeString(parts[2])
	if err != nil {
		return false
	}
	want, err := base64.StdEncoding.DecodeString(parts[3])
	if err != nil {
		return false
	}
	mac := hmac.New(sha1.New, salt)
	mac.Write([]byte(host))
	return hmac.Equal(mac.Sum(nil), want)
}

var errScanned = errors.New("host key recorded")

// ScanHostKey connects to an SSH server only to read its host key and
// returns it as a known_hosts line with its fingerprint (trust on first use,
// for connections created without known_hosts).
func ScanHostKey(ctx context.Context, addr string) (line, fingerprint string, err error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", addr)
	if err != nil {
		return "", "", err
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	var found ssh.PublicKey
	_, _, _, err = ssh.NewClientConn(conn, addr, &ssh.ClientConfig{
		User: "git",
		HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
			found = key
			return errScanned
		},
		// Ed25519 first: the key type most hosts prefer today.
		HostKeyAlgorithms: []string{ssh.KeyAlgoED25519, ssh.KeyAlgoECDSA256, ssh.KeyAlgoRSASHA256, ssh.KeyAlgoRSASHA512},
	})
	if found == nil {
		if err == nil {
			err = errors.New("no host key received")
		}
		return "", "", err
	}
	return knownhosts.Line([]string{knownhosts.Normalize(addr)}, found), ssh.FingerprintSHA256(found), nil
}
