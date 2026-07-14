import { useState, useEffect, useCallback, type ReactElement } from 'react';
import { ChevronDown, RotateCcw, Loader2 } from 'lucide-react';
import { Modal } from './Modal';
import { fetchModelConfig, saveModelConfig, deleteModelConfig, fetchModelConfigCapabilities } from '../lib/api';
import { getMockModelConfig, setMockModelConfig, deleteMockModelConfig, getMockModelConfigCapabilities } from '../lib/mockData';
import type { ModelConfig } from '../types';

// A node this model is resident on, paired with the runtime serving it there
// (e.g. { name: 'gpu-node-02', runtime: 'vllm' }). Configuration is always
// scoped to one (model, node) pair, since the same model name can carry a
// different profile per node  --  different runtime, different VRAM budget.
export interface ModelConfigNode {
  name: string;
  runtime: string;
}

// Editable form shape: same keys as ModelConfig, but `stop`/`logit_bias` are
// held as raw text while being edited (comma-separated / JSON respectively)
// so free-typing doesn't fight JSON.parse on every keystroke.
type FormState = Omit<ModelConfig, 'model' | 'stop' | 'logit_bias'> & {
  model: string;
  stop_text: string;
  logit_bias_text: string;
};

// Some ModelConfig fields are injected under a different wire name on some
// runtimes than the JSON key that stores the value (vLLM's OpenAI-compatible
// server calls it "repetition_penalty"; the ModelConfig/Ollama/llama.cpp name
// is "repeat_penalty"  --  see internal/proxy/model_config.go and
// internal/store/model_config_capabilities.go's OpenAICompatExtraFields).
// The capabilities endpoint returns wire names, so aliases map a form field
// key to every wire name that means the same underlying field.
const FIELD_ALIASES: Record<string, string[]> = {
  repeat_penalty: ['repeat_penalty', 'repetition_penalty'],
};

// normalizeRuntime mirrors store.SupportedFieldsFor's convention: "" (or
// anything outside the known runtime set) is treated as "ollama".
function normalizeRuntime(runtime: string | undefined): string {
  const r = (runtime || '').toLowerCase();
  return r === 'vllm' || r === 'tgi' || r === 'llamacpp' ? r : 'ollama';
}

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

// Ollama-only  --  verified against Ollama's current api/types.go
// Options/Runner structs. flash_attention, offload_kv_cache_to_gpu,
// rope_frequency_base/scale, use_mlock, and tensor_parallelism were removed:
// none of them are real per-request parameters in current Ollama (some were
// pruned as Ollama moved away from wrapping llama.cpp's full option set
// directly; none ever existed on any other runtime).
const LOAD_TIME_FIELDS: FieldDef[] = [
  { key: 'num_ctx', label: 'Context Length (num_ctx)', help: 'Max context window in tokens.', type: 'slider', min: 2048, max: 131072, step: 2048, sliderDefault: 4096 },
  { key: 'num_gpu', label: 'GPU Layers (num_gpu)', help: 'Layers offloaded to GPU. Higher = more VRAM, less CPU. Depends on model size  --  leave unset to let Ollama decide.', type: 'int', min: 0 },
  { key: 'main_gpu', label: 'Main GPU (main_gpu)', help: 'Index of the primary GPU when running across multiple devices.', type: 'int', min: 0 },
  { key: 'num_batch', label: 'Batch Size (num_batch)', help: 'Tokens processed in parallel during prompt eval.', type: 'slider', min: 32, max: 2048, step: 32, sliderDefault: 512 },
  { key: 'num_thread', label: 'CPU Threads (num_thread)', help: 'CPU threads for generation. 0 = auto.', type: 'int', min: 0 },
  { key: 'use_mmap', label: 'Use mmap', help: 'Memory-maps the model file for faster loads.', type: 'bool' },
  { key: 'draft_num_predict', label: 'Draft Tokens (draft_num_predict)', help: 'Speculative-decoding draft length, when a draft model is configured.', type: 'int', min: 0 },
  { key: 'ttl', label: 'TTL (seconds)', help: 'Idle seconds before auto-unload. 0 = disabled. e.g. 300 = 5 min, 1800 = 30 min, 3600 = 1 hour.', type: 'int', min: 0 },
];

