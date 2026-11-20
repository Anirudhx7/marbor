import type { Analytics } from '../types';
import { cacheWindowLabel, hitPctLabel } from '../lib/cacheView';

interface Props {
  data: Analytics | null;
  loading: boolean;
}

const EMPTY_HINT =
  'No cache data reported yet - requires vLLM started with --enable-prompt-tokens-details or llama.cpp. Ollama and MLX do not report it.';

const COLUMN_HEADERS = ['Requests', 'Reported', 'Cached / input tokens', 'Hit'];

function tokens(cached: number, input: number, reporting: number): string {
  if (reporting === 0) return '-';
  return `${cached.toLocaleString('en-US')} / ${input.toLocaleString('en-US')}`;
}

interface Row {
  name: string;
  window_requests: number;
  reporting_requests: number;
  cached_tokens: number;
  input_tokens: number;
  hit_pct: number | null;
}

function GroupTable({ title, nameHeader, rows }: { title: string; nameHeader: string; rows: Row[] }) {
  if (rows.length === 0) return null;
  return (
    <div className="px-4 sm:px-6 py-3">
      <h4 className="text-xs font-semibold text-foreground mb-2">{title}</h4>
      <div className="hidden md:block overflow-x-auto scroll-region" tabIndex={0} role="region" aria-label={`${title}: scroll horizontally for more columns`}>
        <table className="w-full text-sm">
          <thead>
            <tr className="bg-secondary/50 text-muted-foreground">
              <th className="px-3 py-2 text-left font-medium">{nameHeader}</th>
              {COLUMN_HEADERS.map((h) => (
                <th key={h} className="px-3 py-2 text-right font-medium whitespace-nowrap">{h}</th>
              ))}
            </tr>
          </thead>
          <tbody className="divide-y divide-border">
            {rows.map((r) => (
              <tr key={r.name} className="hover:bg-secondary/30 transition-colors">
                <td className="px-3 py-2 font-mono text-foreground break-all">{r.name}</td>
                <td className="px-3 py-2 text-right font-mono">{r.window_requests.toLocaleString('en-US')}</td>
                <td className="px-3 py-2 text-right font-mono">{r.reporting_requests.toLocaleString('en-US')}</td>
                <td className="px-3 py-2 text-right font-mono">{tokens(r.cached_tokens, r.input_tokens, r.reporting_requests)}</td>
                <td className="px-3 py-2 text-right font-semibold">{hitPctLabel(r.hit_pct)}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
      <div className="md:hidden space-y-2">
        {rows.map((r) => (
          <div key={r.name} className="bg-card/50 border border-border/60 rounded-xl p-3">
            <p className="font-mono text-sm text-foreground mb-2 break-all">{r.name}</p>
            <div className="grid grid-cols-2 gap-x-3 gap-y-2">
              <div>
                <p className="text-[10px] uppercase tracking-wider text-muted-foreground">Hit</p>
                <p className="text-sm font-semibold">{hitPctLabel(r.hit_pct)}</p>
              </div>
              <div>
                <p className="text-[10px] uppercase tracking-wider text-muted-foreground">Reported</p>
                <p className="text-sm font-mono">{r.reporting_requests.toLocaleString('en-US')} of {r.window_requests.toLocaleString('en-US')}</p>
              </div>
              <div className="col-span-2">
                <p className="text-[10px] uppercase tracking-wider text-muted-foreground">Cached / input tokens</p>
                <p className="text-sm font-mono break-words">{tokens(r.cached_tokens, r.input_tokens, r.reporting_requests)}</p>
              </div>
            </div>
          </div>
        ))}
      </div>
    </div>
  );
}

// Prompt-cache reuse over the most recent requests on record. It is a window
// of requests, not a time period, and only requests whose runtime reported a
// split count toward a hit rate, so the caption always says how many did.
export function CacheWindowCard({ data, loading }: Props) {
  const cw = data?.cache_window;
  const reporting = cw?.reporting_requests ?? 0;
  return (
    <div className="bg-card border border-border shadow-sm rounded-xl overflow-hidden">
      <div className="px-6 py-4 border-b border-border bg-secondary/30">
        <h3 className="text-sm font-semibold text-foreground">Prompt cache reuse</h3>
        {cw && cw.window_requests > 0 && (
          <p className="text-xs text-muted-foreground mt-0.5">
            {cacheWindowLabel(cw.window_requests, cw.reporting_requests)}. Hit rates cover only requests that reported.
          </p>
        )}
      </div>
      {loading ? (
        <div className="px-6 py-4 space-y-3 animate-pulse">
          <div className="h-4 bg-secondary rounded w-1/3" />
          <div className="h-4 bg-secondary rounded w-1/2" />
        </div>
      ) : !cw || cw.window_requests === 0 ? (
        <div className="px-6 py-8 text-center text-sm text-muted-foreground font-medium">
          No requests on record yet. {EMPTY_HINT}
        </div>
      ) : reporting === 0 ? (
        <div className="px-6 py-8 text-center text-sm text-muted-foreground font-medium">
          {EMPTY_HINT}
        </div>
      ) : (
        <>
          <GroupTable
            title="By model"
            nameHeader="Model"
            rows={cw.by_model.map((m) => ({ ...m, name: m.model }))}
          />
          <GroupTable
            title="By key"
            nameHeader="Key"
            rows={cw.by_key.map((k) => ({ ...k, name: k.key }))}
          />
        </>
      )}
    </div>
  );
}
