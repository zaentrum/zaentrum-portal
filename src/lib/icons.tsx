import type { LucideIcon } from 'lucide-react';
import {
  Library,
  Radar,
  Download,
  Tv,
  Music,
  Clapperboard,
  Settings,
  LayoutGrid,
  Server,
  Boxes,
  Globe,
  Wrench,
  FileText,
  Image,
  ListVideo,
  Users,
  Gauge,
  Database,
} from 'lucide-react';
import { CGlyph, TGlyph, MGlyph, ZGlyph } from '../glyphs';

// A curated icon palette (kept explicit so the bundle stays lean — a dynamic
// `lucide[name]` lookup would pull the entire icon set in). `glyph:x` names map
// to the >x brand marks; everything else is a lucide name (kebab or lower case).
const GLYPHS: Record<string, LucideIcon> = { c: CGlyph, t: TGlyph, m: MGlyph, z: ZGlyph };

const MAP: Record<string, LucideIcon> = {
  library: Library,
  radar: Radar,
  download: Download,
  tv: Tv,
  music: Music,
  clapperboard: Clapperboard,
  settings: Settings,
  'layout-grid': LayoutGrid,
  server: Server,
  boxes: Boxes,
  globe: Globe,
  wrench: Wrench,
  'file-text': FileText,
  image: Image,
  'list-video': ListVideo,
  users: Users,
  gauge: Gauge,
  database: Database,
};

// resolveIcon turns a stored icon name into a renderable icon component, with a
// neutral fallback so an unknown name never breaks a tile.
export function resolveIcon(name: string | undefined): LucideIcon {
  if (!name) return LayoutGrid;
  if (name.startsWith('glyph:')) return GLYPHS[name.slice(6)] ?? LayoutGrid;
  return MAP[name] ?? LayoutGrid;
}

// ICON_CHOICES is the palette offered in the settings console icon picker.
export const ICON_CHOICES: string[] = [
  'glyph:c',
  'glyph:t',
  'glyph:m',
  'glyph:z',
  ...Object.keys(MAP),
];
