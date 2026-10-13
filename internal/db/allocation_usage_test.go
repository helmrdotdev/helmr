package db

import (
	"os"
	"regexp"
	"testing"
)

func TestAllocationUsageCopiesStayIdentical(t *testing.T) {
	copies := regexp.MustCompile(`(?s)\((SELECT 'computer'::text kind,environment_id,.*?)\) allocation_usage`)
	for _, name := range []string{"allocation_planning.sql", "worker_capacity_hosts.sql", "worker_drain.sql", "worker_groups.sql", "worker_lifecycle.sql", "worker_startup.sql"} {
		t.Run(name, func(t *testing.T) {
			source, err := os.ReadFile("query/" + name)
			if err != nil {
				t.Fatal(err)
			}
			matches := copies.FindAllSubmatch(source, -1)
			if len(matches) == 0 {
				t.Fatal("physical occupancy must use the shared allocation relation")
			}
			for _, match := range matches {
				if string(match[1]) != AllocationUsageSQL {
					t.Fatal("sqlc allocation usage diverges from allocator usage")
				}
			}
		})
	}
}
