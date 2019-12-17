package testhelpers

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/pkg/errors"
	"github.com/sclevine/spec"
	"github.com/sclevine/spec/report"

	"github.com/buildpacks/pack/internal/api"
	"github.com/buildpacks/pack/internal/blob"
	"github.com/buildpacks/pack/internal/builder"
	"github.com/buildpacks/pack/internal/style"
)

const (
	envPackPath              = "PACK_PATH"
	envPreviousPackPath      = "PREVIOUS_PACK_PATH"
	envLifecyclePath         = "LIFECYCLE_PATH"
	envPreviousLifecyclePath = "PREVIOUS_LIFECYCLE_PATH"
	envAcceptanceSuiteConfig = "ACCEPTANCE_SUITE_CONFIG"
)

type ComboTest func(*testing.T, spec.G, spec.S, RunCombo, *ServiceProvider)

func NewComboSuite(t *testing.T, name string, test ComboTest) *SuiteManager {
	suite := spec.New(name, spec.Report(report.Terminal{}))
	suiteManager := &SuiteManager{
		t:     t,
		suite: suite,
		out:   t.Logf,
	}

	tmpDir, err := ioutil.TempDir("", "combo-test")
	AssertNil(t, err)
	suiteManager.DoOnceAfterAll(tmpDir, func() error {
		return os.RemoveAll(tmpDir)
	})

	packPath := os.Getenv(envPackPath)
	if packPath == "" {
		packPath = buildPack(t, tmpDir)
	}
	previousPackPath := os.Getenv(envPreviousPackPath)

	lifecycleDescriptor := builder.LifecycleDescriptor{
		Info: builder.LifecycleInfo{
			Version: builder.VersionMustParse(builder.DefaultLifecycleVersion),
		},
		API: builder.LifecycleAPI{
			BuildpackVersion: api.MustParse(builder.DefaultBuildpackAPIVersion),
			PlatformVersion:  api.MustParse(builder.DefaultPlatformAPIVersion),
		},
	}
	lifecyclePath := os.Getenv(envLifecyclePath)
	if lifecyclePath != "" {
		lifecyclePath, err := filepath.Abs(lifecyclePath)
		if err != nil {
			t.Fatal(err)
		}

		lifecycleDescriptor, err = extractLifecycleDescriptor(lifecyclePath)
		if err != nil {
			t.Fatal(err)
		}
	}

	previousLifecycleDescriptor := lifecycleDescriptor
	previousLifecyclePath := os.Getenv(envPreviousLifecyclePath)
	if previousLifecyclePath != "" {
		previousLifecyclePath, err := filepath.Abs(previousLifecyclePath)
		if err != nil {
			t.Fatal(err)
		}

		previousLifecycleDescriptor, err = extractLifecycleDescriptor(previousLifecyclePath)
		if err != nil {
			t.Fatal(err)
		}
	}

	combos := []runComboConfig{
		{Pack: "current", PackForCreateBuilder: "current", Lifecycle: "current"},
	}

	suiteConfig := os.Getenv(envAcceptanceSuiteConfig)
	if suiteConfig != "" {
		var err error
		combos, err = parseSuiteConfig(suiteConfig)
		AssertNil(t, err)
	}

	resolvedCombos, err := resolveRunCombinations(
		tmpDir,
		combos,
		packPath,
		previousPackPath,
		lifecyclePath,
		lifecycleDescriptor,
		previousLifecyclePath,
		previousLifecycleDescriptor,
	)
	AssertNil(t, err)

	for k, combo := range resolvedCombos {
		t.Logf(`setting up run combination %s:
pack:
 |__ path: %s
 |__ version: %s
 |__ fixtures: %s

pack (for create builder):
 |__ path: %s
 |__ version: %s
 |__ fixtures: %s

lifecycle:
 |__ path: %s
 |__ version: %s
 |__ buildpack api: %s
 |__ platform api: %s
`,
			style.Symbol(k),
			combo.Pack.Path,
			combo.Pack.Version,
			combo.Pack.FixturesDir,
			combo.PackForCreateBuilder.Path,
			combo.PackForCreateBuilder.Version,
			combo.PackForCreateBuilder.FixturesDir,
			combo.LifecyclePath,
			combo.LifecycleDescriptor.Info.Version,
			combo.LifecycleDescriptor.API.BuildpackVersion,
			combo.LifecycleDescriptor.API.PlatformVersion,
		)

		combo := combo

		suite(k, func(t *testing.T, when spec.G, it spec.S) {
			var (
				packHome string
			)

			it.Before(func() {
				var err error
				packHome, err = ioutil.TempDir("", "buildpack.pack.home.")
				AssertNil(t, err)
			})

			it.After(func() {
				AssertNil(t, os.RemoveAll(packHome))
			})

			test(t, when, it, combo, &ServiceProvider{
				t:            t,
				suiteManager: suiteManager,
				runCombo:     combo,
			})

		}, spec.Report(report.Terminal{}))
	}

	return suiteManager
}

type runComboConfig struct {
	Pack                 string `json:"pack"`
	PackForCreateBuilder string `json:"pack_create_builder"`
	Lifecycle            string `json:"lifecycle"`
}

