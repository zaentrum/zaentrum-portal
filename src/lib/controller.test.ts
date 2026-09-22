// node --test (type stripping, Node >= 22.18): the pure helpers behind the
// controller card. Excluded from the app's tsc program.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import {
  controllerNote,
  controllerPath,
  controllerUpdate,
  controllerUpdateLabel,
  controllerVersion,
  hasController,
  isVersionLike,
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

// `v` and a digit is the release convention; dotted numbers are a release too;
// everything else is a name that outlives the images it points at.
test('a release is told apart from a moving tag', () => {
  for (const v of ['v0.5.0', 'v1', 'V2.0.0', '1.5.0', '1.5', '2.0.0-rc1', '1.4.0+build.7']) {
    assert.equal(isVersionLike(v), true, v);
  }
  for (const m of ['latest', 'stable', 'edge', 'main', 'nightly', 'sha-19ea431', '1', 'v', '', '  ', 'release-1.5', 'latest.1']) {
    assert.equal(isVersionLike(m), false, m);
  }
  assert.equal(isVersionLike(undefined), false);
});

// Two different facts arrive in one field and cannot share a sentence. Live,
// an install running :sha-19ea431 against a channel serving :latest rendered
// as "update available: latest", which reads as a version number and names
// nothing a reader can compare themselves against.
test('a version is offered by name, a channel tag as a moved channel', () => {
  const pinned: OperatorController = { version: 'v0.4.1', image: 'ghcr.io/example/operator:v0.4.1' };
  assert.equal(controllerUpdateLabel({ ...pinned, availableUpdate: 'v0.5.0' }), 'update available: v0.5.0');
  assert.equal(controllerUpdateLabel({ ...pinned, availableUpdate: '1.5.0' }), 'update available: 1.5.0');

  const commit: OperatorController = { version: 'sha-19ea431', image: 'ghcr.io/example/operator:sha-19ea431' };
  for (const tag of ['latest', 'stable', 'edge']) {
    const label = controllerUpdateLabel({ ...commit, availableUpdate: tag });
    assert.equal(label, `"${tag}" serves a different image`);
    // Whatever it says, it must not read as a version being offered.
    assert.ok(!label.includes(`update available: ${tag}`), label);
  }

  // Nothing to say stays nothing to say.
  assert.equal(controllerUpdateLabel({ ...pinned, availableUpdate: '' }), '');
  assert.equal(controllerUpdateLabel({ ...pinned, availableUpdate: 'v0.4.1' }), '');
  assert.equal(controllerUpdateLabel(undefined), '');
});
