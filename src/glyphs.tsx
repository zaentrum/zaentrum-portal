import * as React from 'react';
import type { LucideIcon } from 'lucide-react';

/**
 * Product glyphs — the >x brand mark (blue chevron + mono letter). One per
 * product so a launchpad Tile can render it through its `icon` slot (the design
 * system calls icons as `<Glyph size strokeWidth />`, so these mimic a Lucide icon).
 *   >z zaentrum · >c chino · >t tv · >m musig
 */
const makeGlyph = (letter: string): LucideIcon => {
  const Glyph = React.forwardRef<
    SVGSVGElement,
    React.SVGProps<SVGSVGElement> & { size?: number; strokeWidth?: number }
  >(function Glyph({ size = 24, strokeWidth = 2, color: _c, ...rest }, ref) {
    const s = Number(size) || 24;
    return (
      <svg
        ref={ref}
        width={s}
        height={s}
        viewBox="0 0 24 24"
        fill="none"
        role="img"
        aria-label={'>' + letter}
        {...rest}
      >
        <path
          d="M4.5 6 L10 12 L4.5 18"
          stroke="var(--cloud-blue)"
          strokeWidth={Math.max(Number(strokeWidth) + 0.6, 2.4)}
          strokeLinecap="round"
          strokeLinejoin="round"
        />
        <text
          x="12"
          y="16.6"
          fontFamily="var(--ff-mono, ui-monospace, monospace)"
          fontWeight={800}
          fontSize={13}
          fill="var(--fg-2, #C9D1D9)"
        >
          {letter}
        </text>
      </svg>
    );
  });
  return Glyph as unknown as LucideIcon;
};

export const ZGlyph = makeGlyph('z');
export const CGlyph = makeGlyph('c');
export const TGlyph = makeGlyph('t');
export const MGlyph = makeGlyph('m');

/** zaentrum wordmark lockup — blue chevron + mono wordmark + blinking cursor. */
export function ZaentrumLockup({ height = 26 }: { height?: number }) {
  const vbW = 170;
  const vbH = 40;
  const width = (height / vbH) * vbW;
  return (
    <svg height={height} width={width} viewBox={`0 0 ${vbW} ${vbH}`} role="img" aria-label="zaentrum" style={{ display: 'block' }}>
      <path d="M6 9 L17 20 L6 31" fill="none" stroke="var(--cloud-blue)" strokeWidth={4.5} strokeLinecap="round" strokeLinejoin="round" />
      <text x="28" y="28" fontFamily="var(--ff-mono, ui-monospace, monospace)" fontWeight={700} fontSize={22} fill="var(--fg-2, #C9D1D9)" letterSpacing="0.5">
        zaentrum
      </text>
      <rect x="151" y="11" width="9" height="19" fill="var(--cloud-blue)">
        <animate attributeName="opacity" values="1;1;0;0" keyTimes="0;.5;.5;1" dur="1.05s" repeatCount="indefinite" />
      </rect>
    </svg>
  );
}
