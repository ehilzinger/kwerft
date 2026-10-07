// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

package backups

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"io"
	"path"
	"strings"
)

// The contents of a Velero backup: a gzipped tarball with one JSON file per
// object, at resources/<resource>.<group>/[<version>-preferredversion/]
// namespaces/<namespace>/<name>.json, or …/cluster/<name>.json for
// cluster-scoped objects (core resources have no group suffix). Velero
// hands it out through a DownloadRequest (kind BackupContents).

// Contents are a backup's objects, by group-resource, namespace and name.
type Contents struct {
	objects map[objectKey][]byte
}

type objectKey struct {
	resource, namespace, name string
	versioned                 bool
}

// ErrTooLarge: the tarball is bigger than the reader accepts.
var ErrTooLarge = errors.New("the backup's contents are too large to read")

// ReadContents reads a backup tarball (gzip) of at most limit bytes
// uncompressed.
func ReadContents(r io.Reader, limit int64) (*Contents, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return nil, err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	c := &Contents{objects: map[objectKey][]byte{}}
	var total int64
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return c, nil
		}
		if err != nil {
			return nil, err
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		key, ok := parsePath(h.Name)
		if !ok {
			continue
		}
		total += h.Size
		if total > limit {
			return nil, ErrTooLarge
		}
		raw, err := io.ReadAll(io.LimitReader(tr, h.Size))
		if err != nil {
			return nil, err
		}
		c.objects[key] = raw
	}
}

func parsePath(p string) (objectKey, bool) {
	parts := strings.Split(strings.TrimPrefix(path.Clean(p), "./"), "/")
	if len(parts) < 4 || parts[0] != "resources" || !strings.HasSuffix(parts[len(parts)-1], ".json") {
		return objectKey{}, false
	}
	k := objectKey{resource: parts[1]}
	rest := parts[2:]
	if strings.HasSuffix(rest[0], "-preferredversion") {
		k.versioned = true
		rest = rest[1:]
	}
	name := strings.TrimSuffix(rest[len(rest)-1], ".json")
	switch {
	case len(rest) == 2 && rest[0] == "cluster":
		k.name = name
	case len(rest) == 3 && rest[0] == "namespaces":
		k.namespace, k.name = rest[1], name
	default:
		return objectKey{}, false
	}
	return k, true
}

// Get returns one object's JSON. resource is "<resource>.<group>" (e.g.
// "apps.kwerft.dev") or a core resource ("secrets"); namespace is empty for
// cluster-scoped objects.
func (c *Contents) Get(resource, namespace, name string) ([]byte, bool) {
	if c == nil {
		return nil, false
	}
	for _, v := range []bool{false, true} {
		if raw, ok := c.objects[objectKey{resource: resource, namespace: namespace, name: name, versioned: v}]; ok {
			return raw, true
		}
	}
	return nil, false
}

// Names lists the objects of a resource in a namespace.
func (c *Contents) Names(resource, namespace string) []string {
	if c == nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for k := range c.objects {
		if k.resource == resource && k.namespace == namespace && !seen[k.name] {
			seen[k.name] = true
			out = append(out, k.name)
		}
	}
	return out
}
