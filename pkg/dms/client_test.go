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

package dms

import (
	"context"
	"errors"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/utils/exec"
	execTesting "k8s.io/utils/exec/testing"

	"github.com/Mellanox/nic-configuration-operator/api/v1alpha1"
	"github.com/Mellanox/nic-configuration-operator/pkg/consts"
	"github.com/Mellanox/nic-configuration-operator/pkg/types"
)

const (
	testPCI       = "0000:00:00.0"
	testBindAddr  = "localhost:9339"
	testTarget    = "pci/0000:00:00.0"
	bf3DeviceType = consts.BlueField3DeviceID
)

// createFakeCmd creates a fake exec Command with specified output and error.
func createFakeCmd(output []byte, err error) *execTesting.FakeCmd {
	action := func() ([]byte, []byte, error) { return output, nil, err }
	return &execTesting.FakeCmd{
		OutputScript:         []execTesting.FakeAction{action},
		CombinedOutputScript: []execTesting.FakeAction{action},
	}
}

// capturingExec records every command (cmd + args) and returns canned output/err. It is
// repeatable (unlike FakeExec's fixed CommandScript), so it handles N invocations.
type capturingExec struct {
	exec.Interface
	captured *[][]string
	output   []byte
	err      error
}

func (e *capturingExec) record(cmd string, args ...string) exec.Cmd {
	*e.captured = append(*e.captured, append([]string{cmd}, args...))
	return createFakeCmd(e.output, e.err)
}
func (e *capturingExec) Command(cmd string, args ...string) exec.Cmd { return e.record(cmd, args...) }
func (e *capturingExec) CommandContext(_ context.Context, cmd string, args ...string) exec.Cmd {
	return e.record(cmd, args...)
}

func newTestClient(captured *[][]string, output []byte, err error, deviceType string) *dmsClient {
	return &dmsClient{
		device:        v1alpha1.NicDeviceStatus{SerialNumber: "SN-1", Type: deviceType, Ports: []v1alpha1.NicDevicePortSpec{{PCI: testPCI}}},
		targetPCI:     testPCI,
		bindAddress:   testBindAddr,
		authParams:    []string{"--insecure"},
		execInterface: &capturingExec{captured: captured, output: output, err: err},
	}
}

// joined returns the captured command at index i as a single space-joined string.
func joined(captured [][]string, i int) string {
	return strings.Join(captured[i], " ")
}

