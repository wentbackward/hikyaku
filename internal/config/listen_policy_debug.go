//go:build !hardened

package config

// hardenedListenPolicy is a no-op in inspect builds: the existing
// allow_plaintext escape hatch in ValidateListenPolicy applies unchanged.
func hardenedListenPolicy(_ *Config, _ listenPolicy) error { return nil }