const INFERENCE_FIELDS: FieldDef[] = [
  { key: 'temperature', label: 'Temperature', help: 'Sampling randomness. 0 = deterministic/focused, 2 = max chaos. Most chat use cases: 0.6–0.9.', type: 'slider', min: 0, max: 2, step: 0.05, sliderDefault: 0.8 },
  { key: 'top_p', label: 'Top P (nucleus sampling)', help: 'Only sample from the smallest set of tokens whose cumulative probability reaches this. Lower = more focused.', type: 'slider', min: 0, max: 1, step: 0.05, sliderDefault: 0.9 },
  { key: 'top_k', label: 'Top K', help: 'Only sample from the top K candidate tokens. Lower = more focused, higher = more variety.', type: 'slider', min: 0, max: 100, step: 1, sliderDefault: 40 },
  { key: 'min_p', label: 'Min P', help: 'Minimum token probability relative to the top token’s probability.', type: 'slider', min: 0, max: 1, step: 0.01, sliderDefault: 0 },
  { key: 'typical_p', label: 'Typical P', help: 'Locally typical sampling threshold  --  filters out tokens with atypical information content.', type: 'slider', min: 0, max: 1, step: 0.05, sliderDefault: 1 },
  { key: 'num_keep', label: 'Keep Tokens (num_keep)', help: 'Prompt tokens kept when the context window fills up. -1 = keep all.', type: 'int', min: -1 },
  { key: 'max_tokens', label: 'Max Tokens (num_predict)', help: 'Max tokens to generate. -1 = unlimited.', type: 'int', min: -1 },
  { key: 'seed', label: 'Seed', help: 'Fixed RNG seed for reproducible output. -1 = random.', type: 'int', min: -1 },
  { key: 'repeat_penalty', label: 'Repeat Penalty', help: 'Penalizes repeated tokens. 1.0 = off, higher = less repetition.', type: 'slider', min: 0.5, max: 2, step: 0.05, sliderDefault: 1.1 },
  { key: 'repeat_last_n', label: 'Repeat Last N', help: 'Lookback window (in tokens) for the repeat penalty. -1 = whole context.', type: 'int', min: -1 },
  { key: 'presence_penalty', label: 'Presence Penalty', help: 'Penalizes tokens already used at all, regardless of how often.', type: 'slider', min: -2, max: 2, step: 0.1, sliderDefault: 0 },
  { key: 'frequency_penalty', label: 'Frequency Penalty', help: 'Penalizes tokens by how often they’ve already recurred.', type: 'slider', min: -2, max: 2, step: 0.1, sliderDefault: 0 },
  {
    key: 'mirostat', label: 'Mirostat Mode', help: 'Adaptive sampling that targets a constant perplexity instead of tuning top-k/top-p by hand. llama.cpp only. 2.0 is the modern variant.', type: 'select', numeric: true,
    options: [{ value: '0', label: 'Off' }, { value: '1', label: 'Mirostat 1.0' }, { value: '2', label: 'Mirostat 2.0' }],
  },
  { key: 'mirostat_tau', label: 'Mirostat Tau', help: 'Target entropy for Mirostat sampling. llama.cpp only. Only used when Mirostat mode is on.', type: 'slider', min: 0, max: 10, step: 0.1, sliderDefault: 5 },
  { key: 'mirostat_eta', label: 'Mirostat Eta', help: 'Learning rate for Mirostat sampling. llama.cpp only. Only used when Mirostat mode is on.', type: 'slider', min: 0, max: 1, step: 0.01, sliderDefault: 0.1 },
  {
    key: 'response_format', label: 'Response Format', help: 'Force a structured output format.', type: 'select',
    options: [{ value: 'json', label: 'JSON object' }],
  },
];

