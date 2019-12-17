// +build acceptance

package acceptance

import (
	"path/filepath"
	"runtime"
	"testing"

	"github.com/sclevine/spec"

	h "github.com/buildpacks/pack/testhelpers"
)

func TestReport(t *testing.T) {
	suite := h.NewComboSuite(t, "Report Test", testReport)
	suite.Run()
}

func testReport(t *testing.T, when spec.G, it spec.S, combo h.RunCombo, _ *h.ServiceProvider) {
	when("report", func() {
		it.Before(func() {
			h.SkipIf(t, !combo.Pack.Supports("report"), "pack does not support 'report' command")
		})

		when("default builder is set", func() {
			it.Before(func() {
				h.Run(t, combo.Pack.Exec("set-default-builder", "cnbs/sample-builder:bionic"))
			})

			it("outputs information", func() {
				output := h.Run(t, combo.Pack.Exec("report"))

				outputTemplate := filepath.Join(combo.Pack.FixturesDir, "report_output.txt")
				expectedOutput := fillTemplate(t, outputTemplate,
					map[string]interface{}{
						"DefaultBuilder": "cnbs/sample-builder:bionic",
						"Version":        combo.Pack.Version,
						"OS":             runtime.GOOS,
						"Arch":           runtime.GOARCH,
					},
				)
				h.AssertEq(t, output, expectedOutput)
			})
		})
	})
}
