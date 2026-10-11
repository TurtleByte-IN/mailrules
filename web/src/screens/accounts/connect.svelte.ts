import type { AccountInput, FormInput, FormStepName, Preset, ServerCert, TestResult } from '../../lib/api/accounts';
import * as accountsApi from '../../lib/api/accounts';
import { ApiError } from '../../lib/api/client';
import { accounts, connect, load } from '../../lib/state/accounts.svelte';
import { startOneClick } from '../../lib/state/oneclick';
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

/**
 * Presets whose server the person may change although the preset names one: Proton Mail Bridge
 * runs on the person's own machine, and its port and encryption can be changed in Bridge.
 */
export const editableServer: Preset['name'][] = ['proton'];

/**
 * How a tile signs in: with a pasted password or app password, through the provider's own sign-in (one-click,
 * `one_click_url`), or through a sign-in form a module answers (`form_url`).
 */
export type SignInVia = 'password' | 'one_click' | 'form';

/** View state for one run of the connect wizard. Thrown away when the wizard closes. */
export class Wizard {
  step = $state(0);
  presetId = $state<Preset['name']>('icloud');
  /** How the tile picked signs in. */
  via = $state<SignInVia>('password');
  /**
   * The mailbox a one-click sign-in or a module's sign-in form already connected: the wizard goes on at Rules
   * for it, so Provider and Sign in are behind it and the last step saves only the starter rules.
   */
  accountId = $state<number>();
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
  /** The server's certificate, while the person is asked to accept it. */
  cert = $state<ServerCert>();
  /** The fingerprint of the certificate the person accepted; sent with every test and the save. */
  certFingerprint = $state('');
  busy = $state(false);
  /** The step of a module's sign-in form being asked for, and the module's note of which sign-in it belongs to. */
  formStep = $state<FormStepName>('sign_in');
  #formState = '';
  /** The module's sentence about the last step, such as a refused password; empty when it said none. */
  formMessage = $state('');
  /** What the form's later steps ask for. Like `password`, cleared once posted. */
  code = $state('');
  mailboxPassword = $state('');
  templates = $state(starterTemplates.map((t) => ({ ...t })));
  #run = 0;

  /**
   * `added`: the id of a mailbox a one-click sign-in just connected, to go on with at Rules. `from`: where a
   * one-click sign-in this run starts should come back to.
   */
  readonly from: 'setup' | 'accounts';
  constructor(added?: number, from: 'setup' | 'accounts' = 'accounts') {
    this.from = from;
    if (added) [this.accountId, this.step, this.test] = [added, 2, 'ok'];
  }

  /** The first step this run shows: Rules when it goes on for a mailbox already connected. */
  get first() {
    return this.accountId ? 2 : 0;
  }

  get preset() {
    return accounts.presets.find((p) => p.name === this.presetId);
  }

  /** Host, encryption and port are on the form: for a server the person names, and for Proton Mail Bridge. */
  get serverFields() {
    return !!this.preset && (!this.preset.host || editableServer.includes(this.presetId));
  }

  /** The field to show the error beside; empty when that field is not on the form (a preset's own host). */
  get errorField() {
    const shown = this.presetId === 'zoho' ? ['username', 'password', 'host'] : this.serverFields ? ['username', 'password', 'host', 'port', 'tls_mode'] : ['username', 'password'];
    return this.test === 'err' && shown.includes(this.errorPath) ? this.errorPath : '';
  }

  get nextLabel() {
    const n = this.templates.filter((t) => t.on).length;
    if (this.step === 3 && this.accountId) return this.busy ? 'Saving…' : 'Finish';
    if (this.step === 3) return this.busy ? 'Connecting…' : 'Connect';
    if (this.step === 1 && this.via === 'form') return this.busy ? 'Signing in…' : this.formStep === 'sign_in' ? 'Sign in' : 'Continue';
    if (this.step === 1 && this.test !== 'ok') return 'Test and continue';
    if (this.step === 2) return `Preview with ${n} ${n === 1 ? 'rule' : 'rules'}`;
    return 'Continue';
  }

  get testLabel() {
    return this.test === 'testing' ? 'Testing connection…' : this.test === 'ok' ? 'Connection works' : 'Test connection';
  }

  /** Picks a provider and how to sign in to it; a preset whose server is on the form fills it in. */
  choose(name: Preset['name'], via: SignInVia = 'password') {
    this.presetId = name;
    this.via = via;
    [this.formStep, this.#formState, this.formMessage] = ['sign_in', '', ''];
    const p = this.preset;
    if (p && this.serverFields) {
      [this.host, this.port, this.tls] = [p.host, p.port, p.tls_mode];
      this.#portEdited = false;
    }
    this.serverEdited();
  }

  /** Any change to the sign-in fields voids the last test, including one still in flight. */
  edited() {
    this.#run++;
    this.test = 'idle';
    this.cert = undefined;
  }

  /** Another server: a certificate accepted for the old one does not carry over. */
  serverEdited() {
    this.certFingerprint = '';
    this.edited();
  }

  portEdited() {
    this.#portEdited = true;
    this.serverEdited();
  }

  /** The port follows the encryption mode until the user has set one by hand, or the preset names it. */
  setTls(mode: Preset['tls_mode']) {
    this.tls = mode;
    if (!this.#portEdited && !this.preset?.host) this.port = mode === 'implicit' ? 993 : 143;
    this.serverEdited();
  }

  #input(): AccountInput {
    const c: AccountInput = { preset: this.presetId, username: this.email.trim(), password: this.password };
    if (this.certFingerprint) c.cert_fingerprint = this.certFingerprint;
    if (this.presetId === 'zoho') return { ...c, host: zohoRegions.find((r) => r.id === this.region)?.host ?? this.preset?.host };
    return this.serverFields ? { ...c, host: this.host.trim(), port: this.port, tls_mode: this.tls } : c;
  }

  #fail(message: string, path = '') {
    this.error = message;
    this.errorPath = path;
    this.test = 'err';
  }

