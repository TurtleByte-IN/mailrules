import { cleanup, fireEvent, render, screen } from '@testing-library/svelte';
import { afterEach, expect, it, vi } from 'vitest';
import { modelChoice, modelLabel } from './text';
import ModelPicker from './ModelPicker.svelte';

afterEach(cleanup);

const select = () => screen.getByLabelText<HTMLSelectElement>('Model');
const field = () => screen.queryByLabelText<HTMLInputElement>('Model as name:model');

it.each([
  ['', '', 'Default'],
  ['jev', 'jev', 'Jev'],
  ['clef', 'clef', 'Clef'],
  ['anthropic', 'anthropic', 'Claude Haiku 4.5'],
  ['openai:gpt-4o-mini', 'openai', 'openai:gpt-4o-mini'],
  ['ollama:llama3.2', 'ollama', 'ollama:llama3.2'],
  ['anthropic:claude-sonnet-4', 'other', 'anthropic:claude-sonnet-4'],
  ['jev:v2', 'other', 'jev:v2'],
])('reads the stored model %j as the choice %j and the label %j', (model, choice, label) => {
  expect([modelChoice(model), modelLabel(model)]).toEqual([choice, label]);
});

it.each([
  ['openai:gpt-4o-mini', 'OpenAI-compatible…'],
  ['ollama:llama3.2', 'Ollama…'],
  ['anthropic:claude-sonnet-4', 'Other (name:model)…'],
])('shows the stored value %s as it is, selected, without changing it', (value, option) => {
  const onchange = vi.fn();
  render(ModelPicker, { id: 'm', value, onchange });
  expect(select().selectedOptions[0].textContent).toBe(option);
  expect(field()!.value).toBe(value);
  expect(onchange).not.toHaveBeenCalled();
});

it('saves a listed model as soon as it is picked, with no text field', async () => {
  const onchange = vi.fn();
  render(ModelPicker, { id: 'm', value: '', onchange });
  expect(field()).toBeNull();
  await fireEvent.change(select(), { target: { value: 'clef' } });
  expect(onchange).toHaveBeenCalledExactlyOnceWith('clef');
});

it.each([
  ['openai', 'openai:', 'openai:gpt-4o'],
  ['ollama', 'ollama:', 'ollama:qwen3'],
  ['other', '', 'jev:v2'],
])('asks for the model name after %s, and saves nothing until Set model', async (choice, start, typed) => {
  const onchange = vi.fn();
  render(ModelPicker, { id: 'm', value: '', onchange });
  await fireEvent.change(select(), { target: { value: choice } });
  expect(field()!.value).toBe(start);
  expect(onchange).not.toHaveBeenCalled();
  await fireEvent.input(field()!, { target: { value: ` ${typed} ` } });
  await fireEvent.click(screen.getByRole('button', { name: 'Set model' }));
  expect(onchange).toHaveBeenCalledExactlyOnceWith(typed);
});

it('keeps the choice and the typed text when the daemon refuses the value', async () => {
  const onchange = vi.fn().mockResolvedValue(undefined); // the parent shows the refusal; the stored value stays
  render(ModelPicker, { id: 'm', value: '', onchange });
  await fireEvent.change(select(), { target: { value: 'openai' } });
  await fireEvent.click(screen.getByRole('button', { name: 'Set model' }));
  expect(onchange).toHaveBeenCalledExactlyOnceWith('openai:');
  await vi.waitFor(() => expect(select().value).toBe('openai'));
  expect(field()!.value).toBe('openai:');
});

it('does not offer Set model for the value already stored, or for nothing', async () => {
  render(ModelPicker, { id: 'm', value: 'ollama:llama3.2', onchange: vi.fn() });
  const set = screen.getByRole('button', { name: 'Set model' }) as HTMLButtonElement;
  expect(set.disabled).toBe(true);
  await fireEvent.input(field()!, { target: { value: '' } });
  expect(set.disabled).toBe(true);
  await fireEvent.input(field()!, { target: { value: 'ollama:phi4' } });
  expect(set.disabled).toBe(false);
});

it('marks the controls invalid when told the model was refused', () => {
  render(ModelPicker, { id: 'm', value: 'openai:', invalid: { 'aria-invalid': true }, onchange: vi.fn() });
  expect(select().getAttribute('aria-invalid')).toBe('true');
  expect(field()!.getAttribute('aria-invalid')).toBe('true');
});
