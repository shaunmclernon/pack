package build_test

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/docker/docker/client"
	"github.com/sclevine/spec"
	"github.com/sclevine/spec/report"

	"github.com/buildpacks/pack/internal/build"
	ilogging "github.com/buildpacks/pack/internal/logging"
	"github.com/buildpacks/pack/internal/stringset"
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
	when.Focus("#Detect", func() {
		it("creates a phase and then runs it", func() {
			lifecycle := fakeLifecycle(t)
			fakePhase := &FakePhase{}
			fakePhaseManager := fakePhaseManager(whichReturnsForNew(fakePhase))

			err := lifecycle.Detect(context.Background(), "test", fakePhaseManager)
			h.AssertNil(t, err)

			h.AssertEq(t, fakePhase.CleanupCallCount, 1)
			h.AssertEq(t, fakePhase.RunCallCount, 1)
		})

		it("configures the phase with the expected app and platform arguments", func() {
			lifecycle := fakeLifecycle(t)
			fakePhaseManager := fakePhaseManager()

			err := lifecycle.Detect(context.Background(), "test", fakePhaseManager)
			h.AssertNil(t, err)

			h.AssertEq(t, fakePhaseManager.NewCalledWithName, "detector")
			h.AssertEq(t, fakePhaseManager.WithArgsCallCount, 1)
			assertIncludeAllExpectedArgPatterns(t,
				fakePhaseManager.WithArgsReceived,
				[]string{"-app", "/workspace"},
				[]string{"-platform", "/platform"},
			)
		})
	})
}

func fakeLifecycle(t *testing.T) *build.Lifecycle {
	var outBuf bytes.Buffer
	logger := ilogging.NewLogWithWriters(&outBuf, &outBuf)

	// TODO: see if we can use a fake docker client
	docker, err := client.NewClientWithOpts(client.FromEnv, client.WithVersion("1.38"))
	h.AssertNil(t, err)

	lifecycle, err := CreateFakeLifecycle(filepath.Join("testdata", "fake-app"), docker, logger)
	h.AssertNil(t, err)

	return lifecycle
}

func fakePhaseManager(ops ...func(*FakePhaseManager)) *FakePhaseManager {
	fakePhaseManager := &FakePhaseManager{
		ReturnForNew: &FakePhase{},
	}

	for _, op := range ops {
		op(fakePhaseManager)
	}

	return fakePhaseManager
}

func whichReturnsForNew(phase build.RunnerCleaner) func(*FakePhaseManager) {
	return func(manager *FakePhaseManager) {
		manager.ReturnForNew = phase
	}
}

func assertIncludeAllExpectedArgPatterns(t *testing.T, receivedArgs []string, expectedPatterns ...[]string) {
	missingPatterns := [][]string{}

	for _, expectedPattern := range expectedPatterns {
		if !patternExists(expectedPattern, receivedArgs) {
			missingPatterns = append(missingPatterns, expectedPattern)
		}
	}

	assertSliceEmpty(t,
		missingPatterns,
		"Expected the patterns %s to exist in [%s]",
		missingPatterns,
		strings.Join(receivedArgs, " "),
	)
}

func patternExists(expectedPattern []string, receivedArgs []string) bool {
	_, missing, _ := stringset.Compare(receivedArgs, expectedPattern)
	if len(missing) > 0 {
		return false
	}

	if len(expectedPattern) == 1 {
		return true
	}

	for _, loc := range matchLocations(expectedPattern[0], receivedArgs) {
		finalElementLoc := loc + len(expectedPattern)

		receivedSubSlice := receivedArgs[loc:finalElementLoc]

		if reflect.DeepEqual(receivedSubSlice, expectedPattern) {
			return true
		}
	}

	return false
}

func matchLocations(expectedArg string, receivedArgs []string) []int {
	indices := []int{}

	for i, receivedArg := range receivedArgs {
		if receivedArg == expectedArg {
			indices = append(indices, i)
		}
	}

	return indices
}

func assertSliceEmpty(t *testing.T, actual interface{}, msg string, msgArgs ...interface{}) {
	empty, err := sliceEmpty(actual)

	if err != nil {
		t.Fatalf("assertSliceNotEmpty error: %s", err.Error())
	}

	if !empty {
		t.Fatalf(msg, msgArgs...)
	}
}

func sliceEmpty(slice interface{}) (bool, error) {
	switch reflect.TypeOf(slice).Kind() {
	case reflect.Slice:
		return reflect.ValueOf(slice).Len() == 0, nil
	default:
		return true, fmt.Errorf("invoked with non slice actual: %v", slice)
	}
}
