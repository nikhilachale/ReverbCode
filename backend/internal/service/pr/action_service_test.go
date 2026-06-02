package pr

import (
	"context"
	"testing"
)

func TestMerge_ReturnsSquash(t *testing.T) {
	svc := NewActionService()
	res, err := svc.Merge(context.Background(), "42")
	if err != nil {
		t.Fatal(err)
	}
	if res.Method != "squash" {
		t.Errorf("method = %q, want squash", res.Method)
	}
}

func TestResolveComments_ReturnsOK(t *testing.T) {
	svc := NewActionService()
	_, err := svc.ResolveComments(context.Background(), "1", nil)
	if err != nil {
		t.Fatal(err)
	}
}
