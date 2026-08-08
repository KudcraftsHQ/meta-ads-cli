package dispatch

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/KudcraftsHQ/meta-ads-cli/internal/graph"
)

// run drives the CLI end to end against a stub Graph API, which is the only way
// to test the parts that only exist once flags, config, dispatch and the client
// are wired together.
func run(t *testing.T, srv *httptest.Server, args ...string) (string, string, error) {
	t.Helper()
	t.Setenv("META_ACCESS_TOKEN", "test-token-value")
	t.Setenv("META_ACCOUNT_ID", "act_1")
	t.Setenv("META_ADS_CONFIG", t.TempDir()+"/absent.toml")
	if srv != nil {
		t.Setenv("META_GRAPH_URL", srv.URL)
	}

	var out, errOut bytes.Buffer
	root := NewRoot(args, &out, &errOut)
	root.SetArgs(args)
	root.SetOut(&out)
	root.SetErr(&errOut)
	err := root.Execute()
	return out.String(), errOut.String(), err
}

func stub(body string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(body))
	}))
}

func TestGeneratedCommandCallsTheRightEndpoint(t *testing.T) {
	var gotMethod, gotPath, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := new(bytes.Buffer)
		buf.ReadFrom(r.Body)
		gotMethod, gotPath, gotBody = r.Method, r.URL.Path, buf.String()
		w.Write([]byte(`{"id":"120200"}`))
	}))
	defer srv.Close()

	out, _, err := run(t, srv, "ad-account", "create-campaign",
		"--name", "Launch", "--objective", "OUTCOME_SALES", "--special-ad-categories", "[]")
	if err != nil {
		t.Fatal(err)
	}
	if gotMethod != "POST" || !strings.HasSuffix(gotPath, "/act_1/campaigns") {
		t.Errorf("called %s %s", gotMethod, gotPath)
	}
	if !strings.Contains(gotBody, "objective=OUTCOME_SALES") {
		t.Errorf("body = %q", gotBody)
	}
	if !strings.Contains(out, "120200") {
		t.Errorf("stdout = %q", out)
	}
}

func TestAccountIDDefaultsFromEnvironment(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Write([]byte(`{"data":[]}`))
	}))
	defer srv.Close()

	if _, _, err := run(t, srv, "ad-account", "get-campaigns"); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(gotPath, "/act_1/campaigns") {
		t.Errorf("path = %q; --id should default to the configured account", gotPath)
	}
}

func TestBareAccountNumberGetsActPrefix(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Write([]byte(`{"id":"act_999"}`))
	}))
	defer srv.Close()

	if _, _, err := run(t, srv, "ad-account", "get", "--id", "999"); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(gotPath, "/act_999") {
		t.Errorf("path = %q, want the act_ prefix added", gotPath)
	}
}

