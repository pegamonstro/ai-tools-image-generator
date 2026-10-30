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

func TestCompileDefaultsAndBudgetRange(t *testing.T) {
	cl := mustCompile(t, ValidateSpec{Checks: []CheckSpec{{Op: "tag_roundtrip"}}}, Availability{Tagger: true})
	if cl.Budget != BudgetDefault || cl.OnFail != "deliver" {
		t.Fatalf("defaults: %+v", cl)
	}
	// Budget max (3) is accepted as-is; out-of-range budgets are rejected,
	// never clamped (F2: submit-time validation contract).
	cl = mustCompile(t, ValidateSpec{Checks: []CheckSpec{{Op: "tile_conform"}}, Budget: ip(3), OnFail: "fail"}, Availability{})
	if cl.Budget != 3 || cl.OnFail != "fail" {
		t.Fatalf("budget max: %+v", cl)
	}
	if cl := mustCompile(t, ValidateSpec{Checks: []CheckSpec{{Op: "tile_conform"}}, Budget: ip(0)}, Availability{}); cl.Budget != 0 {
		t.Fatalf("explicit zero must stay evaluate-only: %+v", cl)
	}
	for _, b := range []int{-1, 4, 9} {
		_, err := CompileSpec(ValidateSpec{Checks: []CheckSpec{{Op: "tile_conform"}}, Budget: ip(b)}, Availability{})
		if err == nil || !strings.Contains(err.Error(), "validate budget must be 0..3") {
			t.Fatalf("budget %d: err = %v, want ~\"validate budget must be 0..3\"", b, err)
		}
	}
}

func TestCompileErrors(t *testing.T) {
	vlmCheckSpec := CheckSpec{Op: "vlm_check", Arg: json.RawMessage(`"a"`)}
	tileConformSpec := CheckSpec{Op: "tile_conform", Arg: json.RawMessage(`{"max_ratio":9}`)}
	cases := []struct {
		spec ValidateSpec
		av   Availability
		want string
	}{
		{ValidateSpec{Checks: make([]CheckSpec, 9)}, Availability{}, "at most 8 check"},
		{ValidateSpec{Checks: []CheckSpec{vlmCheckSpec, vlmCheckSpec, vlmCheckSpec, vlmCheckSpec, vlmCheckSpec}}, Availability{}, "at most 4 vlm_check"},
		{ValidateSpec{Checks: []CheckSpec{{Op: "tag_roundtrip"}}, OnFail: "explode"}, Availability{Tagger: true}, "on_fail must be"},
		{ValidateSpec{Palette: []string{"zzz"}, Checks: []CheckSpec{{Op: "palette_conform"}}}, Availability{}, "bad color"},
		{ValidateSpec{Checks: []CheckSpec{tileConformSpec}}, Availability{}, "max_ratio must be"},
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
	// compile {tile_conform, arg {"max_ratio":1.1}} + {vlm_check, arg "subject matches"}
	spec := ValidateSpec{
		Checks: []CheckSpec{
			{Op: "tile_conform", Arg: json.RawMessage(`{"max_ratio":1.1}`)},
			{Op: "vlm_check", Arg: json.RawMessage(`"subject matches"`)},
		},
	}
	av := Availability{} // Tagger not needed for these ops
	cl, err := CompileSpec(spec, av)
	if err != nil {
		t.Fatalf("CompileSpec: %v", err)
	}
	if len(cl.Checks) != 2 {
		t.Fatalf("expected 2 checks, got %d", len(cl.Checks))
	}
	if cl.Checks[0].MaxRatio != 1.1 {
		t.Fatalf("expected MaxRatio 1.1 for tile_conform, got %f", cl.Checks[0].MaxRatio)
	}
	if cl.Checks[1].Spec != "subject matches" {
		t.Fatalf("expected Spec 'subject matches' for vlm_check, got %q", cl.Checks[1].Spec)
	}
}
