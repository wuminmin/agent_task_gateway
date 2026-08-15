package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"

	"taskbound.local/agent-data-gateway/internal/ordinal"
	"taskbound.local/agent-data-gateway/internal/snapshotbundle"
)

const (
	maximumBundleManifestBytes = 4 << 20
	maximumHotBytes            = 1024 << 20
	maximumSidecarBytes        = 512 << 20
)

type loadedPublication struct {
	Day       string
	Input     snapshotbundle.CompilerInput
	Bundle    snapshotbundle.BundleManifest
	Index     *ordinal.HotDictionary
	Directory string
}

func loadVerifiedPublication(day, inputDirectory, artifactRoot string) (loadedPublication, error) {
	input, err := readCompilerInput(filepath.Join(inputDirectory, day+".json"))
	if err != nil {
		return loadedPublication{}, err
	}
	if err := validateCandidateIdentity(day, input); err != nil {
		return loadedPublication{}, err
	}
	if err := requireExpectedDigests(input.ExpectedDigests); err != nil {
		return loadedPublication{}, err
	}
	directory := filepath.Join(artifactRoot, input.PublicationName)
	if err := verifyPublicationDirectory(directory, input.PublicationName); err != nil {
		return loadedPublication{}, err
	}
	manifestPath := filepath.Join(directory, input.PublicationName+".bundle.json")
	manifestBytes, err := readRegularFile(manifestPath, maximumBundleManifestBytes)
	if err != nil {
		return loadedPublication{}, err
	}
	bundle, err := snapshotbundle.DecodeBundleManifest(bytes.NewReader(manifestBytes))
	if err != nil {
		return loadedPublication{}, err
	}
	if err := matchInputBundle(input, bundle); err != nil {
		return loadedPublication{}, err
	}

	hotBytes, err := readDescriptorFile(directory, bundle.Hot, maximumHotBytes)
	if err != nil {
		return loadedPublication{}, fmt.Errorf("HOT: %w", err)
	}
	index, err := ordinal.ParseHotDictionary(hotBytes, bundle.ManifestDigest)
	if err != nil {
		return loadedPublication{}, fmt.Errorf("parse HOT: %w", err)
	}
	if err := matchHot(bundle, index); err != nil {
		return loadedPublication{}, err
	}
	if err := verifyColdDescriptor(filepath.Join(directory, bundle.Cold.Name), bundle.Cold, bundle.ManifestDigest); err != nil {
		return loadedPublication{}, fmt.Errorf("COLD: %w", err)
	}
	if err := verifySidecarDescriptor(filepath.Join(directory, bundle.Sidecar.Name), bundle.Sidecar, bundle, index); err != nil {
		return loadedPublication{}, fmt.Errorf("sidecar: %w", err)
	}
	return loadedPublication{Day: day, Input: input, Bundle: bundle, Index: index, Directory: directory}, nil
}

func requireExpectedDigests(value snapshotbundle.ExpectedDigests) error {
	for name, digest := range map[string]string{
		"sidecar": value.SidecarDigest, "dictionary": value.DictionaryDigest,
		"manifest": value.ManifestDigest, "cold payload": value.ColdPayloadDigest,
		"hot index": value.HotIndexDigest,
	} {
		if !sha256Regexp.MatchString(digest) {
			return fmt.Errorf("approved input %s digest is missing or invalid", name)
		}
	}
	return nil
}

func verifyPublicationDirectory(directory, publicationName string) error {
	info, err := os.Lstat(directory)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("publication path must be a real directory")
	}
	expected := map[string]struct{}{
		publicationName + ".bundle.json": {}, publicationName + ".hot.tgord": {},
		publicationName + ".cold.tgord": {}, publicationName + ".sidecar.ndjson": {},
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		return err
	}
	if len(entries) != len(expected) {
		return errors.New("publication directory is incomplete or contains extra files")
	}
	for _, entry := range entries {
		if _, ok := expected[entry.Name()]; !ok {
			return fmt.Errorf("unexpected publication file %q", entry.Name())
		}
		entryInfo, err := entry.Info()
		if err != nil || !entryInfo.Mode().IsRegular() || entryInfo.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("publication file %q is not regular", entry.Name())
		}
	}
	return nil
}

func matchInputBundle(input snapshotbundle.CompilerInput, bundle snapshotbundle.BundleManifest) error {
	manifest := bundle.DictionaryManifest
	expected := input.ExpectedDigests
	if bundle.PublicationName != input.PublicationName || bundle.CatalogSource != input.CatalogSource ||
		bundle.OrdinalSidecar != input.OrdinalSidecar || manifest.SourceID != input.Snapshot.SourceID ||
		manifest.SourceNamespace != input.Snapshot.SourceNamespace || manifest.Snapshot != input.Snapshot.Snapshot ||
		manifest.SchemaDigest != input.Snapshot.SchemaDigest || expected.ManifestDigest != bundle.ManifestDigest ||
		expected.DictionaryDigest != manifest.DictionaryDigest || expected.SidecarDigest != manifest.SidecarDigest ||
		expected.ColdPayloadDigest != manifest.ColdPayloadDigest || expected.HotIndexDigest != manifest.HotIndexDigest {
		return errors.New("bundle identity or digest differs from approved compiler input")
	}
	return nil
}

