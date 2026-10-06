// Dictation through the browser's Web Speech API. TypeScript's DOM types cover the
// results but not the recognizer, so the part used here is declared by hand.
interface Recognizer {
  continuous: boolean;
  interimResults: boolean;
  onresult: ((e: { results: ArrayLike<ArrayLike<{ transcript: string }>> }) => void) | null;
  onerror: ((e: { error: string }) => void) | null;
  onend: (() => void) | null;
  start(): void;
  stop(): void;
}

const recognizer = () => {
  const w = window as unknown as Record<string, (new () => Recognizer) | undefined>;
  return w.SpeechRecognition ?? w.webkitSpeechRecognition;
};

export const supported = () => !!recognizer();

/**
 * Starts listening. `onText` gets everything heard so far each time it grows; `onEnd` runs
 * once when listening stops, with the browser's error code if it stopped on one.
 * Returns the function that stops it, or null when the browser has no speech recognition.
 */
export function listen(onText: (text: string) => void, onEnd: (error?: string) => void) {
  const Recognizer = recognizer();
  if (!Recognizer) return null;
  const r = new Recognizer();
  let error: string | undefined;
  r.continuous = true;
  r.interimResults = true;
  r.onresult = (e) => onText(Array.from(e.results, (alt) => alt[0].transcript).join(''));
  r.onerror = (e) => (error = e.error);
  r.onend = () => onEnd(error);
  r.start();
  return () => r.stop();
}
