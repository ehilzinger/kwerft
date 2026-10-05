package controllers

import (
	"slices"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
)

// Args go to the entrypoint: the image's own without a command (a Compose
// file's command), or the App's command. Tasks from the App run its
// entrypoint with its args, unless they bring a command of their own.
func TestAppArgs(t *testing.T) {
	app := &kwerftv1.App{ObjectMeta: metav1.ObjectMeta{Name: "minio", Namespace: "p"}, Spec: kwerftv1.AppSpec{
		Source: kwerftv1.AppSource{Image: &kwerftv1.ImageSource{Ref: "minio:1"}},
		Args:   []string{"server", "/data"},
	}}
	ctr := newAppRender(app, "minio:1", "p").podTemplate().Spec.Containers[0]
	if len(ctr.Command) != 0 || !slices.Equal(ctr.Args, []string{"server", "/data"}) {
		t.Errorf("container command %q args %q", ctr.Command, ctr.Args)
	}

	img := &resolvedImage{image: "minio:1"}
	run, wait := resolveTask(&kwerftv1.Task{ObjectMeta: metav1.ObjectMeta{Name: "t", Namespace: "p"}, Spec: kwerftv1.TaskSpec{FromApp: "minio"}}, app, img, "p")
	if wait != nil || !slices.Equal(run.args, app.Spec.Args) {
		t.Errorf("task from the app: args %q (%v)", run.args, wait)
	}
	run, _ = resolveTask(&kwerftv1.Task{ObjectMeta: metav1.ObjectMeta{Name: "t", Namespace: "p"}, Spec: kwerftv1.TaskSpec{FromApp: "minio", Command: []string{"mc", "ls"}}}, app, img, "p")
	if len(run.args) != 0 {
		t.Errorf("a task with its own command keeps the app's args %q", run.args)
	}
}
