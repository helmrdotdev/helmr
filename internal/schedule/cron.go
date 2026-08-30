package schedule

import (
	_ "embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/robfig/cron/v3"
)

const CronSemanticsVersion = "robfig-cron-v3.0.1/standard-5-field"

var cronParser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)

var (
	locationCache sync.Map
	zoneNamesOnce sync.Once
	zoneNames     map[string]struct{}
)

const tzdbRoot = "/usr/share/zoneinfo"

//go:embed tzdb_names.txt
var zoneNamesManifest string

func ValidateCron(expression string) error {
	if _, err := cronParser.Parse(expression); err != nil {
		return fmt.Errorf("cron is invalid: %w", err)
	}
	return nil
}

func ValidateTimezone(name string) error {
	if name == "" || name != strings.TrimSpace(name) || name == "Local" {
		return errors.New("timezone must be an exact IANA timezone identifier")
	}
	if err := validateZoneName(name); err != nil {
		return errors.New("timezone must be an exact IANA timezone identifier")
	}
	return nil
}

func NextCronTime(expression string, timezone string, anchor time.Time) (time.Time, error) {
	if err := ValidateCron(expression); err != nil {
		return time.Time{}, err
	}
	if err := ValidateTimezone(timezone); err != nil {
		return time.Time{}, err
	}
	loc, err := loadLocation(timezone)
	if err != nil {
		return time.Time{}, fmt.Errorf("load timezone rules: %w", err)
	}
	spec, _ := cronParser.Parse(expression)
	next := spec.Next(anchor.In(loc)).UTC()
	if next.IsZero() {
		return time.Time{}, errors.New("cron has no future occurrences")
	}
	return next, nil
}

func NextCronTimes(expression string, timezone string, anchor time.Time, count int) ([]time.Time, error) {
	if count < 0 {
		return nil, errors.New("cron occurrence count cannot be negative")
	}
	if err := ValidateCron(expression); err != nil {
		return nil, err
	}
	if err := ValidateTimezone(timezone); err != nil {
		return nil, err
	}
	loc, err := loadLocation(timezone)
	if err != nil {
		return nil, fmt.Errorf("load timezone rules: %w", err)
	}
	spec, _ := cronParser.Parse(expression)
	result := make([]time.Time, 0, count)
	cursor := anchor.In(loc)
	for len(result) < count {
		next := spec.Next(cursor)
		if next.IsZero() {
			return nil, errors.New("cron has no future occurrences")
		}
		result = append(result, next.UTC())
		cursor = next
	}
	return result, nil
}

func loadLocation(name string) (*time.Location, error) {
	if cached, ok := locationCache.Load(name); ok {
		return cached.(*time.Location), nil
	}
	if err := validateZoneName(name); err != nil {
		return nil, err
	}
	location, err := loadLocationFromRoot(name, tzdbRoot)
	if err != nil {
		return nil, err
	}
	actual, _ := locationCache.LoadOrStore(name, location)
	return actual.(*time.Location), nil
}

func loadLocationFromRoot(name string, root string) (*time.Location, error) {
	if filepath.IsAbs(name) || strings.Contains(name, `\`) {
		return nil, errors.New("timezone path must be relative")
	}
	parts := strings.Split(name, "/")
	current := root
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return nil, errors.New("timezone path is invalid")
		}
		entries, err := os.ReadDir(current)
		if err != nil {
			return nil, err
		}
		found := false
		for _, entry := range entries {
			if entry.Name() == part {
				found = true
				break
			}
		}
		if !found {
			return nil, errors.New("timezone identifier does not exist")
		}
		current = filepath.Join(current, part)
	}
	data, err := os.ReadFile(current)
	if err != nil {
		return nil, err
	}
	location, err := time.LoadLocationFromTZData(name, data)
	if err != nil {
		return nil, err
	}
	return location, nil
}

func validateZoneName(name string) error {
	zoneNamesOnce.Do(func() {
		zoneNames = make(map[string]struct{})
		for zoneName := range strings.FieldsSeq(zoneNamesManifest) {
			zoneNames[zoneName] = struct{}{}
		}
	})
	if _, ok := zoneNames[name]; !ok {
		return errors.New("timezone identifier is absent from the pinned tzdb manifest")
	}
	return nil
}
