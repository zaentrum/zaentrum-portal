// node --test (type stripping, Node >= 22.18): how the registry console reads
// and writes who may see a tile or a space. Excluded from the app's tsc program.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { audienceChoice, audienceFor, audienceLabel, parseRoles } from './audience.ts';

test('an audience reads as the choice an admin made', () => {
  assert.equal(audienceChoice([]), 'everyone');
  assert.equal(audienceChoice(undefined), 'everyone');
  assert.equal(audienceChoice(['zaentrum-admin']), 'admins');
  assert.equal(audienceChoice(['acme-admin'], 'acme-admin'), 'admins');
  // The default admin role under another name is just a role.
  assert.equal(audienceChoice(['zaentrum-admin'], 'acme-admin'), 'roles');
  assert.equal(audienceChoice(['zaentrum-admin', 'ops']), 'roles');
});

test('a choice writes the audience it stands for', () => {
  assert.deepEqual(audienceFor('everyone', 'ops'), []);
  assert.deepEqual(audienceFor('admins', '', 'acme-admin'), ['acme-admin']);
  assert.deepEqual(audienceFor('roles', ' ops, beta  ops,,'), ['ops', 'beta']);
  assert.deepEqual(parseRoles(''), []);
});

test('a table cell names it plainly', () => {
  assert.equal(audienceLabel([]), 'everyone');
  assert.equal(audienceLabel(['zaentrum-admin']), 'admins');
  assert.equal(audienceLabel(['ops', 'beta']), 'ops, beta');
});
