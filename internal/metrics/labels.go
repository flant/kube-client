package metrics

// Metric label keys used across the Kubernetes client metrics. Centralising
// them as constants prevents typos and keeps the label set obvious at call
// sites.
const (
	LabelVerb   = "verb"
	LabelURL    = "url"
	LabelCode   = "code"
	LabelMethod = "method"
	LabelHost   = "host"
)
