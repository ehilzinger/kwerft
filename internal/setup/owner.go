// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

package setup

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/ehilzinger/kwerft/internal/auth"
	"github.com/ehilzinger/kwerft/internal/store"
)

// The owner from `install.sh --config` (its owner: block) reaches the
// console as Secret kwerft-bootstrap in Kwerft's namespace, written by the
// installer's Kwerft stage:
//
//	email     the owner's address
//	name      their name (optional; the address's local part otherwise)
//	password  the contents of owner.passwordFile
//
// The console creates the owner from it when no user exists yet, then
// replaces the Secret's data with the outcome, so the password does not stay
// in the cluster:
//
//	email     as before
//	status    created (the owner with this address exists), exists (setup
//	          was already complete with other accounts; nothing was created)
//	          or rejected (see reason)
//	reason    why the owner was rejected (never the password)
//
// The installer's Handoff stage waits for the status, reports it and deletes
// the Secret; when the owner was not created it hands out a setup token.
const (
	OwnerSecretName = "kwerft-bootstrap"

	OwnerCreated  = "created"
	OwnerExists   = "exists"
	OwnerRejected = "rejected"
)

// OwnerBootstrap turns the installer's kwerft-bootstrap Secret into the
// owner account. It is safe with several consoles and restarts: the store
// creates at most one owner, and a Secret without a password is done.
type OwnerBootstrap struct {
	Reader    client.Reader // uncached, so a Secret the installer writes later is seen
	Writer    client.Writer
	Namespace string
	Store     *store.Store
	// Tokens, when set, has its setup token consumed once the owner exists,
	// like the setup wizard does.
	Tokens   TokenSource
	Logger   *slog.Logger
	Interval time.Duration // between checks while setup is pending; default 5s
	Now      func() time.Time
}

// Run checks for the Secret until an owner exists and no password waits in
// the cluster: at start, and every Interval while setup is pending (an
// installer re-run may add the owner to a console that is already running).
func (b *OwnerBootstrap) Run(ctx context.Context) {
	interval := b.Interval
	if interval <= 0 {
		interval = 5 * time.Second
	}
	wait := interval
	for {
		done, err := b.Once(ctx)
		if done {
			return
		}
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			b.log().Warn("cannot check the owner from install.sh --config", "secret", OwnerSecretName, "err", err)
			wait = min(2*wait, time.Minute)
		} else {
			wait = interval
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// Once handles the Secret if it holds a password. done reports that an owner
// exists and nothing is left to do.
func (b *OwnerBootstrap) Once(ctx context.Context) (done bool, err error) {
	var sec corev1.Secret
	err = b.Reader.Get(ctx, client.ObjectKey{Namespace: b.Namespace, Name: OwnerSecretName}, &sec)
	if err != nil && !apierrors.IsNotFound(err) {
		return false, err
	}
	if err != nil || len(sec.Data["password"]) == 0 {
		n, err := b.Store.CountUsers(ctx)
		return err == nil && n > 0, err
	}

	email := strings.TrimSpace(string(sec.Data["email"]))
	status, reason, err := b.createOwner(ctx, email, strings.TrimSpace(string(sec.Data["name"])),
		strings.TrimRight(string(sec.Data["password"]), "\r\n"))
	if err != nil {
		return false, err
	}
	// The outcome replaces the password; the installer reads and deletes it.
	sec.Data = map[string][]byte{"email": []byte(email), "status": []byte(status)}
	if reason != "" {
		sec.Data["reason"] = []byte(reason)
	}
	sec.StringData = nil
	if err := b.Writer.Update(ctx, &sec); err != nil {
		return false, err
	}
	return status != OwnerRejected, nil
}

func (b *OwnerBootstrap) log() *slog.Logger {
	if b.Logger == nil {
		return slog.Default()
	}
	return b.Logger
}

func (b *OwnerBootstrap) createOwner(ctx context.Context, email, name, password string) (status, reason string, err error) {
	if !auth.ValidEmail(email) {
		b.log().Warn("the owner from install.sh --config was rejected: owner.email is not an email address")
		return OwnerRejected, "owner.email must be an address like you@example.com", nil
	}
	if err := auth.CheckPassword(password); err != nil {
		b.log().Warn("the owner from install.sh --config was rejected: the password does not qualify", "email", email, "err", err)
		return OwnerRejected, "the password in owner.passwordFile does not qualify: " + err.Error(), nil
	}
	if name == "" {
		name, _, _ = strings.Cut(email, "@")
	}
	hash, err := auth.HashPassword(password)
	if err != nil {
		return "", "", err
	}
	u := &store.User{Name: name, Email: email, PasswordHash: hash}
	if err := b.Store.CreateOwner(ctx, u); err != nil {
		if !errors.Is(err, store.ErrSetupComplete) {
			return "", "", err
		}
		// Created by an earlier attempt that could not record it, or by the
		// setup wizard with the same address: the owner exists either way.
		existing, err := b.Store.UserByEmail(ctx, email)
		if err == nil && existing.Role == store.RoleOwner {
			return OwnerCreated, "", nil
		}
		b.log().Info("setup was already complete; the owner from install.sh --config was not created", "email", email)
		return OwnerExists, "", nil
	}
	now := time.Now
	if b.Now != nil {
		now = b.Now
	}
	if err := b.Store.Audit(ctx, store.AuditEntry{At: now(), Actor: "setup", Action: "setup.owner_created",
		Target: email, Detail: "from install.sh --config"}); err != nil {
		b.log().Error("audit write failed", "action", "setup.owner_created", "err", err)
	}
	b.log().Info("owner created from install.sh --config", "email", email)
	if b.Tokens != nil {
		// A setup token from an earlier run is of no use any more.
		if err := b.Tokens.Consume(ctx); err != nil {
			b.log().Error("could not delete the setup token", "err", err)
		}
	}
	return OwnerCreated, "", nil
}
