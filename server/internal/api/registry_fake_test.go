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

func (f *fakeAddonStore) Launchpad(ctx context.Context) (model.Launchpad, error) {
	spaces, _ := f.ListSpaces(ctx)
	apps, _ := f.ListApps(ctx)
	tiles, _ := f.ListTiles(ctx)
	return store.AssembleLaunchpad(spaces, apps, tiles), nil
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
