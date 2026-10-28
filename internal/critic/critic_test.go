package critic

import (
	"encoding/json"
	"strings"
	"testing"
)

func ip[T any](v T) *T { return &v }

func mustCompile(t *testing.T, spec ValidateSpec, av Availability) Checklist {
	t.Helper()
	cl, err := CompileSpec(spec, av)
	if err != nil {
		t.Fatalf("CompileSpec: %v", err)
	}
	return cl
}

func TestCompileDefaultsAndClamps(t *testing.T) {
	cl := mustCompile(t, ValidateSpec{Checks: []CheckSpec{{Op: "tag_roundtrip"}}}, Availability{Tagger: true})
	if cl.Budget != BudgetDefault || cl.OnFail != "deliver" {
		t.Fatalf("defaults: %+v", cl)
	}
	cl = mustCompile(t, ValidateSpec{Checks: []CheckSpec{{Op: "tile_conform"}}, Budget: ip(9), OnFail: "fail"}, Availability{})
	if cl.Budget != 3 || cl.OnFail != "fail" {
		t.Fatalf("clamp: %+v", cl)
	}
	if cl := mustCompile(t, ValidateSpec{Checks: []CheckSpec{{Op: "tile_conform"}}, Budget: ip(0)}, Availability{}); cl.Budget != 0 {
		t.Fatalf("explicit zero must stay evaluate-only: %+v", cl)
	}
}

func TestCompileErrors(t *testing.T) {
	cases := []struct {
		spec ValidateSpec
		av   Availability
		want string
	}{
		{ValidateSpec{Checks: []CheckSpec{{Op: "wat"}}}, Availability{}, "unknown check op"},
		{ValidateSpec{Checks: []CheckSpec{{Op: "tag_roundtrip"}}}, Availability{}, "tag_roundtrip requires the WD14 tagger sidecar (TAGGER_URL)"},
	}
	for _, c := range cases {
		_, err := CompileSpec(c.spec, c.av)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("CompileSpec(%+v) err=%v want ~%q", c.spec, err, c.want)
		}
	}
}

func TestCompileTileArgAndVLMText(t *testing.T) {
	cl := mustCompile(t, ValidateSpec{Checks: []CheckSpec{
		{Op: "tile_conform", Arg: json.RawMessage(`{"max_ratio":1.1}`)},
		{Op: "vlm_check", Arg: json.RawMessage(`"subject matches"`)}},
	}, Availability{})
	if cl.Checks[0].MaxRatio != 1.1 {
		t.Fatalf("tile arg lost: %+v", cl.Checks[0])
	}
	if cl.Checks[1].Spec != "subject matches" {
		t.Fatalf("vlm spec lost: %+v", cl.Checks[1])
	}
}
