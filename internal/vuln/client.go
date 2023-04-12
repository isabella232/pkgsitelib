// Copyright 2023 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package vuln

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"sync"

	"golang.org/x/pkgsite/internal/derrors"
	"golang.org/x/pkgsite/internal/osv"
	"golang.org/x/sync/errgroup"
)

// Client reads Go vulnerability databases.
type Client struct {
	src source
}

// NewClient returns a client that can read from the vulnerability
// database in src (a URL representing either a http or file source).
func NewClient(src string) (*Client, error) {
	s, err := NewSource(src)
	if err != nil {
		return nil, err
	}

	return &Client{src: s}, nil
}

// NewInMemoryClient creates an in-memory vulnerability client for use
// in tests.
func NewInMemoryClient(entries []*osv.Entry) (*Client, error) {
	inMemory, err := newInMemorySource(entries)
	if err != nil {
		return nil, err
	}
	return &Client{src: inMemory}, nil
}

type PackageRequest struct {
	// Module is the module path to filter on.
	// ByPackage will only return entries that affect this module.
	// This must be set (if empty, ByPackage will always return nil).
	Module string
	// The package path to filter on.
	// ByPackage will only return entries that affect this package.
	// If empty, ByPackage will not filter based on the package.
	Package string
	// The version to filter on.
	// ByPackage will only return entries affected at this module
	// version.
	// If empty, ByPackage will not filter based on version.
	Version string
}

// ByPackage returns the OSV entries matching the package request.
func (c *Client) ByPackage(ctx context.Context, req *PackageRequest) (_ []*osv.Entry, err error) {
	derrors.Wrap(&err, "ByPackage(%v)", req)

	b, err := c.modules(ctx)
	if err != nil {
		return nil, err
	}

	dec, err := newStreamDecoder(b)
	if err != nil {
		return nil, err
	}

	var ids []string
	for dec.More() {
		var m ModuleMeta
		err := dec.Decode(&m)
		if err != nil {
			return nil, err
		}
		if m.Path == req.Module {
			for _, v := range m.Vulns {
				// We need to download the full entry if there is no fix,
				// or the requested version is less than the vuln's
				// highest fixed version.
				if v.Fixed == "" || osv.LessSemver(req.Version, v.Fixed) {
					ids = append(ids, v.ID)
				}
			}
			// We found the requested module, so skip the rest.
			break
		}
	}

	if len(ids) == 0 {
		return nil, nil
	}

	// Fetch all the entries in parallel, and create a slice
	// containing all the actually affected entries.
	g, gctx := errgroup.WithContext(ctx)
	var mux sync.Mutex
	g.SetLimit(10)
	entries := make([]*osv.Entry, 0, len(ids))
	for _, id := range ids {
		id := id
		g.Go(func() error {
			entry, err := c.ByID(gctx, id)
			if err != nil {
				return err
			}

			if entry == nil {
				return fmt.Errorf("vulnerability %s was found in %s but could not be retrieved", id, modulesEndpoint)
			}

			if isAffected(entry, req) {
				mux.Lock()
				entries = append(entries, entry)
				mux.Unlock()
			}

			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}

	sort.SliceStable(entries, func(i, j int) bool {
		return entries[i].ID < entries[j].ID
	})

	return entries, nil
}

func isAffected(e *osv.Entry, req *PackageRequest) bool {
	for _, a := range e.Affected {
		if a.Module.Path != req.Module || !osv.AffectsSemver(a.Ranges, req.Version) {
			continue
		}
		if packageMatches := func() bool {
			if req.Package == "" {
				return true //  match module only
			}
			if len(a.EcosystemSpecific.Packages) == 0 {
				return true // no package info available, so match on module
			}
			for _, p := range a.EcosystemSpecific.Packages {
				if req.Package == p.Path {
					return true // package matches
				}
			}
			return false
		}(); !packageMatches {
			continue
		}
		return true
	}
	return false
}

// ByID returns the OSV entry with the given ID or (nil, nil)
// if there isn't one.
func (c *Client) ByID(ctx context.Context, id string) (_ *osv.Entry, err error) {
	derrors.Wrap(&err, "ByID(%s)", id)

	b, err := c.entry(ctx, id)
	if err != nil {
		// entry only fails if the entry is not found, so do not return
		// the error.
		return nil, nil
	}

	var entry osv.Entry
	if err := json.Unmarshal(b, &entry); err != nil {
		return nil, err
	}

	return &entry, nil
}

// ByAlias returns the Go ID of the OSV entry that has the given alias,
// or a NotFound error if there isn't one.
func (c *Client) ByAlias(ctx context.Context, alias string) (_ string, err error) {
	derrors.Wrap(&err, "ByAlias(%s)", alias)

	b, err := c.vulns(ctx)
	if err != nil {
		return "", err
	}

	dec, err := newStreamDecoder(b)
	if err != nil {
		return "", err
	}

	for dec.More() {
		var v VulnMeta
		err := dec.Decode(&v)
		if err != nil {
			return "", err
		}
		for _, vAlias := range v.Aliases {
			if alias == vAlias {
				return v.ID, nil
			}
		}
	}

	return "", derrors.NotFound
}

// Entries returns all entries in the database.
func (c *Client) Entries(ctx context.Context) (_ []*osv.Entry, err error) {
	derrors.Wrap(&err, "Entries()")

	ids, err := c.ids(ctx)
	if err != nil {
		return nil, err
	}

	entries := make([]*osv.Entry, len(ids))
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(4)
	for i, id := range ids {
		i, id := i, id
		g.Go(func() error {
			e, err := c.ByID(gctx, id)
			if err != nil {
				return err
			}
			entries[i] = e
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}

	return entries, nil
}

// ids returns a list of the ids of all the entries in the database.
func (c *Client) ids(ctx context.Context) (_ []string, err error) {
	b, err := c.vulns(ctx)
	if err != nil {
		return nil, err
	}

	dec, err := newStreamDecoder(b)
	if err != nil {
		return nil, err
	}

	var ids []string
	for dec.More() {
		var v VulnMeta
		err := dec.Decode(&v)
		if err != nil {
			return nil, err
		}
		ids = append(ids, v.ID)
	}

	return ids, nil
}

// newStreamDecoder returns a decoder that can be used
// to read an array of JSON objects.
func newStreamDecoder(b []byte) (*json.Decoder, error) {
	dec := json.NewDecoder(bytes.NewBuffer(b))

	// skip open bracket
	_, err := dec.Token()
	if err != nil {
		return nil, err
	}

	return dec, nil
}

func (c *Client) modules(ctx context.Context) ([]byte, error) {
	return c.src.get(ctx, modulesEndpoint)
}

func (c *Client) vulns(ctx context.Context) ([]byte, error) {
	return c.src.get(ctx, vulnsEndpoint)
}

func (c *Client) entry(ctx context.Context, id string) ([]byte, error) {
	return c.src.get(ctx, filepath.Join(idDir, id))
}
