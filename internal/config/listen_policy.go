package config

// listenPolicy holds runtime assertions an embedder can make about how the
// listening sockets are secured — facts that are NOT expressible in config.yaml
// because they depend on what the embedding program wires at startup rather than
// on any configured value.
type listenPolicy struct {
	// externalGatewayTLS asserts the gateway listener is served with a TLS
	// certificate the embedder supplies at runtime (e.g. via
	// tls.Config.GetCertificate for an attested RA-TLS cert), which therefore has
	// no server.tls.cert/key file path.
	externalGatewayTLS bool
}

// ListenPolicyOption configures Config.ValidateListenPolicy for programs that
// embed the proxy via the public facade and provide TLS material at runtime
// instead of from config files.
type ListenPolicyOption func(*listenPolicy)

// WithExternalGatewayTLS asserts that the embedding program serves the gateway
// listener with a TLS certificate it supplies at runtime — for example an
// attested RA-TLS certificate produced via tls.Config.GetCertificate, which has
// no server.tls.cert/key file path. With it set, the listen policy treats the
// gateway TLS requirement as satisfied in BOTH inspect and hardened builds.
//
// This is a code-level assertion, deliberately NOT a config field: the proxy's
// own binary never sets it (it has no runtime certificate source), so the
// secure-by-default file/opt-in rules are unchanged for ordinary deployments.
// Only an embedder that ACTUALLY wires such a certificate may pass it, and it
// should be passed from the same code path that builds the runtime tls.Config so
// the assertion cannot drift from reality.
func WithExternalGatewayTLS() ListenPolicyOption {
	return func(p *listenPolicy) { p.externalGatewayTLS = true }
}
