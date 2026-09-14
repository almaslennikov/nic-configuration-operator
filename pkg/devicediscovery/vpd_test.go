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

package devicediscovery

import (
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func makeLargeVPDResource(tag byte, value []byte) []byte {
	resource := make([]byte, 3, 3+len(value))
	resource[0] = tag
	resource[1] = byte(len(value))
	resource[2] = byte(len(value) >> 8)
	return append(resource, value...)
}

func makeVPDKeyword(keyword, value string) []byte {
	field := make([]byte, 3, 3+len(value))
	field[0] = keyword[0]
	field[1] = keyword[1]
	field[2] = byte(len(value))
	return append(field, value...)
}

func makeValidVPD(modelName, pn, sn string) []byte {
	data := make([]byte, 0)
	if modelName != "" {
		data = append(data, makeLargeVPDResource(vpdLargeResourceIdentifierString, []byte(modelName))...)
	}
	readOnlyData := append(makeVPDKeyword("PN", pn), makeVPDKeyword("SN", sn)...)
	data = append(data, makeLargeVPDResource(vpdLargeResourceReadOnlyData, readOnlyData)...)
	return append(data, 0x78)
}

var _ = Describe("PCI VPD", func() {
	Describe("parsePCIVPD", func() {
		It("parses the identifier string and required read-only keywords", func() {
			modelName := "NVIDIA ConnectX-9 C9180 HHHL SuperNIC"

			vpd, err := parsePCIVPD(makeValidVPD(modelName, partNumber, serialNumber))

			Expect(err).NotTo(HaveOccurred())
			Expect(vpd.ModelName).To(Equal(modelName))
			Expect(vpd.PartNumber).To(Equal(partNumber))
			Expect(vpd.SerialNumber).To(Equal(serialNumber))
		})

		It("preserves the optional-model behavior when the identifier string is absent", func() {
			vpd, err := parsePCIVPD(makeValidVPD("", partNumber, serialNumber))

			Expect(err).NotTo(HaveOccurred())
			Expect(vpd.ModelName).To(BeEmpty())
		})

		It("parses a realistic ConnectX VPD fixture with binary and writable fields", func() {
			encoded, err := os.ReadFile(filepath.Join("testdata", "connectx6dx.vpd.hex"))
			Expect(err).NotTo(HaveOccurred())
			data, err := hex.DecodeString(strings.TrimSpace(string(encoded)))
			Expect(err).NotTo(HaveOccurred())

			vpd, err := parsePCIVPD(data)

			Expect(err).NotTo(HaveOccurred())
			Expect(vpd.ModelName).To(Equal("ConnectX-6 Dx EN adapter card, 100GbE, Dual-port QSFP56, PCIe 4.0 x16, Crypto, No Secure Boot"))
			Expect(vpd.PartNumber).To(Equal(partNumber))
			Expect(vpd.SerialNumber).To(Equal(serialNumber))
		})

		It("rejects empty VPD data", func() {
			vpd, err := parsePCIVPD(nil)

			Expect(err).To(MatchError("VPD data is empty"))
			Expect(vpd).To(BeNil())
		})

		It("rejects a truncated large-resource header", func() {
			vpd, err := parsePCIVPD([]byte{vpdLargeResourceIdentifierString, 0x01})

			Expect(err).To(MatchError(ContainSubstring("truncated large-resource header at offset 0")))
			Expect(vpd).To(BeNil())
		})

		It("rejects a truncated large-resource value", func() {
			vpd, err := parsePCIVPD([]byte{vpdLargeResourceIdentifierString, 0x04, 0x00, 'x'})

			Expect(err).To(MatchError(ContainSubstring("declares 4 bytes, only 1 remain")))
			Expect(vpd).To(BeNil())
		})

		It("rejects a truncated small-resource value", func() {
			vpd, err := parsePCIVPD([]byte{0x7a, 'x'})

			Expect(err).To(MatchError(ContainSubstring("small resource 0x7a at offset 0 declares 2 bytes, only 1 remain")))
			Expect(vpd).To(BeNil())
		})

		It("rejects a truncated keyword header", func() {
			data := append(makeLargeVPDResource(vpdLargeResourceReadOnlyData, []byte{'P', 'N'}), 0x78)

			vpd, err := parsePCIVPD(data)

			Expect(err).To(MatchError(ContainSubstring("truncated VPD keyword header")))
			Expect(vpd).To(BeNil())
		})

		It("rejects a truncated keyword value", func() {
			readOnlyData := []byte{'P', 'N', 0x04, 'x'}
			data := append(makeLargeVPDResource(vpdLargeResourceReadOnlyData, readOnlyData), 0x78)

			vpd, err := parsePCIVPD(data)

			Expect(err).To(MatchError(ContainSubstring("VPD keyword \"PN\"")))
			Expect(err).To(MatchError(ContainSubstring("declares 4 bytes, only 1 remain")))
			Expect(vpd).To(BeNil())
		})

		It("rejects VPD without an end tag", func() {
			data := makeValidVPD("model", partNumber, serialNumber)
			data = data[:len(data)-1]

			vpd, err := parsePCIVPD(data)

			Expect(err).To(MatchError("VPD data is missing the end tag"))
			Expect(vpd).To(BeNil())
		})

		It("rejects VPD without a read-only data resource", func() {
			data := append(makeLargeVPDResource(vpdLargeResourceIdentifierString, []byte("model")), 0x78)

			vpd, err := parsePCIVPD(data)

			Expect(err).To(MatchError("VPD data is missing the read-only data resource"))
			Expect(vpd).To(BeNil())
		})

		DescribeTable("rejects missing required fields",
			func(pn, sn, missing string) {
				vpd, err := parsePCIVPD(makeValidVPD("model", pn, sn))

				Expect(err).To(MatchError(ContainSubstring("missing required keyword(s): " + missing)))
				Expect(vpd).To(BeNil())
			},
			Entry("missing PN", "", serialNumber, "PN"),
			Entry("missing SN", partNumber, "", "SN"),
			Entry("missing PN and SN", "", "", "PN, SN"),
		)

		It("rejects invalid text in a required field", func() {
			readOnlyData := append([]byte{'P', 'N', 0x01, 0xff}, makeVPDKeyword("SN", serialNumber)...)
			data := append(makeLargeVPDResource(vpdLargeResourceReadOnlyData, readOnlyData), 0x78)

			vpd, err := parsePCIVPD(data)

			Expect(err).To(MatchError("VPD field PN contains invalid UTF-8"))
			Expect(vpd).To(BeNil())
		})
	})

	Describe("getVPDFromPath", func() {
		It("reads the requested PCI function so the kernel can mediate shared VPD access", func() {
			root := GinkgoT().TempDir()
			sharedVPDPCIAddress := "0000:03:00.1"
			vpdPath := filepath.Join(root, sharedVPDPCIAddress, "vpd")
			Expect(os.MkdirAll(filepath.Dir(vpdPath), 0o755)).To(Succeed())
			Expect(os.WriteFile(vpdPath, makeValidVPD("model", partNumber, serialNumber), 0o600)).To(Succeed())

			vpd, err := getVPDFromPath(root, sharedVPDPCIAddress)

			Expect(err).NotTo(HaveOccurred())
			Expect(vpd.PartNumber).To(Equal(partNumber))
			Expect(vpd.SerialNumber).To(Equal(serialNumber))
		})

		It("returns a useful error when the sysfs VPD file is missing", func() {
			root := GinkgoT().TempDir()

			vpd, err := getVPDFromPath(root, pciAddress)

			Expect(err).To(MatchError(And(
				ContainSubstring("reading PCI VPD for "+pciAddress),
				ContainSubstring(filepath.Join(root, pciAddress, "vpd")),
			)))
			Expect(vpd).To(BeNil())
		})

		It("rejects a PCI address that could escape the sysfs base path", func() {
			vpd, err := getVPDFromPath(GinkgoT().TempDir(), "../03:00.0")

			Expect(err).To(MatchError(`invalid PCI address "../03:00.0"`))
			Expect(vpd).To(BeNil())
		})
	})
})
