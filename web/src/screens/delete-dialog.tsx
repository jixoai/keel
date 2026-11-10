// The one confirmation the app asks: a page raises a question, the kit's
// `AlertDialog` draws it. This file owns no chrome — the scrim, the dialog's
// width, the z-index, the icon block and the two button faces are all the kit's
// (`web/src/ui/kit/components/alert-dialog.tsx`: its own blur scrim, its
// `width 400 / maxWidth 92%`, its alert glyph, its Button pair — the cancel an
// `outline`, the confirm `destructive` or `default`). What this file owns is the
// *question*: a title, a description, the two labels and the one action.
//
// `ConfirmRequest` is deliberately generic: a question is a title, a
// description, the two labels (the kit's defaults when absent) and the one
// action the confirm runs. The dialog shares no business logic between
// questions — it closes the question first and then runs that request's single
// `onConfirm`, so every caller keeps its own.
//
// Raised through it today: the session delete (`requestDelete(sessionId,
// title)` — this file's own instance, the one that knows which session a row's
// delete entry means; it exposes that id as `pendingDeleteId`, which has no
// consumer yet). The two settings questions — the model reset and the secret
// delete — still raise their own `AlertDialog`s in `screens/settings.tsx`;
// moving them here is a `requestConfirm(request)` call each, which this file
// takes unchanged.
//
// Mounted once, at the app root: `<DeleteDialog />`. That replaces the app
// root's own copy (`DeleteConfirm` in `App.tsx`) together with the
// `deleteRequest` shim at the bottom of this file.
import { signal } from "nsh";
import { store } from "../data/store";
import { AlertDialog } from "../ui/kit";

/** One confirmation: the question, its explanation, the two labels (the kit's
 * defaults when absent) and the single action the confirm runs. */
export type ConfirmRequest = {
  title: string;
  description: string;
  confirmLabel?: string;
  cancelLabel?: string;
  destructive: boolean;
  onConfirm: () => void;
};

/** The question a confirmation is showing, if any — the one source of truth
 * for the dialog. Every closer (confirm, cancel, Escape, backdrop) clears it. */
export const confirmRequest = signal<ConfirmRequest | null>(null);

/** The session a delete question is about, if any. Written by `requestDelete`
 * only, so every other question leaves it null. */
const sessionUnderConfirm = signal<{ sessionId: string; title: string } | null>(null);

/** The session a delete dialog is waiting on, if any — exported for a list that
 * wants to mark that row. Nothing reads it yet (the delete path today is
 * `requestDelete` → the app root's interim `AlertDialog`). */
export const pendingDeleteId = () => sessionUnderConfirm()?.sessionId ?? null;

/** Ask the one confirmation. Each page brings its own action and its own
 * business: the dialog never knows what a confirm *does*, only that it closes
 * and then runs it. */
export const requestConfirm = (request: ConfirmRequest) => {
  sessionUnderConfirm.set(null);
  confirmRequest.set(request);
};

/** Dismiss without acting: the cancel button, Escape, the backdrop. */
export const cancelConfirm = () => {
  sessionUnderConfirm.set(null);
  confirmRequest.set(null);
};

/** The session delete instance — a list row's delete entry. The question names
 * the session; the action is this file's own. */
export const requestDelete = (sessionId: string, title: string) => {
  sessionUnderConfirm.set({ sessionId, title });
  confirmRequest.set({
    title: "删除会话？",
    description: `「${title}」及其消息将被删除，此操作不可撤销。`,
    confirmLabel: "删除",
    cancelLabel: "取消",
    destructive: true,
    onConfirm: () => {
      void store.deleteSession(sessionId);
    },
  });
};

/** The one confirmation: the kit's AlertDialog, filled from the request the
 * current page raised. `cancelLabel` defaults to the kit's 取消 — a question
 * with a destructive answer always keeps a way out. */
export function DeleteDialog() {
  return (
    <AlertDialog
      testID="confirm-dialog"
      open={confirmRequest() !== null}
      title={confirmRequest()?.title}
      description={confirmRequest()?.description}
      confirmLabel={confirmRequest()?.confirmLabel}
      cancelLabel={confirmRequest()?.cancelLabel ?? "取消"}
      destructive={confirmRequest()?.destructive ?? false}
      onConfirm={() => {
        // Close first, then act: the question is answered before its action
        // runs, so an action that raises the next question (or fails) cannot
        // land behind a dialog that is still the previous one.
        const request = confirmRequest();
        cancelConfirm();
        request?.onConfirm();
      }}
      onCancel={cancelConfirm}
    />
  );
}

/** Interim compatibility with the app root's own copy of this dialog
 * (`DeleteConfirm` in `App.tsx` reads the pending delete through these): the
 * read is the pending session delete, the write is only the close. Mounting
 * `<DeleteDialog />` at the root replaces that copy; delete it and this export
 * together, and the request above stays the only source. Its `set(null)` clears
 * the request too, so a dialog the root dismissed cannot reopen from a stale
 * question once `DeleteDialog` is the one on screen. */
export const deleteRequest: {
  (): { sessionId: string; title: string } | null;
  set: (value: null) => void;
} = Object.assign(() => sessionUnderConfirm(), {
  set: (value: null) => {
    if (value !== null) return;
    cancelConfirm();
  },
});
