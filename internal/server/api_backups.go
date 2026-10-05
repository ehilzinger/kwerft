package server

import (
	"cmp"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
	"github.com/ehilzinger/kwerft/internal/access"
	"github.com/ehilzinger/kwerft/internal/alerting"
	"github.com/ehilzinger/kwerft/internal/backups"
	"github.com/ehilzinger/kwerft/internal/controllers"
)

// Backups (docs/phase6.md): Settings › Backups (the bucket, its access keys,
// the recovery key, etcd snapshots) and the Backups page (plans, "Back up
// now", the backups Velero made, restores). Owners and admins only, every
// request as the signed-in user and every write audited.
//
// Nothing secret is ever returned: the access keys and the recovery key
// are written into Secrets the user's role may patch but not read
// (roles.yaml), like the DNS token. The one exception is the recovery key
// the console generates on the first save: the answer to that request
// carries it, once, for the owner to store.

type backupsAPI struct {
	*api
	s3 *backups.Client
}

func (a *api) registerBackups(mux *http.ServeMux) {
	b := &backupsAPI{api: a, s3: &backups.Client{HTTP: &http.Client{Timeout: 20 * time.Second}, Now: a.now}}
	if a.cfg.backupsHook != nil {
		a.cfg.backupsHook(b)
	}
	read := func(h http.HandlerFunc) http.HandlerFunc {
		return a.requireUser(a.requireKube(a.requireRole(h, access.RolesWith(access.Backups)...)))
	}
	write := func(h http.HandlerFunc) http.HandlerFunc { return a.sameOrigin(read(h)) }

	mux.HandleFunc("GET /api/v1/settings/backups", read(b.target))
	mux.HandleFunc("PUT /api/v1/settings/backups", write(b.saveTarget))
	mux.HandleFunc("POST /api/v1/settings/backups/check", write(b.checkTarget))

	mux.HandleFunc("GET /api/v1/backups", read(b.list))
	mux.HandleFunc("GET /api/v1/backups/plans", read(b.plans))
	mux.HandleFunc("POST /api/v1/backups/plans", write(b.planCreate))
	mux.HandleFunc("PUT /api/v1/backups/plans/{name}", write(b.planUpdate))
	mux.HandleFunc("DELETE /api/v1/backups/plans/{name}", write(b.planDelete))
	mux.HandleFunc("POST /api/v1/backups/plans/{name}/run", write(b.planRun))
	mux.HandleFunc("GET /api/v1/backups/restores", read(b.restores))
	mux.HandleFunc("POST /api/v1/backups/restores", write(b.restoreCreate))
}

// ---- the target -----------------------------------------------------------------------

type etcdSnapshotsJSON struct {
	Enabled   bool   `json:"enabled"`
	Schedule  string `json:"schedule,omitempty"`
	Retention int32  `json:"retention,omitempty"`
}

type backupTargetJSON struct {
	Configured bool   `json:"configured"`
	Endpoint   string `json:"endpoint,omitempty"`
	Region     string `json:"region,omitempty"`
	Bucket     string `json:"bucket,omitempty"`
	Prefix     string `json:"prefix,omitempty"`
	// DefaultPrefix is what an empty prefix becomes: the console's hostname.
	DefaultPrefix        string            `json:"defaultPrefix"`
	CredentialsSet       bool              `json:"credentialsSet"`
	RecoveryKeySet       bool              `json:"recoveryKeySet"`
	RecoveryKeyCreatedAt *time.Time        `json:"recoveryKeyCreatedAt,omitempty"`
	EtcdSnapshots        etcdSnapshotsJSON `json:"etcdSnapshots"`
	// State: NotConfigured, Pending, Ready, Error (status.backups).
	State            string     `json:"state"`
	Message          string     `json:"message,omitempty"`
	CheckedAt        *time.Time `json:"checkedAt,omitempty"`
	LastSuccessfulAt *time.Time `json:"lastSuccessfulAt,omitempty"`
}

func (b *backupsAPI) targetView(cs *kwerftv1.ConsoleSettings) backupTargetJSON {
	out := backupTargetJSON{DefaultPrefix: b.consoleDomain(), State: "NotConfigured",
		EtcdSnapshots: etcdSnapshotsJSON{Enabled: true}}
	if cs == nil {
		return out
	}
	out.CredentialsSet = cs.Annotations[controllers.AnnotationBackupCredentialsUpdated] != ""
	out.RecoveryKeySet = cs.Annotations[controllers.AnnotationBackupKeyCreated] != ""
	if t, err := time.Parse(time.RFC3339Nano, cs.Annotations[controllers.AnnotationBackupKeyCreated]); err == nil {
		t = t.UTC()
		out.RecoveryKeyCreatedAt = &t
	}
	if s := cs.Spec.Backups; s != nil {
		out.Configured = true
		out.Endpoint, out.Region, out.Bucket, out.Prefix = s.Endpoint, s.Region, s.Bucket, s.Prefix
		out.EtcdSnapshots = etcdSnapshotsJSON{Enabled: s.EtcdSnapshots != nil}
		if e := s.EtcdSnapshots; e != nil {
			out.EtcdSnapshots.Schedule, out.EtcdSnapshots.Retention = e.Schedule, e.Retention
		}
	}
	if st := cs.Status.Backups; st != nil {
		out.State = cmp.Or(st.State, out.State)
		out.Message = st.Message
		out.CheckedAt = timePtr(st.CheckedAt)
		out.LastSuccessfulAt = timePtr(st.LastSuccessfulAt)
		if out.RecoveryKeyCreatedAt == nil {
			out.RecoveryKeyCreatedAt = timePtr(st.RecoveryKeyCreatedAt)
		}
	} else if out.Configured {
		out.State = "Pending"
	}
	return out
}

