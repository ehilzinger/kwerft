package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/ehilzinger/kwerft/internal/auth"
	"github.com/ehilzinger/kwerft/internal/store"
)

// Data-key rotation. KWERFT_DATA_KEY (from the Secret kwerft-data-key, key
// "key") seals secrets in SQLite with AES-256-GCM: today the authenticator
// app seeds. Sealed values name their key ("v2:<key ID>:…", auth.Sealer), so
// several keys can be known at once.
//
// Rotating from Settings (owners, with their password):
//  1. A new key is generated and written to the Secret as "key", the old one
//     as "previous" — before anything is re-sealed, so a restart at any
//     point finds both (KWERFT_DATA_KEY_PREVIOUS).
//  2. The running console makes the new key primary and re-seals every
//     value in one transaction.
//  3. "previous" is removed from the Secret: the old key is retired.
//
// If step 2 fails, both keys stay and the console finishes at its next start
// (resealAtStart). The same start-up step is the manual path: put the new
// key in "key" and the old one in "previous", restart, then remove
// "previous". It also moves values from Kwerft 0.1's format ("v1:", no key
// ID) to the current one.
//
// The Secret is written with the console's own identity, not the owner's:
// a Kubernetes patch answers with the whole object, so a patch right on this
// Secret would let owners read the data key, which nobody should. The
// console checks the owner role and the password itself.

// DataKeySecretKeys are the keys of the data-key Secret.
const (
	DataKeySecretKey      = "key"
	DataKeySecretPrevious = "previous"
)

type dataKeyAPI struct {
	*api
	// save writes the keys to where the console reads them at start-up;
	// previous nil removes it. Nil: rotation is unavailable.
	save func(ctx context.Context, primary, previous []byte) error

	mu      sync.Mutex // one rotation at a time
	current []byte     // the primary key's bytes, written as "previous" next time
}

func (a *api) registerDataKey(mux *http.ServeMux) {
	d := &dataKeyAPI{api: a, current: a.cfg.DataKey}
	if a.cfg.System != nil && a.cfg.DataKeySecret.Name != "" {
		d.save = d.saveToSecret
	}
	if a.cfg.dataKeyHook != nil {
		a.cfg.dataKeyHook(d)
	}
	owner := func(h http.HandlerFunc) http.HandlerFunc { return a.requireUser(a.requireRole(h, store.RoleOwner)) }
	mux.HandleFunc("GET /api/v1/settings/data-key", owner(d.status))
	mux.HandleFunc("POST /api/v1/settings/data-key/rotate", a.sameOrigin(owner(d.rotate)))
}

// saveToSecret merges the keys into the data-key Secret.
func (d *dataKeyAPI) saveToSecret(ctx context.Context, primary, previous []byte) error {
	// The environment variables hold the keys' text form (base64), which
	// the Secret stores base64-encoded once more ([]byte marshals so); null
	// removes "previous".
	data := map[string]any{DataKeySecretKey: []byte(auth.EncodeDataKey(primary)), DataKeySecretPrevious: nil}
	if previous != nil {
		data[DataKeySecretPrevious] = []byte(auth.EncodeDataKey(previous))
	}
	raw, err := json.Marshal(map[string]any{"data": data})
	if err != nil {
		return err
	}
	sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: d.cfg.DataKeySecret.Namespace, Name: d.cfg.DataKeySecret.Name}}
	return d.cfg.System.Patch(ctx, sec, client.RawPatch(types.MergePatchType, raw))
}

func (d *dataKeyAPI) status(w http.ResponseWriter, r *http.Request) {
	s := d.mfa.sealer
	if s == nil {
		writeJSON(w, http.StatusOK, map[string]any{"available": false})
		return
	}
	total, current, err := d.store.SealedValues(r.Context(), s.Current)
	if err != nil {
		d.internalError(w, r, err)
		return
	}
	ids := s.KeyIDs()
	writeJSON(w, http.StatusOK, map[string]any{
		"available": true, "keyId": ids[0], "otherKeyIds": ids[1:], "sealed": total, "upToDate": current,
		"canRotate": d.save != nil,
		"secret":    d.cfg.DataKeySecret.Namespace + "/" + d.cfg.DataKeySecret.Name,
	})
}