var _ = Describe("DMSClient (dms-cli)", func() {
	Describe("SetParameters", func() {
		It("renders one dms-cli invocation per op with sorted leaf=value assignments and pci/ target", func() {
			var captured [][]string
			client := newTestClient(&captured, []byte(""), nil, "1023")

			err := client.SetParameters([]types.DMSConfigOp{
				{Path: "/nvidia/roce", Values: map[string]any{"adaptive-routing": true, "cc-steering-ext": "enabled"}},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(captured).To(HaveLen(1))
			Expect(joined(captured, 0)).To(Equal(
				dmsClientPath + " -a localhost:9339 --insecure -t pci/0000:00:00.0 --timeout 300s " +
					"/nvidia/roce adaptive-routing=true cc-steering-ext=enabled"))
		})

		It("renders list and int values", func() {
			var captured [][]string
			client := newTestClient(&captured, []byte(""), nil, "1023")

			err := client.SetParameters([]types.DMSConfigOp{
				{Path: "/nvidia/link/breakout/module/[0]/port/[1]", Values: map[string]any{"lanes": []any{float64(0), float64(1)}}},
				{Path: "/nvidia/pci", Values: map[string]any{"num-pfs": float64(2)}},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(captured).To(HaveLen(2))
			Expect(captured[0]).To(ContainElement("lanes=[0,1]"))
			Expect(captured[1]).To(ContainElement("num-pfs=2"))
		})

		It("propagates set errors", func() {
			var captured [][]string
			client := newTestClient(&captured, []byte("boom"), errors.New("exit 1"), "1023")
			err := client.SetParameters([]types.DMSConfigOp{{Path: "/nvidia/roce", Values: map[string]any{"x": true}}})
			Expect(err).To(HaveOccurred())
		})
	})

	Describe("GetParameters", func() {
		It("reads each leaf with --plain and keys results by path/leaf", func() {
			var captured [][]string
			client := newTestClient(&captured, []byte("adaptive-routing: true"), nil, "1023")

			values, err := client.GetParameters([]types.DMSConfigOp{
				{Path: "/nvidia/roce", Values: map[string]any{"adaptive-routing": true}},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(values).To(HaveKeyWithValue("/nvidia/roce/adaptive-routing", "true"))
			Expect(joined(captured, 0)).To(ContainSubstring("-t pci/0000:00:00.0"))
			Expect(captured[0]).To(ContainElement("--plain"))
			Expect(captured[0]).To(ContainElement("/nvidia/roce"))
			Expect(captured[0]).To(ContainElement("adaptive-routing"))
		})
	})

	Describe("OpsApplied", func() {
		ops := []types.DMSConfigOp{{Path: "/nvidia/link/admin", Values: map[string]any{"admin-status": "up"}}}

		It("returns true when the current value matches the desired", func() {
			var captured [][]string
			client := newTestClient(&captured, []byte("admin-status: up"), nil, "1023")
			ok, err := OpsApplied(client, ops)
			Expect(err).NotTo(HaveOccurred())
			Expect(ok).To(BeTrue())
		})

		It("returns false when the current value differs", func() {
			var captured [][]string
			client := newTestClient(&captured, []byte("admin-status: down"), nil, "1023")
			ok, err := OpsApplied(client, ops)
			Expect(err).NotTo(HaveOccurred())
			Expect(ok).To(BeFalse())
		})

		It("returns true for an empty op set", func() {
			ok, err := OpsApplied(nil, nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(ok).To(BeTrue())
		})
	})

	Describe("parsePlainValue", func() {
		It("extracts the value for the matching leaf", func() {
			out := "trust-mode: dscp\nenabled-priorities: [3]\nvalue: 96"
			Expect(parsePlainValue(out, "trust-mode")).To(Equal("dscp"))
			Expect(parsePlainValue(out, "enabled-priorities")).To(Equal("[3]"))
			Expect(parsePlainValue(out, "value")).To(Equal("96"))
		})
		It("falls back to the last value when no key matches", func() {
			Expect(parsePlainValue("just-a-value", "missing")).To(Equal("just-a-value"))
		})
	})

	Describe("PFC mask <-> enabled-priorities", func() {
		It("round-trips", func() {
			Expect(pfcMaskToPriorities("0,0,0,1,0,0,0,0")).To(Equal("[3]"))
			Expect(pfcMaskToPriorities("00010000")).To(Equal("[3]"))
			Expect(prioritiesToPFCMask("[3]")).To(Equal("0,0,0,1,0,0,0,0"))
			Expect(prioritiesToPFCMask("[]")).To(Equal("0,0,0,0,0,0,0,0"))
		})
	})

	Describe("SetQoSSettings", func() {
		It("sets trust, PFC and ToS on the flat target-based paths", func() {
			var captured [][]string
			client := newTestClient(&captured, []byte(""), nil, "1023")

			err := client.SetQoSSettings(&v1alpha1.QosSpec{Trust: consts.TrustModeDscp, PFC: "0,0,0,1,0,0,0,0", ToS: 96})
			Expect(err).NotTo(HaveOccurred())
			Expect(captured).To(HaveLen(3))
			Expect(joined(captured, 0)).To(HaveSuffix("/nvidia/qos trust-mode=dscp"))
			Expect(joined(captured, 1)).To(HaveSuffix("/nvidia/qos/pfc enabled-priorities=[3]"))
			Expect(joined(captured, 2)).To(HaveSuffix("/nvidia/roce/tos value=96"))
		})

		It("rejects an invalid trust mode", func() {
			var captured [][]string
			client := newTestClient(&captured, []byte(""), nil, "1023")
			err := client.SetQoSSettings(&v1alpha1.QosSpec{Trust: "bogus"})
			Expect(err).To(HaveOccurred())
		})
	})

	Describe("GetQoSSettings", func() {
		It("reads trust, PFC and ToS back", func() {
			var captured [][]string
			client := newTestClient(&captured, []byte("trust-mode: dscp\nenabled-priorities: [3]\nvalue: 96"), nil, "1023")

			spec, err := client.GetQoSSettings("enp3s0f0np0")
			Expect(err).NotTo(HaveOccurred())
			Expect(spec.Trust).To(Equal("dscp"))
			Expect(spec.PFC).To(Equal("0,0,0,1,0,0,0,0"))
			Expect(spec.ToS).To(Equal(96))
		})
	})

	Describe("InstallBFB", func() {
		It("runs os install then os activate via dms-cli on a BlueField device", func() {
			var captured [][]string
			client := newTestClient(&captured, []byte("ok"), nil, bf3DeviceType)

			err := client.InstallBFB(context.Background(), "1.2.3", "/tmp/fw.bfb")
			Expect(err).NotTo(HaveOccurred())
			Expect(captured).To(HaveLen(2))
			Expect(captured[0]).To(ContainElements(dmsClientPath, "-t", testTarget, "os", "install", "--version", "1.2.3", "--pkg", "/tmp/fw.bfb"))
			Expect(captured[1]).To(ContainElements("os", "activate", "--version", "1.2.3"))
		})

		It("rejects non-BlueField devices", func() {
			var captured [][]string
			client := newTestClient(&captured, []byte(""), nil, "1023")
			err := client.InstallBFB(context.Background(), "1.2.3", "/tmp/fw.bfb")
			Expect(err).To(HaveOccurred())
		})
	})
})
