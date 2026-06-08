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
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/Mellanox/nic-configuration-operator/api/v1alpha1"
)

// spcxDevice builds a NicDevice with a SpectrumXOptimized spec for spectrumx tests.
func spcxDevice(name string, pcis ...string) *v1alpha1.NicDevice {
	ports := make([]v1alpha1.NicDevicePortSpec, 0, len(pcis))
	for _, pci := range pcis {
		ports = append(ports, v1alpha1.NicDevicePortSpec{PCI: pci})
	}
	return &v1alpha1.NicDevice{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: v1alpha1.NicDeviceSpec{
			Configuration: &v1alpha1.NicDeviceConfigurationSpec{
				Template: &v1alpha1.ConfigurationTemplateSpec{
					SpectrumXOptimized: &v1alpha1.SpectrumXOptimizedSpec{
						Enabled:        true,
						Version:        "RA2.2",
						MultiplaneMode: "hwplb",
						NumberOfPlanes: 2,
					},
				},
			},
		},
		Status: v1alpha1.NicDeviceStatus{
			Type:  "1023",
			Ports: ports,
		},
	}
}

var _ = Describe("Plan generation", func() {
	Describe("spectrumXSpecToPlannerArgs", func() {
		It("maps hwplb onto the ra2.2-hwmp profile (matches the example plan)", func() {
			args := spectrumXSpecToPlannerArgs(&v1alpha1.SpectrumXOptimizedSpec{
				Version: "RA2.2", MultiplaneMode: "hwplb", NumberOfPlanes: 2, Overlay: "none",
			})
			Expect(args.Family).To(Equal("spcx"))
			Expect(args.Profile).To(Equal("ra2.2-hwmp"))
			Expect(args.Params).To(Equal(map[string]string{
				"deployment_mode": "host-k8s",
				"multiplane_mode": "hwmp",
				"planes":          "2",
				"overlay":         "none",
			}))
		})

		It("maps swplb onto swmp", func() {
			args := spectrumXSpecToPlannerArgs(&v1alpha1.SpectrumXOptimizedSpec{
				Version: "RA2.2", MultiplaneMode: "swplb", NumberOfPlanes: 4, Overlay: "l3",
			})
			Expect(args.Profile).To(Equal("ra2.2-swmp"))
			Expect(args.Params["multiplane_mode"]).To(Equal("swmp"))
			Expect(args.Params["planes"]).To(Equal("4"))
			Expect(args.Params["overlay"]).To(Equal("l3"))
		})

		It("maps uniplane onto uniplane", func() {
			args := spectrumXSpecToPlannerArgs(&v1alpha1.SpectrumXOptimizedSpec{
				Version: "RA2.2", MultiplaneMode: "uniplane", NumberOfPlanes: 2,
			})
			Expect(args.Profile).To(Equal("ra2.2-uniplane"))
			Expect(args.Params["multiplane_mode"]).To(Equal("uniplane"))
		})

		It("defaults none to swmp, planes to 1, overlay to none", func() {
			args := spectrumXSpecToPlannerArgs(&v1alpha1.SpectrumXOptimizedSpec{
				Version: "RA2.2", MultiplaneMode: "none",
			})
			Expect(args.Profile).To(Equal("ra2.2-swmp"))
			Expect(args.Params["planes"]).To(Equal("1"))
			Expect(args.Params["overlay"]).To(Equal("none"))
		})
	})

	Describe("stubPlanner.RenderPlan", func() {
		var planner Planner
		var device *v1alpha1.NicDevice

		BeforeEach(func() {
			planner = newStubPlanner()
			device = spcxDevice("cx8-plan", "0000:08:00.0")
		})

		It("returns the prepare example plan", func() {
			data, err := planner.RenderPlan(device, StagePrepare, "/some/root")
			Expect(err).NotTo(HaveOccurred())

			var plan struct {
				Plan struct {
					Stage   string `json:"stage"`
					Profile string `json:"profile"`
				} `json:"plan"`
			}
			Expect(json.Unmarshal(data, &plan)).To(Succeed())
			Expect(plan.Plan.Stage).To(Equal("prepare"))
			Expect(plan.Plan.Profile).To(Equal("ra2.2-hwmp"))
		})

		It("returns the configure example plan", func() {
			data, err := planner.RenderPlan(device, StageConfigure, "/some/root")
			Expect(err).NotTo(HaveOccurred())

			var plan struct {
				Plan struct {
					Stage string `json:"stage"`
				} `json:"plan"`
			}
			Expect(json.Unmarshal(data, &plan)).To(Succeed())
			Expect(plan.Plan.Stage).To(Equal("configure"))
		})

		It("errors on an unknown stage", func() {
			_, err := planner.RenderPlan(device, PlanStage("bogus"), "/some/root")
			Expect(err).To(HaveOccurred())
		})

		It("errors on a nil device", func() {
			_, err := planner.RenderPlan(nil, StagePrepare, "/some/root")
			Expect(err).To(HaveOccurred())
		})
	})

	Describe("generatePlan", func() {
		It("renders the plan when the blueprint dir is materialized", func() {
			baseDir, err := os.MkdirTemp("", "blueprints-*")
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() { _ = os.RemoveAll(baseDir) })

			device := spcxDevice("cx8-gen", "0000:08:00.0")
			version := device.Spec.Configuration.Template.SpectrumXOptimized.Version
			Expect(os.MkdirAll(filepath.Join(baseDir, version), 0o755)).To(Succeed())

			m := &spectrumXConfigManager{blueprintsBaseDir: baseDir, planner: newStubPlanner()}

			data, err := m.generatePlan(device, StagePrepare)
			Expect(err).NotTo(HaveOccurred())
			Expect(data).NotTo(BeEmpty())
		})

		It("errors when the blueprint has not been materialized yet", func() {
			baseDir, err := os.MkdirTemp("", "blueprints-*")
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() { _ = os.RemoveAll(baseDir) })

			m := &spectrumXConfigManager{blueprintsBaseDir: baseDir, planner: newStubPlanner()}
			device := spcxDevice("cx8-missing", "0000:08:00.0") // no <base>/<version> dir

			_, err = m.generatePlan(device, StagePrepare)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("not materialized"))
		})

		It("errors when the device has no version", func() {
			m := &spectrumXConfigManager{blueprintsBaseDir: "/tmp", planner: newStubPlanner()}
			device := spcxDevice("cx8-nov", "0000:08:00.0")
			device.Spec.Configuration.Template.SpectrumXOptimized.Version = ""

			_, err := m.generatePlan(device, StagePrepare)
			Expect(err).To(HaveOccurred())
		})
	})
})
