package project

import (
	"reflect"
	"testing"
)

func TestApplyEnvironmentPatchWindowsKeySemantics(t *testing.T) {
	set := map[string]string{"PATH": "new"}
	setKeys, unset, err := validateEnvironmentPatch(PatchEnvironmentInput{Set: set}, true)
	if err != nil {
		t.Fatal(err)
	}
	got := applyEnvironmentPatch(
		map[string]string{"Path": "old", "OTHER": "keep"},
		set,
		setKeys,
		unset,
		true,
	)
	want := map[string]string{"PATH": "new", "OTHER": "keep"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("environment = %#v, want %#v", got, want)
	}

	setKeys, unset, err = validateEnvironmentPatch(PatchEnvironmentInput{Unset: []string{"PATH"}}, true)
	if err != nil {
		t.Fatal(err)
	}
	got = applyEnvironmentPatch(
		map[string]string{"Path": "old", "PATH": "duplicate", "OTHER": "keep"},
		nil,
		setKeys,
		unset,
		true,
	)
	want = map[string]string{"OTHER": "keep"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("environment after unset = %#v, want %#v", got, want)
	}
}

func TestValidateEnvironmentPatchUsesPlatformKeySemantics(t *testing.T) {
	input := PatchEnvironmentInput{
		Set:   map[string]string{"Path": "new"},
		Unset: []string{"PATH"},
	}
	if _, _, err := validateEnvironmentPatch(input, true); err == nil {
		t.Fatal("Windows-style set/unset conflict was accepted")
	}
	if _, _, err := validateEnvironmentPatch(input, false); err != nil {
		t.Fatalf("Unix-style distinct keys were rejected: %v", err)
	}

	if _, _, err := validateEnvironmentPatch(PatchEnvironmentInput{
		Set: map[string]string{"Path": "one", "PATH": "two"},
	}, true); err == nil {
		t.Fatal("Windows-style duplicate set keys were accepted")
	}
}

func TestApplyEnvironmentPatchUnixKeepsDistinctCasing(t *testing.T) {
	set := map[string]string{"PATH": "new"}
	setKeys, unset, err := validateEnvironmentPatch(PatchEnvironmentInput{Set: set}, false)
	if err != nil {
		t.Fatal(err)
	}
	got := applyEnvironmentPatch(
		map[string]string{"Path": "old"},
		set,
		setKeys,
		unset,
		false,
	)
	want := map[string]string{"Path": "old", "PATH": "new"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("environment = %#v, want %#v", got, want)
	}
}
