// Command reasoning-gateway reserves the non-networked process boundary.
package main

// Phase 5 exposes only the in-process gateway API. Starting this binary does
// not open a listener, resolve credentials, or invoke a provider.
func main() {}
