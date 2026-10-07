// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

package git

import (
	"errors"
	"net"
	"net/url"
	"strings"
)

// Repo is a repository URL in one canonical form. The same repository can be
// written as https://github.com/acme/api, https://github.com/acme/api.git,
// git@github.com:acme/api.git or ssh://git@github.com/acme/api; all parse to
// the same Key, which is how webhooks find the Apps a push is for.
type Repo struct {
	Host string // lower case, without port
	Path string // owner/name, or group/subgroup/name on GitLab; no .git suffix

	// SSH is set when the URL was an SSH one; SSHUser and SSHPort keep its
	// user (default git) and port (default 22) for ls-remote.
	SSH     bool
	SSHUser string
	SSHPort string
	// HTTPPort keeps a non-default port of an https URL.
	HTTPPort string
}

var (
	// ErrInvalidRepository: not a repository URL Kwerft understands.
	ErrInvalidRepository = errors.New("not a repository URL; use https://host/owner/name or git@host:owner/name.git")
	// ErrCredentialsInURL: the URL carries a password or token. Credentials
	// belong in a Git connection, where nobody can read them back.
	ErrCredentialsInURL = errors.New("the URL contains credentials; remove them and use a Git connection instead")
)

// ParseRepository parses https, ssh:// and scp-like (git@host:owner/name)
// repository URLs.
func ParseRepository(raw string) (Repo, error) {
	s := strings.TrimSpace(raw)
	if s == "" || strings.ContainsAny(s, " \t\r\n\\") {
		return Repo{}, ErrInvalidRepository
	}
	var r Repo
	switch {
	case strings.Contains(s, "://"):
		u, err := url.Parse(s)
		if err != nil || u.Host == "" || u.RawQuery != "" || u.Fragment != "" {
			return Repo{}, ErrInvalidRepository
		}
		switch u.Scheme {
		case "https", "http":
			if u.User != nil {
				if _, hasPassword := u.User.Password(); hasPassword || u.User.Username() != "" {
					return Repo{}, ErrCredentialsInURL
				}
			}
			r.HTTPPort = u.Port()
		case "ssh":
			r.SSH = true
			r.SSHUser = "git"
			if u.User != nil && u.User.Username() != "" {
				if _, hasPassword := u.User.Password(); hasPassword {
					return Repo{}, ErrCredentialsInURL
				}
				r.SSHUser = u.User.Username()
			}
			r.SSHPort = u.Port()
		default:
			return Repo{}, ErrInvalidRepository
		}
		r.Host = u.Hostname()
		r.Path = u.Path
	default:
		// scp-like: [user@]host:path
		hostPart, path, ok := strings.Cut(s, ":")
		if !ok || strings.Contains(hostPart, "/") {
			return Repo{}, ErrInvalidRepository
		}
		r.SSH = true
		r.SSHUser = "git"
		if user, host, ok := strings.Cut(hostPart, "@"); ok {
			if user == "" {
				return Repo{}, ErrInvalidRepository
			}
			r.SSHUser, hostPart = user, host
		}
		r.Host = hostPart
		r.Path = path
	}
	r.Host = strings.ToLower(strings.TrimSuffix(r.Host, "."))
	if r.Host == "" || net.ParseIP(strings.Trim(r.Host, "[]")) == nil && !validHost(r.Host) {
		return Repo{}, ErrInvalidRepository
	}
	p := strings.Trim(r.Path, "/")
	p = strings.TrimSuffix(p, ".git")
	segs := strings.Split(p, "/")
	if len(segs) < 2 {
		return Repo{}, ErrInvalidRepository
	}
	for _, seg := range segs {
		if seg == "" || seg == "." || seg == ".." || strings.HasPrefix(seg, "-") {
			return Repo{}, ErrInvalidRepository
		}
	}
	r.Path = p
	return r, nil
}

func validHost(h string) bool {
	if len(h) > 253 {
		return false
	}
	for _, label := range strings.Split(h, ".") {
		if label == "" || len(label) > 63 {
			return false
		}
		for _, c := range label {
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
				return false
			}
		}
	}
	return true
}

// Key identifies the repository whatever URL form named it.
func (r Repo) Key() string { return r.Host + "/" + strings.ToLower(r.Path) }

// Owner is the account, organisation or top-level group.
func (r Repo) Owner() string {
	owner, _, _ := strings.Cut(r.Path, "/")
	return owner
}

// Name is the repository's own name (the last path segment).
func (r Repo) Name() string { return r.Path[strings.LastIndexByte(r.Path, '/')+1:] }

// HTTPS is the repository's clone URL over HTTPS.
func (r Repo) HTTPS() string {
	host := r.Host
	if r.HTTPPort != "" && r.HTTPPort != "443" {
		host = net.JoinHostPort(r.Host, r.HTTPPort)
	}
	return "https://" + host + "/" + r.Path + ".git"
}

// SSHAddress is host:port for ls-remote over SSH.
func (r Repo) SSHAddress() string {
	port := r.SSHPort
	if port == "" {
		port = "22"
	}
	return net.JoinHostPort(r.Host, port)
}

// String is the canonical HTTPS form without .git, for messages.
func (r Repo) String() string { return strings.TrimSuffix(r.HTTPS(), ".git") }

// SameRepository reports whether two URLs name the same repository.
func SameRepository(a, b string) bool {
	ra, err := ParseRepository(a)
	if err != nil {
		return false
	}
	rb, err := ParseRepository(b)
	return err == nil && ra.Key() == rb.Key()
}
