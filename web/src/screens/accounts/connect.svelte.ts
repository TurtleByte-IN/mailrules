import type { AccountInput, Preset, TestResult } from '../../lib/api/accounts';
import * as accountsApi from '../../lib/api/accounts';
import { ApiError } from '../../lib/api/client';
import { accounts, connect } from '../../lib/state/accounts.svelte';
import { addTemplatesByName } from '../../lib/state/compose.svelte';
import { flash } from '../../lib/state/toast.svelte';

export const stepNames = ['Provider', 'Sign in', 'Rules', 'Preview'];

// Names match templates in lib/api/templates.ts; the ones left on are saved as rules when the mailbox is connected.
const starterTemplates = [
  { id: 't1', name: 'Newsletters', desc: 'move to Reading and mark read', on: true },
  { id: 't2', name: 'Receipts', desc: 'move to Receipts', on: true },
  { id: 't3', name: 'Login codes', desc: 'keep in Inbox and flag', on: true },
  { id: 't4', name: 'Cold sales', desc: 'trash pitches, unless you have replied to the sender', on: false },
  { id: 't5', name: 'Travel', desc: 'tickets and bookings to Travel', on: false },
];

/** Zoho keeps each region's mail on its own IMAP host; the account's sign-in only works on its own. */
export const zohoRegions = [
  { id: 'com', label: 'United States (zoho.com)', host: 'imap.zoho.com' },
  { id: 'eu', label: 'Europe (zoho.eu)', host: 'imap.zoho.eu' },
  { id: 'in', label: 'India (zoho.in)', host: 'imap.zoho.in' },
  { id: 'com.au', label: 'Australia (zoho.com.au)', host: 'imap.zoho.com.au' },
  { id: 'jp', label: 'Japan (zoho.jp)', host: 'imap.zoho.jp' },
  { id: 'com.cn', label: 'China (zoho.com.cn)', host: 'imap.zoho.com.cn' },
];

/** What the provider calls the secret the user pastes, as the daemon's preset says. */
export const secretLabel = (p: Preset) => p.secret_label;

/** View state for one run of the connect wizard. Thrown away when the wizard closes. */
export class Wizard {
  step = $state(0);
  presetId = $state<Preset['name']>('icloud');
  email = $state('');
  password = $state('');
  host = $state('');
  /** Data centre for the Zoho preset: an id from `zohoRegions`. */
  region = $state('com');
  port = $state(993);
  tls = $state<Preset['tls_mode']>('implicit');
  /** Once the user has typed a port, the encryption choice leaves it alone. */
  #portEdited = false;
  test = $state<'idle' | 'testing' | 'ok' | 'err'>('idle');
  result = $state<TestResult>();
  error = $state('');
  /** The request field the error is about, as the API names it; empty when it names none. */
  errorPath = $state('');
  busy = $state(false);
  templates = $state(starterTemplates.map((t) => ({ ...t })));
  #run = 0;

  get preset() {
    return accounts.presets.find((p) => p.name === this.presetId);
  }

  /** The field to show the error beside; empty when that field is not on the form (a preset's own host). */
  get errorField() {
    const shown = this.presetId === 'zoho' ? ['username', 'password', 'host'] : this.preset?.host ? ['username', 'password'] : ['username', 'password', 'host', 'port', 'tls_mode'];
    return this.test === 'err' && shown.includes(this.errorPath) ? this.errorPath : '';
  }

  get nextLabel() {
    const n = this.templates.filter((t) => t.on).length;
    if (this.step === 3) return this.busy ? 'Connecting…' : 'Connect';
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

  portEdited() {
    this.#portEdited = true;
    this.edited();
  }

  /** The port follows the encryption mode until the user has set one by hand. */
  setTls(mode: Preset['tls_mode']) {
    this.tls = mode;
    if (!this.#portEdited) this.port = mode === 'implicit' ? 993 : 143;
    this.edited();
  }

  #input(): AccountInput {
    const c = { preset: this.presetId, username: this.email.trim(), password: this.password };
    if (this.presetId === 'zoho') return { ...c, host: zohoRegions.find((r) => r.id === this.region)?.host ?? this.preset?.host };
    return this.preset?.host ? c : { ...c, host: this.host.trim(), port: this.port, tls_mode: this.tls };
  }

  #fail(message: string, path = '') {
    this.error = message;
    this.errorPath = path;
    this.test = 'err';
  }

  async runTest() {
    const c = this.#input();
    if (!c.username || !c.password.trim() || (!this.preset?.host && !c.host)) {
      this.#fail('Enter your email and the app-specific password first.');
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
      this.#fail(e.message, e.path);
    }
  }

  /** Moves on one step; on the last one saves the mailbox. Resolves true once it is saved. */
  async next(): Promise<boolean> {
    if (this.step === 0 && !this.preset) return false;
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
      const a = await connect(this.#input()).finally(() => (this.password = ''));
      // The mailbox is connected from here on, whatever happens to the starter rules.
      const names = this.templates.filter((t) => t.on).map((t) => t.name);
      try {
        const n = names.length ? await addTemplatesByName(names) : 0;
        if (n) flash(`${a.label} is connected with ${n} new ${n === 1 ? 'rule' : 'rules'}`);
      } catch (e) {
        flash(e instanceof Error ? e.message : String(e));
      }
      return true;
    } catch (e) {
      if (!(e instanceof ApiError)) throw e;
      // Refused (already connected, or the server stopped accepting the login): back to the form.
      this.#fail(e.message, e.path);
      this.step = 1;
      return false;
    } finally {
      this.busy = false;
    }
  }
}
