package payloadutils

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/beckn-one/beckn-onix/pkg/model"
)

func TestWithWorkbenchPrefix(t *testing.T) {
	got := WithWorkbenchPrefix("action is missing in context")
	if want := "[FROM ONIX WORKBENCH] action is missing in context"; got != want {
		t.Fatalf("WithWorkbenchPrefix() = %q, want %q", got, want)
	}
	if again := WithWorkbenchPrefix(got); again != got {
		t.Fatalf("WithWorkbenchPrefix() on prefixed message = %q, want it unchanged", again)
	}
}

func TestErrorConstructorsArePrefixed(t *testing.T) {
	errs := map[string]error{
		"BadRequestNack":     NewBadRequestNackError("bad", nil),
		"InternalServerNack": NewInternalServerNackError("internal", nil),
		"BadRequestHTTP":     NewBadRequestHTTPError("bad"),
		"PreconditionHTTP":   NewPreconditionFailedHTTPError("precondition"),
		"InternalServerHTTP": NewInternalServerHTTPError("internal"),
		// Re-wrapping a workbench error's message must not double the prefix.
		"Rewrapped": NewInternalServerNackError(NewBadRequestHTTPError("bad").Error(), nil),
	}
	for name, err := range errs {
		msg := err.Error()
		if !strings.HasPrefix(msg, WorkbenchErrorPrefix+" ") || strings.Count(msg, WorkbenchErrorPrefix) != 1 {
			t.Errorf("%s: message %q, want exactly one %q prefix", name, msg, WorkbenchErrorPrefix)
		}
	}
}

func TestAsWorkbenchError(t *testing.T) {
	if AsWorkbenchError(nil) != nil {
		t.Fatalf("AsWorkbenchError(nil) should be nil")
	}

	nack := NewBadRequestNackError("bad", nil)
	if got := AsWorkbenchError(nack); got != nack {
		t.Fatalf("AsWorkbenchError() changed an existing workbench error")
	}

	got := AsWorkbenchError(errors.New("cache is nil"))
	var workbenchErr *model.WorkbenchErr
	if !errors.As(got, &workbenchErr) {
		t.Fatalf("AsWorkbenchError() = %T, want *model.WorkbenchErr", got)
	}
	if workbenchErr.Behavior != "HTTP" || workbenchErr.Err.Code != "500" {
		t.Fatalf("behavior/code = %s/%s, want HTTP/500", workbenchErr.Behavior, workbenchErr.Err.Code)
	}
	if want := "[FROM ONIX WORKBENCH] cache is nil"; workbenchErr.Err.Message != want {
		t.Fatalf("message = %q, want %q", workbenchErr.Err.Message, want)
	}

	// onix wraps step errors with %w; the message it sends must still be the prefixed one.
	wrapped := fmt.Errorf("ondc workbench receive step failed: %w", got)
	if !errors.As(wrapped, &workbenchErr) || !strings.HasPrefix(workbenchErr.BecknError().Message, WorkbenchErrorPrefix) {
		t.Fatalf("wrapped error lost the workbench prefix")
	}
}
