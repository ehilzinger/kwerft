// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
)

// activeConsoleDomain returns where the console is served now — Settings can
// move it, and the Domain reconciler records the result in the
// ConsoleSettings status — read through the manager's cache. Nil without a
// cluster, and in development, where the console runs on localhost.
func activeConsoleDomain(mgr ctrl.Manager, dev bool) func() string {
	if mgr == nil || dev {
		return nil
	}
	return func() string {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		var s kwerftv1.ConsoleSettings
		if err := mgr.GetCache().Get(ctx, client.ObjectKey{Name: kwerftv1.ConsoleSettingsName}, &s); err != nil {
			return ""
		}
		return s.Status.ConsoleDomain
	}
}
