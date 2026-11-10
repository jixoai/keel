// Fixture · the collapsed-sidebar affordance on the terminal host.
//
// Mounts the shell exactly the way a screen does (WorkspaceShell with a sidebar
// and a main column, the collapse flag owned by the fixture's own header
// button), so `sidebar-collapse-affordance.test.tsx` can assert the one thing
// that experiment (dbg10) got wrong: in a bare `Sidebar` inside a panel group
// there is no collapse entry — the entry is the shell's `SidebarCollapseStrip`,
// which the shell draws on its own left edge. This file defines nothing the kit
// exports; it exists for that test only.
import { mount, signal, Text, View } from "nsh";
import { WorkspaceShell } from "./workspace-shell";

const collapsed = signal(false);

function CollapseButton() {
  return (
    <View
      testID="fixture-collapse"
      focusable
      role="button"
      accessibilityLabel="收起侧栏"
      onPress={() => collapsed.set(true)}
      style={{ padding: 4 }}
    >
      <Text>收起</Text>
    </View>
  );
}

function Fixture() {
  return (
    <WorkspaceShell
      testID="fixture-shell"
      sidebarCollapsed={collapsed()}
      onSidebarCollapsedChange={(next) => collapsed.set(next)}
      header={<CollapseButton />}
      sidebar={
        <Text testID="fixture-sidebar-content">侧栏内容</Text>
      }
      main={
        <Text testID="fixture-main-content">主区内容</Text>
      }
    />
  );
}

mount(Fixture);
