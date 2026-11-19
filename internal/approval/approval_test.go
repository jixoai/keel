package approval

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

var (
	testTenant   = uuid.MustParse("11111111-1111-1111-1111-111111111111")
	testSession  = uuid.MustParse("22222222-2222-2222-2222-222222222222")
	testDecision = uuid.MustParse("33333333-3333-3333-3333-333333333333")
)

func testService(t *testing.T) (*Service, *time.Time) {
	t.Helper()
	now := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	clock := now
	return NewService(Options{
		Store: NewMemoryStore(),
		Now:   func() time.Time { return clock },
	}), &clock
}

func testRecord() Record {
	return Record{
		TenantID:   testTenant.String(),
		SessionID:  testSession.String(),
		DecisionID: testDecision.String(),
		Kind:       KindChoice,
		Mode:       ModeChoice,
	}
}

func TestComputeValidatedAndEscape(t *testing.T) {
	cases := []struct {
		name      string
		mode      string
		options   []string
		optionID  string
		text      string
		validated bool
		escape    bool
	}{
		{"confirm declared option", ModeConfirm, []string{"yes", "no"}, "yes", "", true, false},
		{"confirm unknown option", ModeConfirm, []string{"yes", "no"}, "maybe", "", false, false},
		{"prompt free text", ModePrompt, nil, "", "ship it", true, false},
		{"choice option", ModeChoice, []string{"a", "b"}, "b", "", true, false},
		{"choice other escape", ModeChoice, []string{"a", "b"}, "", "something else", false, true},
		{"empty answer", ModeChoice, []string{"a"}, "", "", false, false},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			validated, escape := Compute(test.mode, test.options, test.optionID, test.text)
			if validated != test.validated || escape != test.escape {
				t.Fatalf("Compute = validated=%v escape=%v, want %v/%v",
					validated, escape, test.validated, test.escape)
			}
		})
	}
}

func TestOpenAndAnswerAreIdempotent(t *testing.T) {
	ctx := context.Background()
	service, _ := testService(t)
	opened, created, err := service.Open(ctx, testRecord())
	if err != nil || !created {
		t.Fatalf("open: created=%v err=%v", created, err)
	}
	if opened.Status != StatusOpen || opened.OpenedAt == nil || opened.DeliveredAt == nil {
		t.Fatalf("opened = %+v", opened)
	}
	again, created, err := service.Open(ctx, testRecord())
	if err != nil || created || again.Status != StatusOpen {
		t.Fatalf("second open: created=%v err=%v row=%+v", created, err, again)
	}

	answer := testRecord()
	answer.OptionID = "a"
	answer.Text = ""
	answer.Validated = true
	answer.Actor = "owner@example.test"
	answered, duplicate, err := service.RecordAnswer(ctx, answer)
	if err != nil || duplicate {
		t.Fatalf("answer: duplicate=%v err=%v", duplicate, err)
	}
	if answered.Status != StatusAnswered || !answered.Validated || answered.AnsweredAt == nil {
		t.Fatalf("answered = %+v", answered)
	}
	same, duplicate, err := service.RecordAnswer(ctx, answer)
	if err != nil || !duplicate {
		t.Fatalf("duplicate answer: duplicate=%v err=%v", duplicate, err)
	}
	if same.Actor != "owner@example.test" {
		t.Fatalf("duplicate answer lost the actor: %+v", same)
	}

	conflict := answer
	conflict.OptionID = "b"
	if _, _, err := service.RecordAnswer(ctx, conflict); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflicting answer = %v, want ErrConflict", err)
	}
}

