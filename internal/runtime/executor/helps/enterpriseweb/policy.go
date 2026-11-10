package enterpriseweb

import (
	"context"
	"strings"

	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/thinking"
)

const (
	PolicyArgonOnly    = "argon_only"
	PolicyExcludeArgon = "exclude_argon"
	PolicyUnclassified = "unclassified"
	ArgonModel         = ge.ArgonModel
)

func ValidModelPolicy(policy string) bool {
	return policy == PolicyArgonOnly || policy == PolicyExcludeArgon
}

func (c Credential) Policy() string {
	if !ValidModelPolicy(c.ModelPolicy) {
		return PolicyUnclassified
	}
	return c.ModelPolicy
}

// CanonicalModel accepts an already resolved upstream ID, never a public alias.
func CanonicalModel(model string) string {
	return thinking.ParseSuffix(strings.TrimPrefix(strings.TrimSpace(model), "models/")).ModelName
}

func (c Credential) AllowsModel(model string) bool {
	model = CanonicalModel(model)
	if model == "" || strings.EqualFold(model, "auto") || strings.EqualFold(model, "default") {
		return false
	}
	switch c.Policy() {
	case PolicyArgonOnly:
		return model == ArgonModel
	case PolicyExcludeArgon:
		return model != ArgonModel
	}
	return false
}

func (c Credential) CheckModelPolicy(model string) error {
	if c.Policy() == PolicyUnclassified {
		return Fail(503, "Web credential requires manual model classification before generation")
	}
	if !c.AllowsModel(model) {
		return Fail(400, "Web credential model policy does not allow this upstream model")
	}
	return nil
}

func (c Credential) AllowsModelWithExclusions(model string, excluded []string) bool {
	if !c.AllowsModel(model) {
		return false
	}
	for _, pattern := range excluded {
		if ge.MatchModelPattern(pattern, CanonicalModel(model)) {
			return false
		}
	}
	return true
}

type policyValidatorKey struct{}
type PolicyValidator func(authID string, credential Credential, model string) error

func WithPolicyValidator(ctx context.Context, validate PolicyValidator) context.Context {
	return context.WithValue(ctx, policyValidatorKey{}, validate)
}

// ValidateModelPolicy checks the immutable selection and the current dispatch policy.
func ValidateModelPolicy(ctx context.Context, authID string, c Credential, model string, exclusions ...[]string) error {
	if err := c.ValidateModelPolicy(model, exclusions...); err != nil {
		return err
	}
	if validate, ok := ctx.Value(policyValidatorKey{}).(PolicyValidator); ok && validate != nil {
		return validate(authID, c, model)
	}
	return nil
}

// ValidateModelPolicy checks a credential snapshot without consulting live state.
func (c Credential) ValidateModelPolicy(model string, exclusions ...[]string) error {
	if err := c.CheckModelPolicy(model); err != nil {
		return err
	}
	for _, excluded := range exclusions {
		if !c.AllowsModelWithExclusions(model, excluded) {
			return Fail(400, "Web model is excluded by the current configuration")
		}
	}
	return nil
}
