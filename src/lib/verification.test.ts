// node --test (type stripping, Node >= 22.18): the pure helpers behind the
// verification card. Excluded from the app's tsc program.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import {
  RESULT_ERROR,
  RESULT_FAILED,
  RESULT_PASSED,
  RESULT_RUNNING,
  RESULT_SKIPPED,
  isAnswered,
  since,
  verificationAttention,
  verificationBusy,
  verificationCondition,
  verificationCounts,
  verificationFinished,
  verificationLabel,
  verificationNote,
  verificationOtherCounts,
  verificationTone,
  verificationTrigger,
} from './verification.ts';
import type { OperatorVerification } from './api.ts';

const never: OperatorVerification = { enabled: true, result: null };

const failed: OperatorVerification = {
  enabled: true,
  result: RESULT_FAILED,
  trigger: 'update',
  request: 'tok-1',
  version: 'v0.5.0',
  startedAt: '2026-10-03T08:00:00Z',
  finishedAt: '2026-10-03T08:01:30Z',
  job: 'zaentrum-verify-7f3k2',
  passed: 11,
  failed: 2,
  warned: 1,
  skipped: 0,
  checks: [
    { name: 'workloads ready', status: 'ok', detail: '' },
    { name: 'issuer latency', status: 'warn', detail: '1.8 s, over 1 s' },
    { name: 'katalog-api health', status: 'fail', detail: '503 from /healthz' },
    { name: 'search index', status: 'skip', detail: 'not installed' },
    { name: 'transcoder ready', status: 'fail', detail: '0/1 ready' },
  ],
  message: '2 of 14 checks failed',
};

test('a result has a tone and a lower-case label; no result is "never"', () => {
  assert.equal(verificationTone(RESULT_PASSED), 'green');
  assert.equal(verificationTone(RESULT_RUNNING), 'blue');
  // No red in the design system: both kinds of failure want attention.
  assert.equal(verificationTone(RESULT_FAILED), 'amber');
  assert.equal(verificationTone(RESULT_ERROR), 'amber');
  assert.equal(verificationTone(RESULT_SKIPPED), 'neutral');
  assert.equal(verificationTone(null), 'neutral');

  assert.equal(verificationLabel({ result: RESULT_PASSED }), 'passed');
  assert.equal(verificationLabel(failed), 'failed');
  assert.equal(verificationLabel(never), 'never');
  assert.equal(verificationLabel(undefined), 'never');
  assert.equal(verificationLabel({ result: '  ' }), 'never');
  // A record that could not be read is not the absence of one.
  assert.equal(verificationLabel({ result: null, note: 'the record cannot be read' }), 'unknown');
  // A result this build has not heard of is still the operator's answer.
  assert.equal(verificationLabel({ result: 'Flaky' }), 'flaky');
  assert.equal(verificationTone('Flaky'), 'neutral');
});

test('busy while a run is in progress or a request waits', () => {
  assert.equal(verificationBusy({ ...failed, result: RESULT_RUNNING }), true);
  assert.equal(verificationBusy({ ...failed, pendingRequest: 'tok-2' }), true);
  assert.equal(verificationBusy({ ...never, pendingRequest: 'tok-2' }), true);
  assert.equal(verificationBusy(failed), false);
  assert.equal(verificationBusy(never), false);
  assert.equal(verificationBusy(undefined), false);
});

// The run that answers a request is the one carrying its token — a newer run
// for another reason is not the answer, and a run still going has not ended.
test('a request is answered by its own run, once that run has ended', () => {
  assert.equal(isAnswered({ ...failed, request: 'tok-2' }, 'tok-2'), true);
  assert.equal(isAnswered({ ...failed, request: 'tok-2', result: RESULT_RUNNING }, 'tok-2'), false);
  assert.equal(isAnswered({ ...failed, request: 'tok-1', pendingRequest: 'tok-2' }, 'tok-2'), false);
  assert.equal(isAnswered({ ...failed, request: '' }, ''), false);
  assert.equal(isAnswered(never, 'tok-2'), false);
  assert.equal(isAnswered(undefined, 'tok-2'), false);
});

