/*
2026 NVIDIA CORPORATION & AFFILIATES
Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package spectrumx

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/Mellanox/nic-configuration-operator/pkg/types"
)

// Plan is the parsed do-SPCX plan (the relevant subset of the planner JSON).
type Plan struct {
	Stage          string                   `json:"stage"`
	Profile        string                   `json:"profile"`
	Params         map[string]any           `json:"params"`
	SemanticGroups map[string]SemanticGroup `json:"semantic_groups"`
}

// SemanticGroup is one ordered, scoped bucket of operations.
type SemanticGroup struct {
	Name        string      `json:"-"`
	Order       int         `json:"order"`
	Scope       string      `json:"scope"`
	FanoutOrder string      `json:"fanout_order,omitempty"`
	Operations  []Operation `json:"operations"`
}

// Operation is one typed knob op against a YANG path. A single op may carry several
// field→value pairs under one path; each expands to its own ConfigurationParameter.
type Operation struct {
	Path          string         `json:"path"`
	Values        map[string]any `json:"values"`
	TargetClass   string         `json:"target_class"`
	SourceFeature string         `json:"source_feature"`
	PreparePhase  string         `json:"prepare_phase,omitempty"`
	Scope         string         `json:"scope,omitempty"`
	Condition     *condition     `json:"condition,omitempty"`
}

type condition struct {
	Knob string `json:"knob"`
}

// planEnvelope unwraps the top-level {"plan": {...}} object.
type planEnvelope struct {
	Plan Plan `json:"plan"`
}

// ParsePlan unmarshals a do-SPCX plan and fills in each group's Name.
func ParsePlan(data []byte) (*Plan, error) {
	var env planEnvelope
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, fmt.Errorf("failed to parse do-SPCX plan: %w", err)
	}
	plan := env.Plan
	for name, group := range plan.SemanticGroups {
		group.Name = name
		plan.SemanticGroups[name] = group
	}
	return &plan, nil
}

// OrderedGroups returns the plan's semantic groups sorted by ascending order.
func (p *Plan) OrderedGroups() []SemanticGroup {
	groups := make([]SemanticGroup, 0, len(p.SemanticGroups))
	for _, group := range p.SemanticGroups {
		groups = append(groups, group)
	}
	sort.SliceStable(groups, func(i, j int) bool { return groups[i].Order < groups[j].Order })
	return groups
}

// Group returns the named semantic group.
func (p *Plan) Group(name string) (SemanticGroup, bool) {
	group, ok := p.SemanticGroups[name]
	return group, ok
}

// Ops returns the group's operations as DMS config ops (path + leaf values), in plan
// order. Operations whose condition evaluates false against planParams are skipped;
// unparseable conditions return an error. Each op maps 1:1 onto a dms-cli invocation.
func (g SemanticGroup) Ops(planParams map[string]any) ([]types.DMSConfigOp, error) {
	var ops []types.DMSConfigOp
	for _, op := range g.Operations {
		include, err := evalCondition(op.Condition, planParams)
		if err != nil {
			return nil, fmt.Errorf("group %q op %q: %w", g.Name, op.Path, err)
		}
		if !include {
			continue
		}
		if len(op.Values) == 0 {
			continue
		}
		ops = append(ops, types.DMSConfigOp{Path: op.Path, Values: op.Values})
	}
	return ops, nil
}

// evalCondition evaluates a do-SPCX operation condition against the plan params.
// Returns true (include the op) when there is no condition. The supported grammar
// is the one emitted by host-k8s plans: one or more `@params.<key> == <literal>`
// clauses joined by ` and `, where <literal> is a quoted string, an integer, or a
// Python bool (True/False). Anything outside that grammar is an error rather than a
// silent apply (open item O4).
func evalCondition(c *condition, planParams map[string]any) (bool, error) {
	if c == nil || strings.TrimSpace(c.Knob) == "" {
		return true, nil
	}
	for _, clause := range strings.Split(c.Knob, " and ") {
		ok, err := evalClause(strings.TrimSpace(clause), planParams)
		if err != nil {
			return false, err
		}
		if !ok {
			return false, nil
		}
	}
	return true, nil
}

func evalClause(clause string, planParams map[string]any) (bool, error) {
	lhs, rhs, found := strings.Cut(clause, "==")
	if !found {
		return false, fmt.Errorf("unsupported condition clause %q", clause)
	}
	lhs = strings.TrimSpace(lhs)
	key, ok := strings.CutPrefix(lhs, "@params.")
	if !ok || strings.Contains(key, ".") {
		return false, fmt.Errorf("unsupported condition lhs %q", lhs)
	}

	actual, ok := planParams[key]
	if !ok {
		return false, fmt.Errorf("condition references unknown param %q", key)
	}

	want, err := normalizeLiteral(strings.TrimSpace(rhs))
	if err != nil {
		return false, err
	}
	return normalizeParam(actual) == want, nil
}

// normalizeLiteral canonicalizes a condition literal to a comparison string.
func normalizeLiteral(lit string) (string, error) {
	switch {
	case len(lit) >= 2 && lit[0] == '\'' && lit[len(lit)-1] == '\'':
		return lit[1 : len(lit)-1], nil
	case lit == "True":
		return "true", nil
	case lit == "False":
		return "false", nil
	default:
		if _, err := strconv.Atoi(lit); err == nil {
			return lit, nil
		}
		return "", fmt.Errorf("unsupported condition literal %q", lit)
	}
}

// normalizeParam canonicalizes a JSON param value to a comparison string.
func normalizeParam(v any) string {
	switch val := v.(type) {
	case bool:
		return strconv.FormatBool(val)
	case float64:
		if val == math.Trunc(val) {
			return strconv.FormatInt(int64(val), 10)
		}
		return strconv.FormatFloat(val, 'f', -1, 64)
	case string:
		return val
	default:
		return fmt.Sprintf("%v", val)
	}
}
