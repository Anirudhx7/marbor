// Run with: node --test scripts/
// Pure-logic tests for the vLLM/TGI "download only" pull behavior. The module
// under test has no imports and no browser APIs, so Node can load it directly.
import test from 'node:test';
import assert from 'node:assert/strict';
import {
  DOWNLOAD_ONLY_NOTE,
  DEFAULT_PULL_COMPLETE_TEXT,
  isDownloadOnlyRuntime,
  pullOptionsFor,
  completionText,
  sizeNotCurated,
} from '../src/lib/downloadOnly.ts';

test('vllm and tgi are download-only, everything else is not', () => {
  for (const rt of ['vllm', 'tgi']) assert.equal(isDownloadOnlyRuntime(rt), true, rt);
  for (const rt of ['ollama', 'mlx', 'llamacpp', '', undefined, null, 'VLLM']) {
    assert.equal(isDownloadOnlyRuntime(rt), false, String(rt));
  }
});

test('load check is hidden and forced off on vllm/tgi even when the user asked for it', () => {
  for (const rt of ['vllm', 'tgi']) {
    assert.deepEqual(pullOptionsFor(rt, true), { verifyLoad: false, completionNote: DOWNLOAD_ONLY_NOTE });
    assert.deepEqual(pullOptionsFor(rt, false), { verifyLoad: false, completionNote: DOWNLOAD_ONLY_NOTE });
  }
});

test('other runtimes keep the callers verify choice and the generic completion text', () => {
  for (const rt of ['ollama', 'mlx', 'llamacpp', undefined, null]) {
    assert.deepEqual(pullOptionsFor(rt, true), { verifyLoad: true, completionNote: '' });
    assert.deepEqual(pullOptionsFor(rt, false), { verifyLoad: false, completionNote: '' });
  }
});

test('the widget falls back to "Pull complete." when there is no note', () => {
  assert.equal(completionText(''), 'Pull complete.');
  assert.equal(completionText(undefined), DEFAULT_PULL_COMPLETE_TEXT);
  assert.equal(completionText(DOWNLOAD_ONLY_NOTE), DOWNLOAD_ONLY_NOTE);
});

test('sizeNotCurated: only an unpicked vram_unknown vllm/tgi row with a missing or zero size', () => {
  assert.equal(sizeNotCurated(true, false, 'vram_unknown', [{ size_mb: 0 }]), true);
  assert.equal(sizeNotCurated(true, false, 'vram_unknown', []), true);
  assert.equal(sizeNotCurated(true, false, 'vram_unknown', undefined), true);
  assert.equal(sizeNotCurated(true, false, 'vram_unknown', [{}]), true);
  assert.equal(sizeNotCurated(true, false, 'vram_unknown', [{ size_mb: 14525 }]), false);
  assert.equal(sizeNotCurated(true, false, 'too_large', [{ size_mb: 0 }]), false);
  assert.equal(sizeNotCurated(true, true, 'vram_unknown', [{ size_mb: 0 }]), false);
  assert.equal(sizeNotCurated(false, false, 'vram_unknown', [{ size_mb: 0 }]), false);
});
