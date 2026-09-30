const RUNTIME_STYLES: Record<string, string> = {
  ollama:   'bg-blue-500/20 text-blue-600 dark:text-blue-400 border border-blue-500/30',
  vllm:     'bg-purple-500/20 text-purple-600 dark:text-purple-400 border border-purple-500/30',
  tgi:      'bg-orange-500/20 text-orange-600 dark:text-orange-400 border border-orange-500/30',
  llamacpp: 'bg-green-500/20 text-green-600 dark:text-green-400 border border-green-500/30',
  mlx:      'bg-pink-500/20 text-pink-600 dark:text-pink-400 border border-pink-500/30',
};

const RUNTIME_LABELS: Record<string, string> = {
  ollama:   'Ollama',
  vllm:     'vLLM',
  tgi:      'TGI',
  llamacpp: 'llama.cpp',
  mlx:      'MLX (Apple Silicon)',
};

export function RuntimeBadge({ runtime }: { runtime: string }) {
  const key = (runtime || '').toLowerCase();
  const style = RUNTIME_STYLES[key] ?? 'bg-secondary text-muted-foreground border border-border';
  const label = RUNTIME_LABELS[key] ?? (runtime || 'unknown');
  return (
    <span className={`inline-flex items-center px-2 py-0.5 rounded-md text-xs font-medium ${style}`}>
      {label}
    </span>
  );
}
