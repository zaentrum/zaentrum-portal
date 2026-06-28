import { Badge, Button } from '@nalet/design-system';
import { LogOut } from 'lucide-react';
import { Link, Outlet } from 'react-router-dom';
import { useAuth } from 'react-oidc-context';
import { ZaentrumLockup } from '../glyphs';
import './shell.css';

// The portal shell: persistent chrome (lockup -> home, user badge, sign out)
// around a routed content area. Apps (the launchpad, the katalog console) render
// in the Outlet — the Fiori "shell + app" pattern, on @nalet/design-system.
export function Shell() {
  const auth = useAuth();
  const p = auth.user?.profile;
  const name = (p?.preferred_username as string) || (p?.name as string) || 'you';

  return (
    <div className="sh">
      <header className="sh__bar">
        <Link to="/" className="sh__brand" aria-label="zaentrum home">
          <ZaentrumLockup height={24} />
        </Link>
        <div className="sh__bar-right">
          <Badge tone="blue" dot>
            {name}
          </Badge>
          <Button
            variant="ghost"
            size="sm"
            leading={<LogOut size={15} strokeWidth={1.75} />}
            onClick={() => void auth.signoutRedirect()}
          >
            sign out
          </Button>
        </div>
      </header>
      <main className="sh__main">
        <Outlet />
      </main>
    </div>
  );
}
