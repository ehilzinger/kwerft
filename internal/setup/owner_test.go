// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

package setup

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/ehilzinger/kwerft/internal/auth"
	"github.com/ehilzinger/kwerft/internal/store"
)

const testNS = "kwerft-system"

type ownerEnv struct {
	c    client.Client
	st   *store.Store
	logs *bytes.Buffer
	b    *OwnerBootstrap
	tok  *StaticTokenSource
}

func newOwnerEnv(t *testing.T, data map[string]string, funcs *interceptor.Funcs) *ownerEnv {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "kwerft.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	builder := fake.NewClientBuilder().WithScheme(scheme.Scheme)
	if data != nil {
		builder = builder.WithObjects(&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: OwnerSecretName},
			Data:       bytesMap(data),
		})
	}
	if funcs != nil {
		builder = builder.WithInterceptorFuncs(*funcs)
	}
	c := builder.Build()
	logs := &bytes.Buffer{}
	tok := NewStaticTokenSource("kwft_setup_x", time.Hour)
	return &ownerEnv{c: c, st: st, logs: logs, tok: tok, b: &OwnerBootstrap{
		Reader: c, Writer: c, Namespace: testNS, Store: st, Tokens: tok,
		Logger: slog.New(slog.NewTextHandler(logs, nil)), Interval: time.Millisecond,
	}}
}

func bytesMap(m map[string]string) map[string][]byte {
	out := map[string][]byte{}
	for k, v := range m {
		out[k] = []byte(v)
	}
	return out
}

func (e *ownerEnv) secret(t *testing.T) map[string]string {
	t.Helper()
	var sec corev1.Secret
	if err := e.c.Get(context.Background(), client.ObjectKey{Namespace: testNS, Name: OwnerSecretName}, &sec); err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for k, v := range sec.Data {
		out[k] = string(v)
	}
	return out
}

const ownerPassword = "correct horse battery staple"

func TestOwnerFromConfigIsCreatedAndThePasswordRemoved(t *testing.T) {
	e := newOwnerEnv(t, map[string]string{"email": " you@example.com\n", "name": "Ada", "password": ownerPassword + "\n"}, nil)
	ctx := context.Background()
	done, err := e.b.Once(ctx)
	if err != nil || !done {
		t.Fatalf("Once: done %v, err %v", done, err)
	}
	u, err := e.st.UserByEmail(ctx, "you@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if u.Role != store.RoleOwner || u.Name != "Ada" {
		t.Errorf("user %+v: want owner Ada", u)
	}
	// The trailing newline of the password file is not part of the password.
	if ok, err := auth.VerifyPassword(u.PasswordHash, ownerPassword); err != nil || !ok {
		t.Errorf("password does not verify: %v %v", ok, err)
	}
	got := e.secret(t)
	if got["status"] != OwnerCreated || got["email"] != "you@example.com" || len(got) != 2 {
		t.Errorf("secret after: %v, want email and status created only", got)
	}
	entries, err := e.st.RecentAudit(ctx, 10)
	if err != nil || len(entries) != 1 || entries[0].Action != "setup.owner_created" || entries[0].Actor != "setup" || entries[0].Target != "you@example.com" {
		t.Errorf("audit: %+v %v", entries, err)
	}
	if _, _, err := e.tok.Hash(ctx); !errors.Is(err, ErrNoToken) {
		t.Errorf("setup token still active: %v", err)
	}
	if strings.Contains(e.logs.String(), ownerPassword) {
		t.Error("the password was logged")
	}
	// A restart finds nothing to do.
	if done, err := e.b.Once(ctx); err != nil || !done {
		t.Errorf("second Once: done %v, err %v", done, err)
	}
	if n, _ := e.st.CountUsers(ctx); n != 1 {
		t.Errorf("%d users, want 1", n)
	}
}

func TestOwnerFromConfigNameDefaultsToTheAddress(t *testing.T) {
	e := newOwnerEnv(t, map[string]string{"email": "ops@example.com", "password": ownerPassword}, nil)
	if _, err := e.b.Once(context.Background()); err != nil {
		t.Fatal(err)
	}
	u, err := e.st.UserByEmail(context.Background(), "ops@example.com")
	if err != nil || u.Name != "ops" {
		t.Fatalf("user %+v, %v: want name ops", u, err)
	}
}

