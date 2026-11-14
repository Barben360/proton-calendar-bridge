package ui_test

import (
	"bytes"
	"encoding/json"
	"slices"
	"testing"

	"github.com/Barben360/proton-calendar-bridge/internal/cli"
	"github.com/Barben360/proton-calendar-bridge/internal/ui"
)

// pcb doctor adds the service and daemon results to the checks the app runs.
func TestDoctorChecksShared(t *testing.T) {
	l := newLab(t)
	b := l.start(t)
	l.login(t, b)
	if err := b.Stop(); err != nil { // no daemon: pcb doctor lists the offline checks
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	cli.Main(t.Context(), []string{"doctor", "--json"}, cli.IO{Stdout: &out, Stderr: &errb, Env: l.env})
	var env struct {
		Data struct {
			Results []struct{ Name string } `json:"results"`
		} `json:"data"`
	}
	if err := json.Unmarshal(out.Bytes(), &env); err != nil {
		t.Fatalf("%v: %q", err, out.String())
	}
	var cliNames []string
	for _, r := range env.Data.Results {
		if r.Name != "service" && r.Name != "daemon" {
			cliNames = append(cliNames, r.Name)
		}
	}
	res, err := ui.NewActions(b).Doctor(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var uiNames []string
	for _, r := range res {
		uiNames = append(uiNames, r.Name)
	}
	if len(uiNames) < 8 || !slices.Equal(cliNames, uiNames) {
		t.Errorf("cli checks %v\napp checks %v", cliNames, uiNames)
	}
}
