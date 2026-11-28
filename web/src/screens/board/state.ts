// The board screens' shared UI state: one open dialog, the queue's cursor row,
// the reply draft and the two sheets. Module-level so a refresh does not throw
// away text the reader typed; `resetBoardUi()` runs when the route leaves.
import { signal } from "nsh";

/** The one task dialog open. `taskId` names the row; the task itself is read
 * live from the cache so the copy never freezes a stale title. */
export type BoardDialogKind =
  | "complete"
  | "fail"
  | "block"
  | "unblock"
  | "reopen"
  | "assign"
  | "needs"
  | "cancel"
  | "release"
  | "access"
  | "shortcuts";

export interface BoardDialogState {
  kind: BoardDialogKind;
  taskId?: string;
}

export const boardDialog = signal<BoardDialogState | null>(null);
export const newTaskOpen = signal(false);
export const filterSheetOpen = signal(false);
export const shortcutHelpOpen = signal(false);
export const moreSheetOpen = signal(false);
export const replyDraft = signal("");
export const queueCursor = signal(0);
export const accessSheetOpen = signal(false);
export const syncInboxOpen = signal<string | null>(null);
export const scheduleCancelId = signal<string | null>(null);
export const watcherSheetId = signal<string | null>(null);

/** Dialog drafts. Module-level so a refresh keeps what the reader typed; the
 * dialog resets them when it opens for a new task/kind. */
export const completeResult = signal("");
export const failReason = signal("");
export const blockKind = signal("needs_input");
export const blockReason = signal("");
export const unblockNote = signal("");
export const cancelReason = signal("");
export const assignTarget = signal<string | null>(null);
export const needsQuery = signal("");
export const needsPicked = signal<string[]>([]);

/** The NewTask sheet's fields. */
export const newTaskChannel = signal("");
export const newTaskText = signal("");
export const newTaskLabelKey = signal("");
export const newTaskLabelValue = signal("");
export const newTaskFor = signal<string | null>(null);
export const newTaskClaim = signal(false);
export const newTaskLease = signal(900);
export const newTaskNeeds = signal<string[]>([]);

export function resetBoardUi(): void {
  boardDialog.set(null);
  newTaskOpen.set(false);
  filterSheetOpen.set(false);
  shortcutHelpOpen.set(false);
  moreSheetOpen.set(false);
  replyDraft.set("");
  queueCursor.set(0);
  accessSheetOpen.set(false);
  syncInboxOpen.set(null);
  scheduleCancelId.set(null);
  watcherSheetId.set(null);
  completeResult.set("");
  failReason.set("");
  blockKind.set("needs_input");
  blockReason.set("");
  unblockNote.set("");
  cancelReason.set("");
  assignTarget.set(null);
  needsQuery.set("");
  needsPicked.set([]);
  newTaskChannel.set("");
  newTaskText.set("");
  newTaskLabelKey.set("");
  newTaskLabelValue.set("");
  newTaskFor.set(null);
  newTaskClaim.set(false);
  newTaskLease.set(900);
  newTaskNeeds.set([]);
}

export const openDialog = (kind: BoardDialogKind, taskId?: string): void => {
  boardDialog.set(taskId ? { kind, taskId } : { kind });
};

export const closeDialog = (): void => boardDialog.set(null);
