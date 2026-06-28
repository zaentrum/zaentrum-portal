import { useCallback } from 'react';
import { useAuth } from 'react-oidc-context';

// GraphQL endpoint of the katalog-manager service. Behind the demo's path-routing
// the service is published under /api/manage, and the Go service also mounts the
// GraphQL handler at /api/manage/query (in addition to /query) so this resolves
// both standalone and behind the proxy. Override with VITE_KATALOG_GQL.
export const KATALOG_GQL: string =
  (import.meta.env.VITE_KATALOG_GQL as string | undefined) ?? '/api/manage/query';

export interface GraphQLError {
  message: string;
}

class GqlError extends Error {
  constructor(public errors: GraphQLError[]) {
    super(errors.map((e) => e.message).join('; '));
    this.name = 'GqlError';
  }
}

/** useGql returns a typed GraphQL fetcher bound to the current access token. */
export function useGql() {
  const auth = useAuth();
  const token = auth.user?.access_token;
  return useCallback(
    async function gql<T>(query: string, variables?: Record<string, unknown>): Promise<T> {
      const res = await fetch(KATALOG_GQL, {
        method: 'POST',
        headers: {
          'Content-Type': 'application/json',
          Accept: 'application/json',
          ...(token ? { Authorization: `Bearer ${token}` } : {}),
        },
        body: JSON.stringify({ query, variables: variables ?? {} }),
      });
      if (!res.ok && res.status !== 200) {
        throw new Error(`katalog-manager ${res.status}`);
      }
      const json = (await res.json()) as { data?: T; errors?: GraphQLError[] };
      if (json.errors && json.errors.length) throw new GqlError(json.errors);
      return json.data as T;
    },
    [token],
  );
}
