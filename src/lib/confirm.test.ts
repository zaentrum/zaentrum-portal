// node --test (type stripping, Node >= 22.18): what the console's dialogs say
// before a change, and what they send. Excluded from the app's tsc program.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import {
  MAX_REPLICAS,
  applyUpdateBody,
  canScaleTo,
  channelConfirmation,
  deletable,
  deleteConfirmation,
  minReplicas,
  restartConfirmation,
  scaleConfirmation,
  updateConfirmation,
  updateModeConfirmation,
} from './confirm.ts';
import type { Instance, OperatorInfo } from './api.ts';

const instance = (over: Partial<Instance> = {}): Instance => ({
  name: 'chino-api',
  image: 'ghcr.io/example/chino-api:1.4.0',
  desiredReplicas: 2,
  readyReplicas: 2,
  updatedReplicas: 2,
  availableReplicas: 2,
  restarts: 0,
  phase: 'ready',
  protected: false,
  operatorManaged: false,
  group: 'platform',
  reason: '',
  alwaysPull: false,
  ...over,
});

const op = (over: Partial<OperatorInfo> = {}): OperatorInfo => ({
  present: true,
  name: 'zaentrum',
  channel: 'stable',
  updateMode: 'manual',
  currentVersion: '1.4.0',
  availableUpdate: '1.5.0',
  ...over,
});

// The admin stack — what the console runs on — keeps a replica; the server
// refuses the step to zero, and the console does not offer it.
test('the console offers only the scales the platform makes', () => {
  const admin = instance({ name: 'portal-api', adminStack: true, desiredReplicas: 1 });
  assert.equal(minReplicas(admin), 1);
  assert.equal(canScaleTo(admin, 0), false);
  assert.equal(canScaleTo(admin, 1), true);
  assert.equal(canScaleTo(instance(), 0), true);
  assert.equal(canScaleTo(instance(), MAX_REPLICAS + 1), false);
  assert.equal(canScaleTo(instance({ protected: true }), 1), false);
  // And should one be asked for anyway, the dialog says why it is not done.
  const c = scaleConfirmation(admin, 0);
  assert.match(c.blocked ?? '', /keeps at least 1 replica/);
  assert.match(c.blocked ?? '', /deployment channel/);
});

test('a scale says what stops, what starts, and where the count is kept', () => {
  const down = scaleConfirmation(instance({ desiredReplicas: 1 }), 0);
  assert.equal(down.title, 'scale chino-api from 1 to 0?');
  assert.match(down.lines[0], /chino-api stops: nothing serves it/);
  assert.equal(down.danger, true);
  assert.equal(down.confirm, 'scale to 0');
  assert.match(scaleConfirmation(instance({ desiredReplicas: 3 }), 2).lines[0], /^1 pod stops; 2 keep serving/);
  assert.match(scaleConfirmation(instance({ desiredReplicas: 1 }), 3).lines[0], /^2 more pods start beside the 1 running/);
  assert.match(scaleConfirmation(instance(), 3).lines[1], /scaled directly/);
  assert.match(scaleConfirmation(instance({ operatorManaged: true }), 3).lines[1], /operator's resource records the count/);
  assert.equal(scaleConfirmation(instance(), 3).danger, false);
});

test('a restart says whether the workload keeps answering', () => {
  const rolling = restartConfirmation(instance({ strategy: 'RollingUpdate' }));
  assert.match(rolling.lines[0], /one at a time: chino-api keeps answering/);
  assert.equal(rolling.danger, false);
  const recreate = restartConfirmation(instance({ strategy: 'Recreate' }));
  assert.match(recreate.lines[0], /does not answer until the new pod is ready/);
  assert.equal(recreate.danger, true);
  assert.match(restartConfirmation(instance({ alwaysPull: true })).lines.join(' '), /pulls its image again/);
  const console = restartConfirmation(instance({ name: 'portal-api', adminStack: true }));
  assert.match(console.lines.join(' '), /the console runs on it/);
  assert.equal(console.blocked, undefined);
  // The admin stack is not restarted from here when it recreates its pods —
  // the server refuses it, and the dialog says why instead of asking.
  assert.match(restartConfirmation(instance({ name: 'portal-api', adminStack: true, strategy: 'Recreate' })).blocked ?? '', /recreates its pods/);
});

test('a channel or update mode says what the operator then does on its own', () => {
  const edge = channelConfirmation(op(), 'edge');
  assert.equal(edge.title, 'follow the edge channel?');
  assert.match(edge.lines.join(' '), /instead of stable/);
  assert.match(edge.lines.join(' '), /pre-release/);
  assert.match(edge.lines.join(' '), /nothing is installed until you apply an update/);
  assert.equal(edge.danger, false);
  const auto = channelConfirmation(op({ updateMode: 'auto' }), 'edge');
  assert.match(auto.lines.join(' '), /installs the newest version on edge by itself/);
  assert.equal(auto.danger, true);

  const toAuto = updateModeConfirmation(op(), 'auto');
  assert.match(toAuto.lines.join(' '), /as soon as it finds one, without asking/);
  assert.equal(toAuto.danger, true);
  assert.match(updateModeConfirmation(op({ updateMode: 'auto' }), 'manual').lines[0], /only when you apply it/);
});

// The update applied is the one shown: the body names it, and the server
// refuses it when the operator has found another since.
test('an update names the version it was shown', () => {
  const c = updateConfirmation(op(), 14);
  assert.equal(c.title, 'update the platform to 1.5.0?');
  assert.match(c.lines[0], /from 1.4.0 to 1.5.0, on the stable channel/);
  assert.match(c.lines[1], /\(14\) rolls to it/);
  assert.match(c.lines[2], /nothing is applied/);
  assert.equal(c.confirm, 'update to 1.5.0');
  assert.deepEqual(applyUpdateBody('1.5.0'), { version: '1.5.0' });
});

test('a delete says what goes with it, and a core entry is not deleted', () => {
  const tiles = [
    { appKey: 'katalog', spaceKey: 'manage' },
    { appKey: 'katalog-manage', spaceKey: 'manage' },
    { appKey: 'chino', spaceKey: 'apps' },
  ];
  assert.match(deleteConfirmation('space', { key: 'manage-old' }, { tiles }).lines[0], /holds no tiles/);
  assert.match(deleteConfirmation('space', { key: 'apps2' }, { tiles: [{ appKey: 'x', spaceKey: 'apps2' }] }).lines[0], /the 1 tile in it goes with it/);
  assert.match(deleteConfirmation('app', { key: 'katalog' }, { tiles }).lines[0], /its 1 tile goes with it/);
  assert.equal(deleteConfirmation('tile', { key: 'chino.open' }).danger, true);

  assert.equal(deletable({ core: true }), false);
  assert.equal(deletable({}), true);
  assert.match(deleteConfirmation('app', { key: 'chino', core: true }).blocked ?? '', /core entry.*disable it instead/);
  assert.match(deleteConfirmation('space', { key: 'apps', core: true }).blocked ?? '', /core entry/);
});
