terraform {
  backend "s3" {
    key          = "ledger/sandbox.tfstate"
    encrypt      = true
    use_lockfile = true
  }
}
