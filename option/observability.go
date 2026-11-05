package option

type ObservabilityOptions struct {
	Listen       string   `json:"listen,omitempty"`
	ListenPort   uint16   `json:"listen_port,omitempty"`
	Metrics      bool     `json:"metrics,omitempty"`
	MetricsUsers []string `json:"metrics_users,omitempty"`
	Diagnostics  bool     `json:"diagnostics,omitempty"`
	Secret       string   `json:"secret,omitempty"`
	HealthListen string   `json:"health_listen,omitempty"`
	HealthPort   uint16   `json:"health_port,omitempty"`
}
