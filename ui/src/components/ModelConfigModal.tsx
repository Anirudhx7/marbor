import { useState, useEffect, useCallback, type ReactElement } from 'react';
import { ChevronDown, RotateCcw, Loader2 } from 'lucide-react';
import { Modal } from './Modal';
import { fetchModelConfig, saveModelConfig, deleteModelConfig } from '../lib/api';
import { getMockModelConfig, setMockModelConfig, deleteMockModelConfig } from '../lib/mockData';
import type { ModelConfig } from '../types';

// Editable form shape: same keys as ModelConfig, but `stop`/`logit_bias` are
// held as raw text while being edited (comma-separated / JSON respectively)
// so free-typing doesn't fight JSON.parse on every keystroke.
type FormState = Omit<ModelConfig, 'model' | 'stop' | 'logit_bias'> & {
  model: string;
  stop_text: string;
  logit_bias_text: string;
};

type FieldType = 'int' | 'float' | 'bool' | 'text' | 'textarea' | 'slider' | 'select';

interface FieldDef {
  key: keyof Omit<FormState, 'model' | 'stop_text' | 'logit_bias_text'>;
  label: string;
  help: string;
  type: FieldType;
  min?: number;
  max?: number;
  step?: number;
  // slider only: the value the control seeds to the first time a user
  // switches it on (the field's real-world common default, so dragging
  // starts from something sane instead of the min).
  sliderDefault?: number;
  // select only: fixed enum options (value + human label).
  options?: { value: string; label: string }[];
  // select only: store the chosen option as a number instead of a string
  // (e.g. mirostat mode 0/1/2 is an int field in ModelConfig).
  numeric?: boolean;
}

// Common power-of-2 context windows, LM-Studio style, so users pick a size
// instead of guessing a token count. "Custom" reveals the raw number input.
const NUM_CTX_PRESETS = [2048, 4096, 8192, 16384, 32768, 65536, 131072];

const LOAD_TIME_FIELDS: FieldDef[] = [
  { key: 'num_ctx', label: 'Context Length (num_ctx)', help: 'Max context window in tokens.', type: 'slider', min: 2048, max: 131072, step: 2048, sliderDefault: 4096 },
  { key: 'num_gpu', label: 'GPU Layers (num_gpu)', help: 'Layers offloaded to GPU. Higher = more VRAM, less CPU. Depends on model size — leave unset to let Ollama decide.', type: 'int', min: 0 },
  { key: 'flash_attention', label: 'Flash Attention', help: 'Faster attention kernel, lower VRAM for long contexts.', type: 'bool' },
  { key: 'offload_kv_cache_to_gpu', label: 'Offload KV Cache to GPU', help: 'Keeps the KV cache resident on GPU instead of CPU.', type: 'bool' },
  { key: 'num_batch', label: 'Batch Size (num_batch)', help: 'Tokens processed in parallel during prompt eval.', type: 'slider', min: 32, max: 2048, step: 32, sliderDefault: 512 },
  { key: 'num_thread', label: 'CPU Threads (num_thread)', help: 'CPU threads for generation. 0 = auto.', type: 'int', min: 0 },
  { key: 'use_mmap', label: 'Use mmap', help: 'Memory-maps the model file for faster loads.', type: 'bool' },
  { key: 'use_mlock', label: 'Use mlock', help: 'Locks model pages in RAM to prevent swapping.', type: 'bool' },
  { key: 'rope_frequency_base', label: 'RoPE Frequency Base', help: 'Base frequency for RoPE context scaling. Rarely needs changing — leave unset unless extending context past a model’s trained length.', type: 'int', min: 0, step: 1000 },
  { key: 'rope_frequency_scale', label: 'RoPE Frequency Scale', help: 'Scale factor for RoPE context expansion (used for context-window stretching).', type: 'slider', min: 0, max: 4, step: 0.05, sliderDefault: 1 },
  { key: 'ttl', label: 'TTL (seconds)', help: 'Idle seconds before auto-unload. 0 = disabled. e.g. 300 = 5 min, 1800 = 30 min, 3600 = 1 hour.', type: 'int', min: 0 },
  { key: 'tensor_parallelism', label: 'Tensor Parallelism', help: 'Split tensor compute across multiple GPUs.', type: 'bool' },
];

