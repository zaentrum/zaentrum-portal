// node --test (type stripping, Node >= 22.18): when the launchpad tells an
// admin why the consoles are closed. Excluded from the app's tsc program.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { adminElsewhere } from './access.ts';

const me = (roles: string[], isAdmin: boolean, client = 'chino-web') => ({
  username: 'admin', roles, isAdmin, adminRole: 'zaentrum-admin', client,
});

test('the admin role through another client is named', () => {
  assert.deepEqual(adminElsewhere(me(['zaentrum-admin'], false)), { role: 'zaentrum-admin', client: 'chino-web' });
  // An admin through the portal's own client needs no note; a viewer has
  // nothing to be told.
  assert.equal(adminElsewhere(me(['zaentrum-admin'], true, 'zaentrum-web')), null);
  assert.equal(adminElsewhere(me(['zaentrum-user'], false)), null);
  // An older portal-api says no admin role: nothing to compare with.
  assert.equal(adminElsewhere({ username: 'a', roles: ['zaentrum-admin'], isAdmin: false }), null);
  assert.equal(adminElsewhere(null), null);
});
