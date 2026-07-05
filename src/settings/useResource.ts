import { useCallback, useEffect, useState } from 'react';
import { usePortalApi } from '../lib/api';

interface Resource<T> {
  items: T[];
  loading: boolean;
  error: string | null;
  reload: () => void;
  save: (item: T, isNew: boolean) => Promise<void>;
  remove: (key: string) => Promise<void>;
}

// useResource is a small CRUD hook over a keyed portal-api collection
// (/apps, /spaces, /tiles). POST creates, PATCH /{key} replaces, DELETE removes.
export function useResource<T extends { key: string }>(path: string): Resource<T> {
  const api = usePortalApi();
  const [items, setItems] = useState<T[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);

  const reload = useCallback(() => {
    setLoading(true);
    setError(null);
    api<T[]>(path)
      .then(setItems)
      .catch((e) => setError(e instanceof Error ? e.message : String(e)))
      .finally(() => setLoading(false));
  }, [api, path]);

  useEffect(reload, [reload]);

  const save = useCallback(
    async (item: T, isNew: boolean) => {
      await api<T>(isNew ? path : `${path}/${encodeURIComponent(item.key)}`, {
        method: isNew ? 'POST' : 'PATCH',
        body: JSON.stringify(item),
      });
      reload();
    },
    [api, path, reload],
  );

  const remove = useCallback(
    async (key: string) => {
      await api<void>(`${path}/${encodeURIComponent(key)}`, { method: 'DELETE' });
      reload();
    },
    [api, path, reload],
  );

  return { items, loading, error, reload, save, remove };
}
