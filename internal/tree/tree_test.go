package tree

import (
	"testing"
)

// The tree is generated, so these tests guard the shape of the generated data
// rather than the code that reads it: a generator change that silently drops
// enums or renames the account resource would break the CLI everywhere at once.

func TestMetaIsPresent(t *testing.T) {
	m, err := LoadMeta()
	if err != nil {
		t.Fatal(err)
	}
	if m.APIVersion == "" || m.APIVersion[0] != 'v' {
		t.Errorf("api version = %q, want something like v26.0", m.APIVersion)
	}
	if m.GraphURL == "" || m.GraphVideoURL == "" {
		t.Errorf("graph urls missing: %+v", m)
	}
}

func TestResourceNamesAreComplete(t *testing.T) {
	names, err := ResourceNames()
	if err != nil {
		t.Fatal(err)
	}
	if len(names) < 100 {
		t.Fatalf("only %d resources; the generator probably failed", len(names))
	}
	for _, want := range []string{"ad-account", "campaign", "ad-set", "ad", "ad-creative"} {
		if !HasResource(want) {
			t.Errorf("missing core resource %q", want)
		}
	}
	if HasResource("abstract-crud-object") {
		t.Error("SDK base classes should not be exposed as resources")
	}
}

func TestAdAccountShape(t *testing.T) {
	res, err := LoadResource("ad-account")
	if err != nil {
		t.Fatal(err)
	}
	if res.Class != "AdAccount" {
		t.Errorf("class = %q", res.Class)
	}
	if len(res.Fields) == 0 {
		t.Error("ad-account should expose selectable fields")
	}

	get, ok := res.Op("get")
	if !ok {
		t.Fatal("ad-account has no get operation")
	}
	if get.Method != "GET" || get.APIType != TypeNode {
		t.Errorf("get = %s %s (%s)", get.Method, get.Endpoint, get.APIType)
	}

	create, ok := res.Op("create-campaign")
	if !ok {
		t.Fatal("ad-account has no create-campaign operation")
	}
	if create.Method != "POST" || create.Endpoint != "/campaigns" || create.APIType != TypeEdge {
		t.Errorf("create-campaign = %s %s (%s)", create.Method, create.Endpoint, create.APIType)
	}

	// Enum resolution is the part of generation most likely to break quietly:
	// the values live in a different SDK module from the method that uses them.
	objective, ok := create.Param("objective")
	if !ok {
		t.Fatal("create-campaign has no objective parameter")
	}
	if objective.Kind != KindEnum {
		t.Errorf("objective kind = %q", objective.Kind)
	}
	if !contains(objective.Enum, "OUTCOME_SALES") {
		t.Errorf("objective enum did not resolve: %v", objective.Enum)
	}

	cats, ok := create.Param("special_ad_categories")
	if !ok {
		t.Fatal("create-campaign has no special_ad_categories parameter")
	}
	if cats.Kind != KindList || cats.Item != KindEnum {
		t.Errorf("special_ad_categories = kind %q item %q", cats.Kind, cats.Item)
	}
	if !contains(cats.Enum, "HOUSING") {
		t.Errorf("list enum did not resolve: %v", cats.Enum)
	}
}

func TestInsightsEdgeExists(t *testing.T) {
	res, err := LoadResource("ad-account")
	if err != nil {
		t.Fatal(err)
	}
	op, ok := res.Op("get-insights")
	if !ok {
		t.Fatal("ad-account has no get-insights operation")
	}
	level, ok := op.Param("level")
	if !ok {
		t.Fatal("get-insights has no level parameter")
	}
	for _, want := range []string{"account", "campaign", "adset", "ad"} {
		if !contains(level.Enum, want) {
			t.Errorf("level enum missing %q: %v", want, level.Enum)
		}
	}
}

func TestUnknownResource(t *testing.T) {
	if _, err := LoadResource("no-such-resource"); err == nil {
		t.Fatal("expected an error for an unknown resource")
	}
	if HasResource("no-such-resource") {
		t.Error("HasResource should be false for an unknown name")
	}
}

func TestSuggest(t *testing.T) {
	if got := Suggest("ad-acc", 5); !contains(got, "ad-account") {
		t.Errorf("Suggest(ad-acc) = %v", got)
	}
	if got := Suggest("campaig", 5); !contains(got, "campaign") {
		t.Errorf("Suggest(campaig) = %v", got)
	}
}

func TestParamLookupAcceptsFlagForm(t *testing.T) {
	res, _ := LoadResource("ad-account")
	op, _ := res.Op("create-campaign")
	byName, okName := op.Param("daily_budget")
	byFlag, okFlag := op.Param("daily-budget")
	if !okName || !okFlag || byName.Name != byFlag.Name {
		t.Errorf("a parameter should be findable by API name and by flag name")
	}
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