// settings reads the ConsoleSettings as the user; nil when none exist.
func (b *backupsAPI) settings(ctx context.Context, c client.Client) (*kwerftv1.ConsoleSettings, error) {
	var cs kwerftv1.ConsoleSettings
	err := c.Get(ctx, client.ObjectKey{Name: kwerftv1.ConsoleSettingsName}, &cs)
	if apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &cs, nil
}

func (b *backupsAPI) target(w http.ResponseWriter, r *http.Request) {
	c, p, ctx, cancel, err := b.userClient(r)
	defer cancel()
	if err != nil {
		b.internalError(w, r, err)
		return
	}
	cs, err := b.settings(ctx, c)
	if err != nil {
		b.kubeError(w, r, p, "settings.read", "backups", "Settings not found.", err)
		return
	}
	writeJSON(w, http.StatusOK, b.targetView(cs))
}

type targetInput struct {
	Endpoint      string             `json:"endpoint"`
	Region        string             `json:"region"`
	Bucket        string             `json:"bucket"`
	Prefix        string             `json:"prefix"`
	AccessKey     string             `json:"accessKey"`
	SecretKey     string             `json:"secretKey"`
	RecoveryKey   string             `json:"recoveryKey"`
	EtcdSnapshots *etcdSnapshotsJSON `json:"etcdSnapshots"`
}

