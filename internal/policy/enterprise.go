package policy

// enterprisePolicyFile is the one place the enterprise policy's name is
// spelled; it lives beside the user config, in ~/.centrol.
const enterprisePolicyFile = "enterprise-policy.json"

// EnterpriseSource answers policy keys from the enterprise policy file,
// which the enterprise tier writes and this tier only reads. It sits
// above every other source except the session contract, so a key it
// answers is shown and treated as locked by enterprise. Its behavior
// for an absent or invalid file is policyFile's.
type EnterpriseSource struct{ policyFile }

// NewEnterpriseSource reads ~/.centrol/enterprise-policy.json.
func NewEnterpriseSource() *EnterpriseSource {
	return NewEnterpriseSourceAt(centrolHomeFile(enterprisePolicyFile))
}

// NewEnterpriseSourceAt reads the policy from path instead of the
// default location. An empty path never finds a file.
func NewEnterpriseSourceAt(path string) *EnterpriseSource {
	return &EnterpriseSource{policyFile{name: SourceEnterprisePolicy, label: "enterprise policy", path: path, signed: signedPolicyBeside(path)}}
}
