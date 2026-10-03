package api

import (
	"context"
	"errors"
	"sort"

	"github.com/zaentrum/zaentrum-portal/server/internal/model"
	"github.com/zaentrum/zaentrum-portal/server/internal/store"
)

// fakeAddonStore is the whole registry in memory: these are the methods the
// launchpad, the registry console, the slot API and the app proxy use, with
// the rules the database keeps — tiles go with their app or space, a tile
// needs both.

func (f *fakeAddonStore) Launchpad(ctx context.Context, roles []string) (model.Launchpad, error) {
	spaces, _ := f.ListSpaces(ctx)
	apps, _ := f.ListApps(ctx)
	tiles, _ := f.ListTiles(ctx)
	return store.AssembleLaunchpad(spaces, apps, tiles, roles), nil
}

func (f *fakeAddonStore) GetSpace(_ context.Context, key string) (*model.Space, error) {
	if sp, ok := f.spaces[key]; ok {
		return &sp, nil
	}
	return nil, store.ErrNotFound
}

func (f *fakeAddonStore) GetTile(_ context.Context, key string) (*model.Tile, error) {
	if t, ok := f.tiles[key]; ok {
		return &t, nil
	}
	return nil, store.ErrNotFound
}

// keepAudience is the store's rule: a write without an audience keeps the
// stored one, and a new row without one is everyone's.
func keepAudience(next, stored []string, exists bool) []string {
	switch {
	case next != nil:
		return next
	case exists:
		return stored
	}
	return []string{}
}

func (f *fakeAddonStore) UpsertApp(_ context.Context, app model.App) error {
	f.apps[app.Key] = app
	return nil
}

func (f *fakeAddonStore) DeleteApp(_ context.Context, key string) error {
	if _, ok := f.apps[key]; !ok {
		return store.ErrNotFound
	}
	delete(f.apps, key)
	delete(f.addons, key)
	for k, t := range f.tiles {
		if t.AppKey == key {
			delete(f.tiles, k)
		}
	}
	return nil
}

func (f *fakeAddonStore) UpsertSpace(_ context.Context, sp model.Space) error {
	old, exists := f.spaces[sp.Key]
	sp.Audience = keepAudience(sp.Audience, old.Audience, exists)
	f.spaces[sp.Key] = sp
	return nil
}

func (f *fakeAddonStore) DeleteSpace(_ context.Context, key string) error {
	if _, ok := f.spaces[key]; !ok {
		return store.ErrNotFound
	}
	delete(f.spaces, key)
	for k, t := range f.tiles {
		if t.SpaceKey == key {
			delete(f.tiles, k)
		}
	}
	return nil
}

func (f *fakeAddonStore) ListTiles(context.Context) ([]model.Tile, error) {
	out := make([]model.Tile, 0, len(f.tiles))
	for _, t := range f.tiles {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Order != out[j].Order {
			return out[i].Order < out[j].Order
		}
		return out[i].Key < out[j].Key
	})
	return out, nil
}

func (f *fakeAddonStore) UpsertTile(_ context.Context, t model.Tile) error {
	_, app := f.apps[t.AppKey]
	_, space := f.spaces[t.SpaceKey]
	if !app || !space {
		return errors.New("violates foreign key: app_key or space_key does not exist")
	}
	old, exists := f.tiles[t.Key]
	t.Audience = keepAudience(t.Audience, old.Audience, exists)
	f.tiles[t.Key] = t
	return nil
}

func (f *fakeAddonStore) DeleteTile(_ context.Context, key string) error {
	if _, ok := f.tiles[key]; !ok {
		return store.ErrNotFound
	}
	delete(f.tiles, key)
	return nil
}

func (f *fakeAddonStore) ListExtensions(context.Context) ([]model.Extension, error) {
	out := make([]model.Extension, 0, len(f.extensions))
	for _, e := range f.extensions {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

func (f *fakeAddonStore) ListExtensionsForSlot(ctx context.Context, slot string) ([]model.Extension, error) {
	all, _ := f.ListExtensions(ctx)
	var out []model.Extension
	for _, e := range all {
		if e.Slot == slot && e.Enabled {
			out = append(out, e)
		}
	}
	return out, nil
}

func (f *fakeAddonStore) GetExtension(_ context.Context, key string) (*model.Extension, error) {
	if e, ok := f.extensions[key]; ok {
		return &e, nil
	}
	return nil, store.ErrNotFound
}

func (f *fakeAddonStore) UpsertExtension(_ context.Context, e model.Extension) error {
	f.extensions[e.Key] = e
	return nil
}

func (f *fakeAddonStore) DeleteExtension(_ context.Context, key string) error {
	if _, ok := f.extensions[key]; !ok {
		return store.ErrNotFound
	}
	delete(f.extensions, key)
	return nil
}
