package tool

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestJSONHandlerEncodesTypedResult(t *testing.T) {
	handler := JSONHandler(func(context.Context, Arguments) (map[string]int, error) {
		return map[string]int{"count": 2}, nil
	})
	result, err := handler.Execute(context.Background(), Arguments{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Content != "{\n  \"count\": 2\n}" {
		t.Fatalf("content = %q", result.Content)
	}
}

func TestJSONHandlerPreservesOperationError(t *testing.T) {
	want := errors.New("lookup failed")
	handler := JSONHandler(func(context.Context, Arguments) (map[string]int, error) {
		return nil, want
	})
	_, err := handler.Execute(context.Background(), Arguments{})
	if !errors.Is(err, want) {
		t.Fatalf("error = %v", err)
	}
}

func TestJSONHandlerWrapsEncodingError(t *testing.T) {
	handler := JSONHandler(func(context.Context, Arguments) (chan int, error) {
		return make(chan int), nil
	})
	_, err := handler.Execute(context.Background(), Arguments{})
	if err == nil || !strings.Contains(err.Error(), "encode tool result") {
		t.Fatalf("error = %v", err)
	}
}
