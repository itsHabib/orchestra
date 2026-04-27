package customtools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

type fakeHandler struct {
	def Definition
}

func (f fakeHandler) Tool() Definition { return f.def }
func (fakeHandler) Handle(context.Context, *RunContext, string, json.RawMessage) (json.RawMessage, error) {
	return nil, nil
}

func TestRegisterLookup(t *testing.T) {
	t.Cleanup(Reset)
	Reset()

	if err := Register(fakeHandler{def: Definition{Name: "foo"}}); err != nil {
		t.Fatalf("register foo: %v", err)
	}
	got, ok := Lookup("foo")
	if !ok {
		t.Fatalf("expected foo to be registered")
	}
	if got.Tool().Name != "foo" {
		t.Fatalf("Lookup returned wrong handler: %s", got.Tool().Name)
	}
	if _, ok := Lookup("bar"); ok {
		t.Fatalf("bar should not be registered")
	}
}

func TestRegisterIdempotentOverwrite(t *testing.T) {
	t.Cleanup(Reset)
	Reset()

	first := fakeHandler{def: Definition{Name: "foo", Description: "first"}}
	second := fakeHandler{def: Definition{Name: "foo", Description: "second"}}

	if err := Register(first); err != nil {
		t.Fatalf("register first: %v", err)
	}
	if err := Register(second); err != nil {
		t.Fatalf("register second: %v", err)
	}
	got, ok := Lookup("foo")
	if !ok {
		t.Fatalf("expected foo to be registered")
	}
	if got.Tool().Description != "second" {
		t.Fatalf("second register should have replaced first, got %q", got.Tool().Description)
	}
}

func TestRegisterRejectsMalformed(t *testing.T) {
	t.Cleanup(Reset)
	Reset()

	if err := Register(nil); err == nil {
		t.Fatalf("expected error on nil handler")
	}
	if err := Register(fakeHandler{def: Definition{Name: ""}}); err == nil {
		t.Fatalf("expected error on empty tool name")
	}
}

func TestDefinitionsSorted(t *testing.T) {
	t.Cleanup(Reset)
	Reset()

	for _, n := range []string{"zeta", "alpha", "mu"} {
		if err := Register(fakeHandler{def: Definition{Name: n}}); err != nil {
			t.Fatalf("register %s: %v", n, err)
		}
	}
	defs := Definitions()
	want := []string{"alpha", "mu", "zeta"}
	if len(defs) != len(want) {
		t.Fatalf("expected %d definitions, got %d", len(want), len(defs))
	}
	for i, w := range want {
		if defs[i].Name != w {
			t.Fatalf("position %d: want %s got %s", i, w, defs[i].Name)
		}
	}
}

func TestMustRegisterPanicsOnError(t *testing.T) {
	t.Cleanup(Reset)
	Reset()

	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("MustRegister should have panicked on nil handler")
		}
	}()
	MustRegister(nil)
}

func TestMustRegisterPanicCarriesError(t *testing.T) {
	t.Cleanup(Reset)
	Reset()

	defer func() {
		r := recover()
		if r == nil {
			t.Fatalf("expected panic")
		}
		err, ok := r.(error)
		if !ok {
			t.Fatalf("panic value is not an error: %T", r)
		}
		if err == nil {
			t.Fatalf("panic carried a nil error")
		}
		if !strings.Contains(err.Error(), "tool name is empty") {
			t.Fatalf("panic error %q does not mention the malformed-handler reason", err.Error())
		}
	}()
	MustRegister(fakeHandler{def: Definition{Name: ""}})
}
