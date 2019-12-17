// +build acceptance

package acceptance

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"html/template"
	"io"
	"io/ioutil"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Masterminds/semver"
	dockertypes "github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/docker/go-connections/nat"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/pkg/errors"
	"github.com/sclevine/spec"

	"github.com/buildpacks/pack/internal/api"
	"github.com/buildpacks/pack/internal/archive"
	"github.com/buildpacks/pack/internal/builder"
	"github.com/buildpacks/pack/internal/cache"
	h "github.com/buildpacks/pack/testhelpers"
)

const (
	runImage   = "pack-test/run"
	buildImage = "pack-test/build"
)

var (
	dockerCli    *client.Client
	suiteManager *h.SuiteManager
)

func TestAcceptance(t *testing.T) {
	var err error

	h.RequireDocker(t)
	rand.Seed(time.Now().UTC().UnixNano())

	dockerCli, err = client.NewClientWithOpts(client.FromEnv, client.WithVersion("1.38"))
	h.AssertNil(t, err)

	suite := h.NewComboSuite(t, "acceptance test", testAcceptance)
	suite.Run()
}

func testAcceptance(t *testing.T, when spec.G, it spec.S, combo h.RunCombo, serviceProvider *h.ServiceProvider) {
	var (
		packFixturesDir             = combo.Pack.FixturesDir
		packCreateBuilderFixtureDir = combo.PackForCreateBuilder.FixturesDir
		lifecyclePath               = combo.LifecyclePath
		lifecycleDescriptor         = combo.LifecycleDescriptor
		bpDir                       = buildpacksDir(*combo.LifecycleDescriptor.API.BuildpackVersion)
		registry                    *h.TestRegistryConfig
	)

	it.Before(func() {
		var err error
		registry, err = serviceProvider.RequestRegistry()
		h.AssertNil(t, err)
	})

	when("invalid subcommand", func() {
		it("prints usage", func() {
			output, err := h.RunE(combo.Pack.Exec("some-bad-command"))
			h.AssertNotNil(t, err)
			h.AssertContains(t, output, `unknown command "some-bad-command" for "pack"`)
			h.AssertContains(t, output, `Run 'pack --help' for usage.`)
		})
	})

	when("stack is created", func() {
		var (
			runImageMirror string
		)

		it.Before(func() {
			value, err := suiteManager.DoOnceString("create-stack",
				func() (string, error) {
					runImageMirror := registry.RepoName(runImage)
					err := createStack(t, registry, dockerCli, runImageMirror)
					if err != nil {
						return "", err
					}

					return runImageMirror, nil
				})
			h.AssertNil(t, err)

			suiteManager.DoOnceAfterAll("remove-stack-images", func() error {
				return h.DockerRmi(dockerCli, runImage, buildImage, value)
			})

			runImageMirror = value
		})

		when("builder is created", func() {
			var (
				builderName string
			)

			it.Before(func() {
				key := "builder." + combo.Key
				value, err := suiteManager.DoOnceString(key, func() (string, error) {
					return createBuilder(t, registry, runImageMirror, packCreateBuilderFixtureDir, *combo.PackForCreateBuilder, lifecyclePath, lifecycleDescriptor), nil
				})
				h.AssertNil(t, err)
				suiteManager.DoOnceAfterAll("clean-"+key, func() error {
					return h.DockerRmi(dockerCli, value)
				})

				builderName = value
			})

			when("build", func() {
				var repo, repoName string

				it.Before(func() {
					repo = "some-org/" + h.RandString(10)
					repoName = registry.RepoName(repo)
				})

				it.After(func() {
					h.DockerRmi(dockerCli, repoName)
					ref, err := name.ParseReference(repoName, name.WeakValidation)
					h.AssertNil(t, err)
					cacheImage := cache.NewImageCache(ref, dockerCli)
					buildCacheVolume := cache.NewVolumeCache(ref, "build", dockerCli)
					launchCacheVolume := cache.NewVolumeCache(ref, "launch", dockerCli)
					cacheImage.Clear(context.TODO())
					buildCacheVolume.Clear(context.TODO())
					launchCacheVolume.Clear(context.TODO())
				})

				when("default builder is set", func() {
					it.Before(func() {
						h.Run(t, combo.Pack.Exec("set-default-builder", builderName))
					})

					it("creates a runnable, rebuildable image on daemon from app dir", func() {
						appPath := filepath.Join("testdata", "mock_app")
						output := h.Run(t, combo.Pack.Exec("build", repoName, "-p", appPath))
						h.AssertContains(t, output, fmt.Sprintf("Successfully built image '%s'", repoName))
						imgId, err := imgIDForRepoName(repoName)
						if err != nil {
							t.Fatal(err)
						}
						defer h.DockerRmi(dockerCli, imgId)

						t.Log("uses a build cache volume")
						h.AssertContains(t, output, "Using build cache volume")

						t.Log("app is runnable")
						assertMockAppRunsWithOutput(t, repoName, "Launch Dep Contents", "Cached Dep Contents")

						t.Log("selects the best run image mirror")
						h.AssertContains(t, output, fmt.Sprintf("Selected run image mirror '%s'", runImageMirror))

						t.Log("it uses the run image as a base image")
						assertHasBase(t, repoName, runImage)

						t.Log("sets the run image metadata")
						appMetadataLabel := imageLabel(t, dockerCli, repoName, "io.buildpacks.lifecycle.metadata")
						h.AssertContains(t, appMetadataLabel, fmt.Sprintf(`"stack":{"runImage":{"image":"%s","mirrors":["%s"]}}}`, runImage, runImageMirror))

						t.Log("registry is empty")
						contents, err := registry.RegistryCatalog()
						h.AssertNil(t, err)
						if strings.Contains(contents, repo) {
							t.Fatalf("Should not have published image without the '--publish' flag: got %s", contents)
						}

						t.Log("add a local mirror")
						localRunImageMirror := registry.RepoName("pack-test/run-mirror")
						h.AssertNil(t, dockerCli.ImageTag(context.TODO(), runImage, localRunImageMirror))
						defer h.DockerRmi(dockerCli, localRunImageMirror)
						h.Run(t, combo.Pack.Exec("set-run-image-mirrors", runImage, "-m", localRunImageMirror))

						t.Log("rebuild")
						output = h.Run(t, combo.Pack.Exec("build", repoName, "-p", appPath))
						h.AssertContains(t, output, fmt.Sprintf("Successfully built image '%s'", repoName))

						imgId, err = imgIDForRepoName(repoName)
						if err != nil {
							t.Fatal(err)
						}
						defer h.DockerRmi(dockerCli, imgId)

						t.Log("local run-image mirror is selected")
						h.AssertContains(t, output, fmt.Sprintf("Selected run image mirror '%s' from local config", localRunImageMirror))

						t.Log("app is runnable")
						assertMockAppRunsWithOutput(t, repoName, "Launch Dep Contents", "Cached Dep Contents")

						t.Log("restores the cache")
						if lifecycleDescriptor.Info.Version.LessThan(semver.MustParse("0.6.0")) {
							h.AssertContainsMatch(t, output, `(?i)\[restorer] restoring cached layer 'simple/layers:cached-launch-layer'`)
							h.AssertContainsMatch(t, output, `(?i)\[analyzer] using cached launch layer 'simple/layers:cached-launch-layer'`)
						} else {
							h.AssertContainsMatch(t, output, `(?i)\[restorer] Restoring data for "simple/layers:cached-launch-layer" from cache`)
							h.AssertContainsMatch(t, output, `(?i)\[analyzer] Restoring metadata for "simple/layers:cached-launch-layer" from app image`)
						}

						t.Log("exporter reuses unchanged layers")
						h.AssertContainsMatch(t, output, `(?i)\[exporter] reusing layer 'simple/layers:cached-launch-layer'`)

						if lifecycleDescriptor.Info.Version.LessThan(semver.MustParse("0.6.0")) {
							t.Log("cacher reuses unchanged layers")
							h.AssertContainsMatch(t, output, `(?i)\[cacher] reusing layer 'simple/layers:cached-launch-layer'`)
						} else {
							h.AssertContainsMatch(t, output, `(?i)\[exporter] Reusing cache layer 'simple/layers:cached-launch-layer'`)
						}

						t.Log("rebuild with --clear-cache")
						output = h.Run(t, combo.Pack.Exec("build", repoName, "-p", appPath, "--clear-cache"))
						h.AssertContains(t, output, fmt.Sprintf("Successfully built image '%s'", repoName))

						t.Log("skips restore")
						h.AssertContains(t, output, "Skipping 'restore' due to clearing cache")

						t.Log("skips buildpack layer analysis")
						h.AssertContainsMatch(t, output, `(?i)\[analyzer] Skipping buildpack layer analysis`)

						t.Log("exporter reuses unchanged layers")
						h.AssertContainsMatch(t, output, `(?i)\[exporter] reusing layer 'simple/layers:cached-launch-layer'`)

						t.Log("cacher adds layers")
						if lifecycleDescriptor.Info.Version.LessThan(semver.MustParse("0.6.0")) {
							h.AssertContainsMatch(t, output, `(?i)\[cacher] (Caching|adding) layer 'simple/layers:cached-launch-layer'`)
						} else {
							h.AssertContainsMatch(t, output, `(?i)\[exporter] Adding cache layer 'simple/layers:cached-launch-layer'`)
						}

						if combo.Pack.Supports("inspect-image") {
							t.Log("inspect-image")
							output = h.Run(t, combo.Pack.Exec("inspect-image", repoName))

							outputTemplate := filepath.Join(packFixturesDir, "inspect_image_local_output.txt")
							if _, err := os.Stat(outputTemplate); err != nil {
								t.Fatal(err.Error())
							}
							expectedOutput := fillTemplate(t, outputTemplate,
								map[string]interface{}{
									"image_name":             repoName,
									"base_image_id":          h.ImageID(t, runImageMirror),
									"base_image_top_layer":   h.TopLayerDiffID(t, runImageMirror),
									"run_image_local_mirror": localRunImageMirror,
									"run_image_mirror":       runImageMirror,
									"show_reference":         !lifecycleDescriptor.Info.Version.LessThan(semver.MustParse("0.5.0")),
									"show_processes":         !lifecycleDescriptor.Info.Version.LessThan(semver.MustParse("0.6.0")),
								},
							)
							h.AssertEq(t, output, expectedOutput)
						}
					})

					it("supports building app from a zip file", func() {
						appPath := filepath.Join("testdata", "mock_app.zip")
						cmd := combo.Pack.Exec("build", repoName, "-p", appPath)
						output := h.Run(t, cmd)
						h.AssertContains(t, output, fmt.Sprintf("Successfully built image '%s'", repoName))

						imgId, err := imgIDForRepoName(repoName)
						if err != nil {
							t.Fatal(err)
						}
						defer h.DockerRmi(dockerCli, imgId)
					})

					when("--network", func() {
						var buildpackTgz string

						it.Before(func() {
							h.SkipIf(t, combo.Pack.Supports("build --network"), "--network flag not supported for build")

							buildpackTgz = h.CreateTGZ(t, filepath.Join(bpDir, "internet-capable-buildpack"), "./", 0755)
						})

						it.After(func() {
							h.AssertNil(t, os.Remove(buildpackTgz))
							h.AssertNil(t, h.DockerRmi(dockerCli, repoName))
						})

						when("the network mode is not provided", func() {
							it("reports that build and detect are online", func() {
								cmd := combo.Pack.Exec(
									"build",
									repoName,
									"-p",
									filepath.Join("testdata", "mock_app"),
									"--buildpack",
									buildpackTgz,
								)
								output := h.Run(t, cmd)
								h.AssertContains(t, output, "[detector] RESULT: Connected to the internet")
								h.AssertContains(t, output, "[builder] RESULT: Connected to the internet")
							})
						})

						when("the network mode is set to default", func() {
							it("reports that build and detect are online", func() {
								cmd := combo.Pack.Exec(
									"build",
									repoName,
									"-p",
									filepath.Join("testdata", "mock_app"),
									"--buildpack",
									buildpackTgz,
									"--network",
									"default",
								)
								output := h.Run(t, cmd)
								h.AssertContains(t, output, "[detector] RESULT: Connected to the internet")
								h.AssertContains(t, output, "[builder] RESULT: Connected to the internet")
							})
						})

						when("the network mode is set to none", func() {
							it("reports that build and detect are offline", func() {
								cmd := combo.Pack.Exec(
									"build",
									repoName,
									"-p",
									filepath.Join("testdata", "mock_app"),
									"--buildpack",
									buildpackTgz,
									"--network",
									"none",
								)
								output := h.Run(t, cmd)
								h.AssertContains(t, output, "[detector] RESULT: Disconnected from the internet")
								h.AssertContains(t, output, "[builder] RESULT: Disconnected from the internet")
							})
						})
					})

					when("--buildpack", func() {
						when("the argument is a tgz or id", func() {
							var notBuilderTgz string

							it.Before(func() {
								notBuilderTgz = h.CreateTGZ(t, filepath.Join(bpDir, "not-in-builder-buildpack"), "./", 0755)
							})

							it.After(func() {
								h.AssertNil(t, os.Remove(notBuilderTgz))
							})

							it("adds the buildpacks to the builder if necessary and runs them", func() {
								output := h.Run(t, combo.Pack.Exec(
									"build",
									repoName,
									"-p",
									filepath.Join("testdata", "mock_app"),
									"--buildpack",
									notBuilderTgz,
									"--buildpack",
									"simple/layers@simple-layers-version",
									"--buildpack",
									"noop.buildpack",
									"--buildpack",
									"read/env@latest",
									"--env",
									"DETECT_ENV_BUILDPACK=true",
								))
								h.AssertContains(t, output, "NOOP Buildpack")
								h.AssertContains(t, output, "Read Env Buildpack")
								h.AssertContains(t, output, fmt.Sprintf("Successfully built image '%s'", repoName))

								t.Log("app is runnable")
								assertMockAppRunsWithOutput(t, repoName,
									"Local Buildpack Dep Contents",
									"Launch Dep Contents",
									"Cached Dep Contents",
								)
							})
						})

						when("the argument is directory", func() {
							it("adds the buildpacks to the builder if necessary and runs them", func() {
								h.SkipIf(t, runtime.GOOS == "windows", "buildpack directories not supported on windows")

								output := h.Run(t, combo.Pack.Exec(
									"build",
									repoName,
									"-p",
									filepath.Join("testdata", "mock_app"),
									"--buildpack",
									filepath.Join(bpDir, "not-in-builder-buildpack"),
								))
								h.AssertContains(t, output, fmt.Sprintf("Successfully built image '%s'", repoName))
								t.Log("app is runnable")
								assertMockAppRunsWithOutput(t, repoName, "Local Buildpack Dep Contents")
							})
						})

						when("the buildpack stack doesn't match the builder", func() {
							var otherStackBuilderTgz string

							it.Before(func() {
								otherStackBuilderTgz = h.CreateTGZ(t, filepath.Join(bpDir, "other-stack-buildpack"), "./", 0755)
							})

							it.After(func() {
								h.AssertNil(t, os.Remove(otherStackBuilderTgz))
							})

							it("errors", func() {
								txt, err := h.RunE(combo.Pack.Exec(
									"build",
									repoName,
									"-p",
									filepath.Join("testdata", "mock_app"),
									"--buildpack",
									otherStackBuilderTgz,
								))
								h.AssertNotNil(t, err)
								h.AssertContains(t, txt, "other/stack/bp")
								h.AssertContains(t, txt, "other-stack-version")
								h.AssertContains(t, txt, "does not support stack 'pack.test.stack'")
							})
						})
					})

					when("--env-file", func() {
						var envPath string

						it.Before(func() {
							envfile, err := ioutil.TempFile("", "envfile")
							h.AssertNil(t, err)
							defer envfile.Close()

							err = os.Setenv("ENV2_CONTENTS", "Env2 Layer Contents From Environment")
							h.AssertNil(t, err)
							envfile.WriteString(`
            DETECT_ENV_BUILDPACK="true"
			ENV1_CONTENTS="Env1 Layer Contents From File"
			ENV2_CONTENTS
			`)
							envPath = envfile.Name()
						})

						it.After(func() {
							h.AssertNil(t, os.Unsetenv("ENV2_CONTENTS"))
							h.AssertNil(t, os.RemoveAll(envPath))
						})

						it("provides the env vars to the build and detect steps", func() {
							output := h.Run(t, combo.Pack.Exec(
								"build",
								repoName,
								"-p",
								filepath.Join("testdata", "mock_app"),
								"--env-file",
								envPath,
							))
							h.AssertContains(t, output, fmt.Sprintf("Successfully built image '%s'", repoName))
							assertMockAppRunsWithOutput(t,
								repoName,
								"Env2 Layer Contents From Environment",
								"Env1 Layer Contents From File",
							)
						})
					})

					when("--env", func() {
						it.Before(func() {
							h.AssertNil(t,
								os.Setenv("ENV2_CONTENTS", "Env2 Layer Contents From Environment"),
							)
						})

						it.After(func() {
							h.AssertNil(t, os.Unsetenv("ENV2_CONTENTS"))
						})

						it("provides the env vars to the build and detect steps", func() {
							output := h.Run(t, combo.Pack.Exec(
								"build",
								repoName,
								"-p",
								filepath.Join("testdata", "mock_app"),
								"--env",
								"DETECT_ENV_BUILDPACK=true",
								"--env",
								`ENV1_CONTENTS="Env1 Layer Contents From Command Line"`,
								"--env",
								"ENV2_CONTENTS",
							))
							h.AssertContains(t, output, fmt.Sprintf("Successfully built image '%s'", repoName))
							assertMockAppRunsWithOutput(t, repoName, "Env2 Layer Contents From Environment", "Env1 Layer Contents From Command Line")
						})
					})

					when("--run-image", func() {
						var runImageName string

						when("the run-image has the correct stack ID", func() {
							it.Before(func() {
								runImageName = h.CreateImageOnRemote(t, dockerCli, registry, "custom-run-image"+h.RandString(10), fmt.Sprintf(`
													FROM %s
													USER root
													RUN echo "custom-run" > /custom-run.txt
													USER pack
												`, runImage))
							})

							it.After(func() {
								h.DockerRmi(dockerCli, runImageName)
							})

							it("uses the run image as the base image", func() {
								output := h.Run(t, combo.Pack.Exec(
									"build",
									repoName,
									"-p",
									filepath.Join("testdata", "mock_app"),
									"--run-image",
									runImageName,
								))
								h.AssertContains(t, output, fmt.Sprintf("Successfully built image '%s'", repoName))

								t.Log("app is runnable")
								assertMockAppRunsWithOutput(t, repoName, "Launch Dep Contents", "Cached Dep Contents")

								t.Log("pulls the run image")
								h.AssertContains(t, output, fmt.Sprintf("Pulling image '%s'", runImageName))

								t.Log("uses the run image as the base image")
								assertHasBase(t, repoName, runImageName)
							})
						})

						when("the run image has the wrong stack ID", func() {
							it.Before(func() {
								runImageName = h.CreateImageOnRemote(t, dockerCli, registry, "custom-run-image"+h.RandString(10), fmt.Sprintf(`
													FROM %s
													LABEL io.buildpacks.stack.id=other.stack.id
													USER pack
												`, runImage))

							})

							it.After(func() {
								h.DockerRmi(dockerCli, runImageName)
							})

							it("fails with a message", func() {
								txt, err := h.RunE(combo.Pack.Exec(
									"build",
									repoName,
									"-p",
									filepath.Join("testdata", "mock_app"),
									"--run-image",
									runImageName,
								))
								h.AssertNotNil(t, err)
								h.AssertContains(t, txt, "run-image stack id 'other.stack.id' does not match builder stack 'pack.test.stack'")
							})
						})
					})

					when("--publish", func() {
						it("creates image on the registry", func() {
							output := h.Run(t, combo.Pack.Exec(
								"build",
								repoName,
								"-p",
								filepath.Join("testdata", "mock_app"),
								"--publish",
							))
							h.AssertContains(t, output, fmt.Sprintf("Successfully built image '%s'", repoName))

							t.Log("checking that registry has contents")
							contents, err := registry.RegistryCatalog()
							h.AssertNil(t, err)
							if !strings.Contains(contents, repo) {
								t.Fatalf("Expected to see image %s in %s", repo, contents)
							}

							h.AssertNil(t, h.PullImageWithAuth(dockerCli, repoName, registry.RegistryAuth()))
							defer h.DockerRmi(dockerCli, repoName)

							t.Log("app is runnable")
							assertMockAppRunsWithOutput(t, repoName, "Launch Dep Contents", "Cached Dep Contents")

							if combo.Pack.Supports("inspect-image") {
								t.Log("inspect-image")
								output = h.Run(t, combo.Pack.Exec("inspect-image", repoName))

								outputTemplate := filepath.Join(packFixturesDir, "inspect_image_published_output.txt")
								if _, err := os.Stat(outputTemplate); err != nil {
									t.Fatal(err.Error())
								}
								expectedOutput := fillTemplate(t, outputTemplate,
									map[string]interface{}{
										"image_name":           repoName,
										"base_image_ref":       strings.Join([]string{runImageMirror, h.Digest(t, runImageMirror)}, "@"),
										"base_image_top_layer": h.TopLayerDiffID(t, runImageMirror),
										"run_image_mirror":     runImageMirror,
										"show_reference":       !lifecycleDescriptor.Info.Version.LessThan(semver.MustParse("0.5.0")),
										"show_processes":       !lifecycleDescriptor.Info.Version.LessThan(semver.MustParse("0.6.0")),
									},
								)
								h.AssertEq(t, output, expectedOutput)
							}
						})
					})

					when("ctrl+c", func() {
						it("stops the execution", func() {
							var buf bytes.Buffer
							cmd := combo.Pack.Exec("build", repoName, "-p", filepath.Join("testdata", "mock_app"))
							cmd.Stdout = &buf
							cmd.Stderr = &buf

							h.AssertNil(t, cmd.Start())

							go terminateAtStep(t, cmd, &buf, "[detector]")

							err := cmd.Wait()
							h.AssertNotNil(t, err)
							h.AssertNotContains(t, buf.String(), "Successfully built image")
						})
					})
				})

				when("default builder is not set", func() {
					it("informs the user", func() {
						cmd := combo.Pack.Exec("build", repoName, "-p", filepath.Join("testdata", "mock_app"))
						output, err := h.RunE(cmd)
						h.AssertNotNil(t, err)
						h.AssertContains(t, output, `Please select a default builder with:`)
						h.AssertMatch(t, output, `Cloud Foundry:\s+'cloudfoundry/cnb:bionic'`)
						h.AssertMatch(t, output, `Cloud Foundry:\s+'cloudfoundry/cnb:cflinuxfs3'`)
						h.AssertMatch(t, output, `Heroku:\s+'heroku/buildpacks:18'`)
					})
				})
			})

			when("inspect-builder", func() {
				it("displays configuration for a builder (local and remote)", func() {
					configuredRunImage := "some-registry.com/pack-test/run1"
					output := h.Run(t, combo.Pack.Exec("set-run-image-mirrors", "pack-test/run", "--mirror", configuredRunImage))
					h.AssertEq(t, output, "Run Image 'pack-test/run' configured with mirror 'some-registry.com/pack-test/run1'\n")

					output = h.Run(t, combo.Pack.Exec("inspect-builder", builderName))

					outputTemplate := filepath.Join(packFixturesDir, "inspect_builder_output.txt")

					// If a different version of pack had created the builder, we need a different (versioned) template for expected output
					versionedTemplate := filepath.Join(packFixturesDir, fmt.Sprintf("inspect_%s_builder_output.txt", strings.TrimPrefix(strings.Split(combo.PackForCreateBuilder.Version, " ")[0], "v")))
					if _, err := os.Stat(versionedTemplate); err == nil {
						outputTemplate = versionedTemplate
					} else if !os.IsNotExist(err) {
						t.Fatal(err.Error())
					}

					expectedOutput := fillTemplate(t, outputTemplate,
						map[string]interface{}{
							"builder_name":          builderName,
							"lifecycle_version":     lifecycleDescriptor.Info.Version.String(),
							"buildpack_api_version": lifecycleDescriptor.API.BuildpackVersion.String(),
							"platform_api_version":  lifecycleDescriptor.API.PlatformVersion.String(),
							"run_image_mirror":      runImageMirror,
							"pack_version":          combo.PackForCreateBuilder.Version,
						},
					)

					h.AssertEq(t, output, expectedOutput)
				})
			})

			when("rebase", func() {
				var repoName, runBefore, origID string
				var buildRunImage func(string, string, string)

				it.Before(func() {
					repoName = registry.RepoName("some-org/" + h.RandString(10))
					runBefore = registry.RepoName("run-before/" + h.RandString(10))

					buildRunImage = func(newRunImage, contents1, contents2 string) {
						h.CreateImage(t, dockerCli, newRunImage, fmt.Sprintf(`
													FROM %s
													USER root
													RUN echo %s > /contents1.txt
													RUN echo %s > /contents2.txt
													USER pack
												`, runImage, contents1, contents2))
					}
					buildRunImage(runBefore, "contents-before-1", "contents-before-2")
					h.Run(t, combo.Pack.Exec(
						"build",
						repoName,
						"-p",
						filepath.Join("testdata",
							"mock_app"),
						"--builder",
						builderName,
						"--run-image",
						runBefore,
						"--no-pull",
					))
					origID = h.ImageID(t, repoName)
					assertMockAppRunsWithOutput(t, repoName, "contents-before-1", "contents-before-2")
				})

				it.After(func() {
					h.DockerRmi(dockerCli, origID, repoName, runBefore)
					ref, err := name.ParseReference(repoName, name.WeakValidation)
					h.AssertNil(t, err)
					buildCacheVolume := cache.NewVolumeCache(ref, "build", dockerCli)
					launchCacheVolume := cache.NewVolumeCache(ref, "launch", dockerCli)
					h.AssertNil(t, buildCacheVolume.Clear(context.TODO()))
					h.AssertNil(t, launchCacheVolume.Clear(context.TODO()))
				})

				when("daemon", func() {
					when("--run-image", func() {
						var runAfter string

						it.Before(func() {
							runAfter = registry.RepoName("run-after/" + h.RandString(10))
							buildRunImage(runAfter, "contents-after-1", "contents-after-2")
						})

						it.After(func() {
							h.AssertNil(t, h.DockerRmi(dockerCli, runAfter))
						})

						it("uses provided run image", func() {
							cmd := combo.Pack.Exec("rebase", repoName, "--no-pull", "--run-image", runAfter)
							output := h.Run(t, cmd)

							h.AssertContains(t, output, fmt.Sprintf("Successfully rebased image '%s'", repoName))
							assertMockAppRunsWithOutput(t, repoName, "contents-after-1", "contents-after-2")
						})
					})

					when("local config has a mirror", func() {
						var localRunImageMirror string

						it.Before(func() {
							localRunImageMirror = registry.RepoName("run-after/" + h.RandString(10))
							buildRunImage(localRunImageMirror, "local-mirror-after-1", "local-mirror-after-2")
							cmd := combo.Pack.Exec("set-run-image-mirrors", runImage, "-m", localRunImageMirror)
							h.Run(t, cmd)
						})

						it.After(func() {
							h.AssertNil(t, h.DockerRmi(dockerCli, localRunImageMirror))
						})

						it("prefers the local mirror", func() {
							cmd := combo.Pack.Exec("rebase", repoName, "--no-pull")
							output := h.Run(t, cmd)

							h.AssertContains(t, output, fmt.Sprintf("Selected run image mirror '%s' from local config", localRunImageMirror))

							h.AssertContains(t, output, fmt.Sprintf("Successfully rebased image '%s'", repoName))
							assertMockAppRunsWithOutput(t, repoName, "local-mirror-after-1", "local-mirror-after-2")
						})
					})

					when("image metadata has a mirror", func() {
						it.Before(func() {
							// clean up existing mirror first to avoid leaking images
							h.AssertNil(t, h.DockerRmi(dockerCli, runImageMirror))

							buildRunImage(runImageMirror, "mirror-after-1", "mirror-after-2")
						})

						it("selects the best mirror", func() {
							cmd := combo.Pack.Exec("rebase", repoName, "--no-pull")
							output := h.Run(t, cmd)

							h.AssertContains(t, output, fmt.Sprintf("Selected run image mirror '%s'", runImageMirror))

							h.AssertContains(t, output, fmt.Sprintf("Successfully rebased image '%s'", repoName))
							assertMockAppRunsWithOutput(t, repoName, "mirror-after-1", "mirror-after-2")
						})
					})
				})

				when("--publish", func() {
					it.Before(func() {
						h.AssertNil(t, h.PushImage(dockerCli, repoName, registry))
					})

					when("--run-image", func() {
						var runAfter string

						it.Before(func() {
							runAfter = registry.RepoName("run-after/" + h.RandString(10))
							buildRunImage(runAfter, "contents-after-1", "contents-after-2")
							h.AssertNil(t, h.PushImage(dockerCli, runAfter, registry))
						})

						it.After(func() {
							h.DockerRmi(dockerCli, runAfter)
						})

						it("uses provided run image", func() {
							cmd := combo.Pack.Exec("rebase", repoName, "--publish", "--run-image", runAfter)
							output := h.Run(t, cmd)

							h.AssertContains(t, output, fmt.Sprintf("Successfully rebased image '%s'", repoName))
							h.AssertNil(t, h.PullImageWithAuth(dockerCli, repoName, registry.RegistryAuth()))
							assertMockAppRunsWithOutput(t, repoName, "contents-after-1", "contents-after-2")
						})
					})
				})
			})

			when("run", func() {
				it.Before(func() {
					h.SkipIf(t, runtime.GOOS == "windows", "Skipping because windows fails to clean up properly")
				})

				when("there is a builder", func() {
					var (
						listeningPort string
						err           error
					)

					it.Before(func() {
						listeningPort, err = h.GetFreePort()
						h.AssertNil(t, err)
					})

					it.After(func() {
						absPath, err := filepath.Abs(filepath.Join("testdata", "mock_app"))
						h.AssertNil(t, err)

						sum := sha256.Sum256([]byte(absPath))
						repoName := fmt.Sprintf("pack.local/run/%x", sum[:8])
						ref, err := name.ParseReference(repoName, name.WeakValidation)
						h.AssertNil(t, err)

						h.DockerRmi(dockerCli, repoName)

						cache.NewImageCache(ref, dockerCli).Clear(context.TODO())
						cache.NewVolumeCache(ref, "build", dockerCli).Clear(context.TODO())
						cache.NewVolumeCache(ref, "launch", dockerCli).Clear(context.TODO())
					})

					it("starts an image", func() {
						var buf bytes.Buffer
						cmd := combo.Pack.Exec(
							"run",
							"--port",
							listeningPort+":8080",
							"-p",
							filepath.Join("testdata", "mock_app"),
							"--builder",
							builderName,
						)
						cmd.Stdout = &buf
						cmd.Stderr = &buf
						h.AssertNil(t, cmd.Start())

						defer ctrlCProc(cmd)

						h.AssertEq(t, isCommandRunning(cmd), true)
						assertMockAppResponseContains(t, listeningPort, 30*time.Second, "Launch Dep Contents", "Cached Dep Contents")
					})
				})

				when("default builder is not set", func() {
					it("informs the user", func() {
						cmd := combo.Pack.Exec("run", "-p", filepath.Join("testdata", "mock_app"))
						output, err := h.RunE(cmd)
						h.AssertNotNil(t, err)
						h.AssertContains(t, output, `Please select a default builder with:`)
						h.AssertMatch(t, output, `Cloud Foundry:\s+'cloudfoundry/cnb:bionic'`)
						h.AssertMatch(t, output, `Cloud Foundry:\s+'cloudfoundry/cnb:cflinuxfs3'`)
						h.AssertMatch(t, output, `Heroku:\s+'heroku/buildpacks:18'`)
					})
				})
			})
		})
	})

	when("suggest-builders", func() {
		it("displays suggested builders", func() {
			cmd := combo.Pack.Exec("suggest-builders")
			output, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("suggest-builders command failed: %s: %s", output, err)
			}
			h.AssertContains(t, string(output), "Suggested builders:")
			h.AssertContains(t, string(output), "cloudfoundry/cnb:bionic")
		})
	})

	when("suggest-stacks", func() {
		it("displays suggested stacks", func() {
			cmd := combo.Pack.Exec("suggest-stacks")
			output, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("suggest-stacks command failed: %s: %s", output, err)
			}
			h.AssertContains(t, string(output), "Stacks maintained by the community:")
		})
	})

	when("set-default-builder", func() {
		it("sets the default-stack-id in ~/.pack/config.toml", func() {
			output := h.Run(t, combo.Pack.Exec("set-default-builder", "cloudfoundry/cnb:bionic"))
			h.AssertContains(t, output, "Builder 'cloudfoundry/cnb:bionic' is now the default builder")
		})
	})

	when("create-package", func() {
		var tmpDir string

		it.Before(func() {
			h.SkipIf(t, !combo.Pack.Supports("create-package"), "pack does not support 'create-package'")

			var err error
			tmpDir, err = ioutil.TempDir("", "create-package-tests")
			h.AssertNil(t, err)

			h.CopyFile(t, filepath.Join(packFixturesDir, "package.toml"), filepath.Join(tmpDir, "package.toml"))

			err = os.Rename(
				h.CreateTGZ(t, filepath.Join(bpDir, "noop-buildpack"), "./", 0755),
				filepath.Join(tmpDir, "noop-buildpack.tgz"),
			)
			h.AssertNil(t, err)

			err = os.Rename(
				h.CreateTGZ(t, filepath.Join(bpDir, "simple-layers-buildpack"), "./", 0755),
				filepath.Join(tmpDir, "simple-layers-buildpack.tgz"),
			)
			h.AssertNil(t, err)

		})

		it.After(func() {
			h.AssertNil(t, os.RemoveAll(tmpDir))
		})

		createPackageLocally := func(absConfigPath string) string {
			t.Helper()
			packageName := "test/package-" + h.RandString(10)
			output, err := h.RunE(combo.Pack.Exec("create-package", packageName, "-p", absConfigPath))
			h.AssertNil(t, err)
			h.AssertContains(t, output, fmt.Sprintf("Successfully created package '%s'", packageName))
			return packageName
		}

		createPackageRemotely := func(absConfigPath string) string {
			t.Helper()
			packageName := registry.RepoName("test/package-" + h.RandString(10))
			output, err := h.RunE(combo.Pack.Exec("create-package", packageName, "-p", absConfigPath, "--publish"))
			h.AssertNil(t, err)
			h.AssertContains(t, output, fmt.Sprintf("Successfully published package '%s'", packageName))
			return packageName
		}

		assertImageExistsLocally := func(name string) {
			t.Helper()
			_, _, err := dockerCli.ImageInspectWithRaw(context.Background(), name)
			h.AssertNil(t, err)

		}

		generateAggregatePackageToml := func(nestedPackageName string) string {
			t.Helper()
			packageTomlData := fillTemplate(t,
				filepath.Join(packFixturesDir, "package_aggregate.toml"),
				map[string]interface{}{"PackageName": nestedPackageName},
			)
			packageTomlFile, err := ioutil.TempFile(tmpDir, "package_aggregate-*.toml")
			h.AssertNil(t, err)
			_, err = io.WriteString(packageTomlFile, packageTomlData)
			h.AssertNil(t, err)
			h.AssertNil(t, packageTomlFile.Close())

			return packageTomlFile.Name()
		}

		it("creates the package", func() {
			t.Log("package w/ only buildpacks")
			nestedPackageName := createPackageLocally(filepath.Join(tmpDir, "package.toml"))
			defer h.DockerRmi(dockerCli, nestedPackageName)
			t.Log("package w/ buildpacks and packages")
			assertImageExistsLocally(nestedPackageName)

			aggregatePackageToml := generateAggregatePackageToml(nestedPackageName)
			packageName := createPackageLocally(aggregatePackageToml)
			defer h.DockerRmi(dockerCli, packageName)
			assertImageExistsLocally(packageName)
		})

		when("--publish", func() {
			it("publishes image to registry", func() {
				nestedPackageName := createPackageRemotely(filepath.Join(tmpDir, "package.toml"))
				defer h.DockerRmi(dockerCli, nestedPackageName)
				aggregatePackageToml := generateAggregatePackageToml(nestedPackageName)

				packageName := registry.RepoName("test/package-" + h.RandString(10))
				defer h.DockerRmi(dockerCli, packageName)
				output := h.Run(t, combo.Pack.Exec("create-package", packageName, "-p", aggregatePackageToml, "--publish"))
				h.AssertContains(t, output, fmt.Sprintf("Successfully published package '%s'", packageName))

				_, _, err := dockerCli.ImageInspectWithRaw(context.Background(), packageName)
				h.AssertError(t, err, "No such image")

				h.AssertNil(t, h.PullImageWithAuth(dockerCli, packageName, registry.RegistryAuth()))

				_, _, err = dockerCli.ImageInspectWithRaw(context.Background(), packageName)
				h.AssertNil(t, err)
			})
		})

		when("--no-pull", func() {
			it("should use local image", func() {
				nestedPackage := createPackageLocally(filepath.Join(tmpDir, "package.toml"))
				defer h.DockerRmi(dockerCli, nestedPackage)
				aggregatePackageToml := generateAggregatePackageToml(nestedPackage)

				packageName := registry.RepoName("test/package-" + h.RandString(10))
				defer h.DockerRmi(dockerCli, packageName)
				h.Run(t, combo.Pack.Exec("create-package", packageName, "-p", aggregatePackageToml, "--no-pull"))

				_, _, err := dockerCli.ImageInspectWithRaw(context.Background(), packageName)
				h.AssertNil(t, err)

			})

			it("should not pull image from registry", func() {
				nestedPackage := createPackageRemotely(filepath.Join(tmpDir, "package.toml"))
				defer h.DockerRmi(dockerCli, nestedPackage)
				aggregatePackageToml := generateAggregatePackageToml(nestedPackage)

				packageName := registry.RepoName("test/package-" + h.RandString(10))
				defer h.DockerRmi(dockerCli, packageName)
				_, err := h.RunE(combo.Pack.Exec("create-package", packageName, "-p", aggregatePackageToml, "--no-pull"))
				h.AssertError(t, err, fmt.Sprintf("image '%s' does not exist on the daemon", nestedPackage))
			})
		})
	})

}

