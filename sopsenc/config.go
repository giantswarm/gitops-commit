package sopsenc

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	sopage "github.com/getsops/sops/v3/age"
	"gopkg.in/yaml.v3"
)

var (
	// ErrNoRule is returned when no creation rule of the .sops.yaml matches a
	// secret file's path; such a file is never committed in plaintext.
	ErrNoRule = errors.New("no creation rule matches")
	// ErrNoRecipients is returned for a creation rule without age recipients;
	// age public keys are the only key type the package encrypts for.
	ErrNoRecipients = errors.New("creation rule has no age recipients")
)

// Config is a repository's parsed .sops.yaml.
type Config struct {
	rules []Rule
}

// Rule is one creation rule: the paths it covers and the recipients it
// encrypts for. Files match the first rule in .sops.yaml order; a rule
// without path_regex matches every path.
type Rule struct {
	PathRegex *regexp.Regexp
	// Recipients are age public keys.
	Recipients        []string
	EncryptedRegex    string
	UnencryptedRegex  string
	EncryptedSuffix   string
	UnencryptedSuffix string
}

type rawConfig struct {
	CreationRules []rawRule `yaml:"creation_rules"`
}

type rawRule struct {
	PathRegex         string `yaml:"path_regex"`
	Age               string `yaml:"age"`
	EncryptedRegex    string `yaml:"encrypted_regex"`
	UnencryptedRegex  string `yaml:"unencrypted_regex"`
	EncryptedSuffix   string `yaml:"encrypted_suffix"`
	UnencryptedSuffix string `yaml:"unencrypted_suffix"`
}

// ParseConfig parses a .sops.yaml. Every rule's path_regex must compile and
// every rule must name at least one valid age recipient.
func ParseConfig(sopsYAML []byte) (Config, error) {
	var raw rawConfig
	if err := yaml.Unmarshal(sopsYAML, &raw); err != nil {
		return Config{}, fmt.Errorf("parsing .sops.yaml: %w", err)
	}
	if len(raw.CreationRules) == 0 {
		return Config{}, errors.New(".sops.yaml has no creation_rules")
	}
	rules := make([]Rule, 0, len(raw.CreationRules))
	for i, r := range raw.CreationRules {
		rule, err := r.compile()
		if err != nil {
			return Config{}, fmt.Errorf("creation rule %d: %w", i, err)
		}
		rules = append(rules, rule)
	}
	return Config{rules: rules}, nil
}

func (r rawRule) compile() (Rule, error) {
	rule := Rule{
		EncryptedRegex:    r.EncryptedRegex,
		UnencryptedRegex:  r.UnencryptedRegex,
		EncryptedSuffix:   r.EncryptedSuffix,
		UnencryptedSuffix: r.UnencryptedSuffix,
	}
	if r.PathRegex != "" {
		re, err := regexp.Compile(r.PathRegex)
		if err != nil {
			return Rule{}, fmt.Errorf("path_regex: %w", err)
		}
		rule.PathRegex = re
	}
	for _, recipient := range strings.Split(r.Age, ",") {
		if recipient = strings.TrimSpace(recipient); recipient != "" {
			rule.Recipients = append(rule.Recipients, recipient)
		}
	}
	if len(rule.Recipients) == 0 {
		return Rule{}, ErrNoRecipients
	}
	if _, err := sopage.MasterKeysFromRecipients(strings.Join(rule.Recipients, ",")); err != nil {
		return Rule{}, fmt.Errorf("age recipients: %w", err)
	}
	return rule, nil
}

// Rule returns the first creation rule whose path_regex matches the
// repository-relative path.
func (c Config) Rule(path string) (Rule, error) {
	for _, rule := range c.rules {
		if rule.PathRegex == nil || rule.PathRegex.MatchString(path) {
			return rule, nil
		}
	}
	return Rule{}, fmt.Errorf("%w: %s", ErrNoRule, path)
}
