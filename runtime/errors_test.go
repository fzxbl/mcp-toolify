package runtime

import (
	"errors"
	"testing"
)

func TestToolError(t *testing.T) {
	res := ToolError(errors.New("boom"))
	if !res.IsError {
		t.Fatal("IsError false")
	}
	if len(res.Content) != 1 {
		t.Fatal("expect 1 content")
	}
}
