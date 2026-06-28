import { Text } from '@nalet/design-system';
import { ZaentrumLockup } from './glyphs';
import './app.css';

// shown while the OIDC round-trip resolves (redirecting to / returning from login).
export function Splash({ message }: { message: string }) {
  return (
    <div className="lp lp--center">
      <div className="splash">
        <ZaentrumLockup height={30} />
        <Text variant="muted">{message}</Text>
      </div>
    </div>
  );
}
