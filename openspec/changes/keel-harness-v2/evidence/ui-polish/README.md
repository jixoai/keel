# UI polish evidence

Browser evidence for the content-surface pass in `task/keel-ui-polish`: accounts,
permission groups, bots, teams, records and the settings forms/lists, plus the
shared kit pieces they use. The system bar and shell navigation are owned by
`task/keel-system-bar` and are unchanged here.

## Layout

- `before/` — the rebased lane tip before this pass (`0766a7b`, the records
  filter one-off on the system-bar tip `733be2b`), driven by the same script.
- `after/` — the final build of this lane.
- Each directory has `report.json` and one PNG per page/viewport. There is no
  hand-written HTML report; `report.json` is the machine-readable one.

## Reproduce

```bash
# after (builds the current worktree)
bash scripts/ui-polish-evidence.sh

# before (same script against the preserved baseline build)
UI_POLISH_EVIDENCE=openspec/changes/keel-harness-v2/evidence/ui-polish/before \
UI_POLISH_SKIP_BUILD=1 UI_POLISH_BUILD=/tmp/keel-ui-polish-before \
bash scripts/ui-polish-evidence.sh
```

The script packages `web/` with `nsh package --target web`, serves it on a
loopback port from the project's range, and drives Chrome through
`playwright-core`. The fixture transport (`?fixture=1`) is the only launch
parameter; no credential is involved.

## Result

- Before: **83 / 108 checks** (25 failures: four are test ids the baseline
  predates, the rest are field heights, footer reachability and numeric
  alignment; the checks whose elements do not exist in the baseline are simply
  not counted).
- After: **124 / 124 checks**, no console errors, page errors or failed
  requests, and no horizontal overflow at 390 or 1440.

What the checks pin:

- One field height per toolbar: 34 px desktop, 44 px phone. Search, Select,
  range segment and filter stamp agree; the 19 px native-input face, the 24 px
  Select trigger and the 50 px segmented tray are gone.
- Table header 34 px and body rows 44 px on bots, teams, accounts, groups and
  records; the members/capabilities numeric columns are right-aligned.
- Dialog fields use the same 34/44 heights; footer actions share one height;
  the long provider form scrolls to its footer at 390.
- Forms keep Field label → control → hint/error spacing without overlap; the
  create-account, create-bot, create-team and provider dialogs are covered.

Views captured: workspace, bots, teams, accounts, groups, records,
settings-general/appearance/models/secrets/skills, the records filter sheet,
the four create dialogs and the provider form, at 390 × 844 and 1440 × 900,
plus accounts/groups tables at 1024.

## Known, deliberate trade-offs

- Books dialog footers stay at the kit's 28 px action height (44 on phones)
  while their fields are 34: the footer is a separate action row, and every
  action inside it is the same height. Settings form dialogs use 34/44 so the
  destructive + cancel + save row matches their fields.
- Page-level secondary actions (导出 CSV, 记录详情里的打开 Bot/复制编号) keep
  the dense 24–28 px action ladder; only page-primary actions and form fields
  move to the 34 px field height.
- The segmented tray no longer has 2 px of inner padding: its options fill the
  full 34/44 row so the touch target is exactly 44. Selected segments therefore
  reach the tray edge.
- The 1440 identity index rows are 52 px (avatar + two text lines) while table
  rows are 44 px; they are different components, and each is internally
  consistent.
