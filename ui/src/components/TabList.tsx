import type { KeyboardEvent, ReactNode } from 'react';

// Tab semantics for a row of buttons that swap content: a tablist whose
// buttons carry role="tab" (use tabProps), with Left/Right/Home/End moving
// focus and selecting, as in the ARIA tabs pattern.
export function tabProps(selected: boolean) {
  return { role: 'tab' as const, 'aria-selected': selected, tabIndex: selected ? 0 : -1 };
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