test('counts say what ran, and nothing when nothing has been counted', () => {
  // Passed and failed are the pair every run has, zero included.
  assert.equal(verificationCounts(failed), '11 passed · 2 failed');
  assert.equal(verificationCounts({ ...failed, passed: 14, failed: 0 }), '14 passed · 0 failed');
  // The rest only when there is some.
  assert.equal(verificationOtherCounts(failed), '1 warned');
  assert.equal(verificationOtherCounts({ ...failed, skipped: 3 }), '1 warned · 3 skipped');
  assert.equal(verificationOtherCounts({ ...failed, warned: 0, skipped: 2 }), '2 skipped');
  assert.equal(verificationOtherCounts({ ...failed, warned: 0 }), '');
  // A row of zeros would claim that checks ran.
  const inProgress = { ...failed, result: RESULT_RUNNING, passed: 0, failed: 0, warned: 0 };
  for (const v of [never, inProgress, undefined]) {
    assert.equal(verificationCounts(v), '');
    assert.equal(verificationOtherCounts(v), '');
  }
});

test('the trigger says what the run came after, and the version it checked', () => {
  assert.equal(verificationTrigger(failed), 'update to v0.5.0');
  assert.equal(verificationTrigger({ ...failed, trigger: 'request' }), 'request on v0.5.0');
  assert.equal(verificationTrigger({ ...failed, version: '' }), 'update');
  assert.equal(verificationTrigger({ ...failed, trigger: '' }), 'v0.5.0');
  assert.equal(verificationTrigger({ ...failed, trigger: 'schedule' }), 'schedule · v0.5.0');
  assert.equal(verificationTrigger({ ...failed, trigger: 'schedule', version: '' }), 'schedule');
  assert.equal(verificationTrigger(never), '');
});

test('failures are listed first, then warnings; ok and skip are only counted', () => {
  assert.deepEqual(
    verificationAttention(failed).map((c) => `${c.status} ${c.name}`),
    ['fail katalog-api health', 'fail transcoder ready', 'warn issuer latency'],
  );
  assert.deepEqual(verificationAttention(never), []);
  assert.deepEqual(verificationAttention(undefined), []);
});

test('the Verified condition reads as one line', () => {
  const v = { ...failed, condition: { status: 'False', reason: 'ChecksFailed', lastTransitionTime: '2026-10-03T08:01:30Z' } };
  assert.equal(verificationCondition(v), 'Verified False (ChecksFailed) since 2026-10-03T08:01:30Z');
  assert.equal(verificationCondition({ ...failed, condition: { status: 'True', reason: '', lastTransitionTime: '' } }), 'Verified True');
  assert.equal(verificationCondition(failed), '');
});

test('since says how long ago, in short words', () => {
  const now = Date.parse('2026-10-03T10:00:00Z');
  assert.equal(since('2026-10-03T09:59:55Z', now), 'just now');
  assert.equal(since('2026-10-03T09:59:20Z', now), '40 s ago');
  assert.equal(since('2026-10-03T09:57:00Z', now), '3 min ago');
  assert.equal(since('2026-10-03T05:00:00Z', now), '5 h ago');
  assert.equal(since('2026-10-01T11:00:00Z', now), '47 h ago');
  assert.equal(since('2026-09-29T10:00:00Z', now), '4 d ago');
  // A clock ahead of the browser's is not a negative age.
  assert.equal(since('2026-10-03T10:05:00Z', now), 'just now');
  assert.equal(since('', now), '');
  assert.equal(since(undefined, now), '');
  assert.equal(since('yesterday', now), '');
});

test('the note says what the card cannot', () => {
  // Off wins: whatever the record says, nothing will run.
  assert.match(verificationNote({ ...failed, enabled: false, pendingRequest: 'tok-2' }), /off.*spec\.verification\.enabled is false/);
  assert.match(verificationNote({ ...never, note: 'the record cannot be read' }), /cannot be read/);
  assert.match(verificationNote({ ...failed, result: RESULT_RUNNING, pendingRequest: 'tok-2' }), /starts when the one in progress ends/);
  assert.match(verificationNote({ ...never, pendingRequest: 'tok-2' }), /older than verification never starts it/);
  assert.match(verificationNote({ ...failed, pendingRequest: 'tok-2' }), /waits for the operator to start it/);
  // Never is said, never left blank: silence would read as "nothing to check".
  assert.match(verificationNote(never), /No run has been reported/);
  // A run that ended is the operator's own line, or nothing.
  assert.equal(verificationNote(failed), '2 of 14 checks failed');
  assert.equal(verificationNote({ ...failed, message: '' }), '');
});

test('the toolbar says how a requested run ended', () => {
  assert.equal(verificationFinished(failed), 'verification failed: 2 of 14 checks failed');
  assert.equal(verificationFinished({ ...failed, result: RESULT_PASSED, message: '' }), 'verification passed');
});
