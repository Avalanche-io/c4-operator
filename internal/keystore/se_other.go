//go:build !(darwin && cgo && c4se)

package keystore

import "errors"

// Without the Secure Enclave in this build (not a Mac, or a Mac build
// without -tags c4se), the store keeps key files.

func seAvailable() bool { return false }

var errNoSE = errors.New("this c4 is built without the Secure Enclave: its key is a key file")

func makeSE(string, Kind) (Key, error) { return nil, errNoSE }

func loadSE([]byte) (Key, error) {
	// a blob a build with the Secure Enclave made, read by one without it
	return nil, errors.New("this machine's key is in the Secure Enclave, and this c4 is built without it: use the c4 that made it")
}
