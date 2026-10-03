// node --test (type stripping, Node >= 22.18): how the values dialog plans a
// change before it applies it, and puts an addon back. Excluded from the
// app's tsc program.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { changesPlanned, restorePatch, valuesPatch, writeState, withAddonDefaults } from './addons.ts';
import type { AddonChart } from './api.ts';

// Saving suspends the addon, whatever the server's default: the operator
// plans the new spec and applies nothing until the plan is installed.
test('saving values plans them first', () => {
  assert.deepEqual(valuesPatch({ logLevel: 'debug' }, {}, [], []), { values: { logLevel: 'debug' }, suspend: true });
  assert.deepEqual(valuesPatch({}, {}, [], []), { values: null, suspend: true });
  assert.deepEqual(valuesPatch({}, { 'db.password': 'x' }, ['old.key', 'not.set'], ['old.key']), {
    values: null,
    suspend: true,
    secretValues: { 'db.password': 'x' },
    clearSecrets: ['old.key'],
  });
});

test('putting it back restores values and suspension', () => {
  assert.deepEqual(restorePatch({ values: { worker: { replicas: 2 } }, suspended: false }), {
    values: { worker: { replicas: 2 } },
    suspend: false,
  });
  assert.deepEqual(restorePatch({ values: null, suspended: true }), { values: null, suspend: true });
  assert.deepEqual(restorePatch({ values: {}, suspended: false }), { values: null, suspend: false });
});

const chart = (over: Partial<AddonChart>): AddonChart => ({
  name: 'example',
  chart: { ref: 'oci://registry.example.org/charts/example', version: '1.2.0' },
  suspended: true,
  phase: 'Planned',
  message: '',
  generation: 5,
  observedGeneration: 5,
  plan: { chart: { name: 'example', version: '1.2.0' }, valuesSchema: '', valuesErrors: null, violations: null, objects: null, workloads: null },
  components: [],
  lastAppliedChart: { ref: 'oci://registry.example.org/charts/example', version: '1.2.0' },
  values: null,
  secretKeys: [],
  registered: true,
  ...over,
});

// The plan shown is the one for this write; once someone else writes, it is
// not, and the dialog must not install or put back on their behalf.
test('a write is planning, planned, or moved on', () => {
  assert.equal(writeState(null, 5), 'planning');
  assert.equal(writeState(chart({ observedGeneration: 4 }), 5), 'planning');
  assert.equal(writeState(chart({ plan: null }), 5), 'planning');
  assert.equal(writeState(chart({}), 5), 'planned');
  assert.equal(writeState(chart({ generation: 6, observedGeneration: 6 }), 5), 'moved');
});

test('an installed addon with changes waiting says so', () => {
  const installed = withAddonDefaults({
    key: 'example',
    chart: { ref: 'oci://registry.example.org/charts/example', version: '1.2.0', lastApplied: { ref: 'oci://registry.example.org/charts/example', version: '1.2.0' } },
    suspended: true,
  });
  assert.equal(changesPlanned(installed), true);
  assert.equal(changesPlanned({ ...installed, suspended: false }), false);
  assert.equal(changesPlanned({ ...installed, chart: { ...installed.chart!, lastApplied: null } }), false); // planned, never installed
});
