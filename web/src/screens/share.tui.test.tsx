// TUI verification for the A6 share surface — the session header dialog, the
// Bot detail share action, the TeamBot visibility copy and the session list's
// source filter. The fixture transport carries real in-memory tuples, so the
// add/revoke transitions these tests drive are the same calls the live
// transport makes.
import { expect, open, test } from "nsh/test";

const app = (width = 1440) => open("../../App.tsx", { width, host: { params: { fixture: true } } });

test("TUI: 会话共享——共享给我筛选、所有者门、添加与撤销", async () => {
  const view = await app();
  await view.waitFor("id:session-list");
  await view.waitFor("text:发布准备周报");

  // 共享给我: only the session 陈墨 shared with 李娜, with the attribution line.
  await view.press("id:session-filter-shared");
  await view.waitFor("text:/由 陈墨 共享/");
  const shared = view.texts().join("\n");
  expect(shared).toContain("由 陈墨 共享");
  expect(shared).not.toContain("发布审批");

  // A shared-with-me session has no share action: 李娜 is not its owner.
  await view.press("label:打开会话 发布准备周报");
  await view.waitFor("id:workspace-header-title");
  expect(view.shows("id:workspace-header-share")).toBe(false);

  // 我的: back to the reader's own sessions (the list stays in the sidebar at
  // this width).
  await view.press("id:session-filter-mine");
  await view.waitFor("text:发布审批");
  expect(view.texts().join("\n")).not.toContain("由 陈墨 共享");

  // The owner's header offers 共享.
  await view.press("label:打开会话 发布审批");
  await view.waitFor("id:workspace-header-share");
  await view.press("id:workspace-header-share");
  await view.waitFor("id:share-dialog");
  expect(view.texts().join("\n")).toContain("还没有共享给任何人");

  // Add 陈墨 at 可查看 (the account directory is readable for this admin).
  await view.press("label:选择共享对象");
  await view.press("text:陈墨（chenmo）");
  await view.press("id:share-add");
  await view.waitFor("text:已共享。");
  expect(view.texts()).toContain("陈墨");

  // Revoke through the destructive confirmation; the list updates immediately.
  await view.press("label:撤销对 陈墨 的共享");
  await view.waitFor("id:share-revoke-dialog");
  await view.press("id:share-revoke-dialog-confirm");
  await view.waitFor("text:/还没有共享给任何人/");
  await view.press("id:share-dialog-close");
});

test("TUI: Bot 共享——已有共享、添加权限组、撤销与已共享徽标", async () => {
  const view = await app();
  await view.press("id:books-chapter-bots");
  await view.waitFor("id:bots-head");
  await view.press("label:打开 Bot 客服一号");
  await view.waitFor("id:bot-capabilities");
  await view.press("label:更多 Bot 操作");
  await view.press("text:共享 Bot");
  await view.waitFor("id:share-dialog");

  const opened = view.texts().join("\n");
  expect(opened).toContain("已共享给");
  expect(opened).toContain("陈墨");

  // Add the 客服组 permission group at 可查看.
  await view.press("label:共享给账号还是权限组");
  await view.press("text:权限组");
  await view.press("label:选择共享对象");
  await view.press("text:客服组");
  await view.press("id:share-add");
  await view.waitFor("text:已共享。");
  expect(view.texts()).toContain("客服组");

  // Revoke it: the seeded account share stays.
  await view.press("label:撤销对 客服组 的共享");
  await view.waitFor("id:share-revoke-dialog");
  await view.press("id:share-revoke-dialog-confirm");
  await view.waitFor("text:已撤销共享。");
  expect(view.texts()).toContain("陈墨");

  await view.press("id:share-dialog-close");
  await view.waitFor("id:bot-capabilities");
  expect(view.texts().join("\n")).toContain("已共享 1");
});

test("TUI: 团队共享——只影响可见性的文案与已有权限组共享", async () => {
  const view = await app();
  await view.press("id:books-chapter-teams");
  await view.waitFor("id:teams-head");
  await view.press("label:打开团队 客服团队");
  await view.waitFor("id:team-page");
  await view.press("label:更多团队操作");
  await view.press("text:共享团队");
  await view.waitFor("id:share-dialog");

  const screen = view.texts().join("\n");
  expect(screen).toContain("共享只影响可见性");
  expect(screen).toContain("启动和修改仍然需要管理权");
  expect(screen).toContain("客服组");
  await view.press("id:share-dialog-close");
});
