package schedule

import (
	"testing"
	"time"
)

func TestCronContract(t *testing.T) {
	valid := []string{
		"0 9 * * *",
		"*/5 * * * *",
		"0,15,30,45 9-17 * * 1-5",
		"0 9 * JAN MON",
		"0 9 ? * *",
		"00 9 * * *",
		"0 9 * * 2,1",
		"0  9 * * *",
		" 0 9 * * * ",
	}
	for _, value := range valid {
		if err := ValidateCron(value); err != nil {
			t.Errorf("ValidateCron(%q): %v", value, err)
		}
	}
	invalid := []string{
		"@daily",
		"0 0 1 * 0-7",
		"0 9 * * 0,7",
		"0 9 * *",
		"0 9 * * * *",
	}
	for _, value := range invalid {
		if err := ValidateCron(value); err == nil {
			t.Errorf("ValidateCron(%q) succeeded", value)
		}
	}
}

func TestNextCronTimeUsesExactTimezone(t *testing.T) {
	next, err := NextCronTime("0 9 * * *", "Asia/Tokyo", time.Date(2026, 6, 1, 23, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	want := time.Date(2026, 6, 2, 0, 0, 0, 0, time.UTC)
	if !next.Equal(want) {
		t.Fatalf("next = %s, want %s", next, want)
	}
	if _, err := NextCronTime("0 9 * * *", "asia/tokyo", time.Now()); err == nil {
		t.Fatal("lowercase timezone was accepted")
	}
	for _, name := range []string{"localtime", "posixrules", "posix/UTC", "right/UTC"} {
		if err := ValidateTimezone(name); err == nil {
			t.Fatalf("non-IANA tzfile %q was accepted", name)
		}
	}
}
