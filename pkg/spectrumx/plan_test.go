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
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/Mellanox/nic-configuration-operator/pkg/types"
)

// opLeafString returns the dms-cli-stringified value set for <path> <leaf> across ops.
func opLeafString(ops []types.DMSConfigOp, path, leaf string) (string, bool) {
	for _, op := range ops {
		if op.Path != path {
			continue
		}
		if v, ok := op.Values[leaf]; ok {
			s, err := types.StringifyDMSValue(v)
			Expect(err).NotTo(HaveOccurred())
			return s, true
		}
	}
	return "", false
}

var _ = Describe("Plan parsing", func() {
	Describe("ParsePlan over the example plans", func() {
		It("parses the prepare plan groups and orders them", func() {
			plan, err := ParsePlan(examplePreparePlan)
			Expect(err).NotTo(HaveOccurred())
			Expect(plan.Stage).To(Equal("prepare"))

			names := make([]string, 0, len(plan.OrderedGroups()))
			for _, g := range plan.OrderedGroups() {
				names = append(names, g.Name)
			}
			Expect(names).To(Equal([]string{"breakout", "post-breakout-nvconfig"}))

			breakout, ok := plan.Group("breakout")
			Expect(ok).To(BeTrue())
			Expect(breakout.Order).To(Equal(10))
			Expect(breakout.Scope).To(Equal("per_device"))
		})

		It("parses the configure plan groups in ascending order", func() {
			plan, err := ParsePlan(exampleConfigurePlan)
			Expect(err).NotTo(HaveOccurred())
			Expect(plan.Stage).To(Equal("configure"))

			names := make([]string, 0, len(plan.OrderedGroups()))
			for _, g := range plan.OrderedGroups() {
				names = append(names, g.Name)
			}
			Expect(names).To(Equal([]string{"link-runtime", "eswitch", "cc", "link-event"}))

			cc, _ := plan.Group("cc")
			Expect(cc.Order).To(Equal(90))
			Expect(cc.Scope).To(Equal("per_rdma_bond"))
		})
	})

	Describe("SemanticGroup.Ops", func() {
		It("returns ops carrying the path and typed leaf values", func() {
			plan, err := ParsePlan(examplePreparePlan)
			Expect(err).NotTo(HaveOccurred())
			breakout, _ := plan.Group("breakout")

			ops, err := breakout.Ops(plan.Params)
			Expect(err).NotTo(HaveOccurred())

			v, ok := opLeafString(ops, "/nvidia/roce", "adaptive-routing")
			Expect(ok).To(BeTrue())
			Expect(v).To(Equal("true"))

			v, ok = opLeafString(ops, "/nvidia/roce", "cc-steering-ext")
			Expect(ok).To(BeTrue())
			Expect(v).To(Equal("enabled"))

			v, ok = opLeafString(ops, "/nvidia/pci", "num-pfs")
			Expect(ok).To(BeTrue())
			Expect(v).To(Equal("2"))
		})

		It("includes condition-true ops and renders list values (lanes)", func() {
			plan, err := ParsePlan(examplePreparePlan)
			Expect(err).NotTo(HaveOccurred())
			breakout, _ := plan.Group("breakout")

			ops, err := breakout.Ops(plan.Params)
			Expect(err).NotTo(HaveOccurred())

			// condition was @params.nic_type == '1023' (true) -> op included
			v, ok := opLeafString(ops, "/nvidia/link/breakout/module/[0]/port/[1]", "lanes")
			Expect(ok).To(BeTrue())
			Expect(v).To(Equal("[0,1,2,3,4,5,6,7]"))
		})

		It("returns the post-breakout-nvconfig ops", func() {
			plan, err := ParsePlan(examplePreparePlan)
			Expect(err).NotTo(HaveOccurred())
			nv, _ := plan.Group("post-breakout-nvconfig")

			ops, err := nv.Ops(plan.Params)
			Expect(err).NotTo(HaveOccurred())

			v, ok := opLeafString(ops, "/nvidia/link/type", "value")
			Expect(ok).To(BeTrue())
			Expect(v).To(Equal("ETH"))
		})

		It("parses the configure cc group with conditional ops satisfied", func() {
			plan, err := ParsePlan(exampleConfigurePlan)
			Expect(err).NotTo(HaveOccurred())
			cc, _ := plan.Group("cc")

			ops, err := cc.Ops(plan.Params)
			Expect(err).NotTo(HaveOccurred())

			// condition @params.nic_type == '1023' and @params.planes == 2 -> true
			v, ok := opLeafString(ops, "/nvidia/cc/algo/slot/[0]/param/[0]", "value")
			Expect(ok).To(BeTrue())
			Expect(v).To(Equal("400"))
		})
	})

	Describe("condition evaluation", func() {
		params := map[string]any{
			"nic_type":  "1023",
			"planes":    float64(2),
			"multiport": true,
		}

		It("includes ops with no condition", func() {
			ok, err := evalCondition(nil, params)
			Expect(err).NotTo(HaveOccurred())
			Expect(ok).To(BeTrue())
		})

		It("evaluates string equality", func() {
			ok, err := evalCondition(&condition{Knob: "@params.nic_type == '1023'"}, params)
			Expect(err).NotTo(HaveOccurred())
			Expect(ok).To(BeTrue())
		})

		It("evaluates a false clause", func() {
			ok, err := evalCondition(&condition{Knob: "@params.nic_type == '1025'"}, params)
			Expect(err).NotTo(HaveOccurred())
			Expect(ok).To(BeFalse())
		})

		It("evaluates int and bool clauses joined by and", func() {
			ok, err := evalCondition(&condition{Knob: "@params.planes == 2 and @params.multiport == True"}, params)
			Expect(err).NotTo(HaveOccurred())
			Expect(ok).To(BeTrue())
		})

		It("errors on an unknown param", func() {
			_, err := evalCondition(&condition{Knob: "@params.bogus == 1"}, params)
			Expect(err).To(HaveOccurred())
		})

		It("errors on an unsupported grammar", func() {
			_, err := evalCondition(&condition{Knob: "@params.planes > 1"}, params)
			Expect(err).To(HaveOccurred())
		})

		It("skips a false-condition op in Ops", func() {
			group := SemanticGroup{
				Name: "test",
				Operations: []Operation{
					{Path: "/a", Values: map[string]any{"x": true}, Condition: &condition{Knob: "@params.nic_type == 'nope'"}},
					{Path: "/b", Values: map[string]any{"y": float64(5)}},
				},
			}

			out, err := group.Ops(params)
			Expect(err).NotTo(HaveOccurred())
			Expect(out).To(HaveLen(1))
			Expect(out[0].Path).To(Equal("/b"))
		})
	})

	Describe("types.StringifyDMSValue", func() {
		It("renders bool/int/float/string/list", func() {
			v, err := types.StringifyDMSValue(true)
			Expect(err).NotTo(HaveOccurred())
			Expect(v).To(Equal("true"))

			v, _ = types.StringifyDMSValue(float64(400))
			Expect(v).To(Equal("400"))

			v, _ = types.StringifyDMSValue("ETH")
			Expect(v).To(Equal("ETH"))

			v, _ = types.StringifyDMSValue([]any{float64(0), float64(1)})
			Expect(v).To(Equal("[0,1]"))
		})

		It("errors on an unsupported type", func() {
			_, err := types.StringifyDMSValue(map[string]any{})
			Expect(err).To(HaveOccurred())
		})
	})
})
