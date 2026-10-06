package prompting

import (
	"context"
	"errors"
	"testing"

	"img-gen/internal/genres"
)

func TestDirectAllFilled(t *testing.T) {
	tmpl := "A portrait of {subject}, {lighting} lighting, in {setting}."
	got := Direct(tmpl, FieldValues{"subject": "an old fisherman", "lighting": "soft", "setting": "a boat"})
	want := "A portrait of an old fisherman, soft lighting, in a boat."
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestDirectDropsEmptyOptional(t *testing.T) {
	tmpl := "A portrait of {subject}, {?lighting: {lighting} lighting}{?setting:, in {setting}}."
	got := Direct(tmpl, FieldValues{"subject": "an old fisherman", "lighting": "soft", "setting": ""})
	want := "A portrait of an old fisherman, soft lighting."
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestDirectOptionalClauseFullPortrait(t *testing.T) {
	tmpl := "A portrait of {subject}{?age:, age {age}}{?expression:, {expression} expression}" +
		"{?lighting:, {lighting} lighting}{?setting:, in {setting}}."

	all := Direct(tmpl, FieldValues{
		"subject": "an archer", "age": "30s", "expression": "confident",
		"lighting": "soft", "setting": "a boat",
	})
	want := "A portrait of an archer, age 30s, confident expression, soft lighting, in a boat."
	if all != want {
		t.Fatalf("all set: got %q, want %q", all, want)
	}

	unset := Direct(tmpl, FieldValues{"subject": "an archer"})
	wantUnset := "A portrait of an archer."
	if unset != wantUnset {
		t.Fatalf("all unset: got %q, want %q", unset, wantUnset)
	}

	mixed := Direct(tmpl, FieldValues{"subject": "an archer", "lighting": "harsh"})
	wantMixed := "A portrait of an archer, harsh lighting."
	if mixed != wantMixed {
		t.Fatalf("mixed: got %q, want %q", mixed, wantMixed)
	}
}

func TestDirectOptionalClauseFragmentOnly(t *testing.T) {
	// A fragment may hold nothing but the token, or a value-independent
	// literal — both render/blank wholesale.
	got := Direct("start{?weather:, {weather} weather}{?end:. end}done",
		FieldValues{"weather": "clear", "end": "x"})
	if got != "start, clear weather. enddone" {
		t.Fatalf("got %q", got)
	}
	got = Direct("{?weather:, {weather} weather}{?end:. end}done", FieldValues{})
	if got != "done" {
		t.Fatalf("got %q", got)
	}
}

func TestDirectOptionalClauseMalformedStaysLiteral(t *testing.T) {
	// No colon before the close: not a clause — the text passes through
	// untouched (templates are repo-controlled, so this fails loudly enough).
	got := Direct("keep {?nocolon} me", FieldValues{})
	if got != "keep {?nocolon} me" {
		t.Fatalf("got %q", got)
	}
}

func TestResolveBooleans(t *testing.T) {
	fields := []genres.Field{
		{Key: "wildlife", Type: genres.FieldBoolean, TrueText: ", with birds", FalseText: ""},
	}
	got := Resolve(fields, FieldValues{"wildlife": "true"})
	if got["wildlife"] != ", with birds" {
		t.Fatalf("true case: got %q", got["wildlife"])
	}
	got = Resolve(fields, FieldValues{"wildlife": "false"})
	if got["wildlife"] != "" {
		t.Fatalf("false case: got %q", got["wildlife"])
	}
}

func TestEnhanceCallsChat(t *testing.T) {
	chat := func(ctx context.Context, msgs []Message) (string, error) {
		if len(msgs) != 2 {
			t.Fatalf("want 2 messages, got %d", len(msgs))
		}
		if msgs[0].Role != "system" {
			t.Fatalf("first message role = %q", msgs[0].Role)
		}
		return "polished prompt", nil
	}
	got, err := Enhance(context.Background(), chat, "system text", FieldValues{"a": "b"})
	if err != nil {
		t.Fatalf("Enhance: %v", err)
	}
	if got != "polished prompt" {
		t.Fatalf("got %q", got)
	}
}

func TestRefused(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"I cannot fulfill this request. I am prohibited from generating sexually explicit content.", true},
		{"I'm sorry, but I can't help with that.", true},
		{"As an AI, I am unable to generate explicit imagery.", true},
		{"I apologize, but I won't assist with this request.", true},
		{"a portrait of a woman standing in a field at sunset", false},
		{"a man who cannot swim in rough seas", false},
		{"photorealistic, ultra-detailed, sharp focus, 8k", false},
		{"", false},
	}
	for _, c := range cases {
		if got := Refused(c.in); got != c.want {
			t.Errorf("Refused(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestEnhancePropagatesError(t *testing.T) {
	chat := func(ctx context.Context, msgs []Message) (string, error) {
		return "", errors.New("boom")
	}
	if _, err := Enhance(context.Background(), chat, "s", FieldValues{}); err == nil {
		t.Fatal("expected error")
	}
}

func TestEnhancePrompt(t *testing.T) {
	var gotSystem, gotUser string
	chat := func(ctx context.Context, msgs []Message) (string, error) {
		gotSystem = msgs[0].Content
		gotUser = msgs[1].Content
		return "polished prompt", nil
	}
	got, err := EnhancePrompt(context.Background(), chat, "SYSTEM", "make it snow")
	if err != nil {
		t.Fatalf("EnhancePrompt: %v", err)
	}
	if got != "polished prompt" {
		t.Fatalf("got %q", got)
	}
	if gotSystem != "SYSTEM" || gotUser != "make it snow" {
		t.Fatalf("system=%q user=%q", gotSystem, gotUser)
	}
}
