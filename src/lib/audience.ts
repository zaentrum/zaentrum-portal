// Who may see a tile or a space on the launchpad.
//
// An audience is the realm roles that may see it; empty is everyone signed
// in. The server filters the launchpad by the caller's roles, so a tile kept
// from viewers is never sent to them. The registry console offers the three
// choices an admin means: everyone, admins (the role portal-api runs with),
// or roles they name.

export type AudienceChoice = 'everyone' | 'admins' | 'roles';

// The admin role portal-api runs with when /me does not say.
export const DEFAULT_ADMIN_ROLE = 'zaentrum-admin';

// parseRoles reads roles as typed: separated by commas or spaces, each once.
export function parseRoles(text: string): string[] {
  const out: string[] = [];
  for (const r of text.split(/[\s,]+/)) {
    if (r && !out.includes(r)) out.push(r);
  }
  return out;
}

export function audienceChoice(audience: string[] | null | undefined, adminRole = DEFAULT_ADMIN_ROLE): AudienceChoice {
  const a = audience ?? [];
  if (a.length === 0) return 'everyone';
  if (a.length === 1 && a[0] === adminRole) return 'admins';
  return 'roles';
}

// audienceFor is the audience a choice stands for; rolesText is read for
// 'roles' only.
export function audienceFor(choice: AudienceChoice, rolesText: string, adminRole = DEFAULT_ADMIN_ROLE): string[] {
  switch (choice) {
    case 'everyone':
      return [];
    case 'admins':
      return [adminRole];
    case 'roles':
      return parseRoles(rolesText);
  }
}

// audienceLabel words an audience for a table cell.
export function audienceLabel(audience: string[] | null | undefined, adminRole = DEFAULT_ADMIN_ROLE): string {
  switch (audienceChoice(audience, adminRole)) {
    case 'everyone':
      return 'everyone';
    case 'admins':
      return 'admins';
    case 'roles':
      return (audience ?? []).join(', ');
  }
}
