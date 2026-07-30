//go:build linux

package main

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

func requireReadOnlyArtifactRoot(path string) error {
	var stat unix.Statfs_t
	if err := unix.Statfs(path, &stat); err != nil {
		return err
	}
	if stat.Flags&unix.ST_RDONLY == 0 {
		return errors.New("warm verification requires the artifact root to be mounted read-only")
	}
	return nil
}

func identityFromFile(file *os.File) (artifactIdentity, error) {
	if file == nil {
		return artifactIdentity{}, errors.New("artifact file is required")
	}
	var stat unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &stat); err != nil {
		return artifactIdentity{}, err
	}
	return artifactIdentity{
		Device:           uint64(stat.Dev),
		Inode:            stat.Ino,
		Mode:             uint32(stat.Mode),
		Size:             stat.Size,
		ModifiedUnixNano: stat.Mtim.Sec*1_000_000_000 + stat.Mtim.Nsec,
		ChangedUnixNano:  stat.Ctim.Sec*1_000_000_000 + stat.Ctim.Nsec,
	}, nil
}