func TestOwnerFromConfigWhenSetupIsComplete(t *testing.T) {
	e := newOwnerEnv(t, map[string]string{"email": "late@example.com", "password": ownerPassword}, nil)
	ctx := context.Background()
	if err := e.st.CreateOwner(ctx, &store.User{Name: "First", Email: "first@example.com", PasswordHash: "x"}); err != nil {
		t.Fatal(err)
	}
	if done, err := e.b.Once(ctx); err != nil || !done {
		t.Fatalf("Once: done %v, err %v", done, err)
	}
	if got := e.secret(t); got["status"] != OwnerExists || got["password"] != "" {
		t.Errorf("secret after: %v, want status exists without the password", got)
	}
	if _, err := e.st.UserByEmail(ctx, "late@example.com"); err == nil {
		t.Error("a second owner was created")
	}
}

func TestOwnerFromConfigCreatedButNotRecorded(t *testing.T) {
	// The first attempt created the owner but could not update the Secret:
	// the retry reports the owner as created, not as someone else's setup.
	fail := true
	e := newOwnerEnv(t, map[string]string{"email": "you@example.com", "password": ownerPassword}, &interceptor.Funcs{
		Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			if fail {
				fail = false
				return apierrors.NewConflict(corev1.Resource("secrets"), OwnerSecretName, errors.New("modified"))
			}
			return c.Update(ctx, obj, opts...)
		},
	})
	ctx := context.Background()
	if _, err := e.b.Once(ctx); err == nil {
		t.Fatal("want the update error")
	}
	if done, err := e.b.Once(ctx); err != nil || !done {
		t.Fatalf("retry: done %v, err %v", done, err)
	}
	if got := e.secret(t); got["status"] != OwnerCreated {
		t.Errorf("secret after: %v, want status created", got)
	}
	if entries, _ := e.st.RecentAudit(ctx, 10); len(entries) != 1 {
		t.Errorf("audit: %+v, want one owner_created", entries)
	}
}

func TestOwnerFromConfigRejected(t *testing.T) {
	for name, data := range map[string]map[string]string{
		"short password": {"email": "you@example.com", "password": "short\n"},
		"bad email":      {"email": "you at example", "password": ownerPassword},
	} {
		t.Run(name, func(t *testing.T) {
			e := newOwnerEnv(t, data, nil)
			ctx := context.Background()
			done, err := e.b.Once(ctx)
			if err != nil || done {
				t.Fatalf("Once: done %v, err %v; want not done (setup still pending)", done, err)
			}
			got := e.secret(t)
			if got["status"] != OwnerRejected || got["reason"] == "" || got["password"] != "" {
				t.Errorf("secret after: %v, want rejected with a reason and without the password", got)
			}
			if n, _ := e.st.CountUsers(ctx); n != 0 {
				t.Errorf("%d users, want none", n)
			}
			if _, _, err := e.tok.Hash(ctx); err != nil {
				t.Errorf("the setup token must stay usable: %v", err)
			}
			if strings.Contains(e.logs.String(), data["password"]) || strings.Contains(got["reason"], data["password"]) {
				t.Error("the password was logged or recorded")
			}
		})
	}
}

func TestOwnerBootstrapWithoutSecret(t *testing.T) {
	e := newOwnerEnv(t, nil, nil)
	ctx := context.Background()
	// Setup pending: keep looking (an installer re-run may add the owner).
	if done, err := e.b.Once(ctx); err != nil || done {
		t.Fatalf("no users: done %v, err %v", done, err)
	}
	if err := e.st.CreateOwner(ctx, &store.User{Name: "W", Email: "w@example.com", PasswordHash: "x"}); err != nil {
		t.Fatal(err)
	}
	if done, err := e.b.Once(ctx); err != nil || !done {
		t.Fatalf("owner exists: done %v, err %v", done, err)
	}
}

func TestOwnerBootstrapRunPicksUpALaterSecret(t *testing.T) {
	e := newOwnerEnv(t, nil, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	finished := make(chan struct{})
	go func() { e.b.Run(ctx); close(finished) }()
	if err := e.c.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: OwnerSecretName},
		Data:       bytesMap(map[string]string{"email": "you@example.com", "password": ownerPassword}),
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-finished:
	case <-ctx.Done():
		t.Fatal("Run did not finish after creating the owner")
	}
	if n, _ := e.st.CountUsers(context.Background()); n != 1 {
		t.Errorf("%d users, want 1", n)
	}
}
