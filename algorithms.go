// Copyright © 2018 Playground Global, LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package android is a parent package of Android-related code-signing implementations for APKs,
// system images, and OTA images.
package apksigner

import (
	"crypto"
)

// KeyAlgorithm is used to map strings used in e.g. config files to implementations.
type KeyAlgorithm string

const (
	RSA    KeyAlgorithm = "RSA"
	RSAPSS              = "RSAPSS"
	EC                  = "EC"
	DSA                 = "DSA"
)

// HashAlgorithm is used to map strings used in e.g. config files to implementations. This is
// partially redundant with crypto.Hash, but its purpose is to be able to basically map a string
// from a config file into a crypto.Hash elsewhere in code
type HashAlgorithm string

const (
	SHA256 HashAlgorithm = "SHA256"
	SHA512               = "SHA512"
)

// AsHash turns our string-based enum type into a Go crypto.Hash value.
func (h HashAlgorithm) AsHash() crypto.Hash {
	switch h {
	case SHA256:
		return crypto.SHA256
	case SHA512:
		return crypto.SHA512
	default:
		// panic is a smidge aggressive here, but we can't return nil and caller shouldn't have called
		// us on a string not listed above. in normal operation this is pretty bad.
		panic("unknown hash algorithm requested")
	}
}
