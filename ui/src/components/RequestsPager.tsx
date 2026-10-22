import { ChevronLeft, ChevronRight, Pause } from 'lucide-react';
import { CustomSelect } from './Select';
import { PAGE_SIZES } from '../lib/requestsView';

interface RequestsPagerProps {
  summary: string;
  hasNewer: boolean;
  hasOlder: boolean;
  onNewer: () => void;
  onOlder: () => void;
  // The size selector and the live chip appear once, on the top pager.
  size?: number;
  onSize?: (size: number) => void;
  livePaused?: boolean;
  onResume?: () => void;
  busy?: boolean;
  label: string;
  // Only one pager announces its summary to screen readers, so the same line
  // is not read twice.
  announce?: boolean;
}

const NAV_BUTTON =
  'inline-flex items-center gap-1 min-h-[40px] px-3 py-2 bg-secondary hover:bg-secondary/80 text-foreground text-sm font-medium rounded-lg transition-colors aria-disabled:opacity-40 aria-disabled:cursor-not-allowed';

// RequestsPager is the Newer / Older control for the keyset-paged request log.
// There is no total or page number: the server reports only whether a newer or
// older matching row exists, so that is all the buttons can honestly say.
// While a page is loading the buttons are aria-disabled rather than disabled,
// so a button the user just pressed keeps keyboard focus.
export function RequestsPager({
  summary,
  hasNewer,
  hasOlder,
  onNewer,
  onOlder,
  size,
  onSize,
  livePaused,
  onResume,
  busy,
  label,
  announce,
}: RequestsPagerProps) {
  const newerOff = !hasNewer || !!busy;
  const olderOff = !hasOlder || !!busy;
  return (
    <nav aria-label={label} className="flex flex-wrap items-center gap-x-4 gap-y-2">
      <div className="flex items-center gap-2">
        <button
          type="button"
          className={NAV_BUTTON}
          onClick={() => !newerOff && onNewer()}
          aria-disabled={newerOff}
        >
          <ChevronLeft className="w-4 h-4" aria-hidden="true" />
          Newer
        </button>
        <button
          type="button"
          className={NAV_BUTTON}
          onClick={() => !olderOff && onOlder()}
          aria-disabled={olderOff}
        >
          Older
          <ChevronRight className="w-4 h-4" aria-hidden="true" />
        </button>
      </div>
      <span className="text-xs text-muted-foreground" role={announce ? 'status' : undefined}>
        {summary}
      </span>
      {livePaused && onResume && (
        <button
          type="button"
          onClick={onResume}
          className="inline-flex items-center gap-1.5 min-h-[40px] px-3 py-1 rounded-full border border-amber-500/30 bg-amber-500/10 text-xs font-medium text-amber-600 dark:text-amber-400 hover:bg-amber-500/20 transition-colors"
        >
          <Pause className="w-3 h-3" aria-hidden="true" />
          Live paused, Resume
        </button>
      )}
      {size !== undefined && onSize && (
        <div className="sm:ml-auto w-40">
          <CustomSelect
            value={String(size)}
            onChange={(val) => onSize(Number(val))}
            options={PAGE_SIZES.map((n) => ({ value: String(n), label: `${n} per page` }))}
          />
        </div>
      )}
    </nav>
  );
}
