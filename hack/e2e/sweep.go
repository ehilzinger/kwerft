package main

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// sweepResult is what one sweep found and did.
type sweepResult struct {
	Deleted []string
	Kept    int
	Errors  []string
}

// createdAt is when a resource was created: the API's timestamp, or the
// created label if the API gave none.
func createdAt(api time.Time, labels map[string]string) time.Time {
	if !api.IsZero() {
		return api
	}
	if n, err := strconv.ParseInt(labels[labelCreated], 10, 64); err == nil {
		return time.Unix(n, 0)
	}
	return time.Time{}
}

// sweep deletes e2e servers and SSH keys (label kwerft-e2e=true) older than
// maxAge; with run set, every resource of that run regardless of age (the
// workflow's safety net after a run). Nothing without the label is touched.
func sweep(ctx context.Context, c *hcloud, now time.Time, maxAge time.Duration, run string, dryRun bool) (sweepResult, error) {
	var res sweepResult
	selector := labelE2E + "=true"
	if run != "" {
		selector += "," + labelRun + "=" + run
	}
	due := func(created time.Time) bool {
		if run != "" {
			return true
		}
		// Unknown age: old enough, the label says it is ours.
		return created.IsZero() || now.Sub(created) > maxAge
	}
	verb := "deleted"
	if dryRun {
		verb = "would delete"
	}

	servers, err := c.servers(ctx, selector)
	if err != nil {
		return res, fmt.Errorf("list servers: %w", err)
	}
	for _, s := range servers {
		created := createdAt(s.Created, s.Labels)
		if s.Labels[labelE2E] != "true" || !due(created) {
			res.Kept++
			continue
		}
		desc := fmt.Sprintf("server %s (id %d, run %s, age %s)", s.Name, s.ID, cmpOr(s.Labels[labelRun], "?"), age(now, created))
		if !dryRun {
			if err := c.deleteServer(ctx, s.ID); err != nil {
				res.Errors = append(res.Errors, desc+": "+err.Error())
				continue
			}
		}
		res.Deleted = append(res.Deleted, verb+" "+desc)
	}

	keys, err := c.sshKeys(ctx, selector)
	if err != nil {
		return res, fmt.Errorf("list SSH keys: %w", err)
	}
	for _, k := range keys {
		created := createdAt(k.Created, k.Labels)
		if k.Labels[labelE2E] != "true" || !due(created) {
			res.Kept++
			continue
		}
		desc := fmt.Sprintf("SSH key %s (id %d, run %s, age %s)", k.Name, k.ID, cmpOr(k.Labels[labelRun], "?"), age(now, created))
		if !dryRun {
			if err := c.deleteSSHKey(ctx, k.ID); err != nil {
				res.Errors = append(res.Errors, desc+": "+err.Error())
				continue
			}
		}
		res.Deleted = append(res.Deleted, verb+" "+desc)
	}
	if len(res.Errors) > 0 {
		return res, fmt.Errorf("%d deletion(s) failed: %s", len(res.Errors), strings.Join(res.Errors, "; "))
	}
	return res, nil
}

func age(now, created time.Time) string {
	if created.IsZero() {
		return "unknown"
	}
	return now.Sub(created).Round(time.Minute).String()
}
