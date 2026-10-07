// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

package hetzner

import "context"

// Location is a Cloud location (fsn1, nbg1, hel1, ash, hil, sin). Servers
// share a private network only within one network zone.
type Location struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Country     string `json:"country"`
	City        string `json:"city"`
	NetworkZone string `json:"network_zone"`
}

// Locations lists every Cloud location.
func (c *Client) Locations(ctx context.Context) ([]Location, error) {
	return pagedList[Location](ctx, c, "/locations", "locations", nil)
}