var (
	endpointHostRE = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?(:[0-9]{1,5})?$`)
	bucketRE       = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)
	regionRE       = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)
	prefixRE       = regexp.MustCompile(`^[A-Za-z0-9._-]+(/[A-Za-z0-9._-]+)*$`)
)

// normalize checks the target's fields; on false it has answered.
func (in *targetInput) normalize(w http.ResponseWriter, needKeys bool) bool {
	in.Endpoint = strings.TrimRight(strings.TrimSpace(in.Endpoint), "/")
	if !strings.Contains(in.Endpoint, "://") && in.Endpoint != "" {
		in.Endpoint = "https://" + in.Endpoint
	}
	u, err := url.Parse(in.Endpoint)
	if err != nil || u.Scheme != "https" || !endpointHostRE.MatchString(u.Host) || u.Path != "" || u.RawQuery != "" || u.User != nil {
		writeFieldError(w, "endpoint", "Enter the storage's HTTPS endpoint, like https://fsn1.your-objectstorage.com.")
		return false
	}
	in.Region = strings.ToLower(strings.TrimSpace(in.Region))
	if in.Region != "" && !regionRE.MatchString(in.Region) {
		writeFieldError(w, "region", "Enter a region such as fsn1, or leave it empty.")
		return false
	}
	in.Bucket = strings.TrimSpace(in.Bucket)
	if !bucketRE.MatchString(in.Bucket) || strings.Contains(in.Bucket, "..") {
		writeFieldError(w, "bucket", "Enter the bucket's name: 3 to 63 lowercase letters, digits, dots and dashes.")
		return false
	}
	in.Prefix = strings.Trim(strings.TrimSpace(in.Prefix), "/")
	if in.Prefix != "" && (len(in.Prefix) > 200 || !prefixRE.MatchString(in.Prefix) || slices.Contains(strings.Split(in.Prefix, "/"), "..")) {
		writeFieldError(w, "prefix", "Use letters, digits, dots, dashes and slashes (at most 200), or leave it empty for the console's hostname.")
		return false
	}
	in.AccessKey, in.SecretKey = strings.TrimSpace(in.AccessKey), strings.TrimSpace(in.SecretKey)
	switch {
	case (in.AccessKey == "") != (in.SecretKey == ""):
		field := "secretKey"
		if in.AccessKey == "" {
			field = "accessKey"
		}
		writeFieldError(w, field, "Enter both the access key and the secret key.")
		return false
	case in.AccessKey == "" && needKeys:
		writeFieldError(w, "accessKey", "Enter the access key and secret key of the bucket (Hetzner Console › Object Storage › Security credentials).")
		return false
	case len(in.AccessKey) > 256 || len(in.SecretKey) > 256 || strings.ContainsAny(in.AccessKey+in.SecretKey, " \t\r\n"):
		writeFieldError(w, "accessKey", "That does not look like an S3 access key.")
		return false
	}
	if e := in.EtcdSnapshots; e != nil && e.Enabled {
		e.Schedule = strings.TrimSpace(e.Schedule)
		if e.Schedule != "" {
			if _, err := controllers.ParseBackupSchedule(e.Schedule); err != nil || len(e.Schedule) > 100 {
				writeFieldError(w, "etcdSnapshots.schedule", "Enter a cron expression with five fields, like 0 */6 * * *.")
				return false
			}
		}
		if e.Retention != 0 && (e.Retention < 1 || e.Retention > 500) {
			writeFieldError(w, "etcdSnapshots.retention", "Keep from 1 to 500 snapshots.")
			return false
		}
	}
	return true
}

func (in *targetInput) s3Target(defaultPrefix string) backups.Target {
	return backups.Target{Endpoint: in.Endpoint, Region: in.Region, Bucket: in.Bucket, Prefix: cmp.Or(in.Prefix, defaultPrefix)}
}

type checkJSON struct {
	OK bool `json:"ok"`
	// HasObjects: the prefix holds objects already; HasBackups: Velero
	// backups among them (another console's, or this one's before a
	// reinstall: their recovery key reads them).
	HasObjects bool `json:"hasObjects"`
	HasBackups bool `json:"hasBackups"`
}

// check runs the connection check; on false it has answered with the
// problem, as a field error where one field is to blame.
func (b *backupsAPI) check(w http.ResponseWriter, ctx context.Context, t backups.Target, cr backups.Credentials) (*checkJSON, bool) {
	res, err := b.s3.Check(ctx, t, cr)
	if err == nil {
		out := &checkJSON{OK: true, HasObjects: res.HasObjects}
		if res.HasObjects {
			keys, err := b.s3.List(ctx, t, cr, strings.Trim(t.Prefix, "/")+"/velero/", 1)
			out.HasBackups = err == nil && len(keys) > 0
		}
		return out, true
	}
	var se *backups.Error
	var ce *backups.CheckError
	step := ""
	if errors.As(err, &ce) {
		switch ce.Step {
		case backups.ErrWrite:
			step = "write"
		case backups.ErrDelete:
			step = "delete"
		}
	}
	if !errors.As(err, &se) {
		cause := err
		if ce != nil {
			cause = ce.Err
		}
		writeError(w, http.StatusBadGateway, "Could not reach "+t.Endpoint+": "+cause.Error())
		return nil, false
	}
	switch se.Code {
	case "InvalidAccessKeyId":
		writeFieldError(w, "accessKey", "The storage does not know this access key.")
	case "SignatureDoesNotMatch":
		writeFieldError(w, "secretKey", "The secret key does not belong to this access key.")
	case "NoSuchBucket":
		writeFieldError(w, "bucket", "There is no bucket "+t.Bucket+" at "+t.Endpoint+". Create it first (Hetzner Console › Object Storage).")
	case "AuthorizationHeaderMalformed", "InvalidRegion", "IllegalLocationConstraintException":
		writeFieldError(w, "region", "The storage expects another region: "+se.Error())
	case "AccessDenied", "AllAccessDisabled":
		msg := "These keys may not list the bucket " + t.Bucket + "."
		if step != "" {
			msg = "These keys may read the bucket but not " + step + " in it; Velero needs to write and delete backups."
		}
		writeFieldError(w, "accessKey", msg)
	default:
		writeFieldError(w, "endpoint", "The storage refused the check: "+se.Error())
	}
	return nil, false
}

// checkTarget checks a target: the one given with its keys, or the saved
// target with the stored keys (read with the console's own identity; the
// request cannot point them anywhere else).
func (b *backupsAPI) checkTarget(w http.ResponseWriter, r *http.Request) {
	var in targetInput
	if !decode(w, r, &in) {
		return
	}
	c, p, ctx, cancel, err := b.userClient(r)
	defer cancel()
	if err != nil {
		b.internalError(w, r, err)
		return
	}
	if strings.TrimSpace(in.AccessKey) == "" && strings.TrimSpace(in.SecretKey) == "" {
		cs, err := b.settings(ctx, c)
		if err != nil {
			b.kubeError(w, r, p, "settings.read", "backups", "Settings not found.", err)
			return
		}
		if cs == nil || cs.Spec.Backups == nil || cs.Annotations[controllers.AnnotationBackupCredentialsUpdated] == "" {
			writeFieldError(w, "accessKey", "Enter the access key and secret key to check the bucket.")
			return
		}
		cr, err := b.storedCredentials(ctx)
		if err != nil || cr.AccessKey == "" {
			writeError(w, http.StatusServiceUnavailable, "The stored access keys cannot be read right now. Enter them again to check.")
			return
		}
		s := cs.Spec.Backups
		saved := targetInput{Endpoint: s.Endpoint, Region: s.Region, Bucket: s.Bucket, Prefix: s.Prefix}
		if out, ok := b.check(w, ctx, saved.s3Target(b.consoleDomain()), cr); ok {
			writeJSON(w, http.StatusOK, out)
		}
		return
	}
	if !in.normalize(w, true) {
		return
	}
	if out, ok := b.check(w, ctx, in.s3Target(b.consoleDomain()), backups.Credentials{AccessKey: in.AccessKey, SecretKey: in.SecretKey}); ok {
		writeJSON(w, http.StatusOK, out)
	}
}

// storedCredentials reads the stored access keys with the console's own
// identity, for a check of the saved target. They never leave the server.
func (b *backupsAPI) storedCredentials(ctx context.Context) (backups.Credentials, error) {
	reader := b.cfg.SystemReader
	if reader == nil {
		reader = b.cfg.System
	}
	if reader == nil {
		return backups.Credentials{}, errors.New("no system identity")
	}
	var sec corev1.Secret
	if err := reader.Get(ctx, client.ObjectKey{Namespace: controllers.GatewayNamespace, Name: controllers.BackupCredentialsSecret}, &sec); err != nil {
		return backups.Credentials{}, err
	}
	return backups.Credentials{AccessKey: string(sec.Data[controllers.BackupAccessKeyKey]), SecretKey: string(sec.Data[controllers.BackupSecretKeyKey])}, nil
}

// saveTarget stores the target. The first save creates the recovery key
// (unless one of earlier backups is entered) and the plan "cluster".
func (b *backupsAPI) saveTarget(w http.ResponseWriter, r *http.Request) {
	var in targetInput
	if !decode(w, r, &in) {
		return
	}
	c, p, ctx, cancel, err := b.userClient(r)
	defer cancel()
	if err != nil {
		b.internalError(w, r, err)
		return
	}
	cs, err := b.settings(ctx, c)
	if err != nil {
		b.kubeError(w, r, p, "settings.backups", "backups", "Settings not found.", err)
		return
	}
	view := b.targetView(cs)
	if !in.normalize(w, !view.CredentialsSet) {
		return
	}
	enteredKey := ""
	if strings.TrimSpace(in.RecoveryKey) != "" {
		if view.RecoveryKeySet {
			writeFieldError(w, "recoveryKey", "This console already has a recovery key; it never changes.")
			return
		}
		k, err := backups.NormalizeRecoveryKey(in.RecoveryKey)
		if err != nil {
			writeFieldError(w, "recoveryKey", "That is not a recovery key: 52 letters and digits in groups of 4, as the console showed it.")
			return
		}
		enteredKey = k
	}
	prefix := cmp.Or(in.Prefix, view.Prefix, b.consoleDomain())
	if in.Prefix == "" {
		in.Prefix = prefix
	}

	// Check the bucket with new keys before storing anything. A new key
	// cannot read backups already under the prefix: refuse to mix them.
	var check *checkJSON
	if in.AccessKey != "" {
		var ok bool
		if check, ok = b.check(w, ctx, in.s3Target(prefix), backups.Credentials{AccessKey: in.AccessKey, SecretKey: in.SecretKey}); !ok {
			return
		}
		if check.HasBackups && !view.RecoveryKeySet && enteredKey == "" {
			writeFieldError(w, "prefix", "This prefix already holds backups. Enter the recovery key they were made with, or choose another prefix.")
			return
		}
	}

	if cs == nil {
		if err := patchConsoleSettings(ctx, c, nil, map[string]any{}); err != nil {
			b.kubeError(w, r, p, "settings.backups", "backups", "Settings not found.", err)
			return
		}
		if cs, err = b.settings(ctx, c); err != nil || cs == nil {
			b.internalError(w, r, cmp.Or(err, errors.New("settings vanished")))
			return
		}
	}
	var details []string
	if in.AccessKey != "" {
		if err := b.writeSecret(ctx, c, controllers.BackupCredentialsSecret, map[string]string{
			controllers.BackupAccessKeyKey: in.AccessKey, controllers.BackupSecretKeyKey: in.SecretKey}); err != nil {
			b.secretError(w, r, p, err)
			return
		}
		details = append(details, "access keys replaced")
	}

	// The recovery key: claimed on the settings first (a concurrent first
	// save conflicts there), then stored.
	generated := ""
	if !view.RecoveryKeySet {
		key := enteredKey
		if key == "" {
			key = backups.NewRecoveryKey()
			generated = key
		}
		claim := map[string]any{"metadata": map[string]any{"resourceVersion": cs.ResourceVersion,
			"annotations": map[string]any{controllers.AnnotationBackupKeyCreated: b.now().UTC().Format(time.RFC3339Nano)}}}
		if err := patchConsoleSettings(ctx, c, cs, claim); err != nil {
			b.kubeError(w, r, p, "settings.backups", "backups", "Settings not found.", err)
			return
		}
		if err := b.writeSecret(ctx, c, controllers.BackupKeySecret, map[string]string{controllers.BackupKeySecretKey: key}); err != nil {
			undo := map[string]any{"metadata": map[string]any{"annotations": map[string]any{controllers.AnnotationBackupKeyCreated: nil}}}
			_ = patchConsoleSettings(ctx, c, cs, undo)
			b.secretError(w, r, p, err)
			return
		}
		if generated != "" {
			details = append(details, "recovery key created")
		} else {
			details = append(details, "recovery key of earlier backups entered")
		}
	}

	spec := map[string]any{"endpoint": in.Endpoint, "region": nilIfEmpty(in.Region), "bucket": in.Bucket, "prefix": prefix, "etcdSnapshots": nil}
	if in.EtcdSnapshots == nil {
		keep := view.EtcdSnapshots
		in.EtcdSnapshots = &keep
	}
	if e := in.EtcdSnapshots; e.Enabled {
		etcd := map[string]any{"schedule": nilIfEmpty(e.Schedule), "retention": nil}
		if e.Retention > 0 {
			etcd["retention"] = e.Retention
		}
		spec["etcdSnapshots"] = etcd
	}
	patch := map[string]any{"spec": map[string]any{"backups": spec}}
	if in.AccessKey != "" {
		patch["metadata"] = map[string]any{"annotations": map[string]any{
			controllers.AnnotationBackupCredentialsUpdated: b.now().UTC().Format(time.RFC3339Nano)}}
	}
	if err := patchConsoleSettings(ctx, c, cs, patch); err != nil {
		b.kubeError(w, r, p, "settings.backups", "backups", "Settings not found.", err)
		return
	}
	// The first save starts the daily backup of everything.
	planCreated := false
	if !view.Configured {
		plan := &kwerftv1.BackupPlan{ObjectMeta: metav1.ObjectMeta{Name: controllers.DefaultBackupPlan},
			Spec: kwerftv1.BackupPlanSpec{Scope: kwerftv1.BackupCluster, Schedule: controllers.DefaultBackupSchedule,
				Retention: &metav1.Duration{Duration: controllers.DefaultBackupRetention}, Volumes: boolPtr(true)}}
		switch err := c.Create(ctx, plan); {
		case err == nil:
			planCreated = true
			b.audit(r, p.user.Email, "backup.plan_create", plan.Name, planDetail(plan.Spec))
		case apierrors.IsAlreadyExists(err):
		default:
			b.kubeError(w, r, p, "backup.plan_create", plan.Name, "Backup plan not found.", err)
			return
		}
	}
	b.audit(r, p.user.Email, "settings.backups", in.Endpoint+"/"+in.Bucket+"/"+prefix, strings.Join(details, ", "))
	if cs, err = b.settings(ctx, c); err != nil {
		b.internalError(w, r, err)
		return
	}
	out := map[string]any{"settings": b.targetView(cs), "planCreated": planCreated}
	if check != nil {
		out["check"] = check
	}
	if generated != "" {
		out["recoveryKey"] = generated
	}
	writeJSON(w, http.StatusOK, out)
}

func (b *backupsAPI) secretError(w http.ResponseWriter, r *http.Request, p *principal, err error) {
	if apierrors.IsNotFound(err) {
		writeError(w, http.StatusServiceUnavailable, "The console is still preparing the keys' storage. Try again in a moment.")
		return
	}
	b.kubeError(w, r, p, "settings.backups", "backups", "Key storage not found.", err)
}

// writeSecret patches keys of a write-only Secret in kwerft-system as the
// user (their role may patch it, never read it); the reconciler creates it.
func (b *backupsAPI) writeSecret(ctx context.Context, c client.Client, name string, values map[string]string) error {
	data := map[string]string{}
	for k, v := range values {
		data[k] = base64.StdEncoding.EncodeToString([]byte(v))
	}
	raw, err := json.Marshal(map[string]any{"data": data})
	if err != nil {
		return err
	}
	sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: controllers.GatewayNamespace, Name: name}}
	for deadline := time.Now().Add(5 * time.Second); ; {
		err = c.Patch(ctx, sec, client.RawPatch(types.MergePatchType, raw))
		if !apierrors.IsNotFound(err) || time.Now().After(deadline) {
			return err
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// patchConsoleSettings merge-patches the singleton, creating it first when
// cs is nil.
func patchConsoleSettings(ctx context.Context, c client.Client, cs *kwerftv1.ConsoleSettings, patch map[string]any) error {
	if cs == nil {
		cs = &kwerftv1.ConsoleSettings{ObjectMeta: metav1.ObjectMeta{Name: kwerftv1.ConsoleSettingsName}}
		if err := c.Create(ctx, cs); err != nil && !apierrors.IsAlreadyExists(err) {
			return err
		}
	}
	raw, err := json.Marshal(patch)
	if err != nil {
		return err
	}
	return c.Patch(ctx, cs.DeepCopy(), client.RawPatch(types.MergePatchType, raw))
}

// ---- plans ------------------------------------------------------------------------------

type backupRunJSON struct {
	Name        string     `json:"name"`
	Phase       string     `json:"phase"`
	StartedAt   *time.Time `json:"startedAt,omitempty"`
	CompletedAt *time.Time `json:"completedAt,omitempty"`
	Items       int32      `json:"items"`
	Warnings    int32      `json:"warnings"`
	Errors      int32      `json:"errors"`
	Message     string     `json:"message,omitempty"`
}

type planJSON struct {
	Name     string   `json:"name"`
	Scope    string   `json:"scope"`
	Projects []string `json:"projects"`
	Schedule string   `json:"schedule"`
	// Retention as 14d, 72h, …
	Retention        string         `json:"retention"`
	Volumes          bool           `json:"volumes"`
	Paused           bool           `json:"paused"`
	NextRunAt        *time.Time     `json:"nextRunAt,omitempty"`
	LastSuccessfulAt *time.Time     `json:"lastSuccessfulAt,omitempty"`
	LastBackup       *backupRunJSON `json:"lastBackup,omitempty"`
	Backups          int32          `json:"backups"`
	Ready            bool           `json:"ready"`
	Reason           string         `json:"reason,omitempty"`
	Message          string         `json:"message,omitempty"`
	// Running: "Back up now" was asked for and the backup has not finished.
	RunRequestedAt *time.Time `json:"runRequestedAt,omitempty"`
}

func planView(pl *kwerftv1.BackupPlan) planJSON {
	out := planJSON{Name: pl.Name, Scope: string(controllers.PlanScope(pl)), Projects: nonNil(pl.Spec.Projects), Schedule: pl.Spec.Schedule,
		Retention: alerting.FormatDuration(controllers.PlanRetention(pl)), Volumes: controllers.PlanVolumes(pl), Paused: pl.Spec.Paused,
		NextRunAt: timePtr(pl.Status.NextRunAt), LastSuccessfulAt: timePtr(pl.Status.LastSuccessfulAt), Backups: pl.Status.Backups}
	if lb := pl.Status.LastBackup; lb != nil {
		out.LastBackup = &backupRunJSON{Name: lb.Name, Phase: lb.Phase, StartedAt: timePtr(lb.StartedAt), CompletedAt: timePtr(lb.CompletedAt),
			Items: lb.Items, Warnings: lb.Warnings, Errors: lb.Errors, Message: lb.Message}
	}
	if c := meta.FindStatusCondition(pl.Status.Conditions, controllers.ConditionReady); c != nil {
		out.Ready, out.Reason, out.Message = c.Status == metav1.ConditionTrue, c.Reason, c.Message
	}
	if t, err := time.Parse(time.RFC3339Nano, pl.Annotations[controllers.AnnotationRunRequested]); err == nil {
		t = t.UTC()
		out.RunRequestedAt = &t
	}
	return out
}

func planNotFound(name string) string { return fmt.Sprintf("Backup plan %q not found.", name) }

func (b *backupsAPI) plans(w http.ResponseWriter, r *http.Request) {
	c, p, ctx, cancel, err := b.userClient(r)
	defer cancel()
	if err != nil {
		b.internalError(w, r, err)
		return
	}
	var list kwerftv1.BackupPlanList
	if err := c.List(ctx, &list); err != nil {
		b.kubeError(w, r, p, "backup.plans", "plans", "Backup plans not found.", err)
		return
	}
	out := make([]planJSON, 0, len(list.Items))
	for i := range list.Items {
		out = append(out, planView(&list.Items[i]))
	}
	slices.SortFunc(out, func(a, b planJSON) int { return strings.Compare(a.Name, b.Name) })
	writeJSON(w, http.StatusOK, out)
}

type planInput struct {
	Name      string   `json:"name"`
	Scope     string   `json:"scope"`
	Projects  []string `json:"projects"`
	Schedule  string   `json:"schedule"`
	Retention string   `json:"retention"`
	Volumes   *bool    `json:"volumes"`
	Paused    bool     `json:"paused"`
}

const (
	minRetention = time.Hour
	maxRetention = 3650 * 24 * time.Hour
	// maxPlanName keeps "kwerft-<plan>-<YYYYMMDDhhmmss>", the names of the
	// plan's backups, within a label value's 63 characters.
	maxPlanName = 40
)

func planSpec(w http.ResponseWriter, in *planInput) (kwerftv1.BackupPlanSpec, bool) {
	spec := kwerftv1.BackupPlanSpec{Scope: kwerftv1.BackupScope(cmp.Or(in.Scope, string(kwerftv1.BackupCluster))),
		Schedule: strings.Join(strings.Fields(in.Schedule), " "), Volumes: boolPtr(in.Volumes == nil || *in.Volumes), Paused: in.Paused}
	switch spec.Scope {
	case kwerftv1.BackupCluster:
		if len(in.Projects) > 0 {
			writeFieldError(w, "projects", "A Cluster plan backs up every project; choose Projects to pick some.")
			return spec, false
		}
	case kwerftv1.BackupProjects:
		if len(in.Projects) == 0 || len(in.Projects) > 100 {
			writeFieldError(w, "projects", "Pick from 1 to 100 projects.")
			return spec, false
		}
		for _, pr := range in.Projects {
			if len(validation.IsDNS1123Label(pr)) > 0 {
				writeFieldError(w, "projects", fmt.Sprintf("%q is not a project name.", pr))
				return spec, false
			}
		}
		spec.Projects = slices.Compact(sortedCopy(in.Projects))
	default:
		writeFieldError(w, "scope", "Choose Cluster or Projects.")
		return spec, false
	}
	if _, err := controllers.ParseBackupSchedule(spec.Schedule); err != nil || len(spec.Schedule) < 9 || len(spec.Schedule) > 100 {
		writeFieldError(w, "schedule", "Enter a cron expression with five fields, in UTC, like 0 3 * * * (daily at 03:00).")
		return spec, false
	}
	if in.Retention != "" {
		d, err := alerting.ParseDuration(in.Retention)
		if err != nil || d < minRetention || d > maxRetention {
			writeFieldError(w, "retention", "Enter how long to keep each backup, like 14d or 72h (1 hour to 10 years).")
			return spec, false
		}
		spec.Retention = &metav1.Duration{Duration: d}
	} else {
		spec.Retention = &metav1.Duration{Duration: controllers.DefaultBackupRetention}
	}
	return spec, true
}

func sortedCopy(in []string) []string {
	out := slices.Clone(in)
	slices.Sort(out)
	return out
}

func planDetail(spec kwerftv1.BackupPlanSpec) string {
	d := string(spec.Scope) + ", " + spec.Schedule
	if spec.Scope == kwerftv1.BackupProjects {
		d += ", projects " + strings.Join(spec.Projects, " ")
	}
	if spec.Retention != nil {
		d += ", keep " + alerting.FormatDuration(spec.Retention.Duration)
	}
	if spec.Volumes != nil && !*spec.Volumes {
		d += ", without volumes"
	}
	if spec.Paused {
		d += ", paused"
	}
	return d
}

func (b *backupsAPI) planCreate(w http.ResponseWriter, r *http.Request) {
	var in planInput
	if !decode(w, r, &in) {
		return
	}
	name := strings.TrimSpace(in.Name)
	if len(validation.IsDNS1123Label(name)) > 0 || len(name) > maxPlanName {
		writeFieldError(w, "name", fmt.Sprintf("Use lowercase letters, digits and dashes, starting and ending with a letter or digit (at most %d).", maxPlanName))
		return
	}
	spec, ok := planSpec(w, &in)
	if !ok {
		return
	}
	c, p, ctx, cancel, err := b.userClient(r)
	defer cancel()
	if err != nil {
		b.internalError(w, r, err)
		return
	}
	plan := &kwerftv1.BackupPlan{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: spec}
	if err := c.Create(ctx, plan); err != nil {
		b.kubeError(w, r, p, "backup.plan_create", name, planNotFound(name), err)
		return
	}
	b.audit(r, p.user.Email, "backup.plan_create", name, planDetail(spec))
	writeJSON(w, http.StatusCreated, planView(plan))
}

func (b *backupsAPI) planUpdate(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var in planInput
	if !decode(w, r, &in) {
		return
	}
	if in.Name != "" && in.Name != name {
		writeFieldError(w, "name", "A plan cannot be renamed. Create a new one instead.")
		return
	}
	spec, ok := planSpec(w, &in)
	if !ok {
		return
	}
	c, p, ctx, cancel, err := b.userClient(r)
	defer cancel()
	if err != nil {
		b.internalError(w, r, err)
		return
	}
	var plan kwerftv1.BackupPlan
	if err := c.Get(ctx, client.ObjectKey{Name: name}, &plan); err != nil {
		b.kubeError(w, r, p, "backup.plan_update", name, planNotFound(name), err)
		return
	}
	plan.Spec = spec
	if err := c.Update(ctx, &plan); err != nil {
		b.kubeError(w, r, p, "backup.plan_update", name, planNotFound(name), err)
		return
	}
	b.audit(r, p.user.Email, "backup.plan_update", name, planDetail(spec))
	writeJSON(w, http.StatusOK, planView(&plan))
}

// planDelete removes a plan and its schedule; its backups stay until they
// expire.
func (b *backupsAPI) planDelete(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	c, p, ctx, cancel, err := b.userClient(r)
	defer cancel()
	if err != nil {
		b.internalError(w, r, err)
		return
	}
	plan := &kwerftv1.BackupPlan{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if err := c.Delete(ctx, plan); err != nil {
		b.kubeError(w, r, p, "backup.plan_delete", name, planNotFound(name), err)
		return
	}
	b.audit(r, p.user.Email, "backup.plan_delete", name, "its backups stay until they expire")
	w.WriteHeader(http.StatusNoContent)
}

// planRun asks for a backup now (the reconciler makes it).
func (b *backupsAPI) planRun(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	c, p, ctx, cancel, err := b.userClient(r)
	defer cancel()
	if err != nil {
		b.internalError(w, r, err)
		return
	}
	at := b.now().UTC()
	raw, _ := json.Marshal(map[string]any{"metadata": map[string]any{"annotations": map[string]string{
		controllers.AnnotationRunRequested: at.Format(time.RFC3339Nano), controllers.AnnotationRequestedBy: p.user.Email}}})
	plan := &kwerftv1.BackupPlan{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if err := c.Patch(ctx, plan, client.RawPatch(types.MergePatchType, raw)); err != nil {
		b.kubeError(w, r, p, "backup.run", name, planNotFound(name), err)
		return
	}
	b.audit(r, p.user.Email, "backup.run", name, "")
	writeJSON(w, http.StatusAccepted, map[string]any{"plan": planView(plan),
		"backup": controllers.BackupScheduleName(name) + "-" + at.Format("20060102150405")})
}

// ---- backups -----------------------------------------------------------------------------

type backupJSON struct {
	Name        string     `json:"name"`
	Plan        string     `json:"plan,omitempty"`
	Scope       string     `json:"scope,omitempty"`
	Phase       string     `json:"phase"`
	Projects    []string   `json:"projects"`
	Volumes     bool       `json:"volumes"`
	StartedAt   *time.Time `json:"startedAt,omitempty"`
	CompletedAt *time.Time `json:"completedAt,omitempty"`
	ExpiresAt   *time.Time `json:"expiresAt,omitempty"`
	Items       int64      `json:"items"`
	TotalItems  int64      `json:"totalItems"`
	Warnings    int64      `json:"warnings"`
	Errors      int64      `json:"errors"`
	// Bytes of volume data (file system backups); 0 when unknown.
	Bytes       int64  `json:"bytes"`
	Message     string `json:"message,omitempty"`
	RequestedBy string `json:"requestedBy,omitempty"`
	Restorable  bool   `json:"restorable"`
}

type backupListJSON struct {
	// Velero: the cluster serves Velero's kinds (installed).
	Velero  bool         `json:"velero"`
	Backups []backupJSON `json:"backups"`
}

func (b *backupsAPI) list(w http.ResponseWriter, r *http.Request) {
	c, p, ctx, cancel, err := b.userClient(r)
	defer cancel()
	if err != nil {
		b.internalError(w, r, err)
		return
	}
	out := backupListJSON{Velero: true, Backups: []backupJSON{}}
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(controllers.VeleroBackupGVK.GroupVersion().WithKind("BackupList"))
	if err := c.List(ctx, list, client.InNamespace(controllers.VeleroNamespace)); err != nil {
		if meta.IsNoMatchError(err) {
			out.Velero = false
			writeJSON(w, http.StatusOK, out)
			return
		}
		b.kubeError(w, r, p, "backup.list", "backups", "Backups not found.", err)
		return
	}
	sizes := b.volumeBytes(ctx, c)
	for i := range list.Items {
		u := &list.Items[i]
		v := controllers.ReadVeleroBackup(u)
		var projects []string
		for _, ns := range v.Namespaces {
			if !slices.Contains(controllers.BackupPlatformNamespaces, ns) {
				projects = append(projects, ns)
			}
		}
		out.Backups = append(out.Backups, backupJSON{Name: v.Name, Plan: v.Plan, Scope: v.Scope, Phase: v.Phase, Projects: nonNil(projects),
			Volumes: v.Volumes, StartedAt: utc(v.StartedAt), CompletedAt: utc(v.CompletedAt), ExpiresAt: utc(v.ExpiresAt),
			Items: v.Items, TotalItems: v.TotalItems, Warnings: v.Warnings, Errors: v.Errors, Bytes: sizes[v.Name], Message: v.Message,
			RequestedBy: u.GetAnnotations()[controllers.AnnotationRequestedBy], Restorable: controllers.BackupRestorable(v.Phase)})
	}
	slices.SortFunc(out.Backups, func(a, b backupJSON) int {
		return started(b).Compare(started(a))
	})
	writeJSON(w, http.StatusOK, out)
}

func started(b backupJSON) time.Time {
	if b.StartedAt != nil {
		return *b.StartedAt
	}
	return time.Time{}
}

func utc(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}

// volumeBytes sums the file system backups' sizes per backup; empty when
// they cannot be read.
func (b *backupsAPI) volumeBytes(ctx context.Context, c client.Client) map[string]int64 {
	out := map[string]int64{}
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(controllers.VeleroPodVolumeBackupGVK.GroupVersion().WithKind("PodVolumeBackupList"))
	if err := c.List(ctx, list, client.InNamespace(controllers.VeleroNamespace)); err != nil {
		return out
	}
	for _, pvb := range list.Items {
		name := pvb.GetLabels()["velero.io/backup-name"]
		if name == "" {
			continue
		}
		n, _, _ := unstructured.NestedInt64(pvb.Object, "status", "progress", "totalBytes")
		out[name] += n
	}
	return out
}

// ---- restores ----------------------------------------------------------------------------

type restoreJSON struct {
	Name          string     `json:"name"`
	Backup        string     `json:"backup"`
	Project       string     `json:"project"`
	TargetProject string     `json:"targetProject,omitempty"`
	Apps          []string   `json:"apps"`
	Phase         string     `json:"phase"`
	Message       string     `json:"message,omitempty"`
	Warnings      int32      `json:"warnings"`
	Errors        int32      `json:"errors"`
	CreatedAt     time.Time  `json:"createdAt"`
	StartedAt     *time.Time `json:"startedAt,omitempty"`
	CompletedAt   *time.Time `json:"completedAt,omitempty"`
	RequestedBy   string     `json:"requestedBy,omitempty"`
}

func restoreView(rs *kwerftv1.Restore) restoreJSON {
	return restoreJSON{Name: rs.Name, Backup: rs.Spec.Backup, Project: rs.Spec.Project, TargetProject: rs.Spec.TargetProject,
		Apps: nonNil(rs.Spec.Apps), Phase: cmp.Or(rs.Status.Phase, controllers.RestorePending), Message: rs.Status.Message,
		Warnings: rs.Status.Warnings, Errors: rs.Status.Errors, CreatedAt: rs.CreationTimestamp.UTC(),
		StartedAt: timePtr(rs.Status.StartedAt), CompletedAt: timePtr(rs.Status.CompletedAt),
		RequestedBy: rs.Annotations[controllers.AnnotationRequestedBy]}
}

func (b *backupsAPI) restores(w http.ResponseWriter, r *http.Request) {
	c, p, ctx, cancel, err := b.userClient(r)
	defer cancel()
	if err != nil {
		b.internalError(w, r, err)
		return
	}
	var list kwerftv1.RestoreList
	if err := c.List(ctx, &list); err != nil {
		b.kubeError(w, r, p, "backup.restores", "restores", "Restores not found.", err)
		return
	}
	out := make([]restoreJSON, 0, len(list.Items))
	for i := range list.Items {
		out = append(out, restoreView(&list.Items[i]))
	}
	slices.SortFunc(out, func(a, b restoreJSON) int { return b.CreatedAt.Compare(a.CreatedAt) })
	writeJSON(w, http.StatusOK, out)
}

type restoreInput struct {
	Backup        string   `json:"backup"`
	Project       string   `json:"project"`
	TargetProject string   `json:"targetProject"`
	Apps          []string `json:"apps"`
}

// maxRestoreTarget keeps the Restore's name (<target>-<YYYYMMDDhhmmss>)
// and its Velero Restore's (kwerft-<name>) within 63 characters.
const maxRestoreTarget = 40

func (b *backupsAPI) restoreCreate(w http.ResponseWriter, r *http.Request) {
	var in restoreInput
	if !decode(w, r, &in) {
		return
	}
	in.Project, in.TargetProject = strings.TrimSpace(in.Project), strings.TrimSpace(in.TargetProject)
	if len(validation.IsDNS1123Label(in.Project)) > 0 {
		writeFieldError(w, "project", "Choose the project to restore.")
		return
	}
	if in.TargetProject == in.Project {
		in.TargetProject = ""
	}
	target := cmp.Or(in.TargetProject, in.Project)
	if in.TargetProject != "" && (len(validation.IsDNS1123Label(in.TargetProject)) > 0 || kwerftv1.IsReservedProjectName(in.TargetProject)) {
		writeFieldError(w, "targetProject", "Use lowercase letters, digits and dashes for the new project's name (not kube-…, kwerft-… or a platform name).")
		return
	}
	if len(target) > maxRestoreTarget {
		writeFieldError(w, "targetProject", fmt.Sprintf("Restore into a project with a name of at most %d characters.", maxRestoreTarget))
		return
	}
	if len(in.Apps) > 100 {
		writeFieldError(w, "apps", "Pick at most 100 apps, or none for the whole project.")
		return
	}
	for _, app := range in.Apps {
		if len(validation.IsDNS1123Label(app)) > 0 {
			writeFieldError(w, "apps", fmt.Sprintf("%q is not an app name.", app))
			return
		}
	}
	c, p, ctx, cancel, err := b.userClient(r)
	defer cancel()
	if err != nil {
		b.internalError(w, r, err)
		return
	}
	backup := &unstructured.Unstructured{}
	backup.SetGroupVersionKind(controllers.VeleroBackupGVK)
	if err := c.Get(ctx, client.ObjectKey{Namespace: controllers.VeleroNamespace, Name: strings.TrimSpace(in.Backup)}, backup); err != nil {
		if apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
			writeFieldError(w, "backup", "That backup does not exist.")
			return
		}
		b.kubeError(w, r, p, "backup.restore", in.Backup, "Backup not found.", err)
		return
	}
	v := controllers.ReadVeleroBackup(backup)
	switch {
	case !controllers.BackupRestorable(v.Phase):
		writeFieldError(w, "backup", "The backup is "+v.Phase+"; only completed backups can be restored.")
		return
	case !controllers.BackupHolds(v, in.Project):
		writeFieldError(w, "project", "The backup does not hold the project "+in.Project+".")
		return
	}
	if in.TargetProject != "" {
		err := c.Get(ctx, client.ObjectKey{Name: in.TargetProject}, &kwerftv1.Project{})
		switch {
		case err == nil:
			writeFieldError(w, "targetProject", "The project "+in.TargetProject+" exists. Choose a new name; the restore creates the project.")
			return
		case !apierrors.IsNotFound(err):
			b.kubeError(w, r, p, "backup.restore", in.TargetProject, "Project not found.", err)
			return
		}
	}
	name := target + "-" + b.now().UTC().Format("20060102150405")
	rs := &kwerftv1.Restore{
		ObjectMeta: metav1.ObjectMeta{Name: name, Annotations: map[string]string{controllers.AnnotationRequestedBy: p.user.Email}},
		Spec: kwerftv1.RestoreSpec{Backup: v.Name, Project: in.Project, TargetProject: in.TargetProject,
			Apps: slices.Compact(sortedCopy(in.Apps))},
	}
	if err := c.Create(ctx, rs); err != nil {
		b.kubeError(w, r, p, "backup.restore", name, "Restore not found.", err)
		return
	}
	detail := "from " + v.Name + " into " + target
	if len(rs.Spec.Apps) > 0 {
		detail += ", apps " + strings.Join(rs.Spec.Apps, " ")
	}
	b.audit(r, p.user.Email, "backup.restore", in.Project, detail)
	writeJSON(w, http.StatusCreated, restoreView(rs))
}
