package conpty

import (
	"reflect"
	"strings"
	"testing"
)

func TestStripEnvAssignments(t *testing.T) {
	tests := []struct {
		name            string
		argv            []string
		wantAssignments []string
		wantRest        []string
	}{
		{
			name:            "no env prefix returns argv unchanged",
			argv:            []string{"opencode", "--agent", "ao-x"},
			wantAssignments: nil,
			wantRest:        []string{"opencode", "--agent", "ao-x"},
		},
		{
			name:            "env prefix is split from the real command",
			argv:            []string{"env", "OPENCODE_CONFIG=C:/cfg.json", "opencode", "--agent", "ao-x"},
			wantAssignments: []string{"OPENCODE_CONFIG=C:/cfg.json"},
			wantRest:        []string{"opencode", "--agent", "ao-x"},
		},
		{
			name:            "env with no command left is untouched",
			argv:            []string{"env", "A=1", "B=2"},
			wantAssignments: nil,
			wantRest:        []string{"env", "A=1", "B=2"},
		},
		{
			name:            "a binary merely starting with env is not treated as a prefix",
			argv:            []string{"envoy", "--config", "x"},
			wantAssignments: nil,
			wantRest:        []string{"envoy", "--config", "x"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotAssignments, gotRest := stripEnvAssignments(tt.argv)
			if !reflect.DeepEqual(gotAssignments, tt.wantAssignments) {
				t.Errorf("assignments = %#v, want %#v", gotAssignments, tt.wantAssignments)
			}
			if !reflect.DeepEqual(gotRest, tt.wantRest) {
				t.Errorf("rest = %#v, want %#v", gotRest, tt.wantRest)
			}
		})
	}
}

func TestBuildHostEnvironmentProtectsReservedControls(t *testing.T) {
	assignments, _ := stripEnvAssignments([]string{
		"env",
		"ao_data_dir=C:/argv-spoof",
		"Ao_PtY_HoSt_GeNeRaTiOn=argv-spoof",
		"OPENCODE_CONFIG=C:/cfg.json",
		"opencode",
	})
	got := buildHostEnvironment(
		[]string{"Path=C:/Windows", "ao_data_dir=C:/ambient-spoof", "AO_PTY_HOST_GENERATION=ambient-spoof"},
		map[string]string{
			"ao_data_dir":            "C:/project-spoof",
			"Ao_PtY_HoSt_GeNeRaTiOn": "project-spoof",
			dataDirEnv:               "C:/authoritative-data",
			hostGenerationEnv:        "authoritative-generation",
		},
		assignments,
	)

	values := make(map[string][]string)
	for _, entry := range got {
		key, value, ok := strings.Cut(entry, "=")
		if ok {
			values[strings.ToUpper(key)] = append(values[strings.ToUpper(key)], value)
		}
	}
	if got := values[dataDirEnv]; !reflect.DeepEqual(got, []string{"C:/authoritative-data"}) {
		t.Fatalf("case-insensitive %s values = %v", dataDirEnv, got)
	}
	if got := values[hostGenerationEnv]; !reflect.DeepEqual(got, []string{"authoritative-generation"}) {
		t.Fatalf("case-insensitive %s values = %v", hostGenerationEnv, got)
	}
	if got := values["OPENCODE_CONFIG"]; !reflect.DeepEqual(got, []string{"C:/cfg.json"}) {
		t.Fatalf("non-reserved leading env assignment = %v", got)
	}
}
