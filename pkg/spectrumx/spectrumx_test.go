/*
2025 NVIDIA CORPORATION & AFFILIATES
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
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/stretchr/testify/mock"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/Mellanox/nic-configuration-operator/api/v1alpha1"
	dmsmocks "github.com/Mellanox/nic-configuration-operator/pkg/dms/mocks"
	"github.com/Mellanox/nic-configuration-operator/pkg/types"

	execUtils "k8s.io/utils/exec"
)

type fakeCmd struct {
	execUtils.Cmd
	output []byte
	err    error
	delay  time.Duration
}

func (c *fakeCmd) Output() ([]byte, error) {
	// Simulate some runtime for the command; tests that need immedate error set c.err
	if c.delay > 0 {
		time.Sleep(c.delay)
	} else if c.err == nil {
		// Default small delay
		time.Sleep(100 * time.Millisecond)
	}
	return c.output, c.err
}

func (c *fakeCmd) CombinedOutput() ([]byte, error) {
	return c.Output()
}

type fakeExec struct {
	execUtils.Interface
	cmds []*fakeCmd
	pos  int
}

var (
	nextCmd *fakeCmd
)

func (f *fakeExec) next() execUtils.Cmd {
	if f.cmds != nil && f.pos < len(f.cmds) {
		c := f.cmds[f.pos]
		f.pos++
		return c
	}
	return nextCmd
}
func (f *fakeExec) Command(cmd string, args ...string) execUtils.Cmd { return f.next() }
func (f *fakeExec) CommandContext(ctx context.Context, cmd string, args ...string) execUtils.Cmd {
	return f.next()
}

var _ = Describe("SpectrumXConfigManager", func() {
	var (
		dmsMgr   dmsmocks.DMSManager
		dmsCli   dmsmocks.DMSClient
		manager  *spectrumXConfigManager
		device   *v1alpha1.NicDevice
		execFake *fakeExec
	)

	beforeDevice := func() {
		device = &v1alpha1.NicDevice{
			Spec: v1alpha1.NicDeviceSpec{
				Configuration: &v1alpha1.NicDeviceConfigurationSpec{
					Template: &v1alpha1.ConfigurationTemplateSpec{
						SpectrumXOptimized: &v1alpha1.SpectrumXOptimizedSpec{Enabled: true, Version: "v1", Overlay: "none", MultiplaneMode: "none", NumberOfPlanes: 1},
						NumVfs:             1,
						LinkType:           v1alpha1.LinkTypeEnum("Ethernet"),
					},
				},
			},
			ObjectMeta: metav1.ObjectMeta{Name: "spcx-test"},
			Status: v1alpha1.NicDeviceStatus{
				SerialNumber: "SN-1",
				Type:         "1023",
				Ports:        []v1alpha1.NicDevicePortSpec{{PCI: "0000:00:00.0", RdmaInterface: "mlx5_0"}},
			},
		}
	}

	BeforeEach(func() {
		dmsMgr = dmsmocks.DMSManager{}
		dmsCli = dmsmocks.DMSClient{}
		execFake = &fakeExec{}

		blueprintsBaseDir, mkErr := os.MkdirTemp("", "blueprints-*")
		Expect(mkErr).NotTo(HaveOccurred())
		DeferCleanup(func() { _ = os.RemoveAll(blueprintsBaseDir) })
		// Materialize the blueprint dir for the test device's version ("v1") so
		// generatePlan's readiness check passes.
		Expect(os.MkdirAll(filepath.Join(blueprintsBaseDir, "v1"), 0o755)).To(Succeed())

		manager = &spectrumXConfigManager{
			dmsManager:        &dmsMgr,
			blueprintsBaseDir: blueprintsBaseDir,
			planner:           newStubPlanner(),
			execInterface:     execFake,
			ccProcesses:       map[string]*ccProcess{},
			ccTerminationChan: make(chan string, 10),
		}

		beforeDevice()
		dmsMgr.On("GetDMSClientByPCIAddress", "0000:00:00").Return(&dmsCli, nil).Maybe()
	})

	// configureOps collects all configure-stage ops across the plan's semantic groups.
	configureOps := func() []types.DMSConfigOp {
		plan, err := ParsePlan(exampleConfigurePlan)
		Expect(err).NotTo(HaveOccurred())
		var out []types.DMSConfigOp
		for _, g := range plan.OrderedGroups() {
			ops, perr := g.Ops(plan.Params)
			Expect(perr).NotTo(HaveOccurred())
			out = append(out, ops...)
		}
		return out
	}

	// appliedMap builds a DMS GetParameters return ("<path>/<leaf>" -> value) for ops, all matching.
	appliedMap := func(ops []types.DMSConfigOp) map[string]string {
		m := map[string]string{}
		for _, op := range ops {
			for leaf, v := range op.Values {
				s, err := types.StringifyDMSValue(v)
				Expect(err).NotTo(HaveOccurred())
				m[op.Path+"/"+leaf] = s
			}
		}
		return m
	}

	// mismatchMap returns the ops' leaves with a value that won't match the desired one.
	mismatchMap := func(ops []types.DMSConfigOp) map[string]string {
		m := map[string]string{}
		for _, op := range ops {
			for leaf := range op.Values {
				m[op.Path+"/"+leaf] = "___mismatch___"
			}
		}
		return m
	}

	Describe("GetPrepareOps", func() {
		It("returns breakout and post-breakout ops divided by group", func() {
			breakout, postBreakout, err := manager.GetPrepareOps(device)
			Expect(err).NotTo(HaveOccurred())
			Expect(breakout).NotTo(BeEmpty())
			Expect(postBreakout).NotTo(BeEmpty())

			_, ok := opLeafString(breakout, "/nvidia/roce", "adaptive-routing")
			Expect(ok).To(BeTrue(), "breakout should carry the roce knob")
			_, ok = opLeafString(postBreakout, "/nvidia/link/type", "value")
			Expect(ok).To(BeTrue(), "post-breakout should carry the link-type knob")
		})

		It("applies rawNvConfig overrides by leaf path", func() {
			device.Spec.Configuration.Template.RawNvConfig = []v1alpha1.NvConfigParam{
				{Name: "/nvidia/roce/adaptive-routing", Value: "false"},
			}
			breakout, _, err := manager.GetPrepareOps(device)
			Expect(err).NotTo(HaveOccurred())

			v, ok := opLeafString(breakout, "/nvidia/roce", "adaptive-routing")
			Expect(ok).To(BeTrue())
			Expect(v).To(Equal("false"))
		})

		It("propagates plan errors when the blueprint is not materialized", func() {
			device.Spec.Configuration.Template.SpectrumXOptimized.Version = "missing-version"
			_, _, err := manager.GetPrepareOps(device)
			Expect(err).To(HaveOccurred())
		})
	})

	Describe("RuntimeConfigApplied", func() {
		It("returns true when all configure groups are applied and CC runs", func() {
			dmsCli.On("GetParameters", mock.Anything).Return(appliedMap(configureOps()), nil)
			manager.ccProcesses[device.Status.Ports[0].RdmaInterface] = &ccProcess{port: device.Status.Ports[0]}
			manager.ccProcesses[device.Status.Ports[0].RdmaInterface].running.Store(true)

			applied, err := manager.RuntimeConfigApplied(device)
			Expect(err).NotTo(HaveOccurred())
			Expect(applied).To(BeTrue())
		})

		It("returns false when the CC process is not running", func() {
			dmsCli.On("GetParameters", mock.Anything).Return(appliedMap(configureOps()), nil)

			applied, err := manager.RuntimeConfigApplied(device)
			Expect(err).NotTo(HaveOccurred())
			Expect(applied).To(BeFalse())
		})

		It("returns false when a group's knobs are not applied", func() {
			dmsCli.On("GetParameters", mock.Anything).Return(mismatchMap(configureOps()), nil)
			manager.ccProcesses[device.Status.Ports[0].RdmaInterface] = &ccProcess{port: device.Status.Ports[0]}
			manager.ccProcesses[device.Status.Ports[0].RdmaInterface].running.Store(true)

			applied, err := manager.RuntimeConfigApplied(device)
			Expect(err).NotTo(HaveOccurred())
			Expect(applied).To(BeFalse())
		})

		It("propagates DMS errors", func() {
			dmsCli.On("GetParameters", mock.Anything).Return(nil, errors.New("dms boom"))

			_, err := manager.RuntimeConfigApplied(device)
			Expect(err).To(HaveOccurred())
		})
	})

	Describe("ApplyRuntimeConfig", func() {
		It("applies all configure groups and starts the CC binary before the cc group", func() {
			// Assert the CC binary is already running by the time cc knobs are set.
			dmsCli.On("SetParameters", mock.Anything).Run(func(args mock.Arguments) {
				for _, op := range args.Get(0).([]types.DMSConfigOp) {
					if strings.HasPrefix(op.Path, "/nvidia/cc/algo") {
						Expect(manager.ccRunning(device)).To(BeTrue(), "CC binary must be running before cc knobs are applied")
					}
				}
			}).Return(nil)
			nextCmd = &fakeCmd{output: []byte("running"), delay: 5 * time.Second}

			result, err := manager.ApplyRuntimeConfig(device)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.Status).To(Equal(types.ApplyStatusSuccess))
			Expect(manager.ccProcesses).To(HaveKey(device.Status.Ports[0].RdmaInterface))
		})

		It("bubbles up DMS errors", func() {
			dmsCli.On("SetParameters", mock.Anything).Return(errors.New("set boom"))
			nextCmd = &fakeCmd{output: []byte("running"), delay: 5 * time.Second}

			_, err := manager.ApplyRuntimeConfig(device)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("set boom"))
		})
	})

	Describe("GetDocaCCTargetVersion", func() {
		It("returns empty when SpectrumXOptimized is nil", func() {
			device.Spec.Configuration.Template.SpectrumXOptimized = nil
			v, err := manager.GetDocaCCTargetVersion(device)
			Expect(err).NotTo(HaveOccurred())
			Expect(v).To(Equal(""))
		})

		It("returns empty (the plan does not carry a doca_spcx_cc version yet, O12)", func() {
			v, err := manager.GetDocaCCTargetVersion(device)
			Expect(err).NotTo(HaveOccurred())
			Expect(v).To(Equal(""))
		})
	})

	Describe("RunDocaSpcXCC", func() {
		It("returns nil if process already running", func() {
			port := device.Status.Ports[0]
			manager.ccProcesses[port.RdmaInterface] = &ccProcess{port: port}
			manager.ccProcesses[port.RdmaInterface].running.Store(true)
			err := manager.RunDocaSpcXCC(port)
			Expect(err).NotTo(HaveOccurred())
		})

		It("starts process and keeps running", func() {
			nextCmd = &fakeCmd{output: []byte("running"), err: nil, delay: 5 * time.Second}
			port := device.Status.Ports[0]
			err := manager.RunDocaSpcXCC(port)
			Expect(err).NotTo(HaveOccurred())
			Expect(manager.ccProcesses).To(HaveKey(port.RdmaInterface))
		})

		It("returns error if process fails to start within wait window", func() {
			nextCmd = &fakeCmd{output: []byte(""), err: errors.New("failed")}
			port := device.Status.Ports[0]
			err := manager.RunDocaSpcXCC(port)
			Expect(err).To(HaveOccurred())
			Expect(strings.ToLower(err.Error())).To(ContainSubstring("failed to start"))
		})

		It("sends notification on channel when process dies after startup", func() {
			// fakeCmd with 4s delay survives the 3s startup check, then fails
			nextCmd = &fakeCmd{output: []byte(""), err: errors.New("runtime crash"), delay: 4 * time.Second}
			port := device.Status.Ports[0]
			err := manager.RunDocaSpcXCC(port)
			Expect(err).NotTo(HaveOccurred())
			Expect(manager.ccProcesses).To(HaveKey(port.RdmaInterface))

			// Wait for process to die and notification to fire
			Eventually(manager.GetCCTerminationChannel(), 5*time.Second).Should(Receive(Equal(port.RdmaInterface)))
		})

		It("does NOT send notification when process fails during startup", func() {
			nextCmd = &fakeCmd{output: []byte(""), err: errors.New("startup failure")}
			port := device.Status.Ports[0]
			err := manager.RunDocaSpcXCC(port)
			Expect(err).To(HaveOccurred())

			Consistently(manager.GetCCTerminationChannel(), 1*time.Second).ShouldNot(Receive())
		})
	})

	Describe("GetCCTerminationChannel", func() {
		It("returns the termination channel", func() {
			ch := manager.GetCCTerminationChannel()
			Expect(ch).NotTo(BeNil())
		})
	})

})
