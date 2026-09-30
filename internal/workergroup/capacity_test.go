package workergroup

import "testing"

func TestPublicProjectionsRejectUnknownInternalValues(t *testing.T) {
	for name, project := range map[string]func() error{
		"group": func() error { _, err := publicGroupStatus("future"); return err },
		"pool":  func() error { _, err := publicPoolStatus("future"); return err },
		"host":  func() error { _, err := publicHostStatus("future"); return err },
	} {
		if err := project(); err == nil {
			t.Fatalf("%s projected an unknown internal lifecycle value", name)
		}
	}
}
