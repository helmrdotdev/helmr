package main

import "testing"

func TestValidateWorkerMemoryMiB(t *testing.T) {
	for _, test := range []struct {
		name       string
		configured int64
		physical   int64
		wantError  bool
	}{
		{name: "lower", configured: 7168, physical: 8192},
		{name: "equal", configured: 8192, physical: 8192},
		{name: "higher", configured: 8193, physical: 8192, wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := validateWorkerMemoryMiB(test.configured, test.physical)
			if (err != nil) != test.wantError {
				t.Fatalf("validateWorkerMemoryMiB() error = %v, wantError %t", err, test.wantError)
			}
		})
	}
}
