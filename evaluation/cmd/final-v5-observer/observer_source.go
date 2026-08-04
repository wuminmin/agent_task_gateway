package main

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"io/fs"
	"sort"
)

// observerSources is the observer's own decision logic, embedded so a compiled
// binary can state which sources produced it.
//
// The files are named explicitly rather than matched with *.go, because a
// wildcard would also embed the test sources and make the identity of a
// production snapshot depend on test edits. Naming them creates the opposite
// risk -- a new source file silently escaping the inventory -- which
// TestObserverSourceInventoryCoversEveryPackageSource closes by reading the
// package directory and requiring an exact match.
//
//go:embed main.go snapshot_v2.go observer_source.go
var observerSources embed.FS

// observerSourceDomain separates this digest from every other digest.
const observerSourceDomain = "TASKGATE-FINAL-V5-OBSERVER-SOURCE-INVENTORY-V1"

// observerSourceSHA256 digests the embedded source inventory.
//
// It binds path and content with explicit length framing, so a rename that
// preserves the bytes and an edit that preserves the name are both visible, and
// no two inventories can collide by concatenation.
//
// This identity is the observer's own. The deployment harness separately seals
// the whole tracked source tree into the observer build manifest; that binds
// what was built, this binds what is running and answers a question the harness
// manifest cannot: whether the two snapshots of one window came out of the same
// observer.
func observerSourceSHA256() (string, error) {
	names, err := observerSourceNames()
	if err != nil {
		return "", err
	}
	inventory := make([]inventoryEntry, 0, len(names))
	for _, name := range names {
		content, err := observerSources.ReadFile(name)
		if err != nil {
			return "", fmt.Errorf("read embedded observer source %q: %w", name, err)
		}
		inventory = append(inventory, inventoryEntry{name: name, content: content})
	}
	return inventoryDigest(inventory), nil
}

// inventoryEntry is one source file in the observer's inventory.
type inventoryEntry struct {
	name    string
	content []byte
}

// inventoryDigest binds path and content with explicit length framing.
//
// A rename that preserves the bytes and an edit that preserves the name are
// both visible, and no two inventories can collide by concatenation.
func inventoryDigest(inventory []inventoryEntry) string {
	hash := sha256.New()
	hash.Write([]byte(observerSourceDomain))
	hash.Write([]byte{0})
	fmt.Fprintf(hash, "files\x00%d\x00", len(inventory))
	for _, entry := range inventory {
		digest := sha256.Sum256(entry.content)
		fmt.Fprintf(hash, "%d\x00%s\x00%s\x00", len(entry.name), entry.name, hex.EncodeToString(digest[:]))
	}
	return hex.EncodeToString(hash.Sum(nil))
}

// observerSourceNames lists the embedded sources in canonical order.
func observerSourceNames() ([]string, error) {
	var names []string
	err := fs.WalkDir(observerSources, ".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() {
			names = append(names, path)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("enumerate the embedded observer sources: %w", err)
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("the observer embedded no sources; its identity would be a constant")
	}
	sort.Strings(names)
	return names, nil
}
