// The system bar's sound preference and its one cue — the shell IA change.
//
// The bar owns a fast mute/unmute and a small settings face; both write the
// same persisted preference. The cue is played when an approval question
// arrives (`store.ts` calls `playApprovalCue` next to the host notification),
// never for ordinary run output: a working session must not ring on every
// turn.
//
// NSH's `Sound` plays a short decoded sound. The cue is synthesised here into
// a tiny WAV data URL, so the client ships no binary asset and the sound is
// exactly one product fact. Where the host has no sound (the terminal, a
// browser that refuses audio), `Sound.available()` is the honest answer and
// every call degrades to silence.
import { Sound, signal, Storage } from "nsh";

export type SoundVolume = "low" | "medium" | "high";

export interface SoundPreferences {
  /** Whether an approval arrival plays the cue. */
  enabled: boolean;
  /** How loud the cue is. */
  volume: SoundVolume;
}

/** The storage key, one per device (a reading preference, not tenant data). */
export const SOUND_STORAGE_KEY = "keel:sound:preferences";

const DEFAULTS: SoundPreferences = { enabled: true, volume: "medium" };

const VOLUME_VALUE: Record<SoundVolume, number> = { low: 0.35, medium: 0.65, high: 1 };

const preferences = signal<SoundPreferences>(DEFAULTS);

/** Whether the host can play a sound; `null` until the first probe answers. */
const available = signal<boolean | null>(null);

/** The preference the system bar and the sound dialog both read. */
export const soundPreferences = () => preferences();

/** The host answer: `false` means every cue is silent here. */
export const soundAvailable = () => available();

let touched = false;

const persist = () => {
  void Storage.setItem(SOUND_STORAGE_KEY, JSON.stringify(preferences())).catch(() => {
    /* a host without storage just forgets the choice */
  });
};

export function setSoundEnabled(enabled: boolean): void {
  touched = true;
  preferences.update((current) => ({ ...current, enabled }));
  persist();
}

export function setSoundVolume(volume: SoundVolume): void {
  touched = true;
  preferences.update((current) => ({ ...current, volume }));
  persist();
}

/** Boot: the stored preference (the defaults when nothing is stored), then
 * the host's sound answer. A choice made before the read lands wins. */
export async function restoreSoundPreferences(): Promise<void> {
  let stored: string | null = null;
  try {
    stored = await Storage.getItem(SOUND_STORAGE_KEY);
  } catch {
    /* no storage: the defaults stand */
  }
  if (!touched && stored) {
    try {
      const parsed = JSON.parse(stored) as Partial<SoundPreferences>;
      const enabled = typeof parsed.enabled === "boolean" ? parsed.enabled : DEFAULTS.enabled;
      const volume =
        parsed.volume === "low" || parsed.volume === "medium" || parsed.volume === "high"
          ? parsed.volume
          : DEFAULTS.volume;
      preferences.set({ enabled, volume });
    } catch {
      /* a malformed value is not a preference: the defaults stand */
    }
  }
  try {
    available.set(await Sound.available());
  } catch {
    available.set(false);
  }
}

// ---- the cue --------------------------------------------------------------

const CUE_RATE = 8_000;
const CUE_SECONDS = 0.14;

/** A two-note rising cue (G5 → C6), 8 kHz mono 16-bit: small enough to build
 * at boot and long enough to be heard. */
const cueSource = (): string => {
  const frames = Math.floor(CUE_RATE * CUE_SECONDS);
  const bytes = new Uint8Array(44 + frames * 2);
  const view = new DataView(bytes.buffer);
  const ascii = (offset: number, text: string) => {
    for (let index = 0; index < text.length; index += 1) view.setUint8(offset + index, text.charCodeAt(index));
  };
  ascii(0, "RIFF");
  view.setUint32(4, 36 + frames * 2, true);
  ascii(8, "WAVE");
  ascii(12, "fmt ");
  view.setUint32(16, 16, true);
  view.setUint16(20, 1, true); // PCM
  view.setUint16(22, 1, true); // mono
  view.setUint32(24, CUE_RATE, true);
  view.setUint32(28, CUE_RATE * 2, true);
  view.setUint16(32, 2, true);
  view.setUint16(34, 16, true);
  ascii(36, "data");
  view.setUint32(40, frames * 2, true);
  const half = CUE_SECONDS / 2;
  for (let index = 0; index < frames; index += 1) {
    const seconds = index / CUE_RATE;
    const frequency = seconds < half ? 784 : 1046.5;
    const attack = Math.min(1, index / (CUE_RATE * 0.01));
    const release = Math.max(0, 1 - index / frames);
    const sample = Math.sin(2 * Math.PI * frequency * seconds) * attack * release * 0.45;
    view.setInt16(44 + index * 2, Math.round(sample * 32767), true);
  }
  let binary = "";
  for (let index = 0; index < bytes.length; index += 1) binary += String.fromCharCode(bytes[index] ?? 0);
  return `data:audio/wav;base64,${btoa(binary)}`;
};

let cue: Promise<Sound> | null = null;

/** Play the cue now, whether or not the preference is on (the dialog's
 * 试听). Resolves with whether a sound actually played. */
export async function previewSoundCue(): Promise<boolean> {
  return playCue(true);
}

/** The approval arrival: honours the preference. Best effort — a host that
 * refuses audio is silent, never an error the session has to handle. */
export async function playApprovalCue(): Promise<boolean> {
  return playCue(false);
}

async function playCue(force: boolean): Promise<boolean> {
  if (!force && !preferences().enabled) return false;
  try {
    const canPlay = await Sound.available();
    available.set(canPlay);
    if (!canPlay) return false;
    cue ??= Sound.load(cueSource());
    const sound = await cue;
    sound.play({ volume: VOLUME_VALUE[preferences().volume] });
    return true;
  } catch {
    /* a decode/autoplay refusal frees the cache so a later gesture can retry */
    cue = null;
    return false;
  }
}
