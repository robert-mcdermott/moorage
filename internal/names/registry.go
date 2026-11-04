package names

import "regexp"

var registryHost = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]*[a-z0-9])?(:[0-9]{1,5})?$`)

// ValidRegistryHost reports whether a name could be a container registry's: its host, as
// the engines look credentials up by.
func ValidRegistryHost(host string) bool { return registryHost.MatchString(host) }
