package run

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestMetadataMutationNormalizesAndApplies(t *testing.T) {
	amount := 2.0
	for _, test := range []struct {
		name      string
		current   json.RawMessage
		operation string
		key       string
		value     json.RawMessage
		patch     json.RawMessage
		amount    *float64
		want      string
		canonical string
	}{
		{
			name: "set", current: json.RawMessage(`{"phase":"queued"}`),
			operation: "set", key: "phase", value: json.RawMessage(`"running"`),
			want:      `{"phase":"running"}`,
			canonical: `{"amount":null,"key":"phase","operation":"set","patch":null,"value":"running"}`,
		},
		{
			name: "patch", current: json.RawMessage(`{"phase":"running","steps":1}`),
			operation: "patch", patch: json.RawMessage(`{"phase":"done","approved":true}`),
			want:      `{"approved":true,"phase":"done","steps":1}`,
			canonical: `{"amount":null,"key":"","operation":"patch","patch":{"approved":true,"phase":"done"},"value":null}`,
		},
		{
			name: "increment", current: json.RawMessage(`{"steps":1}`),
			operation: "increment", key: "steps", amount: &amount,
			want:      `{"steps":3}`,
			canonical: `{"amount":2,"key":"steps","operation":"increment","patch":null,"value":null}`,
		},
		{
			name: "empty metadata", operation: "set", key: "phase", value: json.RawMessage(`"running"`),
			want:      `{"phase":"running"}`,
			canonical: `{"amount":null,"key":"phase","operation":"set","patch":null,"value":"running"}`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			mutation, err := NewMetadataMutation(test.operation, test.key, test.value, test.patch, test.amount)
			if err != nil {
				t.Fatal(err)
			}
			if string(mutation.canonical) != test.canonical {
				t.Fatalf("canonical = %s, want %s", mutation.canonical, test.canonical)
			}
			got, err := mutation.apply(test.current)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != test.want {
				t.Fatalf("metadata = %s, want %s", got, test.want)
			}
		})
	}
}

func TestMetadataMutationRejectsInvalidMutations(t *testing.T) {
	amount := 1.0
	for name, test := range map[string]struct {
		operation, key string
		value, patch   json.RawMessage
		amount         *float64
		want           string
	}{
		"unknown":          {operation: "delete", key: "k", want: "operation must be set, patch, or increment"},
		"set without key":  {operation: "set", value: json.RawMessage(`1`), want: "metadata key must be nonempty UTF-8"},
		"set with amount":  {operation: "set", key: "k", value: json.RawMessage(`1`), amount: &amount, want: "set requires only key and value"},
		"set value":        {operation: "set", key: "k", value: json.RawMessage(`{"a":1,"a":2}`), want: "set value is invalid"},
		"patch with key":   {operation: "patch", key: "k", patch: json.RawMessage(`{}`), want: "patch requires only patch"},
		"patch array":      {operation: "patch", patch: json.RawMessage(`[]`), want: "run metadata patch metadata must be an unambiguous JSON object"},
		"patch empty key":  {operation: "patch", patch: json.RawMessage(`{"":1}`), want: "metadata key must be nonempty UTF-8"},
		"increment amount": {operation: "increment", key: "k", want: "increment requires only key and a finite amount"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := NewMetadataMutation(test.operation, test.key, test.value, test.patch, test.amount)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestMetadataIncrementRejectsNonnumericStoredValue(t *testing.T) {
	amount := 1.0
	mutation, err := NewMetadataMutation("increment", "steps", nil, nil, &amount)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mutation.apply(json.RawMessage(`{"steps":"one"}`)); err == nil || err.Error() != `run metadata key "steps" is not a finite number` {
		t.Fatalf("nonnumeric increment error = %v", err)
	}
}

func TestNormalizeMetadataCanonicalizesBoundedObjects(t *testing.T) {
	got, err := NormalizeMetadata(nil, MaxMetadataBytes, "run")
	if err != nil || string(got) != `{}` {
		t.Fatalf("empty metadata = %s, %v", got, err)
	}
	got, err = NormalizeMetadata(json.RawMessage(`{"b":1,"a":2}`), MaxMetadataBytes, "run")
	if err != nil || string(got) != `{"a":2,"b":1}` {
		t.Fatalf("metadata = %s, %v", got, err)
	}
	if _, err := NormalizeMetadata(json.RawMessage(`null`), MaxMetadataBytes, "run"); err == nil || err.Error() != "run metadata must be an unambiguous JSON object" {
		t.Fatalf("null metadata error = %v", err)
	}
	if _, err := NormalizeMetadata(json.RawMessage(`{"a":1}`), 4, "managed run"); err == nil || err.Error() != "managed run metadata exceeds 4 bytes" {
		t.Fatalf("oversized metadata error = %v", err)
	}
}
