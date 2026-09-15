// Package work defines caller-owned work identity. Keys and tags are opaque:
// consumers may store and compare them, but assign them no tracker semantics.
package work

import (
	"crypto/sha256"
	"encoding/hex"
	"maps"
)

// Key identifies one unit of work. The empty key means no work is addressed.
type Key string

// Ref carries identity and optional caller-defined metadata.
type Ref struct {
	Key  Key               `json:"key,omitempty"`
	Tags map[string]string `json:"tags,omitempty"`
}

// Clone copies metadata so updating a reference cannot change another record.
func (r Ref) Clone() Ref { r.Tags = maps.Clone(r.Tags); return r }

// Matches reports whether the reference contains all the requested tags.
func (r Ref) Matches(tags map[string]string) bool {
	for k, v := range tags {
		if got, ok := r.Tags[k]; !ok || got != v {
			return false
		}
	}
	return true
}

// Filename is a bounded, collision-resistant path component, including on
// case-insensitive filesystems. Stores also verify the key inside the file.
func (k Key) Filename() string {
	sum := sha256.Sum256([]byte(k))
	return "work-" + hex.EncodeToString(sum[:]) + ".json"
}