  /** Shows why the daemon refused, with the certificate to accept when that is why. */
  #refused(e: ApiError) {
    this.#fail(e.message, e.path);
    this.cert = e.code === 'cert_untrusted' || e.code === 'cert_changed' ? e.cert : undefined;
  }

  async runTest() {
    const c = this.#input();
    if (!c.username || !c.password.trim() || (this.serverFields && !c.host)) {
      this.#fail('Enter your email and the app-specific password first.');
      return;
    }
    const run = ++this.#run;
    this.test = 'testing';
    this.cert = undefined;
    try {
      const r = await accountsApi.test(c);
      if (run !== this.#run) return;
      this.result = r;
      this.test = 'ok';
    } catch (e) {
      if (!(e instanceof ApiError)) throw e;
      if (run !== this.#run) return;
      this.#refused(e);
    }
  }

  /** Accepts the certificate shown and tests again with it. */
  async acceptCert() {
    if (!this.cert) return;
    this.certFingerprint = this.cert.fingerprint;
    this.cert = undefined;
    await this.runTest();
  }

  /**
   * Posts the step of the module's sign-in form being asked for. The module answers with the next step, the same
   * one with a sentence to show, or the mailbox it connected, when the wizard goes on at Rules for it. What was
   * typed is cleared once posted, whatever the answer.
   */
  async submitForm() {
    const url = this.preset?.form_url;
    if (!url || this.busy) return;
    const input: FormInput = { step: this.formStep };
    if (this.#formState) input.state = this.#formState;
    if (this.formStep === 'sign_in') [input.username, input.password] = [this.email.trim(), this.password];
    else if (this.formStep === 'code') input.code = this.code.trim();
    else input.password = this.mailboxPassword;
    const empty = this.formStep === 'sign_in' ? !input.username || !input.password : !(input.code || input.password);
    if (empty) {
      this.formMessage = { sign_in: 'Enter your address and password first.', code: 'Enter the code first.', mailbox_password: 'Enter your mailbox password first.' }[this.formStep];
      return;
    }
    this.busy = true;
    try {
      const a = await accountsApi.formStep(url, input);
      if (a.account_id) {
        [this.accountId, this.test, this.step] = [a.account_id, 'ok', 2];
        await load();
        return;
      }
      [this.formStep, this.#formState, this.formMessage] = [a.step ?? 'sign_in', a.state ?? '', a.message ?? ''];
    } catch (e) {
      if (!(e instanceof ApiError)) throw e;
      this.formMessage = e.message;
    } finally {
      [this.password, this.code, this.mailboxPassword, this.busy] = ['', '', '', false];
    }
  }

  /**
   * Moves on one step; on the last one saves the mailbox. Resolves true once it is saved. A one-click tile
   * leaves for the provider's sign-in instead, and the wizard comes back at Rules (`accountId`); a form tile's
   * Sign in step posts the form, and goes on at Rules once the module has connected the mailbox.
   */
  async next(): Promise<boolean> {
    if (this.step === 0 && !this.preset) return false;
    if (this.step === 0 && this.via === 'one_click' && this.preset?.one_click_url) {
      startOneClick(this.preset.one_click_url, this.from);
      return false;
    }
    if (this.step === 1 && this.via === 'form') {
      await this.submitForm();
      return false;
    }
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
    if (this.accountId) {
      try {
        await this.#saveRules(accounts.list.find((a) => a.id === this.accountId)?.label ?? 'Your mailbox');
        return true;
      } finally {
        this.busy = false;
      }
    }
    try {
      const a = await connect(this.#input()).finally(() => (this.password = ''));
      await this.#saveRules(a.label);
      return true;
    } catch (e) {
      if (!(e instanceof ApiError)) throw e;
      // Refused (already connected, the server stopped accepting the login, or it presents another certificate): back to the form.
      this.#refused(e);
      this.step = 1;
      return false;
    } finally {
      this.busy = false;
    }
  }

  /** Saves the starter rules left on. The mailbox is connected already, whatever happens to them. */
  async #saveRules(label: string) {
    const names = this.templates.filter((t) => t.on).map((t) => t.name);
    try {
      const n = names.length ? await addTemplatesByName(names) : 0;
      if (n) flash(`${label} is connected with ${n} new ${n === 1 ? 'rule' : 'rules'}`);
      else if (this.accountId) flash(label + ' is connected');
    } catch (e) {
      flash(e instanceof Error ? e.message : String(e));
    }
  }
}