const INFERENCE_FIELDS: FieldDef[] = [
  { key: 'temperature', label: 'Temperature', help: 'Sampling randomness. 0 = deterministic/focused, 2 = max chaos. Most chat use cases: 0.6–0.9.', type: 'slider', min: 0, max: 2, step: 0.05, sliderDefault: 0.8 },
  { key: 'top_p', label: 'Top P (nucleus sampling)', help: 'Only sample from the smallest set of tokens whose cumulative probability reaches this. Lower = more focused.', type: 'slider', min: 0, max: 1, step: 0.05, sliderDefault: 0.9 },
  { key: 'top_k', label: 'Top K', help: 'Only sample from the top K candidate tokens. Lower = more focused, higher = more variety.', type: 'slider', min: 0, max: 100, step: 1, sliderDefault: 40 },
  { key: 'min_p', label: 'Min P', help: 'Minimum token probability relative to the top token’s probability.', type: 'slider', min: 0, max: 1, step: 0.01, sliderDefault: 0 },
  { key: 'typical_p', label: 'Typical P', help: 'Locally typical sampling threshold — filters out tokens with atypical information content.', type: 'slider', min: 0, max: 1, step: 0.05, sliderDefault: 1 },
  { key: 'tfs_z', label: 'TFS-Z', help: 'Tail-free sampling parameter — trims low-probability tail tokens.', type: 'slider', min: 0, max: 1, step: 0.05, sliderDefault: 1 },
  { key: 'max_tokens', label: 'Max Tokens (num_predict)', help: 'Max tokens to generate. -1 = unlimited.', type: 'int', min: -1 },
  { key: 'seed', label: 'Seed', help: 'Fixed RNG seed for reproducible output. -1 = random.', type: 'int', min: -1 },
  { key: 'repeat_penalty', label: 'Repeat Penalty', help: 'Penalizes repeated tokens. 1.0 = off, higher = less repetition.', type: 'slider', min: 0.5, max: 2, step: 0.05, sliderDefault: 1.1 },
  { key: 'repeat_last_n', label: 'Repeat Last N', help: 'Lookback window (in tokens) for the repeat penalty. -1 = whole context.', type: 'int', min: -1 },
  { key: 'presence_penalty', label: 'Presence Penalty', help: 'Penalizes tokens already used at all, regardless of how often.', type: 'slider', min: -2, max: 2, step: 0.1, sliderDefault: 0 },
  { key: 'frequency_penalty', label: 'Frequency Penalty', help: 'Penalizes tokens by how often they’ve already recurred.', type: 'slider', min: -2, max: 2, step: 0.1, sliderDefault: 0 },
  {
    key: 'mirostat', label: 'Mirostat Mode', help: 'Adaptive sampling that targets a constant perplexity instead of tuning top-k/top-p by hand. 2.0 is the modern variant.', type: 'select', numeric: true,
    options: [{ value: '0', label: 'Off' }, { value: '1', label: 'Mirostat 1.0' }, { value: '2', label: 'Mirostat 2.0' }],
  },
  { key: 'mirostat_tau', label: 'Mirostat Tau', help: 'Target entropy for Mirostat sampling. Only used when Mirostat mode is on.', type: 'slider', min: 0, max: 10, step: 0.1, sliderDefault: 5 },
  { key: 'mirostat_eta', label: 'Mirostat Eta', help: 'Learning rate for Mirostat sampling. Only used when Mirostat mode is on.', type: 'slider', min: 0, max: 1, step: 0.01, sliderDefault: 0.1 },
  {
    key: 'response_format', label: 'Response Format', help: 'Force a structured output format.', type: 'select',
    options: [{ value: 'json', label: 'JSON object' }],
  },
];