// llama.cpp-only sampling extras  --  its server README documents these as
// also accepted on its OpenAI-compatible endpoints, not just its native
// /completion endpoint.
const LLAMACPP_EXTRA_FIELDS: FieldDef[] = [
  { key: 'n_probs', label: 'N Probs', help: 'Number of top token probabilities to return per position.', type: 'int', min: 0 },
  { key: 'min_keep', label: 'Min Keep', help: 'Minimum number of tokens kept for sampling regardless of other filters.', type: 'int', min: 0 },
  { key: 'dry_multiplier', label: 'DRY Multiplier', help: 'Strength of DRY (Don’t Repeat Yourself) repetition penalty. 0 = off.', type: 'slider', min: 0, max: 5, step: 0.1, sliderDefault: 0.8 },
  { key: 'dry_base', label: 'DRY Base', help: 'Base growth rate for the DRY penalty as repeated sequences get longer.', type: 'slider', min: 1, max: 4, step: 0.05, sliderDefault: 1.75 },
  { key: 'dry_allowed_length', label: 'DRY Allowed Length', help: 'Longest repeated sequence allowed before the DRY penalty kicks in.', type: 'int', min: 0 },
  { key: 'dry_penalty_last_n', label: 'DRY Penalty Last N', help: 'Lookback window (in tokens) considered for the DRY penalty. -1 = whole context.', type: 'int', min: -1 },
  { key: 'xtc_probability', label: 'XTC Probability', help: 'Chance of applying XTC (Exclude Top Choices) sampling per token. 0 = off.', type: 'slider', min: 0, max: 1, step: 0.05, sliderDefault: 0 },
  { key: 'xtc_threshold', label: 'XTC Threshold', help: 'Minimum probability a top token needs to be eligible for XTC exclusion.', type: 'slider', min: 0, max: 1, step: 0.05, sliderDefault: 0.1 },
];

// vLLM-only sampling extras  --  its OpenAI-compatible ChatCompletionRequest
// accepts these beyond the strict OpenAI schema.
const VLLM_EXTRA_FIELDS: FieldDef[] = [
  { key: 'length_penalty', label: 'Length Penalty', help: 'Exponential penalty applied to sequence length; >1 favors longer output, <1 favors shorter.', type: 'slider', min: 0, max: 3, step: 0.05, sliderDefault: 1 },
  { key: 'min_tokens', label: 'Min Tokens', help: 'Minimum tokens generated before the model is allowed to stop.', type: 'int', min: 0 },
  { key: 'skip_special_tokens', label: 'Skip Special Tokens', help: 'Strips special tokens (e.g. EOS) from the decoded output text.', type: 'bool' },
  { key: 'truncate_prompt_tokens', label: 'Truncate Prompt Tokens', help: 'Truncates the prompt to at most this many tokens before generation.', type: 'int', min: 1 },
];

// Shared between vLLM and llama.cpp  --  identical wire name/meaning on both.
const IGNORE_EOS_FIELD: FieldDef = { key: 'ignore_eos', label: 'Ignore EOS', help: 'Keeps generating past the end-of-sequence token instead of stopping there.', type: 'bool' };

const META_FIELDS: FieldDef[] = [
  { key: 'system', label: 'System Prompt', help: 'Overrides the model’s default system prompt.', type: 'textarea' },
  { key: 'template', label: 'Prompt Template', help: 'Overrides the model’s default prompt template. Ollama only  --  no equivalent on other runtimes.', type: 'textarea' },
  { key: 'rpm', label: 'Requests / Minute Cap', help: 'Caps requests-per-minute for this model across all keys. Empty = unlimited.', type: 'int', min: 0 },
  { key: 'tpm', label: 'Tokens / Minute Cap', help: 'Caps tokens-per-minute for this model across all keys. Empty = unlimited.', type: 'int', min: 0 },
];

function toFormState(model: string, node: string, cfg: ModelConfig | null): FormState {
  const c = cfg ?? { model, node };
  return {
    ...c,
    model,
    node,
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
      return { cfg: { model: form.model, node: form.node }, error: 'Logit bias must be valid JSON, e.g. {"1234": -5}' };
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
        placeholder="Not set  --  inherits Ollama's own default"
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
  emptyNote,
}: {
  title: string;
  fields: FieldDef[];
  form: FormState;
  setField: (key: FieldDef['key'], v: unknown) => void;
  // Shown instead of the field grid when `fields` has been filtered down to
  // nothing for the selected node's runtime (e.g. Load-time/Engine on a vLLM
  // node)  --  replaces the old whole-section gray-out with per-field filtering
  // driven by the capabilities endpoint.
  emptyNote?: string;
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
          {fields.length === 0 ? (
            <p className="text-xs text-muted-foreground leading-snug">
              {emptyNote ?? 'No fields in this section apply to the selected node’s runtime.'}
            </p>
          ) : (
            <div className="grid grid-cols-1 sm:grid-cols-2 lg:grid-cols-3 gap-x-6 gap-y-5">
              {fields.map((def) => (
                <Field
                  key={String(def.key)}
                  def={def}
                  value={(form as Record<string, unknown>)[def.key as string]}
                  onChange={(v) => setField(def.key, v)}
                  onReset={() => setField(def.key, undefined)}
                />
              ))}
            </div>
          )}
        </div>
      )}
    </div>
  );
}

