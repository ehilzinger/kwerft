// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

package builds

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
)

func TestNew(t *testing.T) {
	app := &kwerftv1.App{ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "shop"},
		Spec: kwerftv1.AppSpec{Source: kwerftv1.AppSource{Git: &kwerftv1.GitSource{Repository: "https://github.com/acme/api.git", Branch: "main"}}}}
	sha := "4f2c1ab9e01d7c4f2c1ab9e01d7c4f2c1ab9e01d"
	b, err := New(app, Request{Commit: Commit{SHA: sha, Branch: "main", Message: "Add export\n\nLong body"}, Trigger: "pull-request", Deploy: true})
	if err != nil {
		t.Fatal(err)
	}
	if b.Namespace != "shop" || !strings.HasPrefix(b.GenerateName, "api-4f2c1ab-") || b.Spec.Message != "Add export" ||
		b.Spec.Deploy || b.Spec.Source.Builder != "dockerfile" || b.Spec.Source.Repository != app.Spec.Source.Git.Repository {
		t.Errorf("build = %+v", b)
	}
	if got := ImageRef("shop", "api", sha); got != "registry.kwerft.internal:5000/shop/api:4f2c1ab9e01d" {
		t.Errorf("ImageRef = %s", got)
	}
	if _, err := New(&kwerftv1.App{}, Request{}); err == nil {
		t.Error("an image app got a build")
	}
}
