// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

package server

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ehilzinger/kwerft/internal/auth"
	"github.com/ehilzinger/kwerft/internal/store"
)

// keyStore stands in for the kwerft-data-key Secret.
type keyStore struct {
	mu       sync.Mutex
	primary  []byte
	previous []byte
	writes   int
	fail     bool
}

func (k *keyStore) save(_ context.Context, primary, previous []byte) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.fail {
		return errors.New("secret not writable")
	}
	k.primary, k.previous = primary, previous
	k.writes++
	return nil
}

func withKeyStore(k *keyStore) func(*Config) {
	return func(c *Config) {
		c.DataKey = testDataKey
		c.dataKeyHook = func(d *dataKeyAPI) { d.save = k.save }
	}
}

func TestDataKeyRotation(t *testing.T) {
	keys := &keyStore{}
	e := newEnv(t, withKeyStore(keys))
	e.completeSetup(t)
	secret, _ := e.enrollTOTP(t)
	u := mustUser(t, e.store, owner["email"])
	oldID := auth.KeyID(testDataKey)

	code, out := e.call(t, "GET", "/api/v1/settings/data-key", nil, nil)
	if code != http.StatusOK || out["keyId"] != oldID || out["sealed"] != float64(1) || out["upToDate"] != float64(1) || out["canRotate"] != true {
		t.Fatalf("status: %d %v", code, out)
	}
	// Owners only, with their password.
	admin, _ := e.member(t, "admin@example.com", store.RoleAdmin)
	if code, _ := admin.call(t, "POST", "/api/v1/settings/data-key/rotate", map[string]string{"password": "member password 1"}, nil); code != http.StatusForbidden {
		t.Errorf("admin rotates: %d", code)
	}
	if code, out := e.call(t, "POST", "/api/v1/settings/data-key/rotate", map[string]string{"password": "wrong password!"}, nil); code != http.StatusBadRequest || out["field"] != "password" {
		t.Errorf("wrong password: %d %v", code, out)
	}
	// A failure to store the new key changes nothing.
	keys.fail = true
	if code, _ := e.call(t, "POST", "/api/v1/settings/data-key/rotate", map[string]string{"password": owner["password"]}, nil); code != http.StatusInternalServerError {
		t.Errorf("store failure: %d", code)
	}
	if _, out := e.call(t, "GET", "/api/v1/settings/data-key", nil, nil); out["keyId"] != oldID {
		t.Errorf("key changed after a failed rotation: %v", out)
	}
	keys.fail = false

	code, out = e.call(t, "POST", "/api/v1/settings/data-key/rotate", map[string]string{"password": owner["password"]}, nil)
	if code != http.StatusOK || out["previousKeyId"] != oldID || out["keyId"] == oldID || out["resealed"] != float64(1) || out["previousRetired"] != true {
		t.Fatalf("rotate: %d %v", code, out)
	}
	newID := out["keyId"].(string)
	// The Secret ends with the new key alone; on the way it held both.
	if keys.writes != 2 || keys.previous != nil || auth.KeyID(keys.primary) != newID {
		t.Errorf("secret: %d writes, primary %s, previous %v", keys.writes, auth.KeyID(keys.primary), keys.previous)
	}
	row, _ := e.store.TOTPFor(context.Background(), u.ID)
	if !strings.HasPrefix(row.Secret, "v2:"+newID+":") {
		t.Errorf("TOTP secret still sealed with the old key: %q", row.Secret)
	}
	// The new key alone opens it, and the authenticator still works.
	only, _ := auth.NewSealer(keys.primary)
	if pt, err := only.Open(row.Secret, totpContext(u.ID)); err != nil || string(pt) != string(secret) {
		t.Errorf("open with the new key: %v", err)
	}
	e.clock.Advance(30 * time.Second)
	c := e.newClient()
	if code, out := c.login(t); code != http.StatusOK || out["secondFactor"] == nil {
		t.Fatalf("login: %d %v", code, out)
	}
	if code, _ := c.call(t, "POST", "/api/v1/session/second-factor/totp", map[string]string{"code": auth.TOTPCode(secret, e.clock.Now())}, nil); code != http.StatusOK {
		t.Errorf("TOTP after rotation: %d", code)
	}
	if a := lastAudit(t, e.store, "settings.data_key_rotated"); a == nil || a.Target != oldID+" → "+newID {
		t.Errorf("audit %+v", a)
	}
}

// sealV1 seals the way Kwerft 0.1 did, without a key ID.
func sealV1(t *testing.T, key []byte, pt []byte, context string) string {
	t.Helper()
	block, _ := aes.NewCipher(key)
	aead, _ := cipher.NewGCM(block)
	nonce := make([]byte, aead.NonceSize())
	return "v1:" + base64.RawStdEncoding.EncodeToString(aead.Seal(nonce, nonce, pt, []byte(context)))
}

// The upgrade path and the manual rotation: at start-up the console
// re-seals values of the 0.1 format and values sealed with the previous key.
func TestDataKeyResealAtStart(t *testing.T) {
	e := newEnv(t, func(c *Config) { c.DataKey = testDataKey })
	e.completeSetup(t)
	ctx := context.Background()
	u := mustUser(t, e.store, owner["email"])
	other := &store.User{Email: "b@example.com", Name: "B", PasswordHash: "x", Role: store.RoleViewer}
	if err := e.store.CreateUser(ctx, other); err != nil {
		t.Fatal(err)
	}
	seed := []byte("12345678901234567890")
	_ = e.store.StartTOTP(ctx, u.ID, sealV1(t, testDataKey, seed, totpContext(u.ID)), time.Now())
	broken, _ := auth.ParseDataKey(auth.NewDataKey())
	_ = e.store.StartTOTP(ctx, other.ID, sealV1(t, broken, seed, totpContext(other.ID)), time.Now())

	// A restart with a new key and the old one as previous.
	next, _ := auth.ParseDataKey(auth.NewDataKey())
	cfg := Config{Store: e.store, DataKey: next, DataKeyPrevious: [][]byte{testDataKey}}
	cfg.Logger = slog.New(slog.DiscardHandler)
	newAPI(cfg)
	row, _ := e.store.TOTPFor(ctx, u.ID)
	if !strings.HasPrefix(row.Secret, "v2:"+auth.KeyID(next)+":") {
		t.Fatalf("not re-sealed at start: %q", row.Secret)
	}
	only, _ := auth.NewSealer(next)
	if pt, err := only.Open(row.Secret, totpContext(u.ID)); err != nil || string(pt) != string(seed) {
		t.Errorf("re-sealed value: %v", err)
	}
	// A value no key opens stays as it was.
	if row, _ := e.store.TOTPFor(ctx, other.ID); !strings.HasPrefix(row.Secret, "v1:") {
		t.Errorf("unreadable value changed: %q", row.Secret)
	}
}

func TestDataKeyRotationUnavailable(t *testing.T) {
	e := newEnv(t, func(c *Config) { c.DataKey = testDataKey }) // no System client: nowhere to store a key
	e.completeSetup(t)
	if _, out := e.call(t, "GET", "/api/v1/settings/data-key", nil, nil); out["canRotate"] != false {
		t.Errorf("status %v", out)
	}
	if code, _ := e.call(t, "POST", "/api/v1/settings/data-key/rotate", map[string]string{"password": owner["password"]}, nil); code != http.StatusServiceUnavailable {
		t.Errorf("rotate: %d", code)
	}
}
