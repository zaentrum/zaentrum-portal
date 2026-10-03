import { useState } from 'react';
import { Button, Modal, Text } from '@nalet/design-system';
import type { Confirmation } from '../lib/confirm';
import './confirm.css';

const errText = (e: unknown) => (e instanceof Error ? e.message : String(e));

// ConfirmDialog asks before a change, and says what it will do: the lines of
// a Confirmation (src/lib/confirm.ts). Confirming runs onConfirm and closes on
// success; a refusal stays on screen, in the server's words. A blocked
// confirmation says why the platform will not do it, and offers nothing to
// confirm.
export function ConfirmDialog({
  confirmation,
  onConfirm,
  onClose,
}: {
  confirmation: Confirmation;
  onConfirm: () => Promise<unknown>;
  onClose: () => void;
}) {
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const c = confirmation;

  async function confirm() {
    setBusy(true);
    setErr(null);
    try {
      await onConfirm();
      onClose();
    } catch (e) {
      setErr(errText(e));
    } finally {
      setBusy(false);
    }
  }

  return (
    <Modal
      open
      width={560}
      onClose={() => !busy && onClose()}
      title={c.title}
      footer={
        c.blocked ? (
          <Button size="sm" onClick={onClose}>
            close
          </Button>
        ) : (
          <>
            <Button variant="ghost" size="sm" disabled={busy} onClick={onClose}>
              cancel
            </Button>
            <Button size="sm" variant={c.danger ? 'danger' : undefined} loading={busy} onClick={confirm}>
              {c.confirm}
            </Button>
          </>
        )
      }
    >
      <div className="confirm">
        {c.blocked ? (
          <Text as="p">{c.blocked}</Text>
        ) : (
          <ul className="confirm__lines">
            {c.lines.map((line) => (
              <li key={line}>{line}</li>
            ))}
          </ul>
        )}
        {err && (
          <span className="confirm__err" role="alert">
            {err}
          </span>
        )}
      </div>
    </Modal>
  );
}
