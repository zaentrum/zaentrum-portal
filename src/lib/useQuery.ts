import { useCallback, useEffect, useRef, useState } from 'react';
import { useGql } from './gql';

export interface QueryState<T> {
  data: T | null;
  loading: boolean;
  error: string | null;
  refetch: () => void;
}

/** useQuery runs a GraphQL query on mount + whenever `deps` change. */
export function useQuery<T>(query: string, variables?: Record<string, unknown>, deps: unknown[] = []): QueryState<T> {
  const gql = useGql();
  const [data, setData] = useState<T | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [nonce, setNonce] = useState(0);
  // keep variables stable across renders by serialising
  const varsKey = JSON.stringify(variables ?? {});
  const mounted = useRef(true);

  useEffect(() => {
    mounted.current = true;
    return () => {
      mounted.current = false;
    };
  }, []);

  useEffect(() => {
    setLoading(true);
    setError(null);
    gql<T>(query, variables)
      .then((d) => mounted.current && setData(d))
      .catch((e: unknown) => mounted.current && setError(e instanceof Error ? e.message : String(e)))
      .finally(() => mounted.current && setLoading(false));
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [query, varsKey, nonce, ...deps]);

  const refetch = useCallback(() => setNonce((n) => n + 1), []);
  return { data, loading, error, refetch };
}
