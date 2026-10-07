package claudecode

import (
	"encoding/json"
	"testing"

	"github.com/Eastsidegunn/JANUS/contracts/gen"
)

// T24: native success must not hide errors or the credential-free login gate.
func TestResultSuccessFailureSignals(t *testing.T) {
	cases := []struct {
		name string
		line string
		want gen.DonePayloadStatus
	}{
		{"login banner", `{"type":"result","subtype":"success","is_error":false,"result":"Not logged in · Please run /login"}`, gen.DonePayloadStatusError},
		{"missing is_error", `{"type":"result","subtype":"success","result":"Not logged in · Please run /login"}`, gen.DonePayloadStatusError},
		{"not logged in case insensitive", `{"type":"result","subtype":"success","is_error":false,"result":"NOT LOGGED IN"}`, gen.DonePayloadStatusError},
		{"login instruction case insensitive", `{"type":"result","subtype":"success","is_error":false,"result":"PLEASE RUN /LOGIN"}`, gen.DonePayloadStatusError},
		{"authentication failed case insensitive", `{"type":"result","subtype":"success","is_error":false,"result":"Authentication FAILED"}`, gen.DonePayloadStatusError},
		{"explicit error", `{"type":"result","subtype":"success","is_error":true,"result":"execution failed"}`, gen.DonePayloadStatusError},
		{"ordinary success", `{"type":"result","subtype":"success","is_error":false,"result":"작업 완료"}`, gen.DonePayloadStatusOk},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			for _, stop := range []bool{false, true} {
				p := NewParser()
				mustParse(t, p, initLine)
				if stop {
					p.NoteStop()
				}
				events, err := p.ParseLine([]byte(c.line))
				if err != nil {
					t.Fatal(err)
				}
				if len(events) != 1 || events[0].Kind != gen.EventKindSubagentDone || !p.Done() {
					t.Fatalf("expected terminal done event: %+v", events)
				}
				var done gen.DonePayload
				if err := json.Unmarshal(events[0].Payload, &done); err != nil {
					t.Fatal(err)
				}
				want := c.want
				if stop {
					want = gen.DonePayloadStatusStopped
				}
				if done.Status != want {
					t.Errorf("stop=%t: status=%s, want %s", stop, done.Status, want)
				}
				var native nativeLine
				if err := json.Unmarshal([]byte(c.line), &native); err != nil {
					t.Fatal(err)
				}
				if done.Result != *native.Result || string(events[0].Raw) != c.line {
					t.Fatal("result text or raw native line changed")
				}
			}
		})
	}
}
