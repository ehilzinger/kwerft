package hetzner

import (
	"context"
	"slices"
	"strings"
)

// Image is an operating system image servers are created from.
type Image struct {
	ID           int64             `json:"id"`
	Type         string            `json:"type"`   // system | app | snapshot | backup
	Status       string            `json:"status"` // available | creating | unavailable
	Name         string            `json:"name"`   // ubuntu-26.04 (system images)
	Description  string            `json:"description"`
	OSFlavor     string            `json:"os_flavor"`
	OSVersion    string            `json:"os_version"`
	Architecture string            `json:"architecture"` // x86 | arm
	Deprecated   *string           `json:"deprecated"`
	Labels       map[string]string `json:"labels"`
}

// UbuntuReleases are the Ubuntu LTS releases install.sh supports, newest
// first; node pools create servers from the newest one Hetzner offers.
var UbuntuReleases = []string{"26.04", "24.04", "22.04"}

// SystemImages lists the system images for one architecture (x86 or arm).
func (c *Client) SystemImages(ctx context.Context, architecture string) ([]Image, error) {
	q := selector("")
	q.Set("type", "system")
	q.Set("status", "available")
	if architecture != "" {
		q.Set("architecture", architecture)
	}
	return pagedList[Image](ctx, c, "/images", "images", q)
}

// NewestUbuntu picks the newest supported Ubuntu LTS image of images for
// the architecture: ubuntu-26.04 where Hetzner has it, else 24.04, then 22.04.
func NewestUbuntu(images []Image, architecture string) (Image, bool) {
	for _, release := range UbuntuReleases {
		i := slices.IndexFunc(images, func(img Image) bool {
			return img.Type == "system" && img.Status == "available" && img.Deprecated == nil &&
				img.Architecture == architecture &&
				(img.Name == "ubuntu-"+release || img.OSFlavor == "ubuntu" && img.OSVersion == release && strings.HasPrefix(img.Name, "ubuntu-"))
		})
		if i >= 0 {
			return images[i], true
		}
	}
	return Image{}, false
}
