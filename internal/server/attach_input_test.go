package server

import (
	"bytes"
	"net"
	"slices"
	"testing"

	"github.com/punklabs-ai/falkn/internal/protocol"
)

func runningRecords(ids ...string) []protocol.SessionRecord {
	records := make([]protocol.SessionRecord, 0, len(ids))
	for index, id := range ids {
		records = append(records, protocol.SessionRecord{
			ID: id, Status: "running", CreatedAt: int64(index + 1),
		})
	}
	return records
}

func TestSwitchTargetWrapsInBothDirections(t *testing.T) {
	running := runningRecords("a", "b", "c")
	for _, testCase := range []struct {
		current string
		request sessionSwitch
		want    string
	}{
		{"a", sessionSwitch{step: 1}, "b"},
		{"c", sessionSwitch{step: 1}, "a"},
		{"a", sessionSwitch{step: -1}, "c"},
		{"b", sessionSwitch{step: -1}, "a"},
		{"a", sessionSwitch{index: 3}, "c"},
		{"a", sessionSwitch{index: 9}, ""},
		{"a", sessionSwitch{index: 0}, ""},
	} {
		if got := selectSwitchTarget(running, testCase.current, testCase.request); got != testCase.want {
			t.Fatalf("from %q %+v = %q, want %q", testCase.current, testCase.request, got, testCase.want)
		}
	}
}

func TestSwitchTargetHandlesAVanishedCurrentSession(t *testing.T) {
	running := runningRecords("a", "b")
	if got := selectSwitchTarget(running, "gone", sessionSwitch{step: 1}); got != "a" {
		t.Fatalf("stepping forward from a vanished session = %q, want the first", got)
	}
	if got := selectSwitchTarget(running, "gone", sessionSwitch{step: -1}); got != "b" {
		t.Fatalf("stepping back from a vanished session = %q, want the last", got)
	}
	if got := selectSwitchTarget(nil, "a", sessionSwitch{step: 1}); got != "" {
		t.Fatalf("switching with no sessions = %q", got)
	}
}

func TestSwitchableSessionsAreOldestFirstAndRunning(t *testing.T) {
	running := switchableSessions([]protocol.SessionRecord{
		{ID: "new", Status: "running", CreatedAt: 30},
		{ID: "stopped", Status: "stopped", CreatedAt: 5},
		{ID: "old", Status: "running", CreatedAt: 10},
		{ID: "archived", Status: "running", CreatedAt: 1, Archived: true},
	})
	if len(running) != 2 || running[0].ID != "old" || running[1].ID != "new" {
		t.Fatalf("switchable sessions = %+v", running)
	}
}

func TestSwitchableSessionsHaveAStableOrderWhenCreatedTogether(t *testing.T) {
	records := []protocol.SessionRecord{
		{ID: "falkn-c", Status: "running", CreatedAt: 10},
		{ID: "falkn-a", Status: "running", CreatedAt: 10},
		{ID: "falkn-b", Status: "running", CreatedAt: 10},
	}
	running := switchableSessions(records)
	if got := []string{running[0].ID, running[1].ID, running[2].ID}; !slices.Equal(got, []string{
		"falkn-a", "falkn-b", "falkn-c",
	}) {
		t.Fatalf("same-second session order = %v", got)
	}
}

func TestEndedSessionFallsBackToTheNearestRunningSession(t *testing.T) {
	records := []protocol.SessionRecord{
		{ID: "one", Status: "running", CreatedAt: 10},
		{ID: "two", Status: "ended", CreatedAt: 20},
		{ID: "three", Status: "running", CreatedAt: 30},
	}
	if got := selectEndedSessionFallback(records, "two"); got != "one" {
		t.Fatalf("ending the middle session fell back to %q, want its predecessor", got)
	}

	records[0].Status = "ended"
	records[1].Status = "running"
	if got := selectEndedSessionFallback(records, "one"); got != "two" {
		t.Fatalf("ending the oldest session fell back to %q, want the next one", got)
	}
}

func TestEndedSessionFallbackRequiresAnotherRunningSession(t *testing.T) {
	for name, records := range map[string][]protocol.SessionRecord{
		"only session ended": {
			{ID: "current", Status: "ended", CreatedAt: 10},
		},
		"stream closed but session still running": {
			{ID: "current", Status: "running", CreatedAt: 10},
			{ID: "other", Status: "running", CreatedAt: 20},
		},
	} {
		if got := selectEndedSessionFallback(records, "current"); got != "" {
			t.Fatalf("%s selected fallback %q", name, got)
		}
	}
}

func TestDeletedSessionFallsBackToTheNewestRunningSession(t *testing.T) {
	records := []protocol.SessionRecord{
		{ID: "one", Status: "running", CreatedAt: 10},
		{ID: "two", Status: "running", CreatedAt: 20},
	}
	if got := selectEndedSessionFallback(records, "deleted"); got != "two" {
		t.Fatalf("deleted session fell back to %q, want the newest running session", got)
	}
}

// readerFor drives the input reader over a pipe and returns the frames the
// session received.
func readerFor(t *testing.T, handlers attachInputHandlers) (*attachInputReader, net.Conn) {
	t.Helper()
	serverConnection, clientConnection := net.Pipe()
	t.Cleanup(func() { serverConnection.Close() })
	handlers.prefix = DefaultAttachPrefix
	return &attachInputReader{
		writer:   &attachFrameWriter{connection: clientConnection},
		handlers: handlers,
	}, serverConnection
}

