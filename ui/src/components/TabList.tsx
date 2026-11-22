import type { KeyboardEvent, ReactNode } from 'react';

// Tab semantics for a row of buttons that swap content: a tablist whose
// buttons carry role="tab" (use tabProps), with Left/Right/Home/End moving
// focus and selecting, as in the ARIA tabs pattern.
// `id` links the tab to its panel: the tab gets `${id}-tab` and points at
// `${id}-panel`, which the matching content carries via panelProps(id).
export function tabProps(selected: boolean, id: string) {
  return {
    role: 'tab' as const,
    id: `${id}-tab`,
    'aria-selected': selected,
    // Only the selected tab's panel is in the page, so only it is referenced.
    'aria-controls': selected ? `${id}-panel` : undefined,
    tabIndex: selected ? 0 : -1,
  };
}

export function panelProps(id: string) {
  return { role: 'tabpanel' as const, id: `${id}-panel`, 'aria-labelledby': `${id}-tab` };
}

// For content that has no single root element to spread panelProps onto. It
// adds no box of its own.
export function TabPanel({ id, children }: { id: string; children: ReactNode }) {
  return <div {...panelProps(id)} className="contents">{children}</div>;
}

export function TabList({ label, className, children }: { label: string; className?: string; children: ReactNode }) {
  const onKeyDown = (e: KeyboardEvent<HTMLDivElement>) => {
    const keys = ['ArrowLeft', 'ArrowRight', 'Home', 'End'];
    if (!keys.includes(e.key)) return;
    const tabs = Array.from(e.currentTarget.querySelectorAll<HTMLElement>('[role="tab"]'));
    const at = tabs.indexOf(document.activeElement as HTMLElement);
    if (at < 0 || tabs.length === 0) return;
    e.preventDefault();
    const next = e.key === 'Home' ? 0
      : e.key === 'End' ? tabs.length - 1
      : (at + (e.key === 'ArrowRight' ? 1 : -1) + tabs.length) % tabs.length;
    tabs[next].focus();
    tabs[next].click();
  };
  return (
    <div role="tablist" aria-label={label} onKeyDown={onKeyDown} className={className}>
      {children}
    </div>
  );
}
