// TUI harness · speech workbench components — mounts Waveform, TranscriptRow
// and RecordButton directly so `components.test.tsx` can check each renders,
// carries its testID and reports its state without the whole surface.
import { mount, signal, Text, View } from "nsh";
import { typeScale } from "../../../ui/kit";
import { RecordButton } from "./record-button";
import { TranscriptRow } from "./transcript-row";
import { Waveform } from "./waveform";

const listening = signal(false);
const presses = signal(0);

function Harness() {
  return (
    <View style={{ flex: 1, gap: 8, padding: 8 }}>
      <Waveform testID="wave" bars={8} seed={3} />
      <TranscriptRow
        testID="row-speech"
        text={() => "一条已完成的语音转写"}
        source={() => "speech"}
        at={() => 0}
      />
      <TranscriptRow
        testID="row-text"
        text={() => "一条文字输入的转写"}
        source={() => "text"}
      />
      <RecordButton
        testID="record"
        stopTestID="stop"
        listening={() => listening()}
        onPress={() => {
          presses.update((value) => value + 1);
          listening.update((value) => !value);
        }}
      />
      <Text testID="readout" style={{ fontSize: typeScale.sm }}>
        {`presses ${presses()} · listening ${listening() ? "yes" : "no"}`}
      </Text>
    </View>
  );
}

mount(Harness);
