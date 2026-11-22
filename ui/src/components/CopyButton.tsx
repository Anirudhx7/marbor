import { useEffect, useRef, useState } from 'react';
import { Check, Copy } from 'lucide-react';
import { copyText } from '../lib/clipboard';

const FEEDBACK_MS = 2000;

// Copy button for a one-time secret. Shows "Copied" with a check for two
// seconds and announces it politely.
export function CopyButton({ text, label = 'Copy' }: { text: string; label?: string }) {
  const [copied, setCopied] = useState(false);
  const timer = useRef<ReturnType<typeof setTimeout> | null>(null);
  useEffect(() => () => { if (timer.current) clearTimeout(timer.current); }, []);
  return (
    <button
      type="button"
      onClick={() => {
        copyText(text);
        setCopied(true);
        if (timer.current) clearTimeout(timer.current);
        timer.current = setTimeout(() => setCopied(false), FEEDBACK_MS);
      }}
      className="flex items-center justify-center gap-1.5 w-full min-h-[44px] sm:min-h-0 py-2 border border-border rounded-lg text-sm font-medium text-foreground hover:bg-secondary transition-colors"
    >
      {copied ? <Check className="w-3.5 h-3.5" aria-hidden="true" /> : <Copy className="w-3.5 h-3.5" aria-hidden="true" />}
      <span aria-live="polite">{copied ? 'Copied' : label}</span>
    </button>
  );
}
