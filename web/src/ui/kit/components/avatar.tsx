// C1 · Avatar — shadcn/ui `Avatar` (image + initials fallback), on NSH's native
// `nsh/base` Avatar: the image's loading state lives in the native component, so
// the fallback shows while loading, on error, and when no `src` is given.
//
// Props
// | prop | type | default | notes |
// |---|---|---|---|
// | src | string? | – | URL / `asset:` / `data:`; omitted renders the fallback |
// | name | string? | – | what screen readers say, and the initials source ("Ada Lovelace" → AL) |
// | size | "sm" \| "default" \| "lg" \| number | "default" | 24 / 32 / 40 px (a number wins) |
// | fallback | JSX.Element? | initials | icon or placeholder in place of the initials |
// | accessibilityLabel | string? | name | "" makes it decorative |
// | children | JSX.Element? | – | presence dot / badge over the picture |
// | testID | string? | – | `id:` selector for tests |
//
// States: loaded (image) · loading/error/empty (fallback: muted face + initials
// or `fallback`). TUI: the fallback initials render as the avatar's text.
import type { JSX, StyleProp, ViewStyle } from "nsh";
import { Text } from "nsh";
import { Avatar as NativeAvatar } from "nsh/base";
import { color, radius, typeScale, weight } from "../tokens";

export type AvatarSize = "sm" | "default" | "lg" | number;

const avatarPx = (size: AvatarSize): number => (typeof size === "number" ? size : size === "sm" ? 24 : size === "lg" ? 40 : 32);
const avatarText = (size: AvatarSize): number => {
  const px = avatarPx(size);
  return px <= 24 ? typeScale["2xs"] : px <= 32 ? typeScale.sm : typeScale.base;
};

export function Avatar(props: {
  src?: string | null;
  name?: string | null;
  size?: AvatarSize;
  fallback?: JSX.Element;
  accessibilityLabel?: string;
  children?: JSX.Element;
  testID?: string;
  style?: StyleProp<ViewStyle>;
}) {
  const px = () => avatarPx(props.size ?? "default");
  const initials = () => (props.name ? initialsOf(props.name) : "");
  return (
    <NativeAvatar
      testID={props.testID}
      src={props.src ?? null}
      name={props.name ?? null}
      accessibilityLabel={props.accessibilityLabel ?? props.name ?? null}
      fallback={
        props.fallback ?? (
          <Text style={{ fontSize: avatarText(props.size ?? "default"), fontWeight: weight.medium, color: color.mutedForeground }}>
            {initials()}
          </Text>
        )
      }
      imageStyle={{ borderRadius: radius.pill }}
      fallbackStyle={{
        borderRadius: radius.pill,
        backgroundColor: color.muted,
        alignItems: "center",
        justifyContent: "center",
      }}
      style={[{ width: px(), height: px(), borderRadius: radius.pill }, props.style ?? null]}
    >
      {props.children}
    </NativeAvatar>
  );
}

/** The same rule NSH's native fallback uses: first letters of the first two
 * words, upper case ("Ada Lovelace" → AL, "keel" → KE). */
export const initialsOf = (name: string): string => {
  const words = name.trim().split(/\s+/).filter(Boolean);
  if (words.length === 0) return "";
  if (words.length === 1) return [...words[0]].slice(0, 2).join("").toUpperCase();
  return `${[...words[0]][0] ?? ""}${[...words[1]][0] ?? ""}`.toUpperCase();
};
