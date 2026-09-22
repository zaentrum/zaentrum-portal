// node --test (type stripping, Node >= 22.18): the pure helpers behind the
// controller card. Excluded from the app's tsc program.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import {
  controllerNote,
  controllerPath,
  controllerUpdate,
  controllerVersion,
  hasController,
  notReportedNote,
} from './controller.ts';
import type { OperatorController } from './api.ts';

// Each source names a DIFFERENT thing to go and do. Getting this wrong is
// worse than saying nothing: it sends an administrator to a subscription that
// does not exist, or to a manifest that is not what installed this cluster.
test('every install source names its own upgrade path', () => {
  assert.match(controllerPath('olm').path, /subscription/);
  assert.match(controllerPath('olm').installed, /OLM/);

  assert.match(controllerPath('manifest').path, /install manifest/);
  assert.match(controllerPath('manifest').installed, /install manifest/);

  assert.match(controllerPath('appliance').path, /appliance/);
  assert.match(controllerPath('appliance').installed, /appliance/);

  // Unknown, and not reported at all, are the same advice: all three paths.
  for (const source of ['unknown', '', undefined]) {
    const p = controllerPath(source);
    assert.equal(p.installed, 'not reported');
    assert.match(p.path, /OLM subscription/);
    assert.match(p.path, /install manifest/);
    assert.match(p.path, /appliance/);
  }

  // A source this build has not heard of is still the truth about the install,
  // so it is shown rather than erased.
  assert.equal(controllerPath('helm').installed, 'helm');
  assert.match(controllerPath('helm').path, /helm/);
});

// The note is a statement, never an instruction the portal could carry out.
test('the note says the platform does not do it', () => {
  for (const source of ['olm', 'manifest', 'appliance', 'unknown']) {
    assert.match(controllerNote(source), /updated outside the platform/);
  }
  assert.match(notReportedNote, /does not report its controller/);
  assert.match(notReportedNote, /outside the platform/);
});

test('the version is what was reported, else what the image says', () => {
  assert.equal(controllerVersion({ version: 'v0.4.1', image: 'ghcr.io/example/operator:v0.4.1' }), 'v0.4.1');
  // No version, but an image: the tag identifies the build.
  assert.equal(controllerVersion({ image: 'ghcr.io/example/operator:v0.4.1' }), 'v0.4.1');
  assert.equal(controllerVersion({ image: `ghcr.io/example/operator@sha256:${'a'.repeat(64)}` }), 'sha256:aaaaaaaaaaaa');
  // A reference with neither tag nor digest is pulled as :latest, which says
  // nothing about which build runs — so it is not reported as a version.
  assert.equal(controllerVersion({ image: 'ghcr.io/example/operator' }), 'unknown');
  assert.equal(controllerVersion({}), 'unknown');
  assert.equal(controllerVersion(undefined), 'unknown');
});

// The field is absent against every operator older than it, and may arrive
// empty. Both have to render as "cannot tell you", not as a row of dashes.
test('an operator that reports nothing is told apart from one that reports something', () => {
  assert.equal(hasController(undefined), false);
  assert.equal(hasController({}), false);
  assert.equal(hasController({ image: '  ' }), false);
  assert.equal(hasController({ source: 'olm' }), true);
  assert.equal(hasController({ image: 'ghcr.io/example/operator:v0.4.1' }), true);
});

test('an update is only reported when it is newer than what runs', () => {
  const running: OperatorController = { version: 'v0.4.1', image: 'ghcr.io/example/operator:v0.4.1' };
  assert.equal(controllerUpdate({ ...running, availableUpdate: 'v0.5.0' }), 'v0.5.0');
  assert.equal(controllerUpdate({ ...running, availableUpdate: '' }), '');
  assert.equal(controllerUpdate(running), '');
  // The channel offering what is already installed is not an update.
  assert.equal(controllerUpdate({ ...running, availableUpdate: 'v0.4.1' }), '');
  assert.equal(controllerUpdate(undefined), '');
});
