package formalbuild

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
)

// BaseImagePinsVersion identifies the pin document schema.
const BaseImagePinsVersion = "taskgate-formal-build-base-images-v1"

// BaseImageRoles are the two stages Dockerfile.formal builds from.
const (
	BuilderRole = "builder"
	RuntimeRole = "runtime"
)

// BaseImagePin pins one base image by registry content digest.
//
// Tag is retained beside Digest rather than replaced by it. The digest is what
// the build is bound to; the tag is what a reviewer reads to know which upstream
// line the pin came from, and losing it would make the pin unmaintainable.
type BaseImagePin struct {
	Role   string `json:"role"`
	Tag    string `json:"tag"`
	Digest string `json:"digest"`
}

// Reference is the immutable reference the builder uses.
//
// It is derived from the tag's repository and the pinned digest, never from the
// tag itself, so the reference cannot resolve to whatever the tag points at now.
func (pin BaseImagePin) Reference() (string, error) {
	if pin.Digest == "" {
		return "", fmt.Errorf("the %s base image %q is not pinned; a formal build cannot use a mutable tag "+
			"(record the pin with final-v5-gateway-build record-base-images)", pin.Role, pin.Tag)
	}
	repository, _, found := strings.Cut(pin.Tag, ":")
	if !found || repository == "" {
		return "", fmt.Errorf("the %s base image tag %q has no repository", pin.Role, pin.Tag)
	}
	return repository + "@" + pin.Digest, nil
}

// BaseImagePins is the source-controlled pin document.
type BaseImagePins struct {
	Version string         `json:"version"`
	Images  []BaseImagePin `json:"images"`
}

// LoadBaseImagePins reads and validates the pin document.
func LoadBaseImagePins(path string) (BaseImagePins, error) {
	var pins BaseImagePins
	payload, err := os.ReadFile(path)
	if err != nil {
		return pins, fmt.Errorf("read formal build base image pins: %w", err)
	}
	if err := json.Unmarshal(payload, &pins); err != nil {
		return pins, fmt.Errorf("decode formal build base image pins: %w", err)
	}
	if err := pins.Validate(); err != nil {
		return pins, err
	}
	return pins, nil
}

// Validate rejects a pin document that could not bind a build.
//
// Structure only: an unpinned digest is legal here so the document can be read
// in order to be filled in. Requiring the pins is Pinned's job, which the build
// path calls and the record path deliberately does not.
func (pins BaseImagePins) Validate() error {
	if pins.Version != BaseImagePinsVersion {
		return fmt.Errorf("formal build base image pin version %q is unsupported; want %s",
			pins.Version, BaseImagePinsVersion)
	}
	wanted := map[string]bool{BuilderRole: false, RuntimeRole: false}
	for _, pin := range pins.Images {
		seen, known := wanted[pin.Role]
		if !known {
			return fmt.Errorf("formal build base image pins name unknown role %q", pin.Role)
		}
		if seen {
			return fmt.Errorf("formal build base image pins name role %q twice", pin.Role)
		}
		wanted[pin.Role] = true
		if strings.TrimSpace(pin.Tag) == "" {
			return fmt.Errorf("the %s base image pin has no tag", pin.Role)
		}
		if pin.Digest != "" && !validDigestReference(pin.Digest) {
			return fmt.Errorf("the %s base image digest %q is not a sha256: content digest", pin.Role, pin.Digest)
		}
	}
	missing := make([]string, 0, len(wanted))
	for role, seen := range wanted {
		if !seen {
			missing = append(missing, role)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return fmt.Errorf("formal build base image pins omit role(s) %v", missing)
	}
	return nil
}

// Pin returns the pin for one role.
func (pins BaseImagePins) Pin(role string) (BaseImagePin, error) {
	for _, pin := range pins.Images {
		if pin.Role == role {
			return pin, nil
		}
	}
	return BaseImagePin{}, fmt.Errorf("formal build base image pins name no %s image", role)
}

// Pinned rejects a document that still carries an unrecorded digest.
//
// The formal build calls this and the ordinary developer build does not, which
// is the whole distinction: a developer may build from a tag, a qualification
// image may not.
func (pins BaseImagePins) Pinned() error {
	if err := pins.Validate(); err != nil {
		return err
	}
	var unpinned []string
	for _, pin := range pins.Images {
		if pin.Digest == "" {
			unpinned = append(unpinned, pin.Role+"("+pin.Tag+")")
		}
	}
	if len(unpinned) > 0 {
		sort.Strings(unpinned)
		return fmt.Errorf("the formal Gateway build requires digest-pinned base images; %v %s unpinned. "+
			"Record them with: go run ./evaluation/cmd/final-v5-gateway-build record-base-images",
			unpinned, plural(len(unpinned), "is", "are"))
	}
	return nil
}

func plural(count int, one, many string) string {
	if count == 1 {
		return one
	}
	return many
}

// validDigestReference accepts only sha256:<64 lowercase hex>.
func validDigestReference(value string) bool {
	const prefix = "sha256:"
	if !strings.HasPrefix(value, prefix) {
		return false
	}
	return validSHA256(strings.TrimPrefix(value, prefix))
}

// WriteBaseImagePins writes the document back in the reviewed on-disk shape.
//
// The comment block the file carries is preserved: it explains why the pins
// exist, and a rewrite that dropped it would leave the next reader with two
// opaque digests.
func WriteBaseImagePins(path string, pins BaseImagePins) error {
	if err := pins.Validate(); err != nil {
		return err
	}
	existing, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read formal build base image pins: %w", err)
	}
	var document map[string]json.RawMessage
	if err := json.Unmarshal(existing, &document); err != nil {
		return fmt.Errorf("decode formal build base image pins: %w", err)
	}
	encoded, err := json.MarshalIndent(pins.Images, "  ", "  ")
	if err != nil {
		return err
	}
	document["images"] = encoded
	rendered, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(rendered, '\n'), 0o644)
}
