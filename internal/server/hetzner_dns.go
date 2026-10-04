package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Hetzner DNS lives in the Hetzner Cloud API since the old DNS Console
// (dns.hetzner.com) was shut down in May 2026: zones and RRsets at
// api.hetzner.cloud/v1, authorised with a Cloud project API token. The
// console only checks that a token sees the zone; cert-manager's Hetzner
// webhook writes the challenge records.

const hetznerCloudAPI = "https://api.hetzner.cloud/v1"

var (
	errTokenRejected = errors.New("token rejected")
	errNoZone        = errors.New("no zone")
)

// hetznerZoneFor finds the zone in the token's project that contains host,
// trying host itself and then each parent with at least two labels.
func hetznerZoneFor(ctx context.Context, hc *http.Client, base, token, host string) (string, error) {
	labels := strings.Split(host, ".")
	for i := 0; i <= len(labels)-2; i++ {
		zone := strings.Join(labels[i:], ".")
		found, err := hetznerZoneExists(ctx, hc, base, token, zone)
		if err != nil {
			return "", err
		}
		if found {
			return zone, nil
		}
	}
	return "", errNoZone
}

func hetznerZoneExists(ctx context.Context, hc *http.Client, base, token, zone string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/zones?name="+url.QueryEscape(zone), nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	res, err := hc.Do(req)
	if err != nil {
		return false, err
	}
	defer res.Body.Close()
	switch {
	case res.StatusCode == http.StatusUnauthorized || res.StatusCode == http.StatusForbidden:
		return false, errTokenRejected
	case res.StatusCode != http.StatusOK:
		return false, fmt.Errorf("Hetzner API answered %s", res.Status)
	}
	var body struct {
		Zones []struct {
			Name string `json:"name"`
		} `json:"zones"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(nil, res.Body, 1<<20)).Decode(&body); err != nil {
		return false, fmt.Errorf("Hetzner API: %w", err)
	}
	for _, z := range body.Zones {
		if strings.EqualFold(strings.TrimSuffix(z.Name, "."), zone) {
			return true, nil
		}
	}
	return false, nil
}
