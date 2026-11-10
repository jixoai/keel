// The app's appearance, one source: the root applies the stored choice at boot
// and the settings page's 外观 section writes it through `applyAppearance`, so
// the two always agree (they read the same `appearancePreference()`).
//
// `setColorScheme` is nsh's own (`nsh.json`'s `appearance` is the start): the
// color scheme, the components' theme and the platform's controls all follow it
// at once.
import { setColorScheme, signal, Storage } from "nsh";

export type Appearance = "light" | "dark" | "system";

/** The key the choice is kept under. */
export const APPEARANCE_STORAGE_KEY = "keel:appearance";

const appearance = signal<Appearance>("system");

/** The appearance in force (reactive). */
export const appearancePreference = () => appearance();

/** Make `scheme` the app's appearance — now and on the next launch. */
export function applyAppearance(scheme: Appearance) {
  appearance.set(scheme);
  setColorScheme(scheme);
  void Storage.setItem(APPEARANCE_STORAGE_KEY, scheme).catch(() => {
    /* a host without storage just forgets the choice */
  });
}

/** Boot: apply what the last session chose (`system` when nothing is stored). */
export async function restoreAppearance() {
  let stored: string | null = null;
  try {
    stored = await Storage.getItem(APPEARANCE_STORAGE_KEY);
  } catch {
    /* a host without storage: the system appearance stands */
  }
  const scheme: Appearance = stored === "light" || stored === "dark" || stored === "system" ? stored : "system";
  appearance.set(scheme);
  setColorScheme(scheme);
}