func TestPrefixedArrowsRequestASwitch(t *testing.T) {
	var requests []sessionSwitch
	reader, _ := readerFor(t, attachInputHandlers{
		onSwitch: func(request sessionSwitch) { requests = append(requests, request) },
	})

	reader.consume([]byte{DefaultAttachPrefix, 0x1b, '[', 'C'})
	reader.consume([]byte{DefaultAttachPrefix, 0x1b, '[', 'D'})
	reader.consume([]byte{DefaultAttachPrefix, '3'})
	want := []sessionSwitch{{step: 1}, {step: -1}, {index: 3}}
	if len(requests) != len(want) {
		t.Fatalf("requests = %+v, want %+v", requests, want)
	}
	for index := range want {
		if requests[index] != want[index] {
			t.Fatalf("request %d = %+v, want %+v", index, requests[index], want[index])
		}
	}
}

func TestPrefixedArrowSurvivesASplitRead(t *testing.T) {
	var requests []sessionSwitch
	reader, _ := readerFor(t, attachInputHandlers{
		onSwitch: func(request sessionSwitch) { requests = append(requests, request) },
	})

	// A terminal may deliver the three bytes of an arrow key in separate reads.
	reader.consume([]byte{DefaultAttachPrefix})
	reader.consume([]byte{0x1b})
	reader.consume([]byte{'['})
	reader.consume([]byte{'C'})
	if len(requests) != 1 || requests[0] != (sessionSwitch{step: 1}) {
		t.Fatalf("split arrow produced %+v", requests)
	}
}

func TestPrefixedNRequestsANewSession(t *testing.T) {
	created := 0
	muted := 0
	reader, _ := readerFor(t, attachInputHandlers{
		supportsNotifications: true,
		onNotificationsMuted:  func(bool) { muted++ },
		onNewSession:          func() { created++ },
	})

	for _, key := range []byte{'n', 'N'} {
		if !reader.consume([]byte{DefaultAttachPrefix, key}) {
			t.Fatalf("prefix-%c stopped the input reader", key)
		}
	}
	if created != 2 {
		t.Fatalf("new-session callback ran %d times, want 2", created)
	}
	if muted != 0 {
		t.Fatalf("prefix-N toggled notifications %d times", muted)
	}
}

func TestUnclaimedPrefixedKeysReachTheSession(t *testing.T) {
	for _, testCase := range []struct {
		input []byte
		want  []byte
	}{
		// An arrow with no switcher wired, and an escape the prefix does not claim.
		{[]byte{DefaultAttachPrefix, 0x1b, '[', 'C'}, []byte{DefaultAttachPrefix, 0x1b, '[', 'C'}},
		{[]byte{DefaultAttachPrefix, 0x1b, 'x'}, []byte{DefaultAttachPrefix, 0x1b, 'x'}},
		{[]byte{DefaultAttachPrefix, 'z'}, []byte{DefaultAttachPrefix, 'z'}},
	} {
		reader, serverConnection := readerFor(t, attachInputHandlers{})
		done := make(chan struct{})
		go func() { defer close(done); reader.consume(testCase.input) }()
		kind, payload := readTestAttachFrame(t, serverConnection)
		<-done
		if kind != attachInputFrame || !bytes.Equal(payload, testCase.want) {
			t.Fatalf("input %v produced kind=%q payload=%v, want %v", testCase.input, kind, payload, testCase.want)
		}
	}
}

func TestPrefixedDigitsDoNotSwitchWithoutASwitcher(t *testing.T) {
	reader, serverConnection := readerFor(t, attachInputHandlers{})
	done := make(chan struct{})
	go func() { defer close(done); reader.consume([]byte{DefaultAttachPrefix, '3'}) }()
	kind, payload := readTestAttachFrame(t, serverConnection)
	<-done
	if kind != attachInputFrame || !bytes.Equal(payload, []byte{DefaultAttachPrefix, '3'}) {
		t.Fatalf("prefixed digit kind=%q payload=%v", kind, payload)
	}
}

func TestHoldingThePrefixIsReported(t *testing.T) {
	var states []bool
	reader, _ := readerFor(t, attachInputHandlers{
		onPrefixArmed: func(armed bool) { states = append(states, armed) },
		onSwitch:      func(sessionSwitch) {},
	})

	// The prefix alone leaves Falkn waiting, and the footer has to say so.
	reader.consume([]byte{DefaultAttachPrefix})
	if len(states) != 1 || !states[0] {
		t.Fatalf("holding the prefix reported %v", states)
	}
	// Completing the key releases it.
	reader.consume([]byte{'3'})
	if len(states) != 2 || states[1] {
		t.Fatalf("completing the key reported %v", states)
	}
}

func TestACompleteKeyDoesNotFlickerThePrefixIndicator(t *testing.T) {
	var states []bool
	reader, _ := readerFor(t, attachInputHandlers{
		onPrefixArmed: func(armed bool) { states = append(states, armed) },
		onSwitch:      func(sessionSwitch) {},
	})

	// A terminal usually delivers a prefixed key in one read. Reporting per byte
	// would light the indicator and clear it within the same repaint.
	reader.consume([]byte{DefaultAttachPrefix, 0x1b, '[', 'C'})
	if len(states) != 0 {
		t.Fatalf("a key delivered whole flickered the indicator: %v", states)
	}
}

func TestThePrefixStaysHeldPartWayThroughAnArrow(t *testing.T) {
	var states []bool
	reader, _ := readerFor(t, attachInputHandlers{
		onPrefixArmed: func(armed bool) { states = append(states, armed) },
		onSwitch:      func(sessionSwitch) {},
	})

	reader.consume([]byte{DefaultAttachPrefix})
	reader.consume([]byte{0x1b, '['})
	if len(states) != 1 || !states[0] {
		t.Fatalf("the prefix was released mid-sequence: %v", states)
	}
	reader.consume([]byte{'C'})
	if len(states) != 2 || states[1] {
		t.Fatalf("finishing the arrow did not release the prefix: %v", states)
	}
}