const META_FIELDS: FieldDef[] = [
  { key: 'system', label: 'System Prompt', help: 'Overrides the model’s default system prompt.', type: 'textarea' },
  { key: 'template', label: 'Prompt Template', help: 'Overrides the model’s default prompt template.', type: 'textarea' },
  { key: 'rpm', label: 'Requests / Minute Cap', help: 'Caps requests-per-minute for this model across all keys. Empty = unlimited.', type: 'int', min: 0 },
  { key: 'tpm', label: 'Tokens / Minute Cap', help: 'Caps tokens-per-minute for this model across all keys. Empty = unlimited.', type: 'int', min: 0 },
];

function toFormState(model: string, cfg: ModelConfig | null): FormState {
  const c = cfg ?? { model };
  return {
    ...c,
    model,
    stop_text: (c.stop ?? []).join(', '),
    logit_bias_text: c.logit_bias ? JSON.stringify(c.logit_bias, null, 2) : '',
  };
}

function fromFormState(form: FormState): { cfg: ModelConfig; error: string | null } {
  const stop = form.stop_text.split(',').map(s => s.trim()).filter(Boolean);
  let logitBias: Record<string, number> | undefined;
  if (form.logit_bias_text.trim()) {
    try {
      logitBias = JSON.parse(form.logit_bias_text);
    } catch {
      return { cfg: { model: form.model }, error: 'Logit bias must be valid JSON, e.g. {"1234": -5}' };
    }
  }
  const { stop_text, logit_bias_text, ...rest } = form;
  void stop_text; void logit_bias_text;
  return {
    cfg: {
      ...rest,
      stop: stop.length > 0 ? stop : undefined,
      logit_bias: logitBias,
    },
    error: null,
  };
}

function Field({
  def,
  value,
  onChange,
  onReset,
  disabled,
}: {
  def: FieldDef;
  value: unknown;
  onChange: (v: unknown) => void;
  onReset: () => void;
  disabled?: boolean;
}) {
  const isSet = value !== undefined && value !== null && value !== '';

  let input: ReactElement;
  if (def.type === 'bool') {
    const boolVal = value as boolean | undefined;
    input = (
      <select
        value={boolVal === undefined ? '' : boolVal ? 'true' : 'false'}
        onChange={(e) => onChange(e.target.value === '' ? undefined : e.target.value === 'true')}
        disabled={disabled}
        className="w-full px-2.5 py-1.5 text-sm bg-secondary border border-border rounded-md text-foreground focus:outline-none focus:ring-1 focus:ring-primary disabled:opacity-50 disabled:cursor-not-allowed"
      >
        <option value="">Default (unset)</option>
        <option value="true">On</option>
        <option value="false">Off</option>
      </select>
    );
  } else if (def.type === 'textarea') {
    input = (
      <textarea
        value={(value as string) ?? ''}
        onChange={(e) => onChange(e.target.value === '' ? undefined : e.target.value)}
        rows={2}
        placeholder="Not set — inherits Ollama's own default"
        disabled={disabled}
        className="w-full px-2.5 py-1.5 text-sm bg-secondary border border-border rounded-md text-foreground placeholder-muted-foreground/60 focus:outline-none focus:ring-1 focus:ring-primary resize-y disabled:opacity-50 disabled:cursor-not-allowed"
      />
    );
  } else if (def.type === 'text') {
    input = (
      <input
        type="text"
        value={(value as string) ?? ''}
        onChange={(e) => onChange(e.target.value === '' ? undefined : e.target.value)}
        placeholder="Not set"
        disabled={disabled}
        className="w-full px-2.5 py-1.5 text-sm bg-secondary border border-border rounded-md text-foreground placeholder-muted-foreground/60 focus:outline-none focus:ring-1 focus:ring-primary disabled:opacity-50 disabled:cursor-not-allowed"
      />
    );
  } else if (def.type === 'select') {
    input = (
      <select
        value={value === undefined || value === null ? '' : String(value)}
        onChange={(e) => {
          if (e.target.value === '') return onChange(undefined);
          onChange(def.numeric ? Number(e.target.value) : e.target.value);
        }}
        disabled={disabled}
        className="w-full px-2.5 py-1.5 text-sm bg-secondary border border-border rounded-md text-foreground focus:outline-none focus:ring-1 focus:ring-primary disabled:opacity-50 disabled:cursor-not-allowed"
      >
        <option value="">Default (unset)</option>
        {def.options?.map((o) => (
          <option key={o.value} value={o.value}>{o.label}</option>
        ))}
      </select>
    );
  } else if (def.type === 'slider') {
    const min = def.min ?? 0;
    const max = def.max ?? 1;
    const step = def.step ?? 0.01;
    const sliderValue = isSet ? (value as number) : (def.sliderDefault ?? min);
    input = (
      <div className="flex items-center gap-3">
        <input
          type="range"
          min={min}
          max={max}
          step={step}
          value={sliderValue}
          onChange={(e) => onChange(Number(e.target.value))}
          disabled={disabled}
          className={`flex-1 accent-primary ${!isSet ? 'opacity-40' : ''} disabled:opacity-30 disabled:cursor-not-allowed`}
        />
        <code className={`font-mono text-xs font-medium min-w-[52px] text-right ${isSet ? 'text-primary' : 'text-muted-foreground/60'}`}>
          {isSet ? sliderValue : 'default'}
        </code>
      </div>
    );
  } else {
    input = (
      <input
        type="number"
        value={value === undefined || value === null ? '' : (value as number)}
        onChange={(e) => onChange(e.target.value === '' ? undefined : Number(e.target.value))}
        min={def.min}
        max={def.max}
        step={def.step ?? (def.type === 'int' ? 1 : 0.01)}
        placeholder="Not set"
        disabled={disabled}
        className="w-full px-2.5 py-1.5 text-sm bg-secondary border border-border rounded-md text-foreground placeholder-muted-foreground/60 focus:outline-none focus:ring-1 focus:ring-primary disabled:opacity-50 disabled:cursor-not-allowed"
      />
    );
  }

  return (
    <div className="space-y-1">
      <div className="flex items-center justify-between gap-2">
        <label className="text-xs font-medium text-foreground">{def.label}</label>
        {isSet && !disabled && (
          <button
            type="button"
            onClick={onReset}
            title="Reset to default"
            className="text-muted-foreground hover:text-destructive transition-colors shrink-0"
          >
            <RotateCcw className="w-3 h-3" />
          </button>
        )}
      </div>
      {input}
      <p className="text-[10px] text-muted-foreground leading-snug">{def.help}</p>
    </div>
  );
}

