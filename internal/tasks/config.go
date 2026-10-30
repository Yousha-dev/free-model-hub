package tasks

// Config is the runtime configuration of the keep-alive engine (projected from global configuration by the composition root).
type Config struct {
	// EnabledRealms are the enabled realms (cn / global).
	EnabledRealms []string
}