func buildpacksDir(bpAPIVersion api.Version) string {
	return filepath.Join("testdata", "mock_buildpacks", bpAPIVersion.String())
}

func createBuilder(t *testing.T, registry *h.TestRegistryConfig, runImageMirror, configDir string, pack h.PackUnderTest, lifecyclePath string, lifecycleDescriptor builder.LifecycleDescriptor) string {
	t.Log("creating builder image...")

	// CREATE TEMP WORKING DIR
	tmpDir, err := ioutil.TempDir("", "create-test-builder")
	h.AssertNil(t, err)
	defer os.RemoveAll(tmpDir)

	// DETERMINE TEST DATA
	buildpacksDir := buildpacksDir(*lifecycleDescriptor.API.BuildpackVersion)
	t.Log("using buildpacks from: ", buildpacksDir)
	h.RecursiveCopy(t, buildpacksDir, tmpDir)

	// ARCHIVE BUILDPACKS
	buildpacks := []string{
		"noop-buildpack",
		"not-in-builder-buildpack",
		"other-stack-buildpack",
		"read-env-buildpack",
		"simple-layers-buildpack", // from package
	}

	for _, v := range buildpacks {
		tgz := h.CreateTGZ(t, filepath.Join(buildpacksDir, v), "./", 0755)
		err := os.Rename(tgz, filepath.Join(tmpDir, v+".tgz"))
		h.AssertNil(t, err)
	}

	// CREATE PACKAGE
	packageImageName := createPackage(t, registry, configDir, tmpDir, pack)

	// RENDER builder.toml
	cfgData := fillTemplate(t, filepath.Join(configDir, "builder.toml"), map[string]interface{}{
		"package_name": packageImageName,
	})
	err = ioutil.WriteFile(filepath.Join(tmpDir, "builder.toml"), []byte(cfgData), os.ModePerm)
	h.AssertNil(t, err)

	builderConfigFile, err := os.OpenFile(filepath.Join(tmpDir, "builder.toml"), os.O_RDWR|os.O_APPEND, os.ModePerm)
	h.AssertNil(t, err)

	// ADD run-image-mirrors
	_, err = builderConfigFile.Write([]byte(fmt.Sprintf("run-image-mirrors = [\"%s\"]\n", runImageMirror)))
	h.AssertNil(t, err)

	// ADD lifecycle
	_, err = builderConfigFile.Write([]byte("[lifecycle]\n"))
	h.AssertNil(t, err)

	if lifecyclePath != "" {
		t.Logf("adding lifecycle path '%s' to builder config", lifecyclePath)
		_, err = builderConfigFile.Write([]byte(fmt.Sprintf("uri = \"%s\"\n", strings.ReplaceAll(lifecyclePath, `\`, `\\`))))
		h.AssertNil(t, err)
	} else {
		t.Logf("adding lifecycle version '%s' to builder config", lifecycleDescriptor.Info.Version.String())
		_, err = builderConfigFile.Write([]byte(fmt.Sprintf("version = \"%s\"\n", lifecycleDescriptor.Info.Version.String())))
		h.AssertNil(t, err)
	}

	builderConfigFile.Close()

	// NAME BUILDER
	bldr := registry.RepoName("test/builder-" + h.RandString(10))

	// CREATE BUILDER
	cmd := pack.Exec("create-builder", "--no-color", bldr, "-b", filepath.Join(tmpDir, "builder.toml"))
	output := h.Run(t, cmd)
	h.AssertContains(t, output, fmt.Sprintf("Successfully created builder image '%s'", bldr))
	h.AssertNil(t, h.PushImage(dockerCli, bldr, registry))

	return bldr
}

func createPackage(t *testing.T, registry *h.TestRegistryConfig, configDir, tmpDir string, pack h.PackUnderTest) string {
	t.Helper()
	t.Log("creating package image...")
	// COPY package.toml
	h.CopyFile(t, filepath.Join(configDir, "package.toml"), filepath.Join(tmpDir, "package.toml"))

	// NAME PACKAGE
	packageImageName := registry.RepoName("test/package-" + h.RandString(10))

	// CREATE PACKAGE
	cmd := pack.Exec("create-package", "--no-color", packageImageName, "-p", filepath.Join(tmpDir, "package.toml"))
	output := h.Run(t, cmd)
	h.AssertContains(t, output, fmt.Sprintf("Successfully created package '%s'", packageImageName))
	h.AssertNil(t, h.PushImage(dockerCli, packageImageName, registry))
	return packageImageName
}

func createStack(t *testing.T, registry *h.TestRegistryConfig, dockerCli *client.Client, runImageMirror string) error {
	t.Helper()
	t.Log("creating stack images...")

	if err := createStackImage(dockerCli, runImage, filepath.Join("testdata", "mock_stack", "run")); err != nil {
		return err
	}
	if err := createStackImage(dockerCli, buildImage, filepath.Join("testdata", "mock_stack", "build")); err != nil {
		return err
	}

	if err := dockerCli.ImageTag(context.Background(), runImage, runImageMirror); err != nil {
		return err
	}

	if err := h.PushImage(dockerCli, runImageMirror, registry); err != nil {
		return err
	}

	return nil
}

func createStackImage(dockerCli *client.Client, repoName string, dir string) error {
	ctx := context.Background()
	buildContext := archive.ReadDirAsTar(dir, "/", 0, 0, -1)

	res, err := dockerCli.ImageBuild(ctx, buildContext, dockertypes.ImageBuildOptions{
		Tags:        []string{repoName},
		Remove:      true,
		ForceRemove: true,
	})
	if err != nil {
		return err
	}

	_, err = io.Copy(ioutil.Discard, res.Body)
	if err != nil {
		return err
	}

	return res.Body.Close()
}

func assertMockAppRunsWithOutput(t *testing.T, repoName string, expectedOutputs ...string) {
	t.Helper()
	containerName := "test-" + h.RandString(10)
	runDockerImageExposePort(t, containerName, repoName)
	defer dockerCli.ContainerKill(context.TODO(), containerName, "SIGKILL")
	defer dockerCli.ContainerRemove(context.TODO(), containerName, dockertypes.ContainerRemoveOptions{Force: true})
	launchPort := fetchHostPort(t, containerName)
	assertMockAppResponseContains(t, launchPort, 10*time.Second, expectedOutputs...)
}

func assertMockAppResponseContains(t *testing.T, launchPort string, timeout time.Duration, expectedOutputs ...string) {
	t.Helper()
	resp := waitForResponse(t, launchPort, timeout)
	for _, expected := range expectedOutputs {
		h.AssertContains(t, resp, expected)
	}
}

func assertHasBase(t *testing.T, image, base string) {
	t.Helper()
	imageInspect, _, err := dockerCli.ImageInspectWithRaw(context.Background(), image)
	h.AssertNil(t, err)
	baseInspect, _, err := dockerCli.ImageInspectWithRaw(context.Background(), base)
	h.AssertNil(t, err)
	for i, layer := range baseInspect.RootFS.Layers {
		h.AssertEq(t, imageInspect.RootFS.Layers[i], layer)
	}
}

func fetchHostPort(t *testing.T, dockerID string) string {
	t.Helper()

	i, err := dockerCli.ContainerInspect(context.Background(), dockerID)
	h.AssertNil(t, err)
	for _, port := range i.NetworkSettings.Ports {
		for _, binding := range port {
			return binding.HostPort
		}
	}

	t.Fatalf("Failed to fetch host port for %s: no ports exposed", dockerID)
	return ""
}

func imgIDForRepoName(repoName string) (string, error) {
	inspect, _, err := dockerCli.ImageInspectWithRaw(context.TODO(), repoName)
	if err != nil {
		return "", errors.Wrapf(err, "could not get image ID for image '%s'", repoName)
	}
	return inspect.ID, nil
}

func runDockerImageExposePort(t *testing.T, containerName, repoName string) string {
	t.Helper()
	ctx := context.Background()

	ctr, err := dockerCli.ContainerCreate(ctx, &container.Config{
		Image:        repoName,
		Env:          []string{"PORT=8080"},
		ExposedPorts: map[nat.Port]struct{}{"8080/tcp": {}},
		Healthcheck:  nil,
	}, &container.HostConfig{
		PortBindings: nat.PortMap{
			"8080/tcp": []nat.PortBinding{{}},
		},
		AutoRemove: true,
	}, nil, containerName)
	h.AssertNil(t, err)

	err = dockerCli.ContainerStart(ctx, ctr.ID, dockertypes.ContainerStartOptions{})
	h.AssertNil(t, err)
	return ctr.ID
}

func waitForResponse(t *testing.T, port string, timeout time.Duration) string {
	t.Helper()
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	for {
		select {
		case <-ticker.C:
			resp, err := h.HTTPGetE("http://localhost:"+port, map[string]string{})
			if err != nil {
				break
			}
			return resp
		case <-timer.C:
			t.Fatalf("timeout waiting for response: %v", timeout)
		}
	}
}

func ctrlCProc(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil || cmd.Process.Pid <= 0 {
		return fmt.Errorf("invalid pid: %#v", cmd)
	}
	if runtime.GOOS == "windows" {
		return exec.Command("taskkill", "/F", "/T", "/PID", fmt.Sprint(cmd.Process.Pid)).Run()
	}
	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		return err
	}
	_, err := cmd.Process.Wait()
	return err
}

func isCommandRunning(cmd *exec.Cmd) bool {
	_, err := os.FindProcess(cmd.Process.Pid)
	if err != nil {
		return false
	}
	return true
}

// FIXME : buf needs a mutex
func terminateAtStep(t *testing.T, cmd *exec.Cmd, buf *bytes.Buffer, pattern string) {
	t.Helper()
	var interruptSignal os.Signal

	if runtime.GOOS == "windows" {
		// Windows does not support os.Interrupt
		interruptSignal = os.Kill
	} else {
		interruptSignal = os.Interrupt
	}

	for {
		if strings.Contains(buf.String(), pattern) {
			h.AssertNil(t, cmd.Process.Signal(interruptSignal))
			return
		}
	}
}

func imageLabel(t *testing.T, dockerCli *client.Client, repoName, labelName string) string {
	t.Helper()
	inspect, _, err := dockerCli.ImageInspectWithRaw(context.Background(), repoName)
	h.AssertNil(t, err)
	label, ok := inspect.Config.Labels[labelName]
	if !ok {
		t.Errorf("expected label %s to exist", labelName)
	}
	return label
}

func fillTemplate(t *testing.T, templatePath string, data map[string]interface{}) string {
	t.Helper()
	outputTemplate, err := ioutil.ReadFile(templatePath)
	h.AssertNil(t, err)

	tpl := template.Must(template.New("").Parse(string(outputTemplate)))

	var expectedOutput bytes.Buffer
	err = tpl.Execute(&expectedOutput, data)
	h.AssertNil(t, err)

	return expectedOutput.String()
}
