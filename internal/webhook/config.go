package webhook

// FeatureFlag is the canonical name of the kill-switch declared in
// ADR §6 Reversibilidade. cmd/server reads it from env / config and
// gates handler registration on it. Default is *off* so a misdeployed
// container never serves requests at the new path.
const FeatureFlag = "webhook.security_v2.enabled"
