terraform {
  required_version = ">= 1.11"
  required_providers {
    aws = { source = "hashicorp/aws", version = "~> 6.0" }
  }
}

provider "aws" {
  region = var.region
  default_tags {
    tags = { Project = "ledger", Environment = "sandbox", ManagedBy = "terraform" }
  }
}

variable "region" {
  type    = string
  default = "us-east-1"
}

variable "image" {
  type        = string
  description = "The ledger image, pinned by digest"
}

data "aws_ssm_parameter" "foundation" {
  for_each = toset(["vpc-id", "vpc-cidr", "private-subnet-ids", "cluster-arn", "namespace-id", "namespace-name", "oidc-provider-arn"])
  name     = "/sandbox/foundation/${each.key}"
}

locals {
  foundation      = { for key, parameter in data.aws_ssm_parameter.foundation : key => nonsensitive(parameter.value) }
  private_subnets = split(",", local.foundation["private-subnet-ids"])
  database        = "postgres://%s@${module.database.endpoint}:5432/${module.database.database}?sslmode=require"
  # The ledger verifies these secrets, so it owns them; each client's own infra reads the ARN.
  clients = {
    corridor-worker = "LEDGER_SECRET_CORRIDOR_WORKER"
    corridor-audit  = "LEDGER_SECRET_CORRIDOR_AUDIT"
    seed            = "LEDGER_SECRET_SEED"
    operator        = "LEDGER_SECRET_OPERATOR"
  }
  passwords = {
    LEDGER_APP_PASSWORD   = "app-password"
    LEDGER_AUDIT_PASSWORD = "audit-password"
  }
}

module "secrets" {
  source    = "../../modules/secrets"
  prefix    = "/sandbox/ledger"
  generated = { for name in concat(keys(local.clients), values(local.passwords)) : name => "" }
}

module "database" {
  source     = "../../modules/database"
  name       = "ledger-sandbox"
  database   = "ledger"
  vpc_id     = local.foundation["vpc-id"]
  subnet_ids = local.private_subnets
}

# Migrations connect as the master user, whose password RDS keeps in Secrets Manager; pgx reads
# PGPASSWORD when the URL carries none. The run also sets the roles' passwords.
module "migrate" {
  source        = "../../modules/service"
  name          = "ledger-migrate"
  cluster_arn   = local.foundation["cluster-arn"]
  vpc_id        = local.foundation["vpc-id"]
  subnet_ids    = local.private_subnets
  image         = var.image
  command       = ["migrate"]
  desired_count = 0
  environment   = { LEDGER_DATABASE_URL = format(local.database, "postgres") }
  secrets = merge(
    { PGPASSWORD = "${module.database.master_secret_arn}:password::" },
    { for env, name in local.passwords : env => module.secrets.arns[name] },
  )
}

module "ledger" {
  source         = "../../modules/service"
  name           = "ledger"
  cluster_arn    = local.foundation["cluster-arn"]
  vpc_id         = local.foundation["vpc-id"]
  subnet_ids     = local.private_subnets
  namespace_id   = local.foundation["namespace-id"]
  image          = var.image
  command        = ["serve"]
  ports          = [9090]
  ingress_cidrs  = [local.foundation["vpc-cidr"]]
  health_command = ["CMD", "/ledger", "healthcheck"]
  environment = {
    LEDGER_DATABASE_URL       = format(local.database, "ledger_app")
    LEDGER_AUDIT_DATABASE_URL = format(local.database, "ledger_audit")
  }
  secrets = merge(
    { for env, name in local.passwords : env => module.secrets.arns[name] },
    { for client, env in local.clients : env => module.secrets.arns[client] },
  )
}

resource "aws_vpc_security_group_ingress_rule" "database" {
  for_each                     = { for service in [module.migrate, module.ledger] : service.name => service.security_group_id }
  security_group_id            = module.database.security_group_id
  referenced_security_group_id = each.value
  ip_protocol                  = "tcp"
  from_port                    = 5432
  to_port                      = 5432
}

module "deploy_role" {
  source            = "../../modules/deploy-role"
  repository        = "xJohnNascimento/ledger"
  oidc_provider_arn = local.foundation["oidc-provider-arn"]
  cluster_arn       = local.foundation["cluster-arn"]
  families          = ["ledger-migrate", "ledger"]
  pass_role_arns = flatten([
    for service in [module.migrate, module.ledger] : [service.execution_role_arn, service.task_role_arn]
  ])
}

resource "aws_ssm_parameter" "published" {
  for_each = merge(
    { endpoint = "http://ledger.${local.foundation["namespace-name"]}:9090" },
    { for client in keys(local.clients) : "clients/${client}/secret-arn" => module.secrets.arns[client] },
  )
  name  = "/sandbox/ledger/${each.key}"
  type  = "String"
  value = each.value
}

output "deploy_role_arn" {
  value = module.deploy_role.arn
}