function Section({
  title,
  fields,
  form,
  setField,
  disabled,
  disabledNote,
}: {
  title: string;
  fields: FieldDef[];
  form: FormState;
  setField: (key: FieldDef['key'], v: unknown) => void;
  disabled?: boolean;
  disabledNote?: string;
}) {
  const [open, setOpen] = useState(true);
  return (
    <div className="border border-border rounded-lg overflow-hidden">
      <button
        type="button"
        onClick={() => setOpen((o) => !o)}
        className="w-full flex items-center justify-between px-3 py-2 bg-secondary/40 hover:bg-secondary/60 transition-colors"
      >
        <span className="text-xs font-semibold text-foreground uppercase tracking-wider">{title}</span>
        <ChevronDown className={`w-3.5 h-3.5 text-muted-foreground transition-transform ${open ? 'rotate-180' : ''}`} />
      </button>
      {open && (
        <div className="p-4 sm:p-5 space-y-4">
          {disabled && disabledNote && (
            <p className="text-xs text-amber-600 dark:text-amber-400 bg-amber-500/10 border border-amber-500/20 rounded-md px-3 py-2 leading-snug">
              {disabledNote}
            </p>
          )}
          <div className="grid grid-cols-1 sm:grid-cols-2 lg:grid-cols-3 gap-x-6 gap-y-5">
            {fields.map((def) => (
              <Field
                key={String(def.key)}
                def={def}
                value={(form as Record<string, unknown>)[def.key as string]}
                onChange={(v) => setField(def.key, v)}
                onReset={() => setField(def.key, undefined)}
                disabled={disabled}
              />
            ))}
          </div>
        </div>
      )}
    </div>
  );
}

