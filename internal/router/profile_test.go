package router

import (
	"reflect"
	"testing"

	"shunt/internal/config"
)

// deepseekProfile is the default profile, which the sanitize tests exercise.
var deepseekProfile = profile(config.Provider{})

func TestProfileDefaultsAndOverrides(t *testing.T) {
	if len(deepseekProfile.TopKeys) == 0 || len(deepseekProfile.BlockTypes) == 0 || len(deepseekProfile.BetaAllow) != 0 {
		t.Fatalf("default profile = %+v", deepseekProfile)
	}
	got := profile(config.Provider{BetaAllow: []string{"b"}, TopKeys: []string{"model"}, BlockTypes: []string{"text"}})
	want := Sanitize{BetaAllow: []string{"b"}, TopKeys: []string{"model"}, BlockTypes: []string{"text"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("profile = %+v, want %+v", got, want)
	}
}
