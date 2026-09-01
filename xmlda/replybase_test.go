package xmlda

import (
	"strings"
	"testing"
	"time"
)

func TestReplyBase_RoundTrip(t *testing.T) {
	rb := ReplyBase{
		RcvTime:             time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC),
		ReplyTime:           time.Date(2024, 1, 1, 12, 0, 1, 0, time.UTC),
		ClientRequestHandle: "CRH1",
		RevisedLocaleID:     "en-us",
		ServerState:         ServerStateRunning,
	}
	out, err := xmlMarshalNamed(t, "Result", rb)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got ReplyBase
	if err := Decode(out, &got); err != nil {
		t.Fatalf("Decode: %v\ndoc: %s", err, out)
	}
	if !got.RcvTime.Equal(rb.RcvTime) || !got.ReplyTime.Equal(rb.ReplyTime) {
		t.Fatalf("times mismatch: got %+v, want %+v", got, rb)
	}
	if got.ClientRequestHandle != rb.ClientRequestHandle || got.RevisedLocaleID != rb.RevisedLocaleID || got.ServerState != rb.ServerState {
		t.Fatalf("got %+v, want %+v", got, rb)
	}
}

func TestRequiresFault(t *testing.T) {
	cases := []struct {
		op        string
		state     ServerState
		wantFault bool
	}{
		{"GetStatus", ServerStateFailed, false},
		{"Read", ServerStateFailed, true},
		{"Write", ServerStateFailed, true},
		{"Browse", ServerStateFailed, true},
		{"Read", ServerStateSuspended, true},
		{"Write", ServerStateSuspended, true},
		{"Subscribe", ServerStateSuspended, true},
		{"Browse", ServerStateSuspended, false},
		{"GetProperties", ServerStateSuspended, false},
		{"Read", ServerStateNoConfig, true},
		{"Read", ServerStateRunning, false},
		{"Read", ServerStateTest, false},
		{"Read", ServerStateCommFault, false},
	}
	for _, tc := range cases {
		fault, code := RequiresFault(tc.op, tc.state)
		if fault != tc.wantFault {
			t.Fatalf("RequiresFault(%q, %q) = %v, want %v", tc.op, tc.state, fault, tc.wantFault)
		}
		if fault && code != ErrServerState {
			t.Fatalf("RequiresFault(%q, %q): got code %v, want ErrServerState", tc.op, tc.state, code)
		}
	}
}

// --- wire timestamps are UTC with millisecond precision ---

// TestWireTime_IsUTCMilliseconds pins the fix for timestamps having gone
// out via time.RFC3339Nano, which emits the server process's local offset
// and a variable-length fractional part. Both are legal xsd:dateTime, but
// the real captured traffic is UTC with milliseconds, and a client that
// subtracts timestamps without applying the offset reads a server in a
// non-UTC zone as hours off.
func TestWireTime_IsUTCMilliseconds(t *testing.T) {
	berlin := time.FixedZone("CEST", 2*60*60)
	ts := time.Date(2026, 3, 4, 11, 30, 0, 123456789, berlin)

	if got, want := formatWireTime(ts), "2026-03-04T09:30:00.123Z"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}

	// The same form reaches the wire through ReplyBase and ItemValue.
	out, err := xmlMarshalNamed(t, "Result", ReplyBase{
		RcvTime: ts, ReplyTime: ts, ServerState: ServerStateRunning,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(out), `RcvTime="2026-03-04T09:30:00.123Z"`) {
		t.Fatalf("ReplyBase did not emit a UTC millisecond timestamp: %s", out)
	}

	iv := ItemValue{ItemName: "A", Timestamp: &ts, Quality: qualityPtr(NewGoodQuality())}
	out, err = xmlMarshalNamed(t, "Items", iv)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(out), `Timestamp="2026-03-04T09:30:00.123Z"`) {
		t.Fatalf("ItemValue did not emit a UTC millisecond timestamp: %s", out)
	}
}