func TestPaginationWithAll(t *testing.T) {
	var srv *httptest.Server
	calls := 0
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.Write([]byte(`{"data":[{"id":"a"}],"paging":{"next":"` + srv.URL + `/page2"}}`))
			return
		}
		w.Write([]byte(`{"data":[{"id":"b"}]}`))
	}))
	defer srv.Close()

	out, _, err := run(t, srv, "ad-account", "get-campaigns", "--all")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Data []map[string]string `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("output was not JSON: %q", out)
	}
	if len(doc.Data) != 2 {
		t.Errorf("expected both pages, got %v", doc.Data)
	}
}

func TestNDJSONStreamsOneItemPerLine(t *testing.T) {
	srv := stub(`{"data":[{"id":"a"},{"id":"b"},{"id":"c"}]}`)
	defer srv.Close()

	out, _, err := run(t, srv, "ad-account", "get-campaigns", "--all", "-o", "ndjson")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 3 {
		t.Fatalf("expected 3 lines, got %d: %q", len(lines), out)
	}
	for _, line := range lines {
		var v map[string]string
		if err := json.Unmarshal([]byte(line), &v); err != nil {
			t.Errorf("line %q is not JSON: %v", line, err)
		}
	}
}

func TestTableOutput(t *testing.T) {
	srv := stub(`{"data":[{"name":"Launch","id":"1","spend":"12.5"},{"name":"Retarget","id":"2","spend":"3"}]}`)
	defer srv.Close()

	out, _, err := run(t, srv, "ad-account", "get-campaigns", "-o", "table")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 3 {
		t.Fatalf("expected a header and two rows, got %q", out)
	}
	// id and name are what anybody scans a table for, so they lead.
	if !strings.HasPrefix(lines[0], "ID") || !strings.Contains(lines[0], "NAME") {
		t.Errorf("header = %q", lines[0])
	}
}

func TestCSVOutput(t *testing.T) {
	srv := stub(`{"data":[{"id":"1","name":"Launch"}]}`)
	defer srv.Close()

	out, _, err := run(t, srv, "ad-account", "get-campaigns", "-o", "csv")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "id,name") || !strings.Contains(out, "1,Launch") {
		t.Errorf("csv = %q", out)
	}
}

func TestRawCommand(t *testing.T) {
	var gotPath, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery = r.URL.Path, r.URL.RawQuery
		w.Write([]byte(`{"data":[]}`))
	}))
	defer srv.Close()

	if _, _, err := run(t, srv, "raw", "GET", "act_1/ads", "--params", `{"fields":"id,name","limit":5}`); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(gotPath, "/act_1/ads") {
		t.Errorf("path = %q", gotPath)
	}
	if !strings.Contains(gotQuery, "limit=5") {
		t.Errorf("query = %q", gotQuery)
	}
}

func TestInsightsDefaults(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		w.Write([]byte(`{"data":[]}`))
	}))
	defer srv.Close()

	if _, _, err := run(t, srv, "insights", "--level", "campaign"); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"date_preset=last_30d", "level=campaign", "campaign_name", "spend"} {
		if !strings.Contains(gotQuery, want) {
			t.Errorf("query %q missing %q", gotQuery, want)
		}
	}
}

func TestInsightsRejectsBadLevel(t *testing.T) {
	_, _, err := run(t, nil, "insights", "--level", "adgroup", "--dry-run")
	if err == nil || !strings.Contains(err.Error(), "--level") {
		t.Fatalf("expected a level validation error, got %v", err)
	}
}

func TestDiscoveryWorksWithoutCredentials(t *testing.T) {
	t.Setenv("META_ADS_CONFIG", t.TempDir()+"/absent.toml")
	var out, errOut bytes.Buffer
	args := []string{"describe", "campaign", "get"}
	root := NewRoot(args, &out, &errOut)
	root.SetArgs(args)
	root.SetOut(&out)
	root.SetErr(&errOut)
	if err := root.Execute(); err != nil {
		t.Fatalf("describe should not require a token: %v", err)
	}
	if !strings.Contains(out.String(), "meta-ads campaign get") {
		t.Errorf("output = %q", out.String())
	}
}

func TestUnknownResourceSuggests(t *testing.T) {
	_, _, err := run(t, nil, "describe", "ad-acount")
	if err == nil {
		t.Fatal("expected an error for a mistyped resource")
	}
	if !strings.Contains(err.Error(), "ad-account") {
		t.Errorf("error should suggest the right name: %v", err)
	}
}

func TestDryRunSendsNothingEndToEnd(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))
	defer srv.Close()

	out, _, err := run(t, srv, "ad-account", "create-campaign", "--name", "X", "--dry-run")
	if err != nil {
		t.Fatal(err)
	}
	if called {
		t.Fatal("--dry-run must not reach the API")
	}
	if !strings.Contains(out, `"dry_run":true`) {
		t.Errorf("output = %q", out)
	}
	if strings.Contains(out, "test-token-value") {
		t.Error("--dry-run leaked the access token")
	}
}

func TestAPIErrorMapsToExitCode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":{"message":"Invalid OAuth access token","code":190}}`))
	}))
	defer srv.Close()

	_, _, err := run(t, srv, "ad-account", "get")
	if err == nil {
		t.Fatal("expected the API error to surface")
	}
	if got := graph.ExitCode(err); got != 3 {
		t.Errorf("exit code = %d, want 3 for an auth failure", got)
	}
}
