package server

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"
)

type Scope string

const (
	ScopeProvision Scope = "provision"
	ScopeMoney     Scope = "money"
	ScopeRead      Scope = "read"
	ScopeAudit     Scope = "audit"
	ScopeOperator  Scope = "operator"
)

const minSecretLength = 32

var (
	clientIDPattern  = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
	namespacePattern = regexp.MustCompile(`^[a-z][a-z0-9_-]*(\.[a-z0-9_-]+)*$`)
	knownScopes      = []Scope{ScopeProvision, ScopeMoney, ScopeRead, ScopeAudit, ScopeOperator}
)

type Client struct {
	ID         string   `yaml:"id"`
	SecretEnv  string   `yaml:"secret_env"`
	LedgerID   string   `yaml:"ledger"`
	Scopes     []Scope  `yaml:"scopes"`
	Namespaces []string `yaml:"namespaces"`
	Purposes   []string `yaml:"purposes"`
}

type Clients struct {
	bySecretDigest map[[32]byte]*Client
}

func LoadClients(path string) (*Clients, error) {
	data, err := os.ReadFile(path) //nolint:gosec // the path is operator configuration, not request input
	if err != nil {
		return nil, err
	}
	return ParseClients(data, os.Getenv)
}

func ParseClients(data []byte, getenv func(string) string) (*Clients, error) {
	var file struct {
		Clients []*Client `yaml:"clients"`
	}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&file); err != nil {
		return nil, fmt.Errorf("clients file: %w", err)
	}
	if len(file.Clients) == 0 {
		return nil, errors.New("clients file: no clients")
	}
	clients := &Clients{bySecretDigest: make(map[[32]byte]*Client, len(file.Clients))}
	ids := make(map[string]bool, len(file.Clients))
	for _, c := range file.Clients {
		if err := c.validate(); err != nil {
			return nil, fmt.Errorf("client %q: %w", c.ID, err)
		}
		if ids[c.ID] {
			return nil, fmt.Errorf("client %q is declared twice", c.ID)
		}
		ids[c.ID] = true
		secret := getenv(c.SecretEnv)
		if len(secret) < minSecretLength {
			return nil, fmt.Errorf("client %q: %s must hold a secret of at least %d characters", c.ID, c.SecretEnv, minSecretLength)
		}
		digest := sha256.Sum256([]byte(secret))
		if _, taken := clients.bySecretDigest[digest]; taken {
			return nil, fmt.Errorf("client %q shares its secret with another client", c.ID)
		}
		clients.bySecretDigest[digest] = c
	}
	return clients, nil
}

func (c *Clients) Authenticate(secret string) (*Client, bool) {
	client, ok := c.bySecretDigest[sha256.Sum256([]byte(secret))]
	return client, ok
}

func (c *Client) Has(scope Scope) bool {
	return slices.Contains(c.Scopes, scope)
}

func (c *Client) AllowsNamespace(namespace string) bool {
	for _, prefix := range c.Namespaces {
		if namespace == prefix || strings.HasPrefix(namespace, prefix+".") {
			return true
		}
	}
	return false
}

func (c *Client) AllowsPurpose(purpose string) bool {
	return slices.Contains(c.Purposes, purpose)
}

func (c *Client) validate() error {
	if !clientIDPattern.MatchString(c.ID) {
		return fmt.Errorf("id must match %s", clientIDPattern)
	}
	if c.SecretEnv == "" {
		return errors.New("secret_env is required")
	}
	if c.LedgerID == "" {
		return errors.New("ledger is required")
	}
	if len(c.Scopes) == 0 {
		return errors.New("at least one scope is required")
	}
	for _, scope := range c.Scopes {
		if !slices.Contains(knownScopes, scope) {
			return fmt.Errorf("unknown scope %q", scope)
		}
	}
	for _, namespace := range c.Namespaces {
		if !namespacePattern.MatchString(namespace) {
			return fmt.Errorf("namespace %q must match %s", namespace, namespacePattern)
		}
	}
	return nil
}