func matchHot(bundle snapshotbundle.BundleManifest, index *ordinal.HotDictionary) error {
	if index == nil || index.ManifestDigest() != bundle.ManifestDigest ||
		index.DictionaryDigest() != bundle.DictionaryManifest.DictionaryDigest ||
		index.RowCount() != bundle.RowCount || !reflect.DeepEqual(index.Manifest(), bundle.DictionaryManifest) {
		return errors.New("HOT index differs from bundle envelope")
	}
	first, found := index.LookupRow(1)
	handle, reverseFound := index.LookupRowHandle(first.EntityKey)
	if !found || first.EntityKey == "" || !reverseFound || handle != 1 {
		return errors.New("HOT index omits its entity-key sidecar binding")
	}
	return nil
}

func readRegularFile(path string, maximum int64) ([]byte, error) {
	file, size, err := openRegularFile(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	if size <= 0 || size > maximum || size > int64(^uint(0)>>1) {
		return nil, errors.New("artifact size is outside the activation limit")
	}
	payload, err := io.ReadAll(io.LimitReader(file, size+1))
	if err != nil || int64(len(payload)) != size {
		return nil, errors.New("artifact size changed while reading")
	}
	return payload, nil
}

func openRegularFile(path string) (*os.File, int64, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return nil, 0, errors.New("artifact must be a regular non-symlink file")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		_ = file.Close()
		return nil, 0, errors.New("artifact identity changed while opening")
	}
	return file, opened.Size(), nil
}

func readDescriptorFile(directory string, descriptor snapshotbundle.FileDescriptor, maximum int64) ([]byte, error) {
	if filepath.Base(descriptor.Name) != descriptor.Name || descriptor.Bytes <= 0 || descriptor.Bytes > maximum ||
		!sha256Regexp.MatchString(descriptor.SHA256) {
		return nil, errors.New("invalid artifact descriptor")
	}
	payload, err := readRegularFile(filepath.Join(directory, descriptor.Name), maximum)
	if err != nil {
		return nil, err
	}
	if int64(len(payload)) != descriptor.Bytes {
		return nil, errors.New("artifact size differs from descriptor")
	}
	digest := sha256.Sum256(payload)
	if hex.EncodeToString(digest[:]) != descriptor.SHA256 {
		return nil, errors.New("artifact SHA-256 differs from descriptor")
	}
	return payload, nil
}

func verifyColdDescriptor(path string, descriptor snapshotbundle.FileDescriptor, manifestDigest string) error {
	if filepath.Base(descriptor.Name) != descriptor.Name || descriptor.Bytes <= sha256.Size ||
		!sha256Regexp.MatchString(descriptor.SHA256) {
		return errors.New("invalid COLD descriptor")
	}
	file, size, err := openRegularFile(path)
	if err != nil {
		return err
	}
	defer file.Close()
	if size != descriptor.Bytes {
		return errors.New("COLD size differs from descriptor")
	}
	digest, err := ordinal.VerifyColdDictionaryEnvelopeReader(file, size, manifestDigest)
	if err != nil {
		return err
	}
	if hex.EncodeToString(digest[:]) != descriptor.SHA256 {
		return errors.New("COLD SHA-256 differs from descriptor")
	}
	return nil
}

func verifySidecarDescriptor(path string, descriptor snapshotbundle.FileDescriptor,
	bundle snapshotbundle.BundleManifest, index ordinal.SnapshotIndex) error {
	if filepath.Base(descriptor.Name) != descriptor.Name || descriptor.Bytes <= 0 ||
		descriptor.Bytes > maximumSidecarBytes || !sha256Regexp.MatchString(descriptor.SHA256) {
		return errors.New("invalid sidecar descriptor")
	}
	file, size, err := openRegularFile(path)
	if err != nil {
		return err
	}
	defer file.Close()
	if size != descriptor.Bytes {
		return errors.New("sidecar size differs from descriptor")
	}
	hash := sha256.New()
	expected := snapshotbundle.SidecarExpectation{
		PublicationName: bundle.PublicationName, OrdinalSidecar: bundle.OrdinalSidecar,
		SourceNamespace: bundle.DictionaryManifest.SourceNamespace,
		ManifestDigest:  bundle.ManifestDigest, SidecarDigest: bundle.DictionaryManifest.SidecarDigest,
	}
	if err := snapshotbundle.VerifySidecarNDJSON(io.TeeReader(file, hash), index, expected); err != nil {
		return err
	}
	if hex.EncodeToString(hash.Sum(nil)) != descriptor.SHA256 {
		return errors.New("sidecar SHA-256 differs from descriptor")
	}
	return nil
}
