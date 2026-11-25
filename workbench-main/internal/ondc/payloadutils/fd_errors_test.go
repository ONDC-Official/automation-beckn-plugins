package payloadutils

import (
	"errors"
	"testing"

	"github.com/beckn-one/beckn-onix/pkg/model"
)

func TestNewNamedNackError(t *testing.T) {
	ondcContext := map[string]any{"message_id": "m1"}
	err := NewNamedNackError(CodeMessageIDReused, "message_id m1 is already used", ondcContext)

	var workbenchErr *model.WorkbenchErr
	if !errors.As(err, &workbenchErr) {
		t.Fatalf("NewNamedNackError() = %T, want *model.WorkbenchErr", err)
	}
	if workbenchErr.Behavior != "NACK" {
		t.Fatalf("Behavior = %q, want NACK (HTTP 200)", workbenchErr.Behavior)
	}
	becknErr := workbenchErr.BecknError()
	if becknErr.Code != "MESSAGE_ID_REUSED" {
		t.Fatalf("Code = %q, want MESSAGE_ID_REUSED", becknErr.Code)
	}
	if want := WorkbenchErrorPrefix + " message_id m1 is already used"; becknErr.Message != want {
		t.Fatalf("Message = %q, want %q", becknErr.Message, want)
	}
	if got, _ := becknErr.Context.(map[string]any); got["message_id"] != "m1" {
		t.Fatalf("Context = %#v, want the request context", becknErr.Context)
	}
	if AsWorkbenchError(err) != err {
		t.Fatalf("AsWorkbenchError() must pass a named NACK through unchanged")
	}
}
