# infra

Terraform that deploys the ledger to the shared AWS sandbox. Local runs never need it.

The sandbox foundation (network, ECS cluster, service discovery, GitHub OIDC provider) belongs to Corridor's `infra/` and is published in SSM Parameter Store under `/sandbox/foundation/`. Apply it first.

## What it creates

| Resource | Notes |
| --- | --- |
| Aurora PostgreSQL 17 Serverless v2 | The ledger's own cluster (database per service), scaling to zero when idle |
| ECS Fargate (ARM64) service `ledger` | Private; registered in the foundation's service discovery namespace |
| Migration task | Migrations run as a one-off ECS task before the service updates: the task runs `ledger migrate` as the master user (RDS keeps its password in Secrets Manager), and each run also sets the `ledger_app` and `ledger_audit` passwords from their secrets |
| Secrets Manager entries | The database roles' passwords and one secret per ledger client (Corridor's worker, Corridor's auditor, the seed runner, the operator). The ledger verifies these, so it owns them; their ARNs are published under `/sandbox/ledger/clients/<client>/secret-arn` for the clients' own infra to read |
| Deploy role | Trusted by the foundation's GitHub OIDC provider, for this repository only |
| SSM parameter `/sandbox/ledger/endpoint` | Read by Corridor's `infra/` |

## Layout

```
infra/
├── modules/             database, service, secrets, deploy-role: copies of Corridor's modules
└── envs/sandbox/        main.tf, backend.tf
```

State lives in an encrypted S3 bucket with S3-native locking. Images are pinned by digest.

## Checks

| Check | Needs credentials |
| --- | --- |
| `make tf-validate`: `terraform fmt -check`, `terraform validate` without a backend, `tflint` | No; runs on every pull request |
| `terraform plan` and `apply` in `envs/sandbox` | Yes |

Generated secrets come from ephemeral resources written through write-only arguments, so no secret value appears in a plan or in state. No secret goes into a `.tfvars` file.
