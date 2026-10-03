import type { BadgeTone } from '@nalet/design-system';
import type { OperatorVerification, VerificationCheck } from './api';

// The platform checking itself, worded for the console.
//
// After every update the operator runs the platform's checks as a Job and
// records how it went; between updates an administrator may ask for a run.
// The console shows the record and asks — it runs nothing and judges nothing.
// What it has to get right is the reading: "never" is not a run of zero
// checks, and a request waiting is not the run that answers it.

// Results a run reports, in the operator's words. Anything else is shown as
// written: a result this build has not heard of is still the operator's.
export const RESULT_PASSED = 'Passed';
export const RESULT_FAILED = 'Failed';
export const RESULT_RUNNING = 'Running';
export const RESULT_SKIPPED = 'Skipped';
export const RESULT_ERROR = 'Error';

// verificationTone colours a result. Failed and Error are both amber: the
// design system has no red, and both want an administrator's attention.
export function verificationTone(result: string | null | undefined): BadgeTone {
  switch (result) {
    case RESULT_PASSED:
      return 'green';
    case RESULT_RUNNING:
      return 'blue';
    case RESULT_FAILED:
    case RESULT_ERROR:
      return 'amber';
    default:
      return 'neutral';
  }
}

// verificationLabel words a result for its badge: lower case like every badge
// in the console, "never" when no run was reported — and "unknown" when the
// record could not be read, which is not the same as there being none.
export function verificationLabel(v: Pick<OperatorVerification, 'result' | 'note'> | undefined): string {
  const r = (v?.result ?? '').trim();
  if (r) return r.toLowerCase();
  return v?.note ? 'unknown' : 'never';
}

// verificationBusy: a run is in progress, or a request waits for one. Asking
// again changes nothing then — the server joins a request that waits — so the
// button waits too.
export function verificationBusy(v: OperatorVerification | undefined): boolean {
  return !!v && (!!v.pendingRequest || v.result === RESULT_RUNNING);
}

// isAnswered: the run that answers request has ended. It is the run carrying
// the request's own token — not whichever run happens to be newest.
export function isAnswered(v: OperatorVerification | undefined, request: string): boolean {
  return !!v && !!request && v.request === request && !!v.result && v.result !== RESULT_RUNNING;
}

// counted: a run has ended, so its counts mean something. Never, and a run
// still in progress, have counted nothing — a row of zeros would say that
// checks ran.
const counted = (v: OperatorVerification | undefined): v is OperatorVerification =>
  !!v?.result && v.result !== RESULT_RUNNING;

// verificationCounts is the pair every run has: "13 passed · 1 failed". ''
// when nothing has been counted.
export function verificationCounts(v: OperatorVerification | undefined): string {
  if (!counted(v)) return '';
  return `${v.passed ?? 0} passed · ${v.failed ?? 0} failed`;
}

// verificationOtherCounts is what else the run counted, only when there is
// some: "1 warned · 2 skipped". '' otherwise.
export function verificationOtherCounts(v: OperatorVerification | undefined): string {
  if (!counted(v)) return '';
  const parts: string[] = [];
  if (v.warned) parts.push(`${v.warned} warned`);
  if (v.skipped) parts.push(`${v.skipped} skipped`);
  return parts.join(' · ');
}

// verificationTrigger is what the run came after, and the version it checked:
// "update to v0.5.0", "request on v0.5.0".
export function verificationTrigger(v: OperatorVerification | undefined): string {
  if (!v?.result) return '';
  const version = (v.version ?? '').trim();
  const trigger = (v.trigger ?? '').trim();
  switch (trigger) {
    case 'update':
      return version ? `update to ${version}` : 'update';
    case 'request':
      return version ? `request on ${version}` : 'request';
    case '':
      return version;
    default:
      return version ? `${trigger} · ${version}` : trigger;
  }
}

// verificationAttention is what needs a reader: the failed checks first, then
// the warnings. ok and skip are counted, not listed.
export function verificationAttention(v: OperatorVerification | undefined): VerificationCheck[] {
  const checks = v?.checks ?? [];
  return [...checks.filter((c) => c.status === 'fail'), ...checks.filter((c) => c.status === 'warn')];
}

// verificationCondition is the Verified condition in one line, for a tooltip:
// "Verified True (ChecksPassed) since 2026-10-03T08:01:30Z". '' when the
// resource has none.
export function verificationCondition(v: OperatorVerification | undefined): string {
  const c = v?.condition;
  if (!c?.status) return '';
  const reason = c.reason ? ` (${c.reason})` : '';
  const at = c.lastTransitionTime ? ` since ${c.lastTransitionTime}` : '';
  return `Verified ${c.status}${reason}${at}`;
}

// since says how long ago an RFC 3339 time was, in short words: "just now",
// "40 s ago", "3 min ago", "5 h ago", "4 d ago". '' for a time it cannot read;
// a time ahead of this browser's clock is "just now", not a negative age.
export function since(iso: string | undefined, now: number): string {
  if (!iso) return '';
  const t = Date.parse(iso);
  if (Number.isNaN(t)) return '';
  const s = Math.max(0, Math.round((now - t) / 1000));
  if (s < 10) return 'just now';
  if (s < 60) return `${s} s ago`;
  const m = Math.floor(s / 60);
  if (m < 60) return `${m} min ago`;
  const h = Math.floor(m / 60);
  if (h < 48) return `${h} h ago`;
  return `${Math.floor(h / 24)} d ago`;
}

// verificationNote is the one sentence under the card, '' when the card has
// said it all. Switched off comes first: whatever the record says, nothing
// will run.
export function verificationNote(v: OperatorVerification): string {
  if (!v.enabled) {
    return 'Verification is off: the operator runs no checks after an update and answers no request (spec.verification.enabled is false).';
  }
  if (v.note) return v.note;
  if (v.pendingRequest) {
    if (v.result === RESULT_RUNNING) return 'A run is requested — it starts when the one in progress ends.';
    if (!v.result) return 'A run is requested and waits for the operator. An operator older than verification never starts it.';
    return 'A run is requested and waits for the operator to start it.';
  }
  if (!v.result) {
    return 'No run has been reported. The operator verifies the platform after every update; one older than verification reports nothing.';
  }
  return (v.message ?? '').trim();
}

// verificationFinished is the toolbar's line once a run this console asked
// for has ended: "verification passed", "verification failed: 2 of 14 checks
// failed".
export function verificationFinished(v: OperatorVerification): string {
  const said = `verification ${verificationLabel(v)}`;
  const message = (v.message ?? '').trim();
  return message ? `${said}: ${message}` : said;
}