func (d *dataKeyAPI) rotate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Password string `json:"password"`
	}
	if !decode(w, r, &req) {
		return
	}
	p := principalOf(r)
	sealer := d.mfa.sealer
	switch {
	case sealer == nil:
		writeError(w, http.StatusServiceUnavailable, "This console runs without a data key.")
		return
	case d.save == nil:
		writeError(w, http.StatusServiceUnavailable,
			"Rotation needs the console's connection to the cluster. Rotate by hand instead: see docs/plan.md (data key).")
		return
	}
	if !d.confirmIdentity(w, r, p.user, req.Password) {
		return
	}
	if !d.mu.TryLock() {
		writeError(w, http.StatusConflict, "A rotation is already running.")
		return
	}
	defer d.mu.Unlock()
	ctx := r.Context()
	oldID := sealer.PrimaryID()
	next, err := auth.ParseDataKey(auth.NewDataKey())
	if err != nil {
		d.internalError(w, r, err)
		return
	}
	// 1. Both keys where a restart finds them.
	if err := d.save(ctx, next, d.current); err != nil {
		d.audit(r, p.user.Email, "settings.data_key_rotate_failed", oldID, "could not store the new key")
		d.internalError(w, r, fmt.Errorf("store the new data key: %w", err))
		return
	}
	// 2. Seal with the new key; re-seal everything.
	if err := sealer.Rotate(next); err != nil {
		d.internalError(w, r, err)
		return
	}
	d.current = next
	n, unreadable, err := d.reseal(ctx)
	if err != nil {
		d.audit(r, p.user.Email, "settings.data_key_rotate_failed", oldID+" → "+sealer.PrimaryID(), "re-sealing failed; finishes at the next start")
		d.internalError(w, r, fmt.Errorf("re-seal with the new data key: %w", err))
		return
	}
	// 3. Retire the old key. A value sealed with it after the re-seal (an
	// authenticator app set up during these milliseconds) is caught by a
	// second pass. Values that no key opened were broken before; the old key
	// stays in the Secret then, in case it is needed to investigate.
	retired := false
	if m, again, err := d.reseal(ctx); err == nil && unreadable == 0 && again == 0 {
		n += m
		if err := d.save(ctx, next, nil); err != nil {
			d.cfg.Logger.Error("could not remove the previous data key from its Secret", "err", err)
		} else {
			retired = true
		}
	}
	detail := fmt.Sprintf("%d values re-sealed", n)
	if unreadable > 0 {
		detail += fmt.Sprintf(", %d unreadable with any key", unreadable)
	}
	d.audit(r, p.user.Email, "settings.data_key_rotated", oldID+" → "+sealer.PrimaryID(), detail)
	writeJSON(w, http.StatusOK, map[string]any{"keyId": sealer.PrimaryID(), "previousKeyId": oldID, "resealed": n,
		"unreadable": unreadable, "previousRetired": retired})
}

// reseal moves every sealed value to the primary key. Values no known key
// opens are left as they are and counted.
func (a *api) reseal(ctx context.Context) (resealed, unreadable int, err error) {
	s := a.mfa.sealer
	if s == nil {
		return 0, 0, nil
	}
	resealed, err = a.store.ResealSecrets(ctx, func(kind, owner, sealed string) (string, bool, error) {
		var out string
		var changed bool
		var err error
		switch kind {
		case store.SealedTOTP:
			out, changed, err = s.Reseal(sealed, totpContext(owner))
		default:
			return "", false, fmt.Errorf("unknown sealed value %q", kind)
		}
		if errors.Is(err, auth.ErrSealed) {
			unreadable++
			return sealed, false, nil
		}
		return out, changed, err
	})
	return resealed, unreadable, err
}

// resealAtStart finishes a rotation (KWERFT_DATA_KEY_PREVIOUS set) and
// upgrades values of the 0.1 format. A value no known key opens is left
// alone and logged: its user sets up the authenticator app again.
func (a *api) resealAtStart(ctx context.Context) {
	if a.mfa.sealer == nil {
		return
	}
	n, unreadable, err := a.reseal(ctx)
	if err != nil {
		a.cfg.Logger.Error("re-sealing secrets with the data key failed", "err", err)
		return
	}
	if n > 0 {
		a.cfg.Logger.Info("secrets re-sealed with the current data key", "count", n, "key", a.mfa.sealer.PrimaryID())
	}
	if unreadable > 0 {
		a.cfg.Logger.Error("secrets in the database that none of the data keys opens; was KWERFT_DATA_KEY changed without KWERFT_DATA_KEY_PREVIOUS?",
			"count", unreadable, "keys", a.mfa.sealer.KeyIDs())
		return
	}
	if len(a.mfa.sealer.KeyIDs()) > 1 {
		a.cfg.Logger.Warn("every secret uses the current data key: remove \"previous\" from the data-key Secret",
			"key", a.mfa.sealer.PrimaryID())
	}
}
