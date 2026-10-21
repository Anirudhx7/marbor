// Run with: npm test. Pure-logic tests for the Model Advisor copy that must
// stay true: the Browse help claim per runtime, the empty-state copy, and the
// honest unknown-VRAM heading and note. The module has no imports.
import test from 'node:test';
import assert from 'node:assert/strict';
import {
  browseHelpClaim,
  browseEmptyCopy,
  isGGUFSearch,
  nodeVramUnknown,
  curatedFitTitle,
  VRAM_UNKNOWN_NOTE,
} from '../src/lib/advisorCopy.ts';

test('GGUF runtimes get the best-effort tag claim and never "can load"', () => {
  for (const rt of ['ollama', 'llamacpp', '', null, undefined]) {
    const c = browseHelpClaim(rt);
    assert.match(c, /leave out models tagged as speech, image, video or detection/, String(rt));
    assert.match(c, /repos without a tag are shown/, String(rt));
    assert.doesNotMatch(c, /can load/i, String(rt));
  }
});

test('vLLM, TGI and MLX keep the text and vision-language claim', () => {
  for (const rt of ['vllm', 'tgi', 'mlx']) {
    const c = browseHelpClaim(rt);
    assert.match(c, /limited to text and vision-language models/, rt);
    assert.doesNotMatch(c, /can load/i, rt);
  }
});

test('a runtime outside the covered list gets no filtering claim', () => {
  assert.equal(browseHelpClaim('sglang'), '');
});

test('GGUF empty state says media models are left out; never that they were hidden', () => {
  for (const rt of ['ollama', 'llamacpp', '', null, undefined]) {
    assert.match(browseEmptyCopy(rt), /speech, image and video models are left out for this runtime/i, String(rt));
    assert.doesNotMatch(browseEmptyCopy(rt), /hidden/, String(rt));
  }
  assert.doesNotMatch(browseEmptyCopy('vllm'), /left out|hidden/);
});

test('isGGUFSearch is the one GGUF rule', () => {
  for (const rt of ['', 'ollama', 'llamacpp', null, undefined]) assert.equal(isGGUFSearch(rt), true, String(rt));
  for (const rt of ['vllm', 'tgi', 'mlx', 'sglang']) assert.equal(isGGUFSearch(rt), false, rt);
});

test('inferred or zero VRAM counts as unknown; declared or detected does not', () => {
  assert.equal(nodeVramUnknown({ vram_total_bytes: 0, vram_source: 'unknown' }), true);
  assert.equal(nodeVramUnknown({ vram_total_bytes: 8e9, vram_source: 'inferred' }), true);
  assert.equal(nodeVramUnknown({ vram_total_bytes: 8e9, vram_source: 'unknown' }), true);
  assert.equal(nodeVramUnknown({ vram_total_bytes: 0, vram_source: 'declared' }), true);
  assert.equal(nodeVramUnknown({ vram_total_bytes: 8e9, vram_source: 'declared' }), false);
  assert.equal(nodeVramUnknown({ vram_total_bytes: 8e9, vram_source: 'agent' }), false);
});

test('curated heading is "Curated models for" when VRAM unknown or incompatible', () => {
  assert.equal(curatedFitTitle('gpu-1', true, false), 'Curated models for gpu-1');
  assert.equal(curatedFitTitle('gpu-1', false, true), 'Curated models for gpu-1');
  assert.equal(curatedFitTitle('gpu-1', true, true), 'Curated models for gpu-1');
  assert.equal(curatedFitTitle('gpu-1', false, false), 'What fits gpu-1');
});

test('the shared note says why fit is missing and how to fix it', () => {
  assert.match(VRAM_UNKNOWN_NOTE, /Fit cannot be computed without this node's VRAM/);
  assert.doesNotMatch(VRAM_UNKNOWN_NOTE, /shows Unknown/);
  assert.match(VRAM_UNKNOWN_NOTE, /Edit node > Capacity overrides/);
  assert.match(VRAM_UNKNOWN_NOTE, /enable the agent/);
});
