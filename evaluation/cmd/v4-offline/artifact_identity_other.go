//go:build !linux

package main

import (
	"errors"
	"os"
)

func requireReadOnlyArtifactRoot(string) error {
	return errors.New("warm verified activation is supported only on Linux read-only mounts")
}

func identityFromFile(*os.File) (artifactIdentity, error) {
	return artifactIdentity{}, errors.New("stable artifact identity is supported only on Linux")
}
