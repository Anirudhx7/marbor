import { Link } from 'react-router-dom';
import { Check, Circle, X, ArrowRight } from 'lucide-react';

interface SetupChecklistProps {
  hasNode: boolean;
  hasAgent: boolean;
  hasKey: boolean;
  hasModel: boolean;
  hasRequest: boolean;
  warmModelName?: string;
  onDismiss: () => void;
}

interface StepMeta {
  title: string;
  done: boolean;
  doneText: string;
  pendingText: string;
  href: string | null;
}

export function SetupChecklist({
  hasNode,
  hasAgent,
  hasKey,
  hasModel,
  hasRequest,
  warmModelName,
  onDismiss,
}: SetupChecklistProps) {
  const steps: StepMeta[] = [
    {
      title: 'Add a GPU node',
      done: hasNode,
      doneText: 'Your first inference node is connected.',
      pendingText: 'Add your first GPU node to Marbor.',
      href: '/gpu-nodes',
    },
    {
      title: 'Install Marbor Agent',
      done: hasAgent,
      doneText: 'Agent is reporting healthy.',
      pendingText: 'Install the Marbor Agent on your GPU node.',
      href: '/gpu-nodes',
    },
    {
      title: 'Pull a model',
      done: hasModel,
      doneText: warmModelName ? `${warmModelName} is warm on your fleet.` : 'A model is warm on your fleet.',
      pendingText: 'Pull a model to your fleet.',
      href: '/models',
    },
    {
      title: 'Create an API key',
      done: hasKey,
      doneText: 'An active API key is ready for inference.',
      pendingText: 'Create an API key to authenticate requests.',
      href: '/api-keys',
    },
    {
      title: 'Serve your first request',
      done: hasRequest,
      doneText: 'Your first inference request was served.',
      pendingText: 'Send your first inference request to Marbor.',
      href: null,
    },
  ];

  const doneCount = steps.filter((s) => s.done).length;
  const total = steps.length;
  const progressPct = (doneCount / total) * 100;

  return (
    <div className="bg-card border border-border rounded-xl p-5">
      {/* Header */}
      <div className="flex items-start justify-between gap-4 mb-1">
        <div className="min-w-0">
          <h3 className="text-sm font-semibold text-foreground">Get started with Marbor</h3>
          <p className="text-sm text-muted-foreground mt-1">Complete these steps to start serving models.</p>
        </div>
        <div className="flex items-center gap-3 shrink-0">
          <span className="text-sm font-mono font-medium text-muted-foreground">
            {doneCount}/{total}
          </span>
          <button
            onClick={onDismiss}
            aria-label="Dismiss setup checklist"
            title="Dismiss - hides checklist (clear localStorage to restore)"
            className="min-w-[36px] min-h-[36px] -m-1 p-2 rounded-md text-muted-foreground hover:text-foreground hover:bg-secondary transition-colors flex items-center justify-center"
          >
            <X className="w-4 h-4" />
          </button>
        </div>
      </div>

      {/* Steps */}
      <div className="mt-4 space-y-3">
        {steps.map((step) => (
          <div key={step.title} className="flex items-start gap-3">
            <div className="shrink-0 mt-0.5">
              {step.done ? (
                <span className="inline-flex items-center justify-center w-5 h-5 rounded-full bg-success/15 border border-success/30">
                  <Check className="w-3 h-3 text-success" strokeWidth={2.5} />
                </span>
              ) : (
                <span className="inline-flex items-center justify-center w-5 h-5 rounded-full border border-border bg-secondary">
                  <Circle className="w-3 h-3 text-muted-foreground" />
                </span>
              )}
            </div>
            <div className="min-w-0 flex-1">
              <div className="flex items-center gap-2 flex-wrap">
                <span className="text-sm font-medium text-foreground">{step.title}</span>
                {!step.done && step.href && (
                  <Link
                    to={step.href}
                    className="inline-flex items-center gap-1 text-xs font-medium text-primary hover:underline"
                  >
                    <ArrowRight className="w-3 h-3" />
                    <span>→ {step.href}</span>
                  </Link>
                )}
              </div>
              <p className={`text-xs mt-0.5 ${step.done ? 'text-success' : 'text-muted-foreground'}`}>
                {step.done ? step.doneText : step.pendingText}
              </p>
            </div>
            {step.done && (
              <span className="shrink-0 text-xs text-success mt-1 hidden sm:block">✓</span>
            )}
          </div>
        ))}
      </div>

      {/* Progress bar */}
      <div className="mt-5">
        <div className="w-full bg-secondary rounded-full h-2 overflow-hidden">
          <div
            className="bg-primary h-2 rounded-full transition-all duration-500 ease-out"
            style={{ width: `${progressPct}%` }}
          />
        </div>
        <div className="flex justify-end mt-1.5">
          <span className="text-xs font-mono text-muted-foreground">
            {doneCount}/{total}
          </span>
        </div>
      </div>
    </div>
  );
}
