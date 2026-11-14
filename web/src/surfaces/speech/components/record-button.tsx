// The STT workbench's one recorder control.
//
// A press starts a real NSH `Speech` session; while it listens the same control
// is the stop button, and stopping releases the microphone. The button reads
// its state through `listening()` / `blocked()` inside JSX, so the label follows
// the session without remounting anything outside this control.
import type { JSX } from "nsh";
import { Button, space } from "../../../ui/kit";

export function RecordButton(props: {
  /** The recognizer is listening now. */
  listening: () => boolean;
  /** The microphone is denied or absent: the same press retries it. */
  blocked?: () => boolean;
  /** Starts, stops or retries according to the current state. */
  onPress: () => void;
  testID?: string;
  stopTestID?: string;
}): JSX.Element {
  const recordStyle = { paddingHorizontal: space.xl, minHeight: space["3xl"] + space.sm };
  return (
    <If
      when={props.listening()}
      else={
        <Button
          testID={props.testID ?? "speech-record"}
          variant={props.blocked?.() ? "secondary" : "default"}
          size="lg"
          icon={props.blocked?.() ? "refresh" : "message"}
          label={props.blocked?.() ? "重试麦克风" : "按下说话"}
          accessibilityLabel={props.blocked?.() ? "重试麦克风" : "按下说话开始录音"}
          onPress={() => props.onPress()}
          style={recordStyle}
        />
      }
    >
      <Button
        testID={props.stopTestID ?? "speech-stop"}
        variant="destructive"
        size="lg"
        icon="stop"
        label="停止并释放"
        accessibilityLabel="停止录音并释放麦克风"
        onPress={() => props.onPress()}
        style={recordStyle}
      />
    </If>
  );
}
