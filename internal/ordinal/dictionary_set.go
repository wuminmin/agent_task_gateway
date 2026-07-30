package ordinal

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
)

const (
	DictionarySetVersion      = "taskgate-ordinal-dictionary-set-v1"
	dictionarySetDigestDomain = "TASKGATE-ORDINAL-DICTIONARY-SET-V1\x00"
)

// DictionarySetManifest is the shared Gateway/Control/receipt/cache binding
// for every snapshot publication used by one query.
type DictionarySetManifest struct {
	Version       string                `json:"version"`
	CatalogDigest string                `json:"catalog_digest"`
	Members       []DictionarySetMember `json:"members"`
}

type DictionarySetMember struct {
	PublicationName  string `json:"publication_name"`
	DictionaryDigest string `json:"dictionary_digest"`
	ManifestDigest   string `json:"manifest_digest"`
}

func NewDictionarySetManifest(catalogDigest string, members ...DictionarySetMember) (DictionarySetManifest, error) {
	result := DictionarySetManifest{Version: DictionarySetVersion, CatalogDigest: catalogDigest,
		Members: append([]DictionarySetMember(nil), members...)}
	sort.Slice(result.Members, func(i, j int) bool {
		if result.Members[i].PublicationName != result.Members[j].PublicationName {
			return result.Members[i].PublicationName < result.Members[j].PublicationName
		}
		if result.Members[i].DictionaryDigest != result.Members[j].DictionaryDigest {
			return result.Members[i].DictionaryDigest < result.Members[j].DictionaryDigest
		}
		return result.Members[i].ManifestDigest < result.Members[j].ManifestDigest
	})
	if err := result.Validate(); err != nil {
		return DictionarySetManifest{}, err
	}
	return result, nil
}

func (m DictionarySetManifest) Validate() error {
	if m.Version != DictionarySetVersion || !validDigest(m.CatalogDigest) || len(m.Members) == 0 {
		return fmt.Errorf("%w: dictionary set header", ErrInvalid)
	}
	seenPublications := make(map[string]struct{}, len(m.Members))
	dictionaryManifests := make(map[string]string, len(m.Members))
	previous := DictionarySetMember{}
	for index, member := range m.Members {
		if !validID(member.PublicationName) || !validDigest(member.DictionaryDigest) || !validDigest(member.ManifestDigest) {
			return fmt.Errorf("%w: dictionary set member %d", ErrInvalid, index)
		}
		if _, duplicate := seenPublications[member.PublicationName]; duplicate {
			return fmt.Errorf("%w: duplicate publication in dictionary set", ErrInvalid)
		}
		if existing, present := dictionaryManifests[member.DictionaryDigest]; present && existing != member.ManifestDigest {
			return fmt.Errorf("%w: one dictionary has conflicting manifests", ErrInvalid)
		}
		if index > 0 && !dictionarySetMemberLess(previous, member) {
			return fmt.Errorf("%w: dictionary set members are not canonical", ErrNonCanonical)
		}
		seenPublications[member.PublicationName] = struct{}{}
		dictionaryManifests[member.DictionaryDigest] = member.ManifestDigest
		previous = member
	}
	return nil
}

func (m DictionarySetManifest) Digest() (string, error) {
	if err := m.Validate(); err != nil {
		return "", err
	}
	hash := sha256.New()
	hash.Write([]byte(dictionarySetDigestDomain))
	writeString(hash, m.Version)
	writeString(hash, m.CatalogDigest)
	writeUint64(hash, uint64(len(m.Members)))
	for _, member := range m.Members {
		writeString(hash, member.PublicationName)
		writeString(hash, member.DictionaryDigest)
		writeString(hash, member.ManifestDigest)
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func dictionarySetMemberLess(left, right DictionarySetMember) bool {
	if left.PublicationName != right.PublicationName {
		return left.PublicationName < right.PublicationName
	}
	if left.DictionaryDigest != right.DictionaryDigest {
		return left.DictionaryDigest < right.DictionaryDigest
	}
	return left.ManifestDigest < right.ManifestDigest
}
