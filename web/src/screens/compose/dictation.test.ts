import { afterEach, expect, it, vi } from 'vitest';
import { listen, supported } from './dictation';

afterEach(() => vi.unstubAllGlobals());

it('reports no dictation when the browser has no speech recognition', () => {
  const onEnd = vi.fn();
  expect(supported()).toBe(false);
  expect(listen(vi.fn(), onEnd)).toBeNull();
  expect(onEnd).not.toHaveBeenCalled();
});

it.each(['SpeechRecognition', 'webkitSpeechRecognition'])('dictates through %s', (name) => {
  let r!: Fake;
  class Fake {
    onresult?: (e: unknown) => void;
    onerror?: (e: unknown) => void;
    onend?: () => void;
    start = vi.fn();
    stop = vi.fn(() => this.onend?.());
    constructor() {
      r = this;
    }
  }
  vi.stubGlobal(name, Fake);
  const onText = vi.fn();
  const onEnd = vi.fn();

  expect(supported()).toBe(true);
  const stop = listen(onText, onEnd)!;
  expect(r.start).toHaveBeenCalled();

  r.onresult?.({ results: [[{ transcript: 'Archive receipts' }], [{ transcript: ' and mark them read' }]] });
  expect(onText).toHaveBeenLastCalledWith('Archive receipts and mark them read');

  r.onerror?.({ error: 'not-allowed' });
  stop();
  expect(onEnd).toHaveBeenCalledExactlyOnceWith('not-allowed');
});
