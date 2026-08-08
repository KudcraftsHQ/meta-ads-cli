package dispatch

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/KudcraftsHQ/meta-ads-cli/internal/tree"
	"github.com/spf13/pflag"
)

func newOp(params ...tree.Param) *tree.Op {
	return &tree.Op{Name: "create", Method: "POST", Endpoint: "/things", Params: params}
}

func bindAndParse(t *testing.T, op *tree.Op, args []string) (map[string]any, error) {
	t.Helper()
	fs := pflag.NewFlagSet("test", pflag.ContinueOnError)
	bindings := bindParams(fs, op, reservedFlags)
	if err := fs.Parse(args); err != nil {
		t.Fatalf("parse %v: %v", args, err)
	}
	return collect(fs, bindings)
}

func TestOnlyChangedFlagsAreSent(t *testing.T) {
	op := newOp(
		tree.Param{Name: "name", Flag: "name", Kind: tree.KindString},
		tree.Param{Name: "daily_budget", Flag: "daily-budget", Kind: tree.KindInt},
		tree.Param{Name: "paused", Flag: "paused", Kind: tree.KindBool},
	)
	got, err := bindAndParse(t, op, []string{"--name", "Launch"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got["name"] != "Launch" {
		t.Fatalf("unset flags must not be sent, got %v", got)
	}

	// A zero budget and an explicit false are both meaningful, so setting them
	// has to be distinguishable from leaving them alone.
	got, err = bindAndParse(t, op, []string{"--daily-budget", "0", "--paused=false"})
	if err != nil {
		t.Fatal(err)
	}
	if got["daily_budget"] != int64(0) {
		t.Errorf("explicit zero should be sent, got %#v", got["daily_budget"])
	}
	if got["paused"] != false {
		t.Errorf("explicit false should be sent, got %#v", got["paused"])
	}
}

func TestEnumValidation(t *testing.T) {
	op := newOp(tree.Param{
		Name: "objective", Flag: "objective", Kind: tree.KindEnum,
		Enum: []string{"OUTCOME_SALES", "OUTCOME_TRAFFIC"},
	})

	if _, err := bindAndParse(t, op, []string{"--objective", "OUTCOME_SALES"}); err != nil {
		t.Fatalf("a valid value was rejected: %v", err)
	}

	_, err := bindAndParse(t, op, []string{"--objective", "OUTCOME_NOPE"})
	if err == nil {
		t.Fatal("expected an unknown enum value to be rejected")
	}
	// The tree can lag Meta, so the error has to name the way around it.
	if !strings.Contains(err.Error(), "--params") {
		t.Errorf("error should point at the escape hatch: %v", err)
	}
	if !strings.Contains(err.Error(), "OUTCOME_SALES") {
		t.Errorf("error should list the accepted values: %v", err)
	}
}

func TestListAcceptsRepeatedFlagsAndJSONArrays(t *testing.T) {
	op := newOp(tree.Param{
		Name: "special_ad_categories", Flag: "special-ad-categories",
		Kind: tree.KindList, Item: tree.KindEnum, Enum: []string{"HOUSING", "NONE"},
	})

	got, err := bindAndParse(t, op, []string{"--special-ad-categories", "HOUSING", "--special-ad-categories", "NONE"})
	if err != nil {
		t.Fatal(err)
	}
	if s := string(got["special_ad_categories"].(json.RawMessage)); s != `["HOUSING","NONE"]` {
		t.Errorf("repeated flags = %s", s)
	}

	// A literal JSON array passes through untouched -- this is how you write
	// an empty list, which no amount of repeating a flag can express.
	got, err = bindAndParse(t, op, []string{"--special-ad-categories", "[]"})
	if err != nil {
		t.Fatal(err)
	}
	if s := string(got["special_ad_categories"].(json.RawMessage)); s != "[]" {
		t.Errorf("json array = %s", s)
	}
}

func TestListRejectsUnknownEnumItem(t *testing.T) {
	op := newOp(tree.Param{
		Name: "cats", Flag: "cats", Kind: tree.KindList,
		Item: tree.KindEnum, Enum: []string{"HOUSING"},
	})
	if _, err := bindAndParse(t, op, []string{"--cats", "CREDIT"}); err == nil {
		t.Fatal("expected an unknown list item to be rejected")
	}
}

func TestJSONParamMustBeValid(t *testing.T) {
	op := newOp(tree.Param{Name: "promoted_object", Flag: "promoted-object", Kind: tree.KindJSON, Type: "Object"})

	if _, err := bindAndParse(t, op, []string{"--promoted-object", `{"page_id":"1"}`}); err != nil {
		t.Fatalf("valid JSON was rejected: %v", err)
	}
	if _, err := bindAndParse(t, op, []string{"--promoted-object", `{page_id: 1}`}); err == nil {
		t.Fatal("expected invalid JSON to be rejected before the request went out")
	}
}

func TestReservedFlagsAreNotShadowed(t *testing.T) {
	// The insights edge has its own "fields" parameter; ours must win.
	op := newOp(tree.Param{Name: "fields", Flag: "fields", Kind: tree.KindList, Item: tree.KindString})
	fs := pflag.NewFlagSet("test", pflag.ContinueOnError)
	bindings := bindParams(fs, op, reservedFlags)
	if len(bindings) != 0 {
		t.Fatalf("a parameter colliding with a reserved flag must be skipped, got %d bindings", len(bindings))
	}
	if fs.Lookup("fields") != nil {
		t.Error("reserved flag was redefined by a generated parameter")
	}
}

func TestExplicitFlagsBeatParams(t *testing.T) {
	merged, err := mergeParams(`{"name":"from-params","other":"kept"}`, map[string]any{"name": "from-flag"})
	if err != nil {
		t.Fatal(err)
	}
	if merged["name"] != "from-flag" {
		t.Errorf("explicit flags should win, got %v", merged["name"])
	}
	if merged["other"] != "kept" {
		t.Errorf("unrelated --params keys should survive, got %v", merged["other"])
	}
}

func TestParamsMustBeObject(t *testing.T) {
	if _, err := mergeParams(`["not","an","object"]`, nil); err == nil {
		t.Fatal("expected a non-object --params to be rejected")
	}
}

func TestParamsStringsAreNotDoubleQuoted(t *testing.T) {
	merged, err := mergeParams(`{"fields":"id,name","limit":5}`, nil)
	if err != nil {
		t.Fatal(err)
	}
	if merged["fields"] != "id,name" {
		t.Errorf("string from --params = %#v, want an unquoted string", merged["fields"])
	}
	if string(merged["limit"].(json.RawMessage)) != "5" {
		t.Errorf("number from --params = %#v", merged["limit"])
	}
}

func TestJoinPath(t *testing.T) {
	cases := []struct{ node, endpoint, want string }{
		{"act_1", "/", "act_1"},
		{"act_1", "", "act_1"},
		{"act_1", "/campaigns", "act_1/campaigns"},
		{"123", "/insights", "123/insights"},
	}
	for _, c := range cases {
		if got := joinPath(c.node, c.endpoint); got != c.want {
			t.Errorf("joinPath(%q,%q) = %q, want %q", c.node, c.endpoint, got, c.want)
		}
	}
}