type RunCombo struct {
	Key                  string
	Pack                 *PackUnderTest
	PackForCreateBuilder *PackUnderTest
	LifecyclePath        string
	LifecycleDescriptor  builder.LifecycleDescriptor
}

func resolveRunCombinations(
	tmpDir string,
	combos []runComboConfig,
	packPath string,
	previousPackPath string,
	lifecyclePath string,
	lifecycleDescriptor builder.LifecycleDescriptor,
	previousLifecyclePath string,
	previousLifecycleDescriptor builder.LifecycleDescriptor,
) (map[string]RunCombo, error) {
	resolved := map[string]RunCombo{}
	for _, c := range combos {
		rc := RunCombo{
			Key:                 fmt.Sprintf("p_%s cb_%s lc_%s", c.Pack, c.PackForCreateBuilder, c.Lifecycle),
			LifecyclePath:       lifecyclePath,
			LifecycleDescriptor: lifecycleDescriptor,
		}

		packHomeDir, err := ioutil.TempDir(tmpDir, "pack-home")
		if err != nil {
			return nil, errors.Wrap(err, "creating pack home dir")
		}

		if c.Pack == "current" {
			rc.Pack, err = NewPackUnderTest(packPath, packHomeDir, filepath.Join("testdata", "pack_current"))
			if err != nil {
				return nil, errors.Wrap(err, "creating pack")
			}
		} else if c.PackForCreateBuilder == "previous" {
			if previousPackPath == "" {
				return nil, errors.Errorf("must provide %s in order to run combination %s", style.Symbol(envPreviousPackPath), style.Symbol(rc.Key))
			}

			rc.Pack, err = NewPackUnderTest(previousPackPath, packHomeDir, filepath.Join("testdata", "pack_previous"))
			if err != nil {
				return nil, errors.Wrap(err, "creating pack")
			}
		}

		packForCBHomeDir, err := ioutil.TempDir(tmpDir, "pack-home-cb")
		if err != nil {
			return nil, errors.Wrap(err, "creating pack home dir")
		}

		if c.PackForCreateBuilder == "current" {
			rc.PackForCreateBuilder, err = NewPackUnderTest(packPath, packForCBHomeDir, filepath.Join("testdata", "pack_current"))
			if err != nil {
				return nil, errors.Wrap(err, "creating pack")
			}
		} else if c.PackForCreateBuilder == "previous" {
			if previousPackPath == "" {
				return nil, errors.Errorf("must provide %s in order to run combination %s", style.Symbol(envPreviousPackPath), style.Symbol(rc.Key))
			}

			rc.PackForCreateBuilder, err = NewPackUnderTest(previousPackPath, packForCBHomeDir, filepath.Join("testdata", "pack_previous"))
			if err != nil {
				return nil, errors.Wrap(err, "creating pack")
			}
		}

		if c.Lifecycle == "previous" {
			if previousLifecyclePath == "" {
				return nil, errors.Errorf("must provide %s in order to run combination %s", style.Symbol(envPreviousLifecyclePath), style.Symbol(rc.Key))
			}

			rc.LifecyclePath = previousLifecyclePath
			rc.LifecycleDescriptor = previousLifecycleDescriptor
		}

		resolved[rc.Key] = rc
	}

	return resolved, nil
}

func buildPack(t *testing.T, tmpDir string) string {
	packTmpDir, err := ioutil.TempDir(tmpDir, "pack.acceptance.binary.")
	AssertNil(t, err)

	packPath := filepath.Join(packTmpDir, "pack")
	if runtime.GOOS == "windows" {
		packPath = packPath + ".exe"
	}

	cwd, err := os.Getwd()
	AssertNil(t, err)

	cmd := exec.Command("go", "build", "-mod=vendor", "-o", packPath, "./cmd/pack")
	if filepath.Base(cwd) == "acceptance" {
		cmd.Dir = filepath.Dir(cwd)
	}

	t.Logf("building pack: [CWD=%s] %s", cmd.Dir, cmd.Args)
	if txt, err := cmd.CombinedOutput(); err != nil {
		t.Fatal("building pack cli:\n", string(txt), err)
	}

	return packPath
}

func extractLifecycleDescriptor(lcPath string) (builder.LifecycleDescriptor, error) {
	lifecycle, err := builder.NewLifecycle(blob.NewBlob(lcPath))
	if err != nil {
		return builder.LifecycleDescriptor{}, errors.Wrapf(err, "reading lifecycle from %s", lcPath)
	}

	return lifecycle.Descriptor(), nil
}

func parseSuiteConfig(config string) ([]runComboConfig, error) {
	var cfgs []runComboConfig
	if err := json.Unmarshal([]byte(config), &cfgs); err != nil {
		return nil, errors.Wrap(err, "parse config")
	}

	validate := func(jsonKey, value string) error {
		switch value {
		case "current", "previous":
			return nil
		default:
			return fmt.Errorf("invalid config: %s not valid value for %s", style.Symbol(value), style.Symbol(jsonKey))
		}
	}

	for _, c := range cfgs {
		if err := validate("pack", c.Pack); err != nil {
			return nil, err
		}

		if err := validate("pack_create_builder", c.PackForCreateBuilder); err != nil {
			return nil, err
		}

		if err := validate("lifecycle", c.Lifecycle); err != nil {
			return nil, err
		}
	}

	return cfgs, nil
}
