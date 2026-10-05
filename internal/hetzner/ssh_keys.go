package hetzner

import (
	"context"
	"errors"
	"net/http"
	"strconv"
)

// SSHKey is a public key stored in the Cloud project, put on new servers.
type SSHKey struct {
	ID          int64             `json:"id"`
	Name        string            `json:"name"`
	Fingerprint string            `json:"fingerprint"`
	PublicKey   string            `json:"public_key"`
	Labels      map[string]string `json:"labels"`
}

// SSHKeys lists the project's SSH keys matching labelSelector ("" for all).
func (c *Client) SSHKeys(ctx context.Context, labelSelector string) ([]SSHKey, error) {
	return pagedList[SSHKey](ctx, c, "/ssh_keys", "ssh_keys", selector(labelSelector))
}

// CreateSSHKey stores a public key in the project.
func (c *Client) CreateSSHKey(ctx context.Context, name, publicKey string, labels map[string]string) (SSHKey, error) {
	var body struct {
		SSHKey SSHKey `json:"ssh_key"`
	}
	err := c.do(ctx, http.MethodPost, "/ssh_keys", map[string]any{"name": name, "public_key": publicKey, "labels": labels}, &body)
	return body.SSHKey, err
}

// DeleteSSHKey removes a key; a missing one is not an error.
func (c *Client) DeleteSSHKey(ctx context.Context, id int64) error {
	err := c.do(ctx, http.MethodDelete, "/ssh_keys/"+strconv.FormatInt(id, 10), nil, nil)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

// ServerSSHKeys are the keys for new servers: those labelled LabelSSHKey,
// or every key of the project when none is. Without any key Hetzner mails
// a root password instead.
func (c *Client) ServerSSHKeys(ctx context.Context) ([]string, error) {
	keys, err := c.SSHKeys(ctx, LabelSSHKey)
	if err != nil {
		return nil, err
	}
	if len(keys) == 0 {
		if keys, err = c.SSHKeys(ctx, ""); err != nil {
			return nil, err
		}
	}
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, strconv.FormatInt(k.ID, 10))
	}
	return out, nil
}
