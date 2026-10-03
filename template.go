package symphony

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/osteele/liquid"
	"github.com/osteele/liquid/expressions"
	"github.com/osteele/liquid/filters"
	"github.com/osteele/liquid/parser"
	"github.com/osteele/liquid/render"
	"github.com/osteele/liquid/tags"
	"github.com/osteele/liquid/values"
)

// The engine's built-in StrictVariables conflates known null with missing and
// checks only output expressions. Its AST provides scopes and statements; this
// pass checks references even in inactive branches and empty loops. Map lookup
// additionally checks dynamic bracket keys during real evaluation.
var liquidTokens = regexp.MustCompile(`"(?:[^"\\]|\\.)*"|'(?:[^'\\]|\\.)*'|[A-Za-z_][A-Za-z0-9_-]*|[0-9]+|[^\s]`)
var liquidIdentifier = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_-]*$`)

type nullOutput struct{}

func (nullOutput) ToLiquid() any { return nil }

type knownNull struct{ values.Value }

func (knownNull) Interface() any { return nullOutput{} }

type strictMap struct {
	values.Value
	object map[string]any
}

func (m strictMap) PropertyValue(key values.Value) values.Value {
	name, ok := key.Interface().(string)
	if !ok {
		panic(fmt.Errorf("unknown map property"))
	}
	value, ok := m.object[name]
	if !ok {
		panic(fmt.Errorf("unknown variable property %q", name))
	}
	return values.ValueOf(value)
}
func (m strictMap) IndexValue(key values.Value) values.Value { return m.PropertyValue(key) }
func strictValue(v any) any {
	switch x := v.(type) {
	case map[string]any:
		object := map[string]any{}
		for k, item := range x {
			object[k] = strictValue(item)
		}
		return strictMap{Value: values.ValueOf(object), object: object}
	case nil:
		return knownNull{values.ValueOf(nil)}
	case []any:
		out := make([]any, len(x))
		for i, item := range x {
			out[i] = strictValue(item)
		}
		return out
	default:
		return v
	}
}

func RenderPrompt(w *Workflow, issue Issue, attempt *int) (prompt string, err error) {
	defer func() {
		if p := recover(); p != nil {
			prompt = ""
			err = fault("template_render_error", fmt.Sprint(p))
		}
	}()
	if w.Prompt == "" {
		return "You are working on an issue from the configured tracker.", nil
	}
	engine := liquid.NewEngine()
	engine.StrictVariables()
	template, parseErr := engine.ParseString(w.Prompt)
	if parseErr != nil {
		return "", fault("template_parse_error", parseErr.Error())
	}
	data, marshalErr := json.Marshal(issue)
	if marshalErr != nil {
		return "", marshalErr
	}
	var object map[string]any
	if err = json.Unmarshal(data, &object); err != nil {
		return "", err
	}
	var attemptValue any
	if attempt != nil {
		attemptValue = *attempt
	}
	if err = validateTemplate(w.Prompt, object, attemptValue, engine); err != nil {
		return "", fault("template_render_error", err.Error())
	}
	result, renderErr := template.RenderString(liquid.Bindings{"issue": strictValue(object), "attempt": strictValue(attemptValue)})
	if renderErr != nil {
		return "", fault("template_render_error", renderErr.Error())
	}
	return result, nil
}

func validateTemplate(source string, issue map[string]any, attempt any, engine *liquid.Engine) error {
	cfg := render.NewConfig()
	filters.AddStandardFilters(&cfg)
	tags.AddStandardTags(cfg)
	root, err := cfg.Parse(source, parser.SourceLoc{})
	if err != nil {
		return err
	}
	schema := map[string]any{}
	for k, v := range issue {
		schema[k] = v
	}
	schema["blocked_by"] = []any{map[string]any{"id": nil, "identifier": nil, "state": nil}}
	schema["labels"] = []any{""}
	names := map[string]any{"issue": schema, "attempt": attempt}
	keywords := map[string]bool{"true": true, "false": true, "nil": true, "null": true, "empty": true, "blank": true, "and": true, "or": true, "contains": true, "in": true, "reversed": true, "offset": true, "limit": true, "cols": true, "continue": true}
	check := func(source string) error {
		tokens := liquidTokens.FindAllString(source, -1)
		for i := 0; i < len(tokens); i++ {
			name := tokens[i]
			if !liquidIdentifier.MatchString(name) {
				continue
			}
			if i > 0 && tokens[i-1] == "|" {
				_, e := engine.ParseAndRenderString(`{{ "" | `+name+` }}`, nil)
				if e != nil && strings.Contains(e.Error(), "undefined filter") {
					return fmt.Errorf("unknown filter %q", name)
				}
				continue
			}
			if keywords[name] {
				continue
			}
			if i > 0 && tokens[i-1] == "." {
				continue
			}
			value, ok := names[name]
			if !ok {
				return fmt.Errorf("unknown variable %q", name)
			}
			for i+1 < len(tokens) {
				field := ""
				consumed := 0
				if i+2 < len(tokens) && tokens[i+1] == "." && liquidIdentifier.MatchString(tokens[i+2]) {
					field = tokens[i+2]
					consumed = 2
				}
				if i+3 < len(tokens) && tokens[i+1] == "[" && tokens[i+3] == "]" {
					key := tokens[i+2]
					if strings.HasPrefix(key, "\"") || strings.HasPrefix(key, "'") {
						field = key[1 : len(key)-1]
						consumed = 3
					} else if _, e := strconv.Atoi(key); e == nil {
						field = key
						consumed = 3
					} else if keyValue, ok := names[key]; ok {
						if s, ok := keyValue.(string); ok {
							field = s
							consumed = 3
						}
					}
				}
				if consumed == 0 {
					break
				}
				i += consumed
				switch v := value.(type) {
				case map[string]any:
					var found bool
					value, found = v[field]
					if !found {
						return fmt.Errorf("unknown variable property %q", field)
					}
				case []any:
					if _, e := strconv.Atoi(field); e == nil && len(v) > 0 {
						value = v[0]
					} else if field == "size" {
						value = len(v)
					} else if (field == "first" || field == "last") && len(v) > 0 {
						value = v[0]
					} else {
						return fmt.Errorf("unknown array property %q", field)
					}
				case string:
					if field == "size" {
						value = len(v)
					} else {
						return fmt.Errorf("unknown string property %q", field)
					}
				default:
					return fmt.Errorf("unknown variable property %q", field)
				}
			}
		}
		return nil
	}
	var walk func(parser.ASTNode) error
	walk = func(node parser.ASTNode) error {
		switch n := node.(type) {
		case *parser.ASTSeq:
			for _, child := range n.Children {
				if e := walk(child); e != nil {
					return e
				}
			}
		case *parser.ASTObject:
			return check(n.Args)
		case *parser.ASTTag:
			if n.Name == "assign" {
				statement, e := expressions.ParseStatement(expressions.AssignStatementSelector, n.Args)
				if e != nil {
					return e
				}
				_, rhs, ok := strings.Cut(n.Args, "=")
				if !ok {
					return fmt.Errorf("invalid assignment")
				}
				if e = check(rhs); e != nil {
					return e
				}
				value, e := statement.ValueFn.Evaluate(expressions.NewContext(names, cfg.Config.Config))
				if e != nil {
					return e
				}
				names[statement.Assignment.Variable] = value
			} else if e := check(n.Args); e != nil {
				return e
			}
		case *parser.ASTBlock:
			switch n.Name {
			case "if", "unless":
				if e := check(n.Args); e != nil {
					return e
				}
				clone := func(source map[string]any) map[string]any {
					copy := map[string]any{}
					for k, v := range source {
						copy[k] = v
					}
					return copy
				}
				base := clone(names)
				evaluate := func(condition string) (bool, error) {
					bindings := clone(base)
					bindings["issue"] = issue
					value, e := expressions.EvaluateString(condition, expressions.NewContext(bindings, cfg.Config.Config))
					if e != nil {
						return false, e
					}
					return values.ValueOf(value).Test(), nil
				}
				selected, e := evaluate(n.Args)
				if e != nil {
					return e
				}
				if n.Name == "unless" {
					selected = !selected
				}
				for _, child := range n.Body {
					if e := walk(child); e != nil {
						return e
					}
				}
				result := base
				if selected {
					result = clone(names)
				}
				for _, clause := range n.Clauses {
					names = clone(base)
					take := clause.Name == "else"
					if clause.Name == "elsif" {
						if e := check(clause.Args); e != nil {
							return e
						}
						take, e = evaluate(clause.Args)
						if e != nil {
							return e
						}
					}
					if e := walk(clause); e != nil {
						return e
					}
					if !selected && take {
						result = clone(names)
						selected = true
					}
				}
				names = result
				return nil
			case "capture":
				name := strings.TrimSpace(n.Args)
				names[name] = ""
			case "for", "tablerow":
				statement, e := expressions.ParseStatement(expressions.LoopStatementSelector, n.Args)
				if e != nil {
					return e
				}
				// The upstream statement parser already validated the binding.
				rhs := strings.TrimSpace(strings.TrimSpace(n.Args)[len(statement.Loop.Variable):])
				rhs = strings.TrimSpace(strings.TrimPrefix(rhs, "in"))
				if e = check(rhs); e != nil {
					return e
				}
				collection, e := statement.Expr.Evaluate(expressions.NewContext(names, cfg.Config.Config))
				if e != nil {
					return e
				}
				oldVar, hadVar := names[statement.Loop.Variable]
				loopName := n.Name + "loop"
				oldLoop, hadLoop := names[loopName]
				names[statement.Loop.Variable] = nil
				if a, ok := collection.([]any); ok && len(a) > 0 {
					names[statement.Loop.Variable] = a[0]
				}
				names[loopName] = map[string]any{"index": 1, "index0": 0, "rindex": 1, "rindex0": 0, "first": true, "last": true, "length": 1, "parentloop": oldLoop, "col": 1, "col0": 0, "col_first": true, "col_last": true}
				defer func() {
					if hadVar {
						names[statement.Loop.Variable] = oldVar
					} else {
						delete(names, statement.Loop.Variable)
					}
					if hadLoop {
						names[loopName] = oldLoop
					} else {
						delete(names, loopName)
					}
				}()
			default:
				if e := check(n.Args); e != nil {
					return e
				}
			}
			for _, child := range n.Body {
				if e := walk(child); e != nil {
					return e
				}
			}
			for _, clause := range n.Clauses {
				if e := walk(clause); e != nil {
					return e
				}
			}
		}
		return nil
	}
	return walk(root)
}
