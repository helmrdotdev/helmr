package config

import "testing"

func setDiagnosticAdmissionEnv(t *testing.T) {
	t.Helper()
	for key, value := range map[string]string{
		"DIAGNOSTIC_CHUNK_BYTES": "1024", "DIAGNOSTIC_SOURCE_BYTES": "4096", "DIAGNOSTIC_SOURCE_RECORDS": "16",
		"DIAGNOSTIC_ENVIRONMENT_BYTES": "16384", "DIAGNOSTIC_ENVIRONMENT_RECORDS": "64",
		"DIAGNOSTIC_QUEUE_BYTES": "65536", "DIAGNOSTIC_QUEUE_RECORDS": "256", "DIAGNOSTIC_DB_MAX_CONNECTIONS": "2",
	} {
		t.Setenv(key, value)
	}
}
func TestDiagnosticAdmissionConfiguration(t *testing.T) {
	setDiagnosticAdmissionEnv(t)
	c, err := LoadDiagnosticAdmission()
	if err != nil || c.MaxConnections != 2 || c.Bounds.ChunkBytes != 1024 {
		t.Fatalf("config=%+v err=%v", c, err)
	}
	for _, tc := range []struct{ key, value string }{
		{"DIAGNOSTIC_CHUNK_BYTES", ""}, {"DIAGNOSTIC_CHUNK_BYTES", "16777217"},
		{"DIAGNOSTIC_SOURCE_BYTES", "512"}, {"DIAGNOSTIC_ENVIRONMENT_RECORDS", "1"},
		{"DIAGNOSTIC_QUEUE_BYTES", "1024"}, {"DIAGNOSTIC_DB_MAX_CONNECTIONS", "0"},
		{"DIAGNOSTIC_DB_MAX_CONNECTIONS", "2147483648"}, {"DIAGNOSTIC_SOURCE_RECORDS", "oops"},
	} {
		t.Run(tc.key+tc.value, func(t *testing.T) {
			setDiagnosticAdmissionEnv(t)
			t.Setenv(tc.key, tc.value)
			if _, err := LoadDiagnosticAdmission(); err == nil {
				t.Fatal("invalid diagnostic configuration accepted")
			}
		})
	}
}

func TestDiagnosticExporterConfiguration(t *testing.T) {
	setDiagnosticAdmissionEnv(t)
	t.Setenv("DIAGNOSTIC_EXPORT_BATCH_RECORDS", "16")
	t.Setenv("DIAGNOSTIC_EXPORT_BATCH_BYTES", "4096")
	c, err := LoadDiagnosticExporter()
	if err != nil || c.Ingest.Batch.Records != 16 || c.Ingest.Batch.Bytes != 4096 || c.Ingest.OperationTimeout >= c.Ingest.Batch.ClaimFor {
		t.Fatalf("config=%+v: %v", c, err)
	}
	for _, tc := range []struct{ key, value string }{
		{"DIAGNOSTIC_EXPORT_BATCH_RECORDS", ""}, {"DIAGNOSTIC_EXPORT_BATCH_RECORDS", "0"}, {"DIAGNOSTIC_EXPORT_BATCH_RECORDS", "2147483648"}, {"DIAGNOSTIC_EXPORT_BATCH_BYTES", ""}, {"DIAGNOSTIC_EXPORT_BATCH_BYTES", "1023"},
	} {
		t.Run(tc.key+tc.value, func(t *testing.T) {
			t.Setenv(tc.key, tc.value)
			if _, err := LoadDiagnosticExporter(); err == nil {
				t.Fatal("invalid export budget accepted")
			}
		})
	}
}
