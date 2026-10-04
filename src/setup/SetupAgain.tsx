import { useEffect, useState } from 'react';
import { useNavigate } from 'react-router-dom';
import { Button, Text } from '@nalet/design-system';
import { ListChecks } from 'lucide-react';
import { usePortalApi } from '../lib/api';
import { completedText, type SetupCompletion } from '../lib/setup';

// SetupAgain is settings' line about first-run setup: who marked it done and
// when, and "Show Setup Again" — which reopens it, so the launchpad shows the
// checklist again, and goes there. While setup is open it says where the
// checklist is. Nothing against a portal-api without setup.
export function SetupAgain() {
  const api = usePortalApi();
  const nav = useNavigate();
  // undefined: not read yet, or no setup on this portal-api.
  const [record, setRecord] = useState<SetupCompletion | null | undefined>(undefined);
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);

  useEffect(() => {
    let live = true;
    api<{ completed: SetupCompletion | null }>('/setup/complete')
      .then((r) => live && setRecord(r.completed ?? null))
      .catch(() => live && setRecord(undefined));
    return () => {
      live = false;
    };
  }, [api]);

  if (record === undefined) return null;

  async function reopen() {
    setBusy(true);
    setErr(null);
    try {
      await api('/setup/complete', { method: 'DELETE' });
      nav('/');
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
      setBusy(false);
    }
  }

  return (
    <div className="set__setup">
      {record ? (
        <>
          <Text variant="dim">{completedText(record, Date.now())}</Text>
          <Button variant="ghost" size="sm" leading={<ListChecks size={14} />} loading={busy} onClick={reopen}>
            Show Setup Again
          </Button>
        </>
      ) : (
        <>
          <Text variant="dim">Setup is open: the launchpad shows its checklist until it is marked done.</Text>
          <Button variant="ghost" size="sm" leading={<ListChecks size={14} />} onClick={() => nav('/')}>
            Open Checklist
          </Button>
        </>
      )}
      {err && (
        <span className="set__err" role="alert">
          {err}
        </span>
      )}
    </div>
  );
}
