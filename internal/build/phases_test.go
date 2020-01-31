package build_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/docker/docker/client"
	"github.com/sclevine/spec"
	"github.com/sclevine/spec/report"

	"github.com/buildpacks/pack/internal/build"
	ilogging "github.com/buildpacks/pack/internal/logging"
	h "github.com/buildpacks/pack/testhelpers"
)

type FakePhase struct {
	CleanupCallCount int
	RunCallCount     int
}

func (p *FakePhase) Cleanup() error {
	p.CleanupCallCount = p.CleanupCallCount + 1

	return nil
}

func (p *FakePhase) Run(ctx context.Context) error {
	p.RunCallCount = p.RunCallCount + 1

	return nil
}

type FakePhaseManager struct {
	NewCallCount      int
	ReturnForNew      build.RunnerCleaner
	NewCalledWithName string
	NewCalledWithOps  []build.PhaseOperation

	WithArgsCallCount int
	WithArgsReceived  []string
}

func (m *FakePhaseManager) New(name string, ops ...build.PhaseOperation) (build.RunnerCleaner, error) {
	m.NewCallCount = m.NewCallCount + 1
	m.NewCalledWithName = name
	m.NewCalledWithOps = ops

	return m.ReturnForNew, nil
}

func (m *FakePhaseManager) WithArgs(args ...string) build.PhaseOperation {
	m.WithArgsCallCount = m.WithArgsCallCount + 1
	m.WithArgsReceived = args

	return func(p *build.Phase) (phase *build.Phase, e error) {
		return nil, nil
	}
}

func TestPhases(t *testing.T) {
	// TODO: shared with other test file; fix CreateFakeLifecycle
	dockerCli, err := client.NewClientWithOpts(client.FromEnv, client.WithVersion("1.38"))
	h.AssertNil(t, err)

	repoName = "phase.test.lc-" + h.RandString(10) // TODO: reponame is globally referenced in CreateFakeLifecycle

	wd, err := os.Getwd()
	h.AssertNil(t, err)

	h.CreateImageFromDir(t, dockerCli, repoName, filepath.Join(wd, "testdata", "fake-lifecycle"))
	defer h.DockerRmi(dockerCli, repoName)

	spec.Run(t, "phases", testPhases, spec.Report(report.Terminal{}), spec.Sequential())
}

func testPhases(t *testing.T, when spec.G, it spec.S) {
	//when("#CreateDetect", func() {
	//	it("returns a phase", func() {
	//		var outBuf bytes.Buffer
	//		logger := ilogging.NewLogWithWriters(&outBuf, &outBuf)
	//
	//		// TODO: see if we can use a fake docker client
	//		docker, err := client.NewClientWithOpts(client.FromEnv, client.WithVersion("1.38"))
	//		h.AssertNil(t, err)
	//
	//		// TODO: see if we can use a fake builder when creating a lifecycle here
	//		lifecycle, err := CreateFakeLifecycle(filepath.Join("testdata", "fake-app"), docker, logger)
	//		h.AssertNil(t, err)
	//
	//		//var pm FakePhaseManager
	//		//var ctx context.Context
	//		//phase, err := lifecycle.CreateDetect(pm, ctx, "some-network-mode")
	//		//h.AssertNotNil(t, phase)
	//		//h.AssertNil(t, err)
	//
	//		// TODO: assert fake phase manager was called with correct args
	//	})
	//})

	when("#Detect", func() {
		it.Focus("creates a phase and then runs it", func() {
			var outBuf bytes.Buffer
			logger := ilogging.NewLogWithWriters(&outBuf, &outBuf)

			// TODO: see if we can use a fake docker client
			docker, err := client.NewClientWithOpts(client.FromEnv, client.WithVersion("1.38"))
			h.AssertNil(t, err)

			lifecycle, err := CreateFakeLifecycle(filepath.Join("testdata", "fake-app"), docker, logger)
			h.AssertNil(t, err)

			fakePhase := &FakePhase{}

			fakePhaseManager := &FakePhaseManager{
				ReturnForNew: fakePhase,
			}
			err = lifecycle.Detect(context.Background(), "test", fakePhaseManager)

			h.AssertNil(t, err)
			h.AssertEq(t, fakePhaseManager.NewCallCount, 1)
			h.AssertEq(t, fakePhase.CleanupCallCount, 1)
			h.AssertEq(t, fakePhase.RunCallCount, 1)
			h.AssertEq(t, fakePhaseManager.NewCalledWithName, "detector")

			h.AssertEq(t, fakePhaseManager.WithArgsCallCount, 1)
		})
	})
}
