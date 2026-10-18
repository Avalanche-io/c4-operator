// Package quickstart carries the quickstart's sample manifests so `c4 farm
// up` builds its local farm from the same files the quickstart applies.
package quickstart

import _ "embed"

// Store is store.yaml: a one-node Garage store with a bucket, quickstart.
//
//go:embed store.yaml
var Store []byte

// StoreKey and StoreSecret are the store's local-trial credentials, as
// store.yaml sets them.
const (
	StoreKey    = "GKc4c4c4c4c4c4c4c4c4c4c4c4"
	StoreSecret = "c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4"
)
