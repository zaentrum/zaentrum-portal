type BadgeTone = 'neutral' | 'green' | 'amber' | 'blue';

// Formats an ISO timestamp compactly; '—' when absent.
export function fmtTime(s: string | null | undefined): string {
  if (!s) return '—';
  const d = new Date(s);
  if (isNaN(d.getTime())) return s;
  return d.toLocaleString(undefined, { dateStyle: 'short', timeStyle: 'short' });
}

// Maps a katalog overall/step/download status to a design-system Badge tone.
export function statusTone(s: string | null | undefined): BadgeTone {
  switch (s) {
    case 'complete':
    case 'done':
    case 'completed':
      return 'green';
    case 'failed':
    case 'partial_failure':
      return 'amber';
    case 'processing':
    case 'queued':
    case 'in_progress':
    case 'downloading':
    case 'pending':
      return 'blue';
    default:
      return 'neutral';
  }
}