export function ModelConfigModal({
  model,
  demoMode,
  runtimes,
  presetNumCtx,
  onClose,
}: {
  model: string | null;
  demoMode: boolean;
  // Runtime(s) hosting this model (e.g. from the node(s) it's resident on).
  // Load-time/engine params (num_ctx, num_gpu, flash_attention, ...) only have
  // a per-request equivalent on Ollama-native /api/* requests — vLLM/TGI/
  // llama.cpp treat them as launch-time-only flags, so injectModelDefaults
  // silently no-ops them there. Omit/leave empty when unknown (assumed
  // ollama, the common case) — pass every runtime the model spans so mixed
  // residency (e.g. one ollama node + one vLLM node) is also gated.
  runtimes?: string[];
  // Pre-fills num_ctx the first time this model's config is opened (no saved
  // config yet) — e.g. from a context-length slider elsewhere in the UI, so
  // that control is a real input into the config rather than decorative.
  presetNumCtx?: number;
  onClose: () => void;
}) {
  const [form, setForm] = useState<FormState | null>(null);
  const [loading, setLoading] = useState(false);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [success, setSuccess] = useState(false);

  // Engine/load-time params are Ollama-only. Unknown runtime defaults to
  // enabled (assume ollama); any non-ollama runtime in the set — including
  // a mixed ollama + non-ollama residency — disables the section.
  const engineParamsSupported =
    !runtimes || runtimes.length === 0 || runtimes.every((r) => (r || '').toLowerCase() === 'ollama');

  const load = useCallback(async () => {
    if (!model) return;
    setLoading(true);
    setError(null);
    setSuccess(false);
    try {
      const cfg = demoMode ? getMockModelConfig(model) : await fetchModelConfig(model);
      const fs = toFormState(model, cfg);
      if (!cfg && presetNumCtx !== undefined) fs.num_ctx = presetNumCtx;
      setForm(fs);
    } catch (e: unknown) {
      setError(e instanceof Error ? e.message : 'Failed to load model config');
      setForm(toFormState(model, null));
    } finally {
      setLoading(false);
    }
  }, [model, demoMode, presetNumCtx]);

  useEffect(() => { load(); }, [load]);

  const setField = (key: FieldDef['key'], v: unknown) => {
    setForm((f) => (f ? { ...f, [key]: v } : f));
  };

  const handleSave = async () => {
    if (!form) return;
    const { cfg, error: parseErr } = fromFormState(form);
    if (parseErr) { setError(parseErr); return; }
    setSaving(true);
    setError(null);
    setSuccess(false);
    try {
      if (demoMode) {
        setMockModelConfig(cfg);
      } else {
        await saveModelConfig(cfg);
      }
      setSuccess(true);
    } catch (e: unknown) {
      setError(e instanceof Error ? e.message : 'Failed to save model config');
    } finally {
      setSaving(false);
    }
  };

  const handleResetAll = async () => {
    if (!model) return;
    setSaving(true);
    setError(null);
    setSuccess(false);
    try {
      if (demoMode) {
        deleteMockModelConfig(model);
      } else {
        await deleteModelConfig(model);
      }
      setForm(toFormState(model, null));
      setSuccess(true);
    } catch (e: unknown) {
      setError(e instanceof Error ? e.message : 'Failed to reset model config');
    } finally {
      setSaving(false);
    }
  };

  return (
    <Modal
      isOpen={!!model}
      onClose={onClose}
      title={model ? `Advanced Settings — ${model}` : 'Advanced Settings'}
      maxWidth="4xl"
    >
      {loading || !form ? (
        <div className="flex items-center justify-center py-12 gap-2 text-sm text-muted-foreground">
          <Loader2 className="w-4 h-4 animate-spin" /> Loading configuration...
        </div>
      ) : (
        <div className="space-y-6">
          <p className="text-xs text-muted-foreground leading-normal">
            Unset fields inherit Ollama's own defaults. Load-time parameters apply the next time
            this model is (re)loaded; Ollama automatically reloads a resident model whose active
            options differ from these. Inference-time parameters are injected only when a client
            request doesn't already specify them.
          </p>

          <Section
            title="Load-time / Engine"
            fields={LOAD_TIME_FIELDS}
            form={form}
            setField={setField}
            disabled={!engineParamsSupported}
            disabledNote="Engine params are Ollama-only. This model's runtime (non-Ollama, or mixed) applies these as launch-time config instead — they can't be set per-request here."
          />
          <Section title="Inference-time / Sampling" fields={INFERENCE_FIELDS} form={form} setField={setField} />

          <div className="space-y-1">
            <div className="flex items-center justify-between gap-2">
              <label className="text-xs font-medium text-foreground">Stop Sequences</label>
              {form.stop_text && (
                <button type="button" onClick={() => setField('stop_text' as never, '')} title="Reset to default" className="text-muted-foreground hover:text-destructive transition-colors">
                  <RotateCcw className="w-3 h-3" />
                </button>
              )}
            </div>
            <input
              type="text"
              value={form.stop_text}
              onChange={(e) => setForm((f) => f && { ...f, stop_text: e.target.value })}
              placeholder="Comma-separated, e.g. </s>, [END]"
              className="w-full px-2.5 py-1.5 text-sm bg-secondary border border-border rounded-md text-foreground placeholder-muted-foreground/60 focus:outline-none focus:ring-1 focus:ring-primary"
            />
            <p className="text-[10px] text-muted-foreground leading-snug">Generation stops immediately when any of these strings is produced.</p>
          </div>

          <div className="space-y-1">
            <div className="flex items-center justify-between gap-2">
              <label className="text-xs font-medium text-foreground">Logit Bias (JSON)</label>
              {form.logit_bias_text && (
                <button type="button" onClick={() => setForm((f) => f && { ...f, logit_bias_text: '' })} title="Reset to default" className="text-muted-foreground hover:text-destructive transition-colors">
                  <RotateCcw className="w-3 h-3" />
                </button>
              )}
            </div>
            <textarea
              value={form.logit_bias_text}
              onChange={(e) => setForm((f) => f && { ...f, logit_bias_text: e.target.value })}
              rows={2}
              placeholder='e.g. {"1234": -5}'
              className="w-full px-2.5 py-1.5 text-sm bg-secondary border border-border rounded-md text-foreground placeholder-muted-foreground/60 focus:outline-none focus:ring-1 focus:ring-primary font-mono resize-y"
            />
            <p className="text-[10px] text-muted-foreground leading-snug">
              Token-ID to bias-value map. OpenAI-compatible clients only — not applied to native Ollama requests.
            </p>
          </div>

          <Section title="Meta / Orchestration" fields={META_FIELDS} form={form} setField={setField} />

          {error && (
            <p className="text-xs text-destructive font-medium bg-destructive/10 border border-destructive/20 rounded-lg p-2.5">
              {error}
            </p>
          )}
          {success && (
            <p className="text-xs text-success font-medium bg-success/10 border border-success/20 rounded-lg p-2.5">
              Saved.
            </p>
          )}

          <div className="flex flex-col sm:flex-row items-stretch sm:items-center justify-between gap-2 pt-2">
            <button
              onClick={handleResetAll}
              disabled={saving}
              className="px-4 py-2 text-xs font-semibold text-muted-foreground hover:text-destructive transition-colors disabled:opacity-50 cursor-pointer"
            >
              Reset all to defaults
            </button>
            <div className="flex items-center gap-3">
              <button
                onClick={onClose}
                disabled={saving}
                className="flex-1 sm:flex-none px-4 py-2 bg-secondary hover:bg-secondary/80 disabled:opacity-50 text-foreground text-sm font-semibold rounded-lg transition-colors cursor-pointer"
              >
                Close
              </button>
              <button
                onClick={handleSave}
                disabled={saving}
                className="flex-1 sm:flex-none inline-flex items-center justify-center gap-2 px-4 py-2 bg-primary hover:bg-primary/90 disabled:opacity-50 text-primary-foreground text-sm font-semibold rounded-lg transition-colors shadow-sm cursor-pointer"
              >
                {saving ? <Loader2 className="w-4 h-4 animate-spin" /> : null}
                Save
              </button>
            </div>
          </div>
        </div>
      )}
    </Modal>
  );
}
