// CardKicker - mono eyebrow label for dashboard card headers, ported from the
// marketing site's kicker/place-head language (11px uppercase mono, wide
// tracking, muted). Gives every operational card the same editorial header
// voice as the landing page's live-trace panels.
export function CardKicker({ children, className = '' }: { children: React.ReactNode; className?: string }) {
  return (
    <p className={`font-mono text-xs uppercase tracking-[0.08em] text-muted-foreground ${className}`}>
      {children}
    </p>
  );
}
