package hetzner

import (
	"context"
	"errors"
	"net/http"
	"strconv"
)

// SpreadGroupMax is how many servers a spread placement group holds; each
// runs on a different physical host.
const SpreadGroupMax = 10

// PlacementGroup keeps its servers on different physical hosts (type spread).
type PlacementGroup struct {
	ID      int64             `json:"id"`
	Name    string            `json:"name"`
	Type    string            `json:"type"`
	Servers []int64           `json:"servers"`
	Labels  map[string]string `json:"labels"`
}

// PlacementGroups lists placement groups matching labelSelector.
func (c *Client) PlacementGroups(ctx context.Context, labelSelector string) ([]PlacementGroup, error) {
	return pagedList[PlacementGroup](ctx, c, "/placement_groups", "placement_groups", selector(labelSelector))
}

// CreatePlacementGroup creates a spread placement group.
func (c *Client) CreatePlacementGroup(ctx context.Context, name string, labels map[string]string) (PlacementGroup, error) {
	var body struct {
		PlacementGroup PlacementGroup `json:"placement_group"`
	}
	err := c.do(ctx, http.MethodPost, "/placement_groups", map[string]any{"name": name, "type": "spread", "labels": labels}, &body)
	return body.PlacementGroup, err
}

// DeletePlacementGroup deletes an empty placement group; a missing one is
// not an error.
func (c *Client) DeletePlacementGroup(ctx context.Context, id int64) error {
	err := c.do(ctx, http.MethodDelete, "/placement_groups/"+strconv.FormatInt(id, 10), nil, nil)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}
