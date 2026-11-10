// TUI harness · C3 containers — Card / ScrollArea / Collapsible / Accordion /
// Tabs / Table with app-owned state, so the TUI gate can open and close them.
import { mount, signal, Text, View } from "nsh";
import { typeScale } from "../tokens";
import {
  Accordion,
  AccordionItem,
  Button,
  Card,
  CardContent,
  CardFooter,
  CardHeader,
  Collapsible,
  CollapsibleTrigger,
  ScrollArea,
  Tab,
  Table,
  TableBody,
  TableBodyRow,
  TableHeaderRow,
  Tabs,
  TabsContent,
  Td,
  Th,
} from "../index";

const detailsOpen = signal(false);
const accordionOpen = signal<string[]>([]);
const tab = signal("overview");

function Harness() {
  return (
    <View style={{ flex: 1, flexDirection: "column", gap: 10, padding: 8 }}>
      <Card testID="card-plan">
        <CardHeader title="发布计划" description="周五上线" />
        <CardContent>
          <Text style={{ fontSize: typeScale.sm }}>内容：灰度 10% → 50% → 100%</Text>
        </CardContent>
        <CardFooter>
          <Button testID="card-confirm" label="确认计划" />
        </CardFooter>
      </Card>

      <Collapsible
        testID="collapsible-details"
        open={detailsOpen()}
        content={
          <Text testID="collapsible-body" style={{ fontSize: typeScale.sm }}>
            展开后的细节内容
          </Text>
        }
      >
        <CollapsibleTrigger
          testID="collapsible-trigger"
          label="高级选项"
          expanded={detailsOpen()}
          onPress={() => detailsOpen.set(!detailsOpen())}
        />
      </Collapsible>

      <Accordion value={accordionOpen()} onChange={(value) => accordionOpen.set(value)}>
        <AccordionItem value="faq" title="什么是 Keel？" testID="accordion-item">
          <Text style={{ fontSize: typeScale.sm }}>Keel 是任务外壳。</Text>
        </AccordionItem>
      </Accordion>

      <Tabs variant="line" accessibilityLabel="详情标签">
        <Tab testID="tab-overview" label="概览" selected={tab() === "overview"} onPress={() => tab.set("overview")} />
        <Tab testID="tab-log" label="日志" selected={tab() === "log"} onPress={() => tab.set("log")} />
      </Tabs>
      <TabsContent value="overview" current={tab()}>
        <Text testID="tab-panel-overview" style={{ fontSize: typeScale.sm }}>
          概览面板
        </Text>
      </TabsContent>
      <TabsContent value="log" current={tab()}>
        <Text testID="tab-panel-log" style={{ fontSize: typeScale.sm }}>
          日志面板
        </Text>
      </TabsContent>

      <ScrollArea testID="table-scroll" maxHeight={120}>
        <Table accessibilityLabel="运行记录">
          <TableHeaderRow>
            <Th>
              <Text style={{ fontSize: typeScale.sm }}>会话</Text>
            </Th>
            <Th>
              <Text style={{ fontSize: typeScale.sm }}>状态</Text>
            </Th>
          </TableHeaderRow>
          <TableBody>
            <TableBodyRow testID="table-row-1">
              <Td>
                <Text style={{ fontSize: typeScale.sm }}>发布周报</Text>
              </Td>
              <Td>
                <Text style={{ fontSize: typeScale.sm }}>完成</Text>
              </Td>
            </TableBodyRow>
          </TableBody>
        </Table>
      </ScrollArea>
    </View>
  );
}

mount(Harness);
