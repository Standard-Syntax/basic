package fixture

type providerConfig struct {
	Mode string
}

func alternateProvider(provider providerConfig) bool {
	if true &&
		provider.Mode == "alternate" {
		return true
	}
	return false
}
