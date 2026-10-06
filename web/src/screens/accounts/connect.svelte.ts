import type { Credentials, TestResult } from '../../lib/api/accounts';
import * as accountsApi from '../../lib/api/accounts';
import { ApiError } from '../../lib/api/client';
import { accounts, connect } from '../../lib/state/accounts.svelte';

export const stepNames = ['Provider', 'Sign in', 'Rules', 'Preview'];

// DEMO: the starter rules become GET /api/templates and the counts a real preview run
// once the template gallery is built (F6). Until then picking templates adds no rules.
const starterTemplates = [
  { id: 't1', name: 'Newsletters', desc: 'move to Reading and mark read', on: true },
  { id: 't2', name: 'Receipts', desc: 'move to Receipts', on: true },
  { id: 't3', name: 'Login codes', desc: 'keep in Inbox and flag', on: true },
  { id: 't4', name: 'Cold sales', desc: 'trash pitches from people you never emailed', on: false },
  { id: 't5', name: 'Travel', desc: 'tickets and bookings to Travel', on: false },
];
export const preview = [
  { count: 41, label: 'to Reading (Newsletters)' },
  { count: 12, label: 'to Receipts' },
  { count: 6, label: 'login codes flagged' },
  { count: 38, label: 'left in Inbox' },
  { count: 3, label: 'to Needs review' },
];

/** View state for one run of the connect wizard. Thrown away when the wizard closes. */
export class Wizard {
  step = $state(0);
  presetId = $state('icloud');
  email = $state('');
  password = $state('');
  host = $state('');
  port = $state(993);
  test = $state<'idle' | 'testing' | 'ok' | 'err'>('idle');
  result = $state<TestResult>();
  error = $state('');
  busy = $state(false);
  templates = $state(starterTemplates.map((t) => ({ ...t })));
  #run = 0;

  get preset() {
    return accounts.presets.find((p) => p.id === this.presetId);
  }

  get nextLabel() {
    const n = this.templates.filter((t) => t.on).length;
    if (this.step === 3) return 'Go live';
    if (this.step === 1 && this.test !== 'ok') return 'Test and continue';
    if (this.step === 2) return `Preview with ${n} ${n === 1 ? 'rule' : 'rules'}`;
    return 'Continue';
  }

  get testLabel() {
    return this.test === 'testing' ? 'Testing connection…' : this.test === 'ok' ? 'Connection works' : 'Test connection';
  }

  /** Any change to the sign-in fields voids the last test, including one still in flight. */
  edited() {
    this.#run++;
    this.test = 'idle';
  }

  #credentials(): Credentials {
    const c = { preset: this.presetId, email: this.email.trim(), password: this.password };
    return this.preset?.knowsHost ? c : { ...c, host: this.host.trim(), port: this.port };
  }

  async runTest() {
    const c = this.#credentials();
    if (!c.email || !c.password.trim() || (!this.preset?.knowsHost && !c.host)) {
      this.error = 'Enter your email and the app-specific password first.';
      this.test = 'err';
      return;
    }
    const run = ++this.#run;
    this.test = 'testing';
    try {
      const r = await accountsApi.test(c);
      if (run !== this.#run) return;
      this.result = r;
      this.test = 'ok';
    } catch (e) {
      if (!(e instanceof ApiError)) throw e;
      if (run !== this.#run) return;
      this.error = e.message;
      this.test = 'err';
    }
  }

  /** Moves on one step; on the last one saves the mailbox. Resolves true once it is saved. */
  async next(): Promise<boolean> {
    if (this.step === 0 && !this.preset?.available) return false;
    if (this.step === 1 && this.test !== 'ok') {
      await this.runTest();
      return false;
    }
    if (this.step < 3) {
      this.step++;
      return false;
    }
    if (this.test !== 'ok' || this.busy) return false;
    this.busy = true;
    try {
      await connect(this.#credentials());
    } finally {
      this.busy = false;
      this.password = '';
    }
    return true;
  }
}
