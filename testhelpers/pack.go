package testhelpers

import (
	"bytes"
	"os"
	"os/exec"
	"strings"

	"github.com/pkg/errors"
)

type PackUnderTest struct {
	Path        string
	HomeDir     string
	FixturesDir string
	Version     string
}

func NewPackUnderTest(execPath string, homeDir string, fixtureDir string) (*PackUnderTest, error) {
	cmdFactory := func() *exec.Cmd {
		return exec.Command(execPath)
	}

	versionCmd := cmdFactory()
	versionCmd.Args = append(versionCmd.Args, "version")
	versionOutput, err := versionCmd.CombinedOutput()
	if err != nil {
		return nil, errors.Wrap(err, "getting pack version")
	}

	return &PackUnderTest{
		Path:        execPath,
		HomeDir:     homeDir,
		FixturesDir: fixtureDir,
		Version:     string(bytes.TrimSpace(versionOutput)),
	}, nil
}

func (p *PackUnderTest) Exec(command string, args ...string) *exec.Cmd {
	cmd := exec.Command(p.Path)
	cmd.Args = append(cmd.Args, command)
	cmd.Args = append(cmd.Args, args...)
	cmd.Args = append(cmd.Args, "--no-color", "--verbose")
	cmd.Env = append(os.Environ(), "PACK_HOME="+p.HomeDir)
	// if dockerConfig != "" {
	// 	cmd.Env = append(cmd.Env, "DOCKER_CONFIG="+dockerConfig)
	// }

	return cmd
}

// packSupports returns whether or not the provided pack binary supports a
// given command string. The command string can take one of three forms:
//   - "<command>" (e.g. "create-builder")
//   - "<flag>" (e.g. "--verbose")
//   - "<command> <flag>" (e.g. "build --network")
//
// Any other form will return false.
func (p *PackUnderTest) Supports(command string) bool {
	parts := strings.Split(command, " ")
	var cmd, search string
	switch len(parts) {
	case 1:
		search = parts[0]
		break
	case 2:
		cmd = parts[0]
		search = parts[1]
	default:
		return false
	}

	output, err := p.Exec("help", cmd).CombinedOutput()
	if err != nil {
		panic(err)
	}

	return bytes.Contains(output, []byte(search))
}