func TestUnknownActorCanBeUpgradedButNeverDowngraded(t *testing.T) {
	ctx := context.Background()
	service, _ := testService(t)
	answer := testRecord()
	answer.OptionID = "a"
	answer.Actor = UnknownActor
	answer.Channel = ChannelUnknown
	first, _, err := service.RecordAnswer(ctx, answer)
	if err != nil {
		t.Fatal(err)
	}
	if first.Actor != UnknownActor {
		t.Fatalf("first actor = %q", first.Actor)
	}
	upgrade := answer
	upgrade.Actor = "owner@example.test"
	upgrade.Channel = ChannelHTTP
	upgraded, duplicate, err := service.RecordAnswer(ctx, upgrade)
	if err != nil || !duplicate {
		t.Fatalf("upgrade: duplicate=%v err=%v", duplicate, err)
	}
	if upgraded.Actor != "owner@example.test" || upgraded.Channel != ChannelHTTP {
		t.Fatalf("upgraded = %+v", upgraded)
	}
	// A later weaker record never overwrites the known actor.
	downgrade := answer
	downgrade.Actor = UnknownActor
	final, duplicate, err := service.RecordAnswer(ctx, downgrade)
	if err != nil || !duplicate {
		t.Fatalf("downgrade: duplicate=%v err=%v", duplicate, err)
	}
	if final.Actor != "owner@example.test" {
		t.Fatalf("actor was downgraded: %+v", final)
	}
}

func TestTimeoutIsTerminalAndCannotBeAnswered(t *testing.T) {
	ctx := context.Background()
	service, _ := testService(t)
	if _, _, err := service.Open(ctx, testRecord()); err != nil {
		t.Fatal(err)
	}
	timeout := testRecord()
	timeout.Status = StatusTimedOut
	timedOut, duplicate, err := service.RecordOutcome(ctx, timeout)
	if err != nil || duplicate {
		t.Fatalf("timeout: duplicate=%v err=%v", duplicate, err)
	}
	if timedOut.Status != StatusTimedOut || timedOut.Channel != ChannelTimeout {
		t.Fatalf("timeout row = %+v", timedOut)
	}
	again, duplicate, err := service.RecordOutcome(ctx, timeout)
	if err != nil || !duplicate || again.Status != StatusTimedOut {
		t.Fatalf("timeout replay: duplicate=%v err=%v", duplicate, err)
	}
	answer := testRecord()
	answer.OptionID = "yes"
	if _, _, err := service.RecordAnswer(ctx, answer); !errors.Is(err, ErrClosed) {
		t.Fatalf("answer after timeout = %v, want ErrClosed", err)
	}
}

func TestCancelledOutcomeCannotOverwriteAnswer(t *testing.T) {
	ctx := context.Background()
	service, _ := testService(t)
	answer := testRecord()
	answer.OptionID = "yes"
	if _, _, err := service.RecordAnswer(ctx, answer); err != nil {
		t.Fatal(err)
	}
	cancel := testRecord()
	cancel.Status = StatusCancelled
	if _, _, err := service.RecordOutcome(ctx, cancel); !errors.Is(err, ErrClosed) {
		t.Fatalf("cancel after answer = %v, want ErrClosed", err)
	}
}

func TestRecordsAreTenantScoped(t *testing.T) {
	ctx := context.Background()
	service, _ := testService(t)
	if _, _, err := service.Open(ctx, testRecord()); err != nil {
		t.Fatal(err)
	}
	otherTenant := uuid.New()
	if _, found, err := service.Get(ctx, otherTenant, testSession, testDecision); err != nil || found {
		t.Fatalf("foreign tenant read = found=%v err=%v", found, err)
	}
	if _, found, err := service.Get(ctx, testTenant, testSession, testDecision); err != nil || !found {
		t.Fatalf("own read = found=%v err=%v", found, err)
	}
	rows, err := service.List(ctx, otherTenant, testSession)
	if err != nil || len(rows) != 0 {
		t.Fatalf("foreign tenant list = %v err=%v", rows, err)
	}
}

func TestBackfillIsHonestAboutMissingContext(t *testing.T) {
	opened := time.Date(2026, 11, 19, 9, 59, 0, 0, time.UTC)
	record := Backfill(KindChoice, []string{"a", "b"}, "", "other", "", "", &opened, opened.Add(time.Minute))
	if record.Actor != UnknownActor || record.Channel != ChannelUnknown {
		t.Fatalf("backfill context = actor %q channel %q", record.Actor, record.Channel)
	}
	if record.Validated || !record.Escape {
		t.Fatalf("backfill validation = validated=%v escape=%v", record.Validated, record.Escape)
	}
}
