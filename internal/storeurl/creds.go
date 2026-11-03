package storeurl

import "github.com/Avalanche-io/c4/store"

// S3 credentials and their sources are the c4 library's: one credential
// chain for the whole C4 suite (store.Static, store.Ambient). storeurl's
// signatures name them under these names.
type (
	Credentials      = store.Credentials
	CredentialSource = store.CredentialSource
)

// keyed reports whether c holds keys to sign with.
func keyed(c Credentials) bool { return c.AccessKeyID != "" && c.SecretAccessKey != "" }