export function ModelConfigModal({
  model,
  demoMode,
  nodes,
  presetNumCtx,
  onClose,
}: {
  model: string | null;
  demoMode: boolean;
  // Every node this model is resident on, paired with the runtime serving it
  // there. Configuration is always scoped to one (model, node) pair  --  the
  // modal shows a node selector when there's more than one, and fetches/
  // saves/resets against whichever node is currently selected.
  nodes: ModelConfigNode[];
  // Pre-fills num_ctx the first time this model's config is opened (no saved
  // config yet)  --  e.g. from a context-length slider elsewhere in the UI, so
  // that control is a real input into the config rather than decorative.
  presetNumCtx?: number;
  onClose: () => void;
}) {
  const [form, setForm] = useState<FormState | null>(null);
  const [loading, setLoading] = useState(false);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [success, setSuccess] = useState(false);
  const [selectedNode, setSelectedNode] = useState<string>('');
  const [resetConfirmOpen, setResetConfirmOpen] = useState(false);
  // Capabilities are fetched once and cached for the modal's lifetime  --  the
  // field list per runtime doesn't change while the modal is open, so
  // switching nodes only needs a re-filter, not a re-fetch. `null` means
  // "not loaded yet"  --  every field is shown in that state so the form
  // doesn't flash empty sections while capabilities are in flight.
  const [capabilities, setCapabilities] = useState<Record<string, string[]> | null>(null);

  // Reset the node selection when the modal is opened for a different model.
  // Keep the current selection if it's still one of the model's nodes (e.g.
  // Models.tsx re-renders with a new-but-equivalent `nodes` array every poll
  //  --  that must not silently kick the user back to the first node).
  useEffect(() => {
    setSelectedNode((prev) => (nodes.some((n) => n.name === prev) ? prev : (nodes[0]?.name ?? '')));
  }, [model, nodes]);

  useEffect(() => {
    let cancelled = false;
    (async () => {
      try {
        const caps = demoMode ? getMockModelConfigCapabilities() : await fetchModelConfigCapabilities();
        if (!cancelled) setCapabilities(caps);
      } catch {
        // Non-fatal  --  falls back to showing every field, same as before
        // this endpoint existed.
      }
    })();
    return () => { cancelled = true; };
  }, [demoMode]);

  const runtime = normalizeRuntime(nodes.find((n) => n.name === selectedNode)?.runtime);
  const supportedFields = capabilities ? (capabilities[runtime] ?? capabilities.ollama ?? []) : null;
  const isSupported = (key: string): boolean => {
    if (!supportedFields) return true;
    const aliases = FIELD_ALIASES[key] ?? [key];
    return aliases.some((a) => supportedFields.includes(a));
  };
  const loadTimeFields = LOAD_TIME_FIELDS.filter((f) => isSupported(String(f.key)));
  const inferenceFields = INFERENCE_FIELDS.filter((f) => isSupported(String(f.key)));
  const extraFields = [...LLAMACPP_EXTRA_FIELDS, ...VLLM_EXTRA_FIELDS, IGNORE_EOS_FIELD].filter((f) => isSupported(String(f.key)));
  const metaFields = META_FIELDS.filter((f) => isSupported(String(f.key)));
  const logitBiasSupported = isSupported('logit_bias');

  const load = useCallback(async () => {
    if (!model || !selectedNode) return;
    setLoading(true);
    setError(null);
    setSuccess(false);
    try {
      const cfg = demoMode ? getMockModelConfig(model, selectedNode) : await fetchModelConfig(model, selectedNode);
      const fs = toFormState(model, selectedNode, cfg);
      if (!cfg && presetNumCtx !== undefined) fs.num_ctx = presetNumCtx;
      setForm(fs);
    } catch (e: unknown) {
      setError(e instanceof Error ? e.message : 'Failed to load model config');
      setForm(toFormState(model, selectedNode, null));
    } finally {
      setLoading(false);
    }
  }, [model, selectedNode, demoMode, presetNumCtx]);

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
    if (!model || !selectedNode) return;
    setSaving(true);
    setError(null);
    setSuccess(false);
    try {
      if (demoMode) {
        deleteMockModelConfig(model, selectedNode);
      } else {
        await deleteModelConfig(model, selectedNode);
      }
      setForm(toFormState(model, selectedNode, null));
      setSuccess(true);
    } catch (e: unknown) {
      setError(e instanceof Error ? e.message : 'Failed to reset model config');
    } finally {
      setSaving(false);
      setResetConfirmOpen(false);
    }
  };

  return (
    <>
    <Modal
      isOpen={!!model}
      onClose={onClose}
      title={model ? `Advanced Settings  --  ${model}` : 'Advanced Settings'}
      maxWidth="4xl"
    >
      {loading || !form ? (
        <div className="flex items-center justify-center py-12 gap-2 text-sm text-muted-foreground">
          <Loader2 className="w-4 h-4 animate-spin" /> Loading configuration...
        </div>
      ) : (
        <div className="space-y-6">
          {nodes.length > 0 && (
            <div className="flex items-center gap-2">
              <label className="text-xs font-medium text-foreground shrink-0">Node</label>
              <select
                value={selectedNode}
                onChange={(e) => setSelectedNode(e.target.value)}
                className="flex-1 px-2.5 py-1.5 text-sm bg-secondary border border-border rounded-md text-foreground focus:outline-none focus:ring-1 focus:ring-primary"
              >
                {nodes.map((n) => (
                  <option key={n.name} value={n.name}>{n.name} ({normalizeRuntime(n.runtime)})</option>
                ))}
              </select>
            </div>
          )}

          <p className="text-xs text-muted-foreground leading-normal">
            Unset fields inherit the backend's own defaults. Load-time parameters apply the next
            time this model is (re)loaded on the selected node; Ollama automatically reloads a
            resident model whose active options differ from these. Inference-time parameters are
            injected only when a client request doesn't already specify them. Fields shown below
            depend on the selected node's runtime  --  options that don't apply there are hidden.
          </p>

          <Section
            title="Load-time / Engine"
            fields={loadTimeFields}
            form={form}
            setField={setField}
            emptyNote="Engine params are Ollama-only. The selected node's runtime applies these as launch-time config instead  --  they can't be set per-request here."
          />
          <Section title="Inference-time / Sampling" fields={inferenceFields} form={form} setField={setField} />
          <Section
            title="Runtime-Specific Extras"
            fields={extraFields}
            form={form}
            setField={setField}
            emptyNote="The selected node's runtime has no additional sampling extras beyond the fields above."
          />

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

          {logitBiasSupported && (
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
                Token-ID to bias-value map.
              </p>
            </div>
          )}

          <Section title="Meta / Orchestration" fields={metaFields} form={form} setField={setField} />

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
              onClick={() => setResetConfirmOpen(true)}
              disabled={saving}
              className="px-4 py-2 text-xs font-semibold text-muted-foreground hover:text-destructive transition-colors disabled:opacity-50 cursor-pointer"
            >
              Reset this node to defaults
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

    <Modal
      isOpen={resetConfirmOpen}
      onClose={() => setResetConfirmOpen(false)}
      title="Reset this node to defaults"
      maxWidth="sm"
    >
      <div className="space-y-4">
        <p className="text-sm text-muted-foreground">
          Are you sure you want to reset <span className="text-foreground font-semibold">{model}</span> on node{' '}
          <span className="text-foreground font-semibold">{selectedNode}</span> to backend defaults?
        </p>
        <p className="text-xs text-muted-foreground">
          This clears every configured override for this (model, node) pair  --  temperature, system prompt, rate
          limits, everything. This action cannot be undone.
        </p>
        {error && (
          <p className="text-sm text-destructive">{error}</p>
        )}
        <div className="flex justify-end gap-3 pt-4 border-t border-border">
          <button
            onClick={() => setResetConfirmOpen(false)}
            disabled={saving}
            className="px-4 py-2 text-sm font-medium text-muted-foreground hover:text-foreground transition-colors disabled:opacity-50"
          >
            Cancel
          </button>
          <button
            onClick={handleResetAll}
            disabled={saving}
            className="inline-flex items-center gap-2 px-4 py-2 bg-destructive hover:bg-destructive/90 disabled:opacity-50 text-destructive-foreground font-medium rounded-lg text-sm transition-colors shadow-sm"
          >
            {saving ? <Loader2 className="w-4 h-4 animate-spin" /> : null}
            Reset to Defaults
          </button>
        </div>
      </div>
    </Modal>
    </>
  );
}
