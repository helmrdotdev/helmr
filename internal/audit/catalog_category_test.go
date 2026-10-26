package audit

import "testing"

func TestEveryActionHasACategory(t *testing.T) {
	for _, d := range Catalog() {
		if Category(d.Action) == "" {
			t.Errorf("%s has no category", d.Action)
		}
	}
}
