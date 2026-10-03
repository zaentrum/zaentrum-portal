import type { Me } from './api';

// adminElsewhere: the caller holds the admin role, but in a token issued to a
// client the admin routes do not take — the portal's consoles stay closed to
// them, and the launchpad says why rather than leaving an admin to wonder
// where the settings went. null for everyone else, and against a portal-api
// that does not say which role is the admin role.
export function adminElsewhere(me: Me | null): { role: string; client: string } | null {
  if (!me || me.isAdmin || !me.adminRole || !me.roles.includes(me.adminRole)) return null;
  return { role: me.adminRole, client: me.client ?? '' };
}
