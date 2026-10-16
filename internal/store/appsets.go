// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gitaginanjar/cystemd/pkg/apis/v1alpha1"
)

// ─── ApplicationSets ────────────────────────────────────────────────────────

const appSetsDir = "applicationsets/"

func decodeAppSet(e *Entry) (*v1alpha1.ApplicationSet, error) {
	var set v1alpha1.ApplicationSet
	if err := json.Unmarshal(e.Value, &set); err != nil {
		return nil, fmt.Errorf("decode %s: %w", e.Key, err)
	}
	set.Metadata.ResourceVersion = strconv.FormatUint(e.ModifyIndex, 10)
	return &set, nil
}

func encodeAppSet(set *v1alpha1.ApplicationSet) ([]byte, error) {
	c := set.DeepCopy()
	c.Metadata.ResourceVersion = ""
	return json.Marshal(c)
}

// ListApplicationSets returns every ApplicationSet, sorted by name.
func (s *Store) ListApplicationSets(ctx context.Context) ([]*v1alpha1.ApplicationSet, error) {
	entries, _, err := s.kv.List(ctx, s.key(appSetsDir, ""))
	if err != nil {
		return nil, err
	}
	out := make([]*v1alpha1.ApplicationSet, 0, len(entries))
	for _, e := range entries {
		if set, err := decodeAppSet(e); err == nil {
			out = append(out, set)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Metadata.Name < out[j].Metadata.Name })
	return out, nil
}

// GetApplicationSet returns one ApplicationSet or ErrNotFound.
func (s *Store) GetApplicationSet(ctx context.Context, name string) (*v1alpha1.ApplicationSet, error) {
	e, err := s.kv.Get(ctx, s.key(appSetsDir, name))
	if err != nil {
		return nil, err
	}
	return decodeAppSet(e)
}

// CreateApplicationSet stores a new ApplicationSet (ErrExists when taken);
// its status starts empty.
func (s *Store) CreateApplicationSet(ctx context.Context, set *v1alpha1.ApplicationSet) (*v1alpha1.ApplicationSet, error) {
	set = set.DeepCopy()
	now := s.now().UTC().Truncate(time.Second)
	set.Metadata.CreationTimestamp = &now
	set.Metadata.Generation = 1
	set.Metadata.DeletionTimestamp = nil
	set.Status = v1alpha1.ApplicationSetStatus{}
	b, err := encodeAppSet(set)
	if err != nil {
		return nil, err
	}
	if err := s.kv.CAS(ctx, s.key(appSetsDir, set.Metadata.Name), b, 0); err != nil {
		if errors.Is(err, ErrConflict) {
			return nil, ErrExists
		}
		return nil, err
	}
	return s.GetApplicationSet(ctx, set.Metadata.Name)
}

// UpdateApplicationSet replaces an ApplicationSet's spec, labels and
// annotations, keeping its status and creation time. With a
// resourceVersion it fails with ErrConflict unless the set is still at
// that version.
func (s *Store) UpdateApplicationSet(ctx context.Context, set *v1alpha1.ApplicationSet) (*v1alpha1.ApplicationSet, error) {
	key := s.key(appSetsDir, set.Metadata.Name)
	for attempt := 0; ; attempt++ {
		cur, err := s.kv.Get(ctx, key)
		if err != nil {
			return nil, err
		}
		if rv := set.Metadata.ResourceVersion; rv != "" {
			if want, err := strconv.ParseUint(rv, 10, 64); err != nil || want != cur.ModifyIndex {
				return nil, ErrConflict
			}
		}
		prev, err := decodeAppSet(cur)
		if err != nil {
			return nil, err
		}
		next := set.DeepCopy()
		next.Metadata.CreationTimestamp = prev.Metadata.CreationTimestamp
		next.Metadata.DeletionTimestamp = prev.Metadata.DeletionTimestamp
		next.Metadata.Generation = prev.Metadata.Generation
		a, _ := json.Marshal(prev.Spec)
		b, _ := json.Marshal(next.Spec)
		if string(a) != string(b) {
			next.Metadata.Generation++
		}
		next.Status = prev.Status
		enc, err := encodeAppSet(next)
		if err != nil {
			return nil, err
		}
		err = s.kv.CAS(ctx, key, enc, cur.ModifyIndex)
		if errors.Is(err, ErrConflict) && set.Metadata.ResourceVersion == "" && attempt < 20 {
			continue
		}
		if err != nil {
			return nil, err
		}
		return s.GetApplicationSet(ctx, set.Metadata.Name)
	}
}

// MutateApplicationSet applies fn to the latest version of an
// ApplicationSet, status included, retrying when another writer won.
func (s *Store) MutateApplicationSet(ctx context.Context, name string, fn func(*v1alpha1.ApplicationSet) error) (*v1alpha1.ApplicationSet, error) {
	key := s.key(appSetsDir, name)
	for attempt := 0; ; attempt++ {
		cur, err := s.kv.Get(ctx, key)
		if err != nil {
			return nil, err
		}
		set, err := decodeAppSet(cur)
		if err != nil {
			return nil, err
		}
		if err := fn(set); err != nil {
			return nil, err
		}
		enc, err := encodeAppSet(set)
		if err != nil {
			return nil, err
		}
		err = s.kv.CAS(ctx, key, enc, cur.ModifyIndex)
		if errors.Is(err, ErrConflict) && attempt < 20 {
			continue
		}
		if err != nil {
			return nil, err
		}
		return s.GetApplicationSet(ctx, name)
	}
}

// DeleteApplicationSet removes an ApplicationSet.
func (s *Store) DeleteApplicationSet(ctx context.Context, name string) error {
	return s.kv.Delete(ctx, s.key(appSetsDir, name))
}

// WatchApplicationSets streams the names of the ApplicationSets that are
// created, changed or deleted, until ctx ends; the first batch names every
// existing one.
func (s *Store) WatchApplicationSets(ctx context.Context) <-chan string {
	ch := make(chan string, 64)
	go func() {
		defer close(ch)
		known := map[string]uint64{}
		var index uint64
		backoff := time.Second
		for ctx.Err() == nil {
			entries, next, err := s.kv.Watch(ctx, s.key(appSetsDir, ""), index)
			if err != nil {
				select {
				case <-ctx.Done():
					return
				case <-time.After(backoff):
				}
				backoff = min(backoff*2, 30*time.Second)
				continue
			}
			backoff = time.Second
			index = next
			seen := map[string]bool{}
			var changed []string
			for _, e := range entries {
				name := strings.TrimPrefix(e.Key, s.key(appSetsDir, ""))
				seen[name] = true
				if known[name] != e.ModifyIndex {
					known[name] = e.ModifyIndex
					changed = append(changed, name)
				}
			}
			for name := range known {
				if !seen[name] {
					delete(known, name)
					changed = append(changed, name)
				}
			}
			for _, name := range changed {
				select {
				case ch <- name:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return ch
}
