package xmlda

import (
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
