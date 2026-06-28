/* Dev-only visual harness — renders the portal+katalog console with a fake auth
   context and stubbed GraphQL so the UI can be screenshotted without Keycloak or
   a live backend. Not part of the production build (lives outside src/). */
import { StrictMode } from 'react';
import { createRoot } from 'react-dom/client';
import { AuthContext } from 'react-oidc-context';
import { MemoryRouter } from 'react-router-dom';
import '@nalet/design-system/styles.css';
import { App } from '../src/App';

const item = {
  id: 'abc-123',
  type: 'movie',
  title: 'tears of steel',
  year: 2012,
  tagline: 'a blender foundation open movie',
  rating: 7.4,
  description: 'a group of warriors and scientists must save the world from machines.',
  posterUrl: null,
  runtimeMin: 12,
  isPackaged: true,
  overallStatus: { overallStatus: 'complete', doneCount: 8, totalSteps: 10 },
  processingSteps: [
    { step: 'tmdb', status: 'done', attempts: 1, error: null, finishedAt: '2026-06-27T21:00:00Z' },
    { step: 'transcode', status: 'done', attempts: 1, error: null, finishedAt: '2026-06-27T21:30:00Z' },
    { step: 'package', status: 'done', attempts: 1, error: null, finishedAt: '2026-06-27T21:45:00Z' },
    { step: 'tidb', status: 'skipped', attempts: 0, error: null, finishedAt: null },
    { step: 'silence', status: 'pending', attempts: 0, error: null, finishedAt: null },
  ],
  assets: [
    { path: '/var/lib/katalog/media/tears_of_steel_720p.mov', kind: 'primary', codec: null, resolution: '1280x720', sizeMB: 354, audioCodec: 'aac', audioChannels: 2 },
    { path: '/var/lib/katalog/packages/movies/ab/abc-123/manifest.json', kind: 'primary', codec: 'hev1.1.6.L120.B0', resolution: '1280x720', sizeMB: 238, audioCodec: 'aac', audioChannels: 2 },
  ],
  segments: [{ kind: 'credits', startMs: 600000, endMs: 720000, source: 'chapter', confidence: 0.9 }],
  chapters: [{ ordinal: 1, title: 'cold open', startMs: 0, endMs: 90000 }],
  trailerLinks: [{ site: 'YouTube', title: 'official trailer', url: 'https://youtu.be/x', downloadedAt: null }],
  genres: [{ name: 'sci-fi' }, { name: 'short' }],
  people: [{ role: 'director', person: { name: 'ian hubert' } }],
  tags: ['open-movie', 'cc-by'],
  externalIds: [{ source: 'tmdb', externalId: '133701' }],
  diagnostics: { sourcePath: '/var/lib/katalog/media/tears_of_steel_720p.mov', sourceSize: 371000000, notes: null },
};

const items = [
  { id: 'abc-123', type: 'movie', title: 'tears of steel', year: 2012, posterUrl: null, isPackaged: true, overallStatus: { overallStatus: 'complete' } },
  { id: 'def-456', type: 'movie', title: 'big buck bunny', year: 2008, posterUrl: null, isPackaged: true, overallStatus: { overallStatus: 'processing' } },
  { id: 'ghi-789', type: 'movie', title: 'sintel', year: 2010, posterUrl: null, isPackaged: false, overallStatus: { overallStatus: 'pending' } },
];

function mock(query: string): unknown {
  if (query.includes('item(id')) return { item };
  if (query.includes('items(')) return { items };
  if (query.includes('genres') && !query.includes('item(')) return { genres: [{ name: 'sci-fi' }, { name: 'short' }, { name: 'animation' }] };
  if (query.includes('scanJobs'))
    return {
      scanJobs: [
        { id: 'j1', source: 'nfs', status: 'done', startedAt: '2026-06-27T21:58:00Z', finishedAt: '2026-06-27T21:58:30Z', filesSeen: 3, itemsInserted: 3, itemsUpdated: 0 },
        { id: 'j2', source: 'nfs', status: 'running', startedAt: '2026-06-28T10:00:00Z', finishedAt: null, filesSeen: 0, itemsInserted: 0, itemsUpdated: 0 },
      ],
    };
  if (query.includes('downloadJobs'))
    return {
      downloadJobs: [
        { id: 'd1', adapter: 'odownloader', clientJobId: 'p1', title: 'sintel 1080p', state: 'downloading', progressPct: 62, sizeBytes: 734003200, lastEventAt: '2026-06-28T10:05:00Z' },
        { id: 'd2', adapter: 'odownloader', clientJobId: 'p2', title: 'cosmos laundromat', state: 'completed', progressPct: 100, sizeBytes: 1073741824, lastEventAt: '2026-06-28T09:00:00Z' },
      ],
      downloadClients: '["odownloader"]',
    };
  if (query.includes('settings'))
    return {
      settings: [
        { id: 's1', key: 'language.whitelist', valueText: 'en,de,fr', valueType: 'list_csv', description: 'audio/subtitle languages to keep' },
        { id: 's2', key: 'validate.small_file_mb', valueText: '50', valueType: 'int', description: 'flag packages smaller than this' },
      ],
    };
  return {};
}

const origFetch = window.fetch;
window.fetch = (input: RequestInfo | URL, init?: RequestInit) => {
  const url = typeof input === 'string' ? input : input.toString();
  if (url.includes('/query')) {
    const body = JSON.parse((init?.body as string) ?? '{}') as { query: string };
    return Promise.resolve(new Response(JSON.stringify({ data: mock(body.query) }), { status: 200, headers: { 'Content-Type': 'application/json' } }));
  }
  return origFetch(input, init);
};

const fakeAuth = {
  isAuthenticated: true,
  isLoading: false,
  activeNavigator: undefined,
  error: undefined,
  user: { access_token: 'mock', profile: { preferred_username: 'demo' } },
  signinRedirect: async () => {},
  signoutRedirect: async () => {},
  removeUser: async () => {},
} as unknown as React.ContextType<typeof AuthContext>;

const route = new URLSearchParams(window.location.search).get('route') ?? '/katalog';

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <AuthContext.Provider value={fakeAuth}>
      <MemoryRouter initialEntries={[route]}>
        <App />
      </MemoryRouter>
    </AuthContext.Provider>
  </StrictMode>,
);
