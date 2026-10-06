let timer: ReturnType<typeof setTimeout> | undefined;

export const toast = $state({ text: '' });

/** Show a one-line confirmation for a few seconds. */
export function flash(text: string) {
  toast.text = text;
  clearTimeout(timer);
  timer = setTimeout(() => (toast.text = ''), 3200);
}
