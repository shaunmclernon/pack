package image

import (
	"github.com/buildpack/imgutil"
	"github.com/docker/docker/client"
	"github.com/google/go-containerregistry/pkg/authn"
)

type DefaultImageFactory struct {
	dockerClient *client.Client
	keychain     authn.Keychain
}

func NewFactory(dockerClient *client.Client, keychain authn.Keychain) *DefaultImageFactory {
	return &DefaultImageFactory{
		dockerClient: dockerClient,
		keychain:     keychain,
	}
}

func (f *DefaultImageFactory) NewImage(repoName string, local bool) (imgutil.Image, error) {
	if local {
		return imgutil.EmptyLocalImage(repoName, f.dockerClient), nil
	}

	return imgutil.NewRemoteImage(repoName, f.keychain)
}
