import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { close, open, subscribe, type Events } from './events';

// Shaped like MessageAction in api/openapi.yaml.
const undoneAction: Events['action.undone'] = {
  id: 21, message_id: 1, account_id: 1, decision_id: 11, batch_id: 5, kind: 'move', folder: 'Jobs', from_folder: 'INBOX', to_folder: 'Jobs',
  status: 'undone', error: '', created_at: 1000, undone_at: 2000,
};

class FakeEventSource {
  static made: FakeEventSource[] = [];
  listeners = new Map<string, (e: MessageEvent) => void>();
  closed = false;
  constructor(public url: string) {
    FakeEventSource.made.push(this);
  }
  addEventListener(name: string, fn: (e: MessageEvent) => void) {
    this.listeners.set(name, fn);
  }
  close() {
    this.closed = true;
  }
  emit(name: string, data: unknown) {
    this.listeners.get(name)?.({ data: JSON.stringify(data) } as MessageEvent);
  }
}

beforeEach(() => {
  FakeEventSource.made = [];
  vi.stubGlobal('EventSource', FakeEventSource);
});
afterEach(() => {
  close();
  vi.unstubAllGlobals();
});

describe('events', () => {
  it('opens no connection until open() is called, and then only one', () => {
    subscribe('action.undone', () => {});
    expect(FakeEventSource.made).toHaveLength(0);
    open();
    open();
    expect(FakeEventSource.made.map((s) => s.url)).toEqual(['/api/events']);
  });

  it('hands parsed data to the subscribers of that event only', () => {
    const undone = vi.fn();
    const changed = vi.fn();
    subscribe('action.undone', undone);
    subscribe('rules.changed', changed);
    open();
    FakeEventSource.made[0].emit('action.undone', undoneAction);
    expect(undone).toHaveBeenCalledExactlyOnceWith(undoneAction);
    expect(changed).not.toHaveBeenCalled();
  });

  it('stops calling a handler after it unsubscribes', () => {
    const handler = vi.fn();
    const stop = subscribe('action.undone', handler);
    open();
    stop();
    FakeEventSource.made[0].emit('action.undone', undoneAction);
    expect(handler).not.toHaveBeenCalled();
  });

  it('close() ends the stream and lets open() start a new one', () => {
    open();
    close();
    expect(FakeEventSource.made[0].closed).toBe(true);
    open();
    expect(FakeEventSource.made).toHaveLength(2);
  });
});
