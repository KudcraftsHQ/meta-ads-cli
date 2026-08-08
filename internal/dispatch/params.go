package dispatch

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/KudcraftsHQ/meta-ads-cli/internal/tree"
	"github.com/spf13/pflag"
)

// binding ties one generated flag to the parameter it came from.
type binding struct {
	param  tree.Param
	str    *string
	num    *int64
	flt    *float64
	bl     *bool
	list   *[]string
	isList bool
}

// bindParams declares a flag per accepted parameter and returns the bindings so
// values can be collected after parsing.
func bindParams(fs *pflag.FlagSet, op *tree.Op, reserved map[string]bool) []*binding {
	bindings := make([]*binding, 0, len(op.Params))
	for _, p := range op.Params {
		flag := p.Flag
		if reserved[flag] {
			// A parameter whose name collides with one of our own flags stays
			// reachable through --params; shadowing --fields would be worse.
			continue
		}
		b := &binding{param: p}
		usage := usageFor(p)

		switch p.Kind {
		case tree.KindList:
			b.isList = true
			b.list = fs.StringArray(flag, nil, usage)
		case tree.KindBool:
			b.bl = fs.Bool(flag, false, usage)
		case tree.KindInt:
			b.num = fs.Int64(flag, 0, usage)
		case tree.KindFloat:
			b.flt = fs.Float64(flag, 0, usage)
		default:
			b.str = fs.String(flag, "", usage)
		}
		bindings = append(bindings, b)
	}
	return bindings
}

func usageFor(p tree.Param) string {
	var b strings.Builder
	b.WriteString(p.Type)
	if len(p.Enum) > 0 {
		b.WriteString(" (")
		b.WriteString(strings.Join(truncate(p.Enum, 6), "|"))
		if len(p.Enum) > 6 {
			fmt.Fprintf(&b, "|... %d more", len(p.Enum)-6)
		}
		b.WriteString(")")
	}
	return b.String()
}

func truncate(values []string, n int) []string {
	if len(values) <= n {
		return values
	}
	return values[:n]
}

// collect turns the flags the user actually set into wire values.
//
// Only changed flags are collected: a zero is a meaningful budget and false is
// a meaningful setting, so an unset flag has to be distinguishable from one set
// to its zero value.
func collect(fs *pflag.FlagSet, bindings []*binding) (map[string]any, error) {
	out := map[string]any{}
	for _, b := range bindings {
		if !fs.Changed(b.param.Flag) {
			continue
		}
		value, err := b.value()
		if err != nil {
			return nil, err
		}
		out[b.param.Name] = value
	}
	return out, nil
}

func (b *binding) value() (any, error) {
	p := b.param
	switch {
	case b.isList:
		return listValue(p, *b.list)
	case b.bl != nil:
		return *b.bl, nil
	case b.num != nil:
		return *b.num, nil
	case b.flt != nil:
		return *b.flt, nil
	}

	raw := *b.str
	switch p.Kind {
	case tree.KindEnum:
		if err := checkEnum(p, raw); err != nil {
			return nil, err
		}
		return raw, nil
	case tree.KindJSON:
		if !json.Valid([]byte(raw)) {
			return nil, fmt.Errorf("--%s expects JSON (%s), got: %s", p.Flag, p.Type, raw)
		}
		return json.RawMessage(raw), nil
	}
	return raw, nil
}

// listValue accepts either a JSON array literal in a single flag, or the flag
// repeated once per element. Both forms end up as a JSON array on the wire,
// which is what the Graph API wants for list<> parameters.
func listValue(p tree.Param, values []string) (any, error) {
	if len(values) == 1 {
		trimmed := strings.TrimSpace(values[0])
		if strings.HasPrefix(trimmed, "[") {
			if !json.Valid([]byte(trimmed)) {
				return nil, fmt.Errorf("--%s looks like a JSON array but does not parse: %s", p.Flag, trimmed)
			}
			return json.RawMessage(trimmed), nil
		}
	}

	items := make([]any, 0, len(values))
	for _, v := range values {
		item, err := coerceItem(p, v)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	raw, err := json.Marshal(items)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(raw), nil
}

func coerceItem(p tree.Param, v string) (any, error) {
	switch p.Item {
	case tree.KindInt:
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("--%s expects %s, got %q", p.Flag, p.Type, v)
		}
		return n, nil
	case tree.KindFloat:
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return nil, fmt.Errorf("--%s expects %s, got %q", p.Flag, p.Type, v)
		}
		return f, nil
	case tree.KindBool:
		bl, err := strconv.ParseBool(v)
		if err != nil {
			return nil, fmt.Errorf("--%s expects %s, got %q", p.Flag, p.Type, v)
		}
		return bl, nil
	case tree.KindEnum:
		if err := checkEnum(p, v); err != nil {
			return nil, err
		}
		return v, nil
	case tree.KindJSON:
		if !json.Valid([]byte(v)) {
			return nil, fmt.Errorf("--%s expects JSON objects (%s), got: %s", p.Flag, p.Type, v)
		}
		return json.RawMessage(v), nil
	}
	return v, nil
}

// checkEnum rejects values the SDK does not know about.
//
// The tree can lag Meta by a release, so the error names the escape hatch
// rather than leaving someone stuck behind a stale enum list.
func checkEnum(p tree.Param, v string) error {
	if len(p.Enum) == 0 {
		return nil
	}
	for _, allowed := range p.Enum {
		if allowed == v {
			return nil
		}
	}
	suggestions := closeMatches(v, p.Enum)
	msg := fmt.Sprintf("--%s: %q is not a known value", p.Flag, v)
	if len(suggestions) > 0 {
		msg += fmt.Sprintf(" (did you mean %s?)", strings.Join(suggestions, ", "))
	}
	msg += fmt.Sprintf("\n  accepted: %s", strings.Join(p.Enum, ", "))
	msg += fmt.Sprintf("\n  if Meta has added a value this build does not know, pass it through with --params '{\"%s\":\"%s\"}'", p.Name, v)
	return fmt.Errorf("%s", msg)
}

func closeMatches(v string, options []string) []string {
	upper := strings.ToUpper(v)
	var out []string
	for _, o := range options {
		if strings.Contains(strings.ToUpper(o), upper) || strings.Contains(upper, strings.ToUpper(o)) {
			out = append(out, o)
		}
	}
	sort.Strings(out)
	if len(out) > 4 {
		out = out[:4]
	}
	return out
}

// mergeParams layers explicit flags over a --params document. Flags win,
// because they are the more specific statement of intent.
func mergeParams(base string, flags map[string]any) (map[string]any, error) {
	out := map[string]any{}
	if strings.TrimSpace(base) != "" {
		var doc map[string]json.RawMessage
		if err := json.Unmarshal([]byte(base), &doc); err != nil {
			return nil, fmt.Errorf("--params must be a JSON object: %w", err)
		}
		for k, v := range doc {
			out[k] = unquoteScalar(v)
		}
	}
	for k, v := range flags {
		out[k] = v
	}
	return out, nil
}

// unquoteScalar keeps strings from --params as strings rather than re-encoding
// them with their quotes, which the Graph API would take literally.
func unquoteScalar(raw json.RawMessage) any {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	return raw
}
