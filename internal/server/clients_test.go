package server_test

import (
	"maps"
	"strings"
	"testing"

	"github.com/xJohnNascimento/ledger/internal/server"
)

const clientsYAML = `
clients:
  - id: worker
    secret_env: WORKER_SECRET
    ledger: corridor
    scopes: [provision, money, read]
    namespaces: [corridor]
    purposes: [funds, cash, clearing, fee_revenue, spread_revenue, fx_difference]
  - id: seed
    secret_env: SEED_SECRET
    ledger: corridor
    scopes: [provision, money]
    namespaces: [seed]
    purposes: [funds, cash, funding_equity]
  - id: auditor
    secret_env: AUDITOR_SECRET
    ledger: corridor
    scopes: [read, audit]
  - id: operator
    secret_env: OPERATOR_SECRET
    ledger: corridor
    scopes: [read, operator]
    namespaces: [operator]
    purposes: [funds, cash, clearing, fee_revenue, spread_revenue, funding_equity, fx_difference]
  - id: stranger
    secret_env: STRANGER_SECRET
    ledger: other
    scopes: [provision, money, read, audit]
    namespaces: [other]
    purposes: [funds, fee_revenue, funding_equity]
`

var secrets = map[string]string{
	"WORKER_SECRET":   "worker-0123456789abcdef0123456789abcdef",
	"SEED_SECRET":     "seed-0123456789abcdef0123456789abcdef",
	"AUDITOR_SECRET":  "auditor-0123456789abcdef0123456789abcdef",
	"OPERATOR_SECRET": "operator-0123456789abcdef0123456789abcdef",
	"STRANGER_SECRET": "stranger-0123456789abcdef0123456789abcdef",
}

func secretsWith(key, value string) map[string]string {
	env := maps.Clone(secrets)
	if value == "" {
		delete(env, key)
		return env
	}
	env[key] = value
	return env
}

func getenv(key string) string {
	return secrets[key]
}

func TestClientsAuthenticateBySecret(t *testing.T) {
	clients, err := server.ParseClients([]byte(clientsYAML), getenv)
	if err != nil {
		t.Fatal(err)
	}
	client, ok := clients.Authenticate(secrets["SEED_SECRET"])
	if !ok || client.ID != "seed" {
		t.Fatalf("seed secret authenticated as %v, %v", client, ok)
	}
	if _, ok := clients.Authenticate("seed-0123456789abcdef0123456789abcdeF"); ok {
		t.Fatal("a wrong secret authenticated")
	}
	if _, ok := clients.Authenticate(""); ok {
		t.Fatal("an empty secret authenticated")
	}
}

func TestClientNamespacesArePrefixesOnDotBoundaries(t *testing.T) {
	clients, err := server.ParseClients([]byte(clientsYAML), getenv)
	if err != nil {
		t.Fatal(err)
	}
	worker, _ := clients.Authenticate(secrets["WORKER_SECRET"])
	for namespace, want := range map[string]bool{
		"corridor":                   true,
		"corridor.reserve-payout":    true,
		"corridor.commit.conversion": true,
		"corridorx":                  false,
		"corridor-x":                 false,
		"seed":                       false,
		"":                           false,
	} {
		if got := worker.AllowsNamespace(namespace); got != want {
			t.Errorf("AllowsNamespace(%q) = %v, want %v", namespace, got, want)
		}
	}
}

func TestClientsFileIsValidatedAtBoot(t *testing.T) {
	cases := map[string]struct {
		yaml string
		env  map[string]string
	}{
		"missing secret": {clientsYAML, secretsWith("SEED_SECRET", "")},
		"short secret":   {clientsYAML, secretsWith("WORKER_SECRET", "short")},
		"shared secret":  {clientsYAML, secretsWith("WORKER_SECRET", secrets["SEED_SECRET"])},
		"unknown scope":  {strings.Replace(clientsYAML, "[read, audit]\n", "[read, admin]\n", 1), secrets},
		"unknown field":  {strings.Replace(clientsYAML, "ledger: corridor\n    scopes: [read, audit]", "ledger: corridor\n    role: x\n    scopes: [read, audit]", 1), secrets},
		"duplicate id":   {strings.Replace(clientsYAML, "id: auditor", "id: seed", 1), secrets},
		"bad namespace":  {strings.Replace(clientsYAML, "[seed]", "[Seed.*]", 1), secrets},
		"no clients":     {"clients: []", secrets},
		"missing ledger": {strings.Replace(clientsYAML, "    ledger: corridor\n    scopes: [read, audit]", "    scopes: [read, audit]", 1), secrets},
	}
	if _, err := server.ParseClients([]byte(clientsYAML), getenv); err != nil {
		t.Fatalf("the valid file is refused: %v", err)
	}
	for name, c := range cases {
		env := c.env
		if _, err := server.ParseClients([]byte(c.yaml), func(k string) string { return env[k] }); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
