package hetzner

import (
	"context"
	"time"
)

// ServerType is a Cloud server plan (cx23, cpx32, cax21, ccx13, ...).
type ServerType struct {
	ID           int64   `json:"id"`
	Name         string  `json:"name"`
	Description  string  `json:"description"`
	Cores        int     `json:"cores"`
	Memory       float64 `json:"memory"`       // GB
	Disk         float64 `json:"disk"`         // GB
	CPUType      string  `json:"cpu_type"`     // shared | dedicated
	Architecture string  `json:"architecture"` // x86 | arm
	Category     string  `json:"category"`
	// Deprecation of the type everywhere; per location in Locations.
	Deprecation *Deprecation         `json:"deprecation"`
	Prices      []ServerTypePrice    `json:"prices"`
	Locations   []ServerTypeLocation `json:"locations"`
}

// Deprecation says a resource goes away.
type Deprecation struct {
	Announced        time.Time `json:"announced"`
	UnavailableAfter time.Time `json:"unavailable_after"`
}

// ServerTypePrice is what a server type costs in one location.
type ServerTypePrice struct {
	Location     string `json:"location"`
	PriceHourly  Price  `json:"price_hourly"`
	PriceMonthly Price  `json:"price_monthly"`
}

// Price is an amount in the project's currency, as decimal strings.
type Price struct {
	Net   string `json:"net"`
	Gross string `json:"gross"`
}

// ServerTypeLocation says whether a server type can be ordered in a location.
type ServerTypeLocation struct {
	ID          int64        `json:"id"`
	Name        string       `json:"name"`
	Available   bool         `json:"available"`
	Recommended bool         `json:"recommended"`
	Deprecation *Deprecation `json:"deprecation"`
}

// ServerTypes lists every server type.
func (c *Client) ServerTypes(ctx context.Context) ([]ServerType, error) {
	return pagedList[ServerType](ctx, c, "/server_types", "server_types", nil)
}

// AvailableIn reports whether new servers of this type can be created in
// location now (listed there, available, not deprecated).
func (t ServerType) AvailableIn(location string) bool {
	if t.Deprecation != nil {
		return false
	}
	for _, l := range t.Locations {
		if l.Name == location {
			return l.Available && l.Deprecation == nil
		}
	}
	return false
}

// MonthlyGross is the monthly price in location (gross), or "".
func (t ServerType) MonthlyGross(location string) string {
	for _, p := range t.Prices {
		if p.Location == location {
			return p.PriceMonthly.Gross
		}
	}
	return ""
}

// FindServerType returns the type named name from types.
func FindServerType(types []ServerType, name string) (ServerType, bool) {
	for _, t := range types {
		if t.Name == name {
			return t, true
		}
	}
	return ServerType{}, false
}
